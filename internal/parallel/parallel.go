// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

// Package parallel contains helpers for spreading CPU-bound work that
// consists of many independent items across all of the available CPUs.
package parallel

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// For calls fn once for each integer from zero up to but not including n,
// potentially from several goroutines at once, and returns once all of the
// calls have returned.
//
// The calls are distributed in batches of the given size, which should be
// chosen so that a batch represents enough work to outweigh the cost of
// coordinating between goroutines. If there's only enough work for a single
// batch then all of the calls happen on the calling goroutine.
//
// fn must be safe to call concurrently with itself. A typical fn writes its
// result to element i of a slice allocated by the caller, so that the
// results end up in the same order as they would with a sequential loop.
func For(n, batch int, fn func(i int)) {
	if batch < 1 {
		batch = 1
	}
	workers := min(runtime.GOMAXPROCS(0), (n+batch-1)/batch)
	if workers <= 1 {
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}

	var next atomic.Int64
	work := func() {
		for {
			end := int(next.Add(int64(batch)))
			start := end - batch
			if start >= n {
				return
			}
			for i := start; i < min(end, n); i++ {
				fn(i)
			}
		}
	}

	var wg sync.WaitGroup
	for range workers - 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			work()
		}()
	}
	work()
	wg.Wait()
}
