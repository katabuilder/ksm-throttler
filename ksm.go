//
// Copyright (c) 2017-2018 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	ksig "github.com/kata-containers/ksm-throttler/pkg/signals"
	"github.com/sirupsen/logrus"
)

type ksmSetting struct {
	// pagesPerScanFactor describes how many pages we want
	// to scan per KSM run.
	// ksmd will san N pages, where N*pagesPerScanFactor is
	// equal to the number of anonymous pages.
	pagesPerScanFactor int64

	// scanIntervalMS is the KSM scan interval in milliseconds.
	scanIntervalMS uint32

	// run describes if we want KSM to be on or off.
	run bool
}

func anonPages() (int64, error) {
	// We're going to parse meminfo
	f, err := os.Open(memInfo)
	if err != nil {
		return -1, err
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()

		// We only care about anonymous pages
		if !strings.HasPrefix(line, "AnonPages:") {
			continue
		}

		// Extract the before last (value) and last (unit) fields
		fields := strings.Split(line, " ")
		value := fields[len(fields)-2]
		totalMemory, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return -1, fmt.Errorf("Invalid integer")
		}

		// meminfo gives us kB
		totalMemory *= 1024

		// Fetch the system page size
		pageSize := (int64)(os.Getpagesize())

		nPages := totalMemory / pageSize
		return nPages, nil
	}

	return 0, fmt.Errorf("Could not compute number of pages")
}

func (s ksmSetting) pagesToScan() (string, error) {
	if s.pagesPerScanFactor == 0 {
		return "", errors.New("Invalid KSM setting")
	}

	nPages, err := anonPages()
	if err != nil {
		return "", err
	}

	pagesToScan := nPages / s.pagesPerScanFactor

	return fmt.Sprintf("%v", pagesToScan), nil
}

type ksmMode string

const (
	ksmInitial    ksmMode = "initial"
	ksmOff        ksmMode = "off"
	ksmSlow       ksmMode = "slow"
	ksmStandard   ksmMode = "standard"
	ksmAggressive ksmMode = "aggressive"
	ksmAuto       ksmMode = "auto"
)

var ksmSettings = map[ksmMode]ksmSetting{
	ksmOff:        {1000, 500, false}, // Turn KSM off
	ksmSlow:       {500, 100, true},   // Every 100ms, we scan 1 page for every 500 pages available in the system
	ksmStandard:   {100, 10, true},    // Every 10ms, we scan 1 page for every 100 pages available in the system
	ksmAggressive: {10, 1, true},      // Every ms, we scan 1 page for every 10 pages available in the system
}

func (k ksmMode) String() string {
	switch k {
	case ksmOff:
		return "off"
	case ksmInitial:
		return "initial"
	case ksmAuto:
		return "auto"
	}

	return ""
}

type sysfsAttribute struct {
	path string
	file *os.File
}

func (attr *sysfsAttribute) open() error {
	file, err := os.OpenFile(attr.path, os.O_RDWR|syscall.O_NONBLOCK, 0660)
	attr.file = file
	return err
}

func (attr *sysfsAttribute) close() error {
	err := attr.file.Close()
	attr.file = nil
	return err
}

