// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"github.com/hashicorp/terraform/internal/states"
)

// updateStateHook calls the PostStateUpdate hook with the current state.
func updateStateHook(ctx EvalContext) error {
	// PostStateUpdate requires that the state be locked and safe to read for
	// the duration of the call.
	stateSync := ctx.State()
	// The hooks only read the state, so a read lock is sufficient and
	// doesn't prevent other readers from making progress.
	state := stateSync.RLock()
	defer stateSync.RUnlock()

	// Call the hook
	err := ctx.Hook(func(h Hook) (HookAction, error) {
		if h, ok := h.(StateSnapshotHook); ok {
			h.BindStateSnapshots(stateSync.Snapshot)
		}
		return h.PostStateUpdate(state)
	})
	return err
}

// StateSnapshotHook is an optional extension of [Hook] for hooks that need
// to capture the state at times other than during a PostStateUpdate call.
//
// Saving a snapshot of a large state takes long enough that it isn't
// practical to do it for every individual state update, so a hook that
// saves snapshots needs to be able to save the latest state some time after
// the most recent update, even if no further updates follow it.
type StateSnapshotHook interface {
	Hook

	// BindStateSnapshots provides a function that returns a copy of the
	// current state each time it's called, or nil once the operation that
	// the state belongs to is complete. It's called before each call to
	// PostStateUpdate, always with an equivalent function for the same
	// operation, and the function remains valid indefinitely.
	BindStateSnapshots(snapshot func() *states.State)
}
