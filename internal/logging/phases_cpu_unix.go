// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build unix

package logging

import (
	"syscall"
	"time"
)

// processCPUTime returns the processor time this process has used so far,
// or zero if that isn't available.
func processCPUTime() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
