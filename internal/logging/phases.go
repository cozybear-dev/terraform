// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package logging

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// envPhases is the name of an environment variable that, when set to any
// non-empty value, makes Terraform report on stderr how long each of the
// major phases of an operation took. This is intended for investigating
// the performance of Terraform itself.
const envPhases = "TF_PROFILE_PHASES"

var phases = struct {
	sync.Mutex
	enabled bool
	start   time.Time
	last    time.Time
	lastCPU time.Duration
}{
	enabled: os.Getenv(envPhases) != "",
	start:   time.Now(),
	last:    time.Now(),
}

// Phase records that the named phase of the current operation has just
// completed. If phase reporting is enabled, it writes to stderr the time
// since the previous call along with the total time since the process
// started, and how many processors the phase kept busy on average, which
// shows whether it was able to make use of more than one.
func Phase(name string) {
	if !phases.enabled {
		return
	}
	phases.Lock()
	defer phases.Unlock()
	now := time.Now()
	cpu := processCPUTime()
	elapsed := now.Sub(phases.last).Seconds()
	busy := 0.0
	if elapsed > 0 {
		busy = (cpu - phases.lastCPU).Seconds() / elapsed
	}
	fmt.Fprintf(os.Stderr, "[phase] %8.2fs  +%7.2fs  %4.1f cpus  %s\n", now.Sub(phases.start).Seconds(), elapsed, busy, name)
	phases.last = now
	phases.lastCPU = cpu
}
