// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package main

import (
	"slices"
	"testing"
)

// A build that is only for applying is used with plans saved by the full
// fork. Two more actors show that it applies those plans as upstream does:
// one where it applies them, and one where upstream does, both compared
// with upstream planning and applying.
func TestActorsWithPlanner(t *testing.T) {
	tc := &toolchain{upstream: "u", fork: "f", planner: "p"}
	got := make(map[string]actor)
	for _, a := range tc.actors() {
		got[a.name] = a
	}
	for _, want := range []actor{
		{name: "full-fork-plans-fork-applies", planner: "p", applier: "f", fork: true},
		{name: "full-fork-plans-upstream-applies", planner: "p", applier: "u", fork: true},
	} {
		if got[want.name] != want {
			t.Errorf("actor %s is %+v, want %+v", want.name, got[want.name], want)
		}
	}
}

func TestActorsWithoutPlanner(t *testing.T) {
	tc := &toolchain{upstream: "u", fork: "f"}
	var names []string
	for _, a := range tc.actors() {
		names = append(names, a.name)
	}
	want := []string{"upstream", "upstream-again", "fork", "upstream-plans-fork-applies", "fork-plans-upstream-applies"}
	if !slices.Equal(names, want) {
		t.Errorf("actors are %v, want %v", names, want)
	}
}
