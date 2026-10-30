// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"bytes"
	"fmt"
	"os"
	"sort"
)

// compareCommand compares two artefacts that were produced some other way,
// such as by planning against a real state with each build, using the same
// rules as the proof uses. It's the tool for the comparison that the corpus
// can't make: the real configuration, the real state, the real providers.
//
//	forkproof compare state      upstream.tfstate fork.tfstate
//	forkproof compare plan-json  upstream.json    fork.json      (terraform show -json)
//	forkproof compare plan-text  upstream.txt     fork.txt       (terraform show -no-color)
//	forkproof compare plan-file  upstream.tfplan  fork.tfplan
//	forkproof compare output     upstream.txt     fork.txt       (what plan or apply printed)
//
// It exits with status 0 if they're the same and 1 if they aren't.
func compareCommand(args []string) int {
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: forkproof compare state|plan-json|plan-text|plan-file|output UPSTREAM FORK")
		return 2
	}
	kind, upstreamPath, forkPath := args[0], args[1], args[2]

	canonical := func(path string) (map[string][]byte, error) {
		if kind == "plan-file" {
			return planFileMembers(path), nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		switch kind {
		case "state":
			src = canonicalState(src)
		case "plan-json":
			if src, err = canonicalPlanJSON(src); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		case "plan-text":
			src = canonicalPlanText(src)
		case "output":
			src = canonicalOutput(src)
		default:
			return nil, fmt.Errorf("unknown kind %q", kind)
		}
		return map[string][]byte{kind: src}, nil
	}
	want, err := canonical(upstreamPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forkproof:", err)
		return 2
	}
	got, err := canonical(forkPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forkproof:", err)
		return 2
	}

	names := make(map[string]bool)
	for name := range want {
		names[name] = true
	}
	for name := range got {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	status := 0
	for _, name := range sorted {
		w, wok := want[name]
		g, gok := got[name]
		switch {
		case !wok:
			fmt.Printf("DIFFERENT %s: only the fork has it\n", name)
			status = 1
		case !gok:
			fmt.Printf("DIFFERENT %s: only upstream has it\n", name)
			status = 1
		case !bytes.Equal(w, g):
			fmt.Printf("DIFFERENT %s: %s\n", name, firstDifference(w, g))
			status = 1
		default:
			fmt.Printf("identical %s (%d bytes after canonicalisation)\n", name, len(w))
		}
	}
	return status
}
