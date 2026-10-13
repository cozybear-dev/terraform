// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// componentMode is one of the modes of the component driver, and how many
// inputs to give it in each tier.
type componentMode struct {
	Name    string
	About   string
	CI      int
	Nightly int
}

var componentModes = []componentMode{
	{"values", "a value of every kind written to the state and to a plan, read back, and written again", 20000, 400000},
	{"dyntypes", "dynamically-typed values whose types are equal but are serialized differently", 2000, 20000},
	{"decode", "serialized attributes written every way JSON allows, including wrongly, decoded and written again", 20000, 400000},
	{"statefile", "whole state files in several layouts, some of them damaged, read, written, and read again", 1200, 12000},
	{"dag", "graphs reduced, checked for cycles, and walked with some of their vertices failing", 6000, 100000},
	{"changes", "sequences of planned changes recorded, looked up and removed", 4000, 60000},
	{"expander", "module and resource expansions registered and queried", 4000, 60000},
}

type componentReport struct {
	Mode   string `json:"mode"`
	About  string `json:"about"`
	Seed   uint64 `json:"seed"`
	Inputs int    `json:"inputs"`
	// Unstable is the number of inputs for which upstream gave different
	// results in two runs, which are excluded from the comparison.
	Unstable int `json:"unstable_in_upstream"`
	// Different is the number of the remaining inputs for which the fork's
	// result isn't upstream's.
	Different int      `json:"different"`
	Examples  []string `json:"examples,omitempty"`
	// Digest identifies upstream's output.
	Digest string `json:"digest"`
	Result string `json:"result"`
}

func runDriver(bin, mode string, seed uint64, first, n int, env []string) ([]string, error) {
	out, rc, err := command("", env, bin, "-mode", mode, "-seed", strconv.FormatUint(seed, 10), "-first", strconv.Itoa(first), "-n", strconv.Itoa(n))
	if err != nil {
		return nil, err
	}
	if rc != 0 {
		return nil, fmt.Errorf("%s -mode %s: exit status %d\n%s", filepath.Base(bin), mode, rc, tail(out, 10))
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n"), nil
}

// runComponents gives each mode of the component driver the same inputs in
// both builds and compares their output line by line.
func runComponents(t *toolchain, nightly bool, seed uint64, jobs int, logf func(string, ...any)) []componentReport {
	reports := make([]componentReport, len(componentModes))
	sem := make(chan struct{}, max(1, jobs))
	var wg sync.WaitGroup
	for i, mode := range componentModes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := mode.CI
			if nightly {
				n = mode.Nightly
			}
			report := componentReport{Mode: mode.Name, About: mode.About, Seed: seed, Inputs: n, Result: "identical"}
			defer func() { reports[i] = report }()

			var outputs [3][]string
			var errs [3]error
			var inner sync.WaitGroup
			for k, bin := range []string{t.driverUpstream, t.driverUpstream, t.driverFork} {
				inner.Add(1)
				go func() {
					defer inner.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					outputs[k], errs[k] = runDriver(bin, mode.Name, seed, 0, n, nil)
				}()
			}
			if t.driverCover != "" {
				// Only for the coverage it records. A tenth of the inputs
				// is enough to reach whatever the rest would.
				inner.Add(1)
				go func() {
					defer inner.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					env := append(os.Environ(), "GOCOVERDIR="+filepath.Join(t.work, "coverage", "driver"))
					runDriver(t.driverCover, mode.Name, seed, 0, max(1, n/10), env)
				}()
			}
			inner.Wait()
			for _, err := range errs {
				if err != nil {
					report.Result = "invalid"
					report.Examples = append(report.Examples, err.Error())
					return
				}
			}
			first, again, fork := outputs[0], outputs[1], outputs[2]
			if len(first) != n || len(again) != n || len(fork) != n {
				report.Result = "invalid"
				report.Examples = append(report.Examples, fmt.Sprintf("expected %d lines and got %d, %d and %d", n, len(first), len(again), len(fork)))
				return
			}
			digest := sha256.New()
			confirmed := 0
			for k := range first {
				if first[k] != again[k] {
					report.Unstable++
					continue
				}
				if first[k] != fork[k] && confirmed < maxConfirmed {
					confirmed++
					if upstreamAlsoGives(t, mode.Name, seed, k, fork[k]) {
						// Upstream agreed with itself twice and still
						// gives this result some of the time.
						report.Unstable++
						continue
					}
				}
				fmt.Fprintln(digest, first[k])
				if first[k] != fork[k] {
					report.Different++
					if len(report.Examples) < 5 {
						report.Examples = append(report.Examples, "upstream: "+clip(first[k], 400), "fork:     "+clip(fork[k], 400))
					}
				}
			}
			report.Digest = hex.EncodeToString(digest.Sum(nil))
			if report.Different != 0 {
				report.Result = "different"
			}
			logf("component %-9s %6d inputs, %d different, %d unstable in upstream", mode.Name, n, report.Different, report.Unstable)
		}()
	}
	wg.Wait()
	return reports
}

// confirmations is how many more times upstream is given an input for which
// the fork's result wasn't upstream's, before that's believed. Only the
// first maxConfirmed such inputs of a mode are given that treatment. An
// input on which upstream varies is rare, and a fork that is really wrong
// is usually wrong about a great many inputs, each of which would otherwise
// cost sixty runs to say so.
const (
	confirmations = 60
	maxConfirmed  = 25
)

// upstreamAlsoGives returns true if upstream, given one input again and
// again, ever gives the result that the fork gave for it.
//
// Running upstream twice notices an input on which its result varies only
// if the two runs happen to disagree. For a result that upstream gives one
// time in eight, they usually don't, and the fork then looks different one
// run in ten. So a difference is only reported once upstream has failed to
// produce the fork's result in a further sixty attempts. This can't hide a
// difference: it only ever accepts a result that upstream itself gives for
// the same input.
func upstreamAlsoGives(t *toolchain, mode string, seed uint64, input int, want string) bool {
	for range confirmations {
		lines, err := runDriver(t.driverUpstream, mode, seed, input, 1, nil)
		if err == nil && len(lines) == 1 && lines[0] == want {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
