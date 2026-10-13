// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

//go:build !unix

package logging

import "time"

// processCPUTime returns the processor time this process has used so far,
// or zero if that isn't available.
func processCPUTime() time.Duration {
	return 0
}