func (attr *sysfsAttribute) read() (string, error) {
	_, err := attr.file.Seek(0, io.SeekStart)
	if err != nil {
		return "", err
	}

	data, err := ioutil.ReadAll(attr.file)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (attr *sysfsAttribute) write(value string) error {
	_, err := attr.file.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	err = attr.file.Truncate(0)
	if err != nil {
		return err
	}

	_, err = attr.file.WriteString(value)

	return err
}

type ksm struct {
	run           sysfsAttribute
	pagesToScan   sysfsAttribute
	sleepInterval sysfsAttribute

	root                 string
	initialPagesToScan   string
	initialSleepInterval string
	initialKSMRun        string

	currentKnob ksmMode

	kickChannel chan bool

	throttling  bool
	initialized bool

	sync.Mutex
}

func (k *ksm) isAvailable() error {
	info, err := os.Stat(k.root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not available", k.root)
	}

	return nil
}

// restoreSysFS is unlocked. You should take the ksm lock before calling it.
func (k *ksm) restoreSysFS() error {
	var err error

	if !k.initialized {
		return errKSMUnavailable
	}

	if err = k.pagesToScan.write(k.initialPagesToScan); err != nil {
		return err
	}

	if err = k.sleepInterval.write(k.initialSleepInterval); err != nil {
		return err
	}

	return k.run.write(k.initialKSMRun)
}

func (k *ksm) restore() error {
	var err error

	k.Lock()
	defer k.Unlock()

	if !k.initialized {
		return errKSMUnavailable
	}

	if err = k.restoreSysFS(); err != nil {
		return err
	}

	if err := k.run.close(); err != nil {
		return err
	}

	if err := k.sleepInterval.close(); err != nil {
		return err
	}

	if err := k.pagesToScan.close(); err != nil {
		return err
	}

	k.initialized = false
	return nil
}

func (k *ksm) throttle() {
	k.Lock()
	defer k.Unlock()

	if !k.initialized {
		throttlerLog.WithError(errKSMUnavailable).Error()
		return
	}

	k.currentKnob = ksmAggressive
	k.throttling = true

	go func() {
		throttleTimer := time.NewTimer(ksmThrottleIntervals[k.currentKnob].interval)

		for {
			select {
			case <-k.kickChannel:
				// We got kicked, this means a new VM has been created.
				// We will enter the aggressive setting until we throttle down.
				_ = throttleTimer.Stop()
				mode := ksmAggressive
				if err := k.tune(ksmSettings[mode]); err != nil {
					throttlerLog.WithError(err).WithField("ksm-mode", mode).Error("kick failed to tune")
					continue
				}

				k.Lock()
				k.currentKnob = ksmAggressive
				k.Unlock()

				_ = throttleTimer.Reset(ksmAggressiveInterval)

			case <-throttleTimer.C:
				// Our throttling down timer kicked in.
				// We will move down to the next knob and start the next time,
				// if necessary.
				var throttle = ksmThrottleIntervals[k.currentKnob]
				if throttle.interval == 0 {
					if throttle.nextKnob == ksmInitial {
						k.Lock()
						if err := k.restoreSysFS(); err != nil {
							throttlerLog.WithError(err).Error("failed to restore sysfs")
						}
						k.Unlock()
					}
					continue
				}

				currentKnob := k.currentKnob
				nextKnob := ksmThrottleIntervals[currentKnob].nextKnob
				interval := ksmThrottleIntervals[currentKnob].interval
				if err := k.tune(ksmSettings[nextKnob]); err != nil {
					throttlerLog.WithError(err).WithFields(logrus.Fields{
						"current-ksm-mode": currentKnob,
						"next-ksm-mode":    nextKnob,
					}).Error("timer failed to tune")
					continue
				}

				k.Lock()
				k.currentKnob = nextKnob
				k.Unlock()

				_ = throttleTimer.Reset(interval)
			}
		}
	}()
}

func (k *ksm) tune(s ksmSetting) error {
	k.Lock()
	defer k.Unlock()

	if !k.initialized {
		return errKSMUnavailable
	}

	if !s.run {
		return k.run.write(ksmStop)
	}

	newPagesToScan, err := s.pagesToScan()
	if err != nil {
		return err
	}

	if err = k.run.write(ksmStop); err != nil {
		return err
	}

	if err = k.pagesToScan.write(newPagesToScan); err != nil {
		return err
	}

	if err = k.sleepInterval.write(fmt.Sprintf("%v", s.scanIntervalMS)); err != nil {
		return err
	}

	return k.run.write(ksmStart)
}

// kick gets us back to the aggressive setting
func (k *ksm) kick() {
	k.Lock()

	if !k.initialized {
		throttlerLog.WithError(errKSMUnavailable).Error()
		k.Unlock()
		return
	}

	// If we're not throttling, we must not kick.
	if !k.throttling {
		k.Unlock()
		return
	}

	k.Unlock()
	k.kickChannel <- true
}

func startKSM(root string, mode ksmMode) (*ksm, error) {
	k, err := newKSM(root)
	if err != nil {
		return k, err
	}

	// We just no-op if going for initial settings
	if mode != ksmInitial {
		// We want to catch termination to restore the initial sysfs values
		c := make(chan os.Signal, 2)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)

		for _, sig := range ksig.HandledSignals() {
			signal.Notify(c, sig)
		}

		go func() {
			for {
				// Block waiting for a signal
				sig := <-c

				if sig == syscall.SIGTERM {
					_ = k.restore()
					os.Exit(0)
				}

				nativeSignal, ok := sig.(syscall.Signal)
				if !ok {
					err := errors.New("unknown signal")
					throttlerLog.WithError(err).WithField("signal", sig.String()).Error()
					continue
				}

				if ksig.FatalSignal(nativeSignal) {
					throttlerLog.WithField("signal", sig).Error("received fatal signal")
					ksig.Die()
				} else if ksig.NonFatalSignal(nativeSignal) {
					if debug {
						throttlerLog.WithField("signal", sig).Debug("handling signal")
						ksig.Backtrace()
					}
				}
			}
		}()

		if mode == ksmAuto {
			k.throttle()
		} else {
			setting, ok := ksmSettings[mode]
			if !ok {
				return k, fmt.Errorf("Invalid KSM mode %v", mode)
			}

			if err := k.tune(setting); err != nil {
				return k, err
			}
		}
	}

	return k, nil
}

func newKSM(root string) (*ksm, error) {
	var err error
	var k ksm

	k.initialized = false
	k.throttling = false
	k.root = root

	if root == "" {
		return nil, errors.New("Invalid KSM root")
	}

	if err := k.isAvailable(); err != nil {
		return nil, err
	}

	k.pagesToScan = sysfsAttribute{
		path: filepath.Join(k.root, ksmPagesToScan),
	}

	k.sleepInterval = sysfsAttribute{
		path: filepath.Join(k.root, ksmSleepMillisec),
	}

	k.run = sysfsAttribute{
		path: filepath.Join(k.root, ksmRunFile),
	}

	defer func(err error) {
		if err != nil {
			_ = k.run.close()
			_ = k.sleepInterval.close()
			_ = k.pagesToScan.close()
		}
	}(err)

	if err := k.run.open(); err != nil {
		return nil, err
	}

	if err := k.sleepInterval.open(); err != nil {
		return nil, err
	}

	if err := k.pagesToScan.open(); err != nil {
		return nil, err
	}

	k.initialPagesToScan, err = k.pagesToScan.read()
	if err != nil {
		return nil, err
	}

	k.initialSleepInterval, err = k.sleepInterval.read()
	if err != nil {
		return nil, err
	}

	k.initialKSMRun, err = k.run.read()
	if err != nil {
		return nil, err
	}

	k.initialized = true
	k.kickChannel = make(chan bool)

	return &k, nil
}
