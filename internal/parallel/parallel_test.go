// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package parallel

import (
	"sync/atomic"
	"testing"
)

func TestFor(t *testing.T) {
	for _, n := range []int{0, 1, 2, 7, 64, 1000} {
		for _, batch := range []int{0, 1, 3, 64, 5000} {
			calls := make([]atomic.Int32, n)
			For(n, batch, func(i int) {
				calls[i].Add(1)
			})
			for i := range calls {
				if got := calls[i].Load(); got != 1 {
					t.Fatalf("n=%d batch=%d: called %d times for %d; want once", n, batch, got, i)
				}
			}
		}
	}
}
