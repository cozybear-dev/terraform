// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"time"
)

// startProfiling enables runtime profiling when the TF_PROFILE_DIR
// environment variable is set, writing a CPU profile (cpu.pprof), a heap
// profile (heap.pprof), and, if TF_PROFILE_TRACE is also set, an execution
// trace (trace.out) into that directory. If TF_PROFILE_WALL is set, it also
// writes a goroutine profile every 100ms (goroutines-NNNNN.pprof), and if
// TF_PROFILE_HEAP is set it writes a heap profile every two seconds
// (heap-NNN.pprof). If TF_PROFILE_MUTEX is set it writes profiles of lock
// contention and blocking (mutex.pprof, block.pprof). The returned function must be
// called before the process exits to flush the profiles.
func startProfiling() (stop func()) {
	dir := os.Getenv("TF_PROFILE_DIR")
	if dir == "" {
		return func() {}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "TF_PROFILE_DIR: %s\n", err)
		return func() {}
	}

	var stops []func()
	if f, err := os.Create(filepath.Join(dir, "cpu.pprof")); err == nil {
		if err := pprof.StartCPUProfile(f); err == nil {
			stops = append(stops, func() {
				pprof.StopCPUProfile()
				f.Close()
			})
		}
	}
	if os.Getenv("TF_PROFILE_TRACE") != "" {
		if f, err := os.Create(filepath.Join(dir, "trace.out")); err == nil {
			if err := trace.Start(f); err == nil {
				stops = append(stops, func() {
					trace.Stop()
					f.Close()
				})
			}
		}
	}

	if os.Getenv("TF_PROFILE_WALL") != "" {
		// Periodic snapshots of what every goroutine is doing, which can be
		// merged with "go tool pprof" to see where wall-clock time goes,
		// including time spent waiting.
		done := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				case <-ticker.C:
					if f, err := os.Create(filepath.Join(dir, fmt.Sprintf("goroutines-%05d.pprof", i))); err == nil {
						pprof.Lookup("goroutine").WriteTo(f, 0)
						f.Close()
					}
				}
			}
		}()
		stops = append(stops, func() {
			close(done)
			<-finished
		})
	}

	if os.Getenv("TF_PROFILE_HEAP") != "" {
		// Periodic heap profiles, to see what is using memory part way
		// through an operation rather than only at the end.
		done := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			// The value can be an interval, for a closer look at a
			// particular part of an operation.
			interval, err := time.ParseDuration(os.Getenv("TF_PROFILE_HEAP"))
			if err != nil || interval <= 0 {
				interval = 2 * time.Second
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				case <-ticker.C:
					if f, err := os.Create(filepath.Join(dir, fmt.Sprintf("heap-%03d.pprof", i))); err == nil {
						pprof.Lookup("heap").WriteTo(f, 0)
						f.Close()
					}
				}
			}
		}()
		stops = append(stops, func() {
			close(done)
			<-finished
		})
	}

	if os.Getenv("TF_PROFILE_MUTEX") != "" {
		// Where goroutines wait for locks and for other blocking
		// operations, which shows what limits concurrency.
		runtime.SetMutexProfileFraction(5)
		runtime.SetBlockProfileRate(100000) // one sample per 100µs blocked
		stops = append(stops, func() {
			for _, name := range []string{"mutex", "block"} {
				if f, err := os.Create(filepath.Join(dir, name+".pprof")); err == nil {
					pprof.Lookup(name).WriteTo(f, 0)
					f.Close()
				}
			}
		})
	}

	return func() {
		for _, stop := range stops {
			stop()
		}
		if f, err := os.Create(filepath.Join(dir, "heap.pprof")); err == nil {
			runtime.GC()
			pprof.WriteHeapProfile(f)
			f.Close()
		}
	}
}
