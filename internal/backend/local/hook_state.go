// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package local

import (
	"log"
	"sync"
	"time"

	"github.com/hashicorp/terraform/internal/schemarepo"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statemgr"
	"github.com/hashicorp/terraform/internal/terraform"
)

// StateHook is a hook that continuously updates the state by calling
// WriteState on a statemgr.Full.
type StateHook struct {
	terraform.NilHook
	sync.Mutex

	StateMgr statemgr.Writer

	// If PersistInterval is nonzero then for any new state update after
	// the duration has elapsed we'll try to persist a state snapshot
	// to the persistent backend too.
	// That's only possible if field Schemas is valid, because the
	// StateMgr.PersistState function for some backends needs schemas.
	PersistInterval time.Duration

	// Schemas are the schemas to use when persisting state due to
	// PersistInterval. This is ignored if PersistInterval is zero,
	// and PersistInterval is ignored if this is nil.
	Schemas *schemarepo.Schemas

	intermediatePersist statemgr.IntermediateStatePersistInfo

	// lastWrite and lastWriteCost record when the most recent call to
	// StateMgr.WriteState completed and how long it took, which we use
	// to limit how much of the total run time is spent writing state
	// snapshots. See nextBackgroundWrite.
	lastWrite     time.Time
	lastWriteCost time.Duration

	// stopping is set once Terraform has been asked to stop, after which
	// we write every state update immediately.
	stopping bool

	// The remaining fields are for dealing with a large state, whose
	// snapshots we write in the background. See PostStateUpdate.

	// large is set once we've decided that the state is large.
	large bool

	// snapshot returns a copy of the current state, if the caller of
	// PostStateUpdate has provided a way to get one. See BindStateSnapshots.
	snapshot func() *states.State

	// dirty is set when there has been a state update that isn't included
	// in any snapshot that we've written or started writing.
	dirty bool

	// firstDirty is when dirty was first set, and estimatedCost is our
	// initial guess at how long writing a snapshot will take, both of
	// which we use to decide when to write the first snapshot.
	firstDirty    time.Time
	estimatedCost time.Duration

	// timer is non-nil while we're waiting to start writing a snapshot in
	// the background.
	timer *time.Timer

	// background is non-nil while a snapshot is being written in the
	// background, and is closed once that work is complete.
	background chan struct{}

	// snapshotting is set while the background work is still taking its
	// snapshot, which needs a lock on the state. discardSnapshot is set to
	// tell it not to write that snapshot once it has it, because a newer
	// state has been written in the meantime.
	snapshotting    bool
	discardSnapshot bool

	// backgroundErr is the error from the most recent background write,
	// if it failed and we've not yet reported that.
	backgroundErr error

	// finished is set once Wait has been called, after which we never
	// start any more work in the background.
	finished bool
}

// StateHook treats a state as large once it has stateLargeInstanceCount
// resource instances. That is decided by the number of instances alone, and
// not by how long a snapshot takes to write, because writing includes
// syncing the file to disk, which on an ordinary disk takes longer than any
// threshold that would be useful, however small the state. A state with
// fewer instances is always saved after every update, as it would be
// without any of this.
const (
	// stateWriteCostPerInstance is a rough estimate of how long it takes to
	// write a snapshot, per resource instance in the state, which StateHook
	// uses only until it has written a snapshot and so knows the real cost.
	stateWriteCostPerInstance = 30 * time.Microsecond

	// stateWriteBackoffFactor decides how long StateHook waits after
	// writing a snapshot of a large state before it will write another, as a
	// multiple of the time that the previous one took. This bounds the time
	// spent writing intermediate snapshots to roughly
	// 1/(1+stateWriteBackoffFactor) of the total run time.
	stateWriteBackoffFactor = 9
)

var _ terraform.StateSnapshotHook = (*StateHook)(nil)

// BindStateSnapshots implements terraform.StateSnapshotHook.
func (h *StateHook) BindStateSnapshots(snapshot func() *states.State) {
	h.Lock()
	defer h.Unlock()
	if h.snapshot == nil {
		h.snapshot = snapshot
	}
}

// PostStateUpdate arranges for the given state to be saved.
//
// For a small state, each update is written to the state manager before this
// returns, and then persisted if that's due.
//
// Writing a snapshot of a large state takes long enough that doing so for
// every individual update would make the total cost quadratic, and our caller
// holds a lock on the state throughout this call, which would leave all other
// work that needs the state waiting. For a large state we therefore only
// record that the state has changed, and write snapshots of the latest state
// in the background at a rate that keeps the time spent on them to a small
// proportion of the total. Each update is included in a snapshot that is
// started within a bounded time, whether or not any further updates follow
// it, and the final state is always written by our caller once the operation
// is complete.
func (h *StateHook) PostStateUpdate(new *states.State) (terraform.HookAction, error) {
	h.Lock()
	defer h.Unlock()

	h.intermediatePersist.RequestedPersistInterval = h.PersistInterval

	if h.intermediatePersist.LastPersist.IsZero() {
		// The first PostStateUpdate starts the clock for intermediate
		// calls to PersistState.
		h.intermediatePersist.LastPersist = time.Now()
	}

	if h.StateMgr == nil {
		return terraform.HookActionContinue, nil
	}

	if err := h.backgroundErr; err != nil {
		h.backgroundErr = nil
		return terraform.HookActionHalt, err
	}

	if h.stopping {
		// While stopping we write and persist every update before
		// returning, once any earlier snapshot is out of the way.
		h.waitForBackgroundWrite()
		h.dirty = false
		return h.writeAndPersist(new)
	}

	if !h.large {
		if instances := countResourceInstances(new, stateLargeInstanceCount); instances >= stateLargeInstanceCount {
			h.large = true
			h.estimatedCost = time.Duration(countResourceInstances(new, 100000)) * stateWriteCostPerInstance
		}
	}
	if !h.large {
		return h.writeAndPersist(new)
	}

	if !h.dirty {
		h.dirty = true
		h.firstDirty = time.Now()
	}
	switch {
	case h.background != nil || h.timer != nil:
		// The latest state will be picked up by the snapshot that follows
		// the work that's already in progress or scheduled.
	case h.snapshot == nil:
		// We have no way to get the state later, so we can only write the
		// state we've been given, if it isn't too soon to do so.
		if !time.Now().Before(h.nextBackgroundWrite()) {
			h.dirty = false
			h.writeInBackground(new.DeepCopy())
		}
	default:
		h.scheduleBackgroundWrite()
	}
	return terraform.HookActionContinue, nil
}

// writeAndPersist passes the given state to the state manager and then
// persists it if that's due. The caller must hold the lock.
func (h *StateHook) writeAndPersist(state *states.State) (terraform.HookAction, error) {
	start := time.Now()
	if err := h.StateMgr.WriteState(state); err != nil {
		return terraform.HookActionHalt, err
	}
	h.lastWrite = time.Now()
	h.lastWriteCost = h.lastWrite.Sub(start)
	if mgrPersist, ok := h.StateMgr.(statemgr.Persister); ok && h.PersistInterval != 0 && h.Schemas != nil {
		if h.shouldPersist() {
			err := mgrPersist.PersistState(h.Schemas)
			if err != nil {
				return terraform.HookActionHalt, err
			}
			h.intermediatePersist.LastPersist = time.Now()
		} else {
			log.Printf("[DEBUG] State storage %T declined to persist a state snapshot", h.StateMgr)
		}
	}
	return terraform.HookActionContinue, nil
}

// nextBackgroundWrite returns the earliest time at which we should start
// writing another snapshot of a large state. The caller must hold the lock.
func (h *StateHook) nextBackgroundWrite() time.Time {
	if h.lastWrite.IsZero() {
		// We've not written a snapshot yet. Rather than writing one
		// immediately, we wait for about as long as we expect writing it
		// to take. If the whole operation completes in less time than
		// that, as is common when only a few objects are changing, then
		// we avoid writing a snapshot that would be replaced straight
		// away by the final state.
		return h.firstDirty.Add(h.estimatedCost)
	}
	return h.lastWrite.Add(h.lastWriteCost * stateWriteBackoffFactor)
}

// scheduleBackgroundWrite arranges to write a snapshot of the latest state
// in the background as soon as it's appropriate to do so. The caller must
// hold the lock, and there must not already be a background write scheduled
// or in progress.
func (h *StateHook) scheduleBackgroundWrite() {
	h.timer = time.AfterFunc(time.Until(h.nextBackgroundWrite()), func() {
		h.Lock()
		h.timer = nil
		if h.finished || h.stopping || h.background != nil || !h.dirty || h.snapshot == nil {
			h.Unlock()
			return
		}
		done := make(chan struct{})
		h.background = done
		h.snapshotting = true
		h.dirty = false
		snapshot := h.snapshot
		h.Unlock()

		// We must not hold our own lock while taking the snapshot, because
		// that needs a lock on the state and callers of PostStateUpdate
		// wait for our lock while holding a lock on the state.
		state := snapshot()

		h.Lock()
		h.snapshotting = false
		discard := h.discardSnapshot
		h.discardSnapshot = false
		if state == nil || discard {
			// Either the operation has completed, so our caller will write
			// the final state, or a newer state has already been written.
			h.background = nil
			h.Unlock()
			close(done)
			return
		}
		h.Unlock()
		h.writeSnapshot(state, done)
	})
}

// writeInBackground does the same work as writeAndPersist but in another
// goroutine, using a snapshot that nothing else will modify. The caller
// must hold the lock, and there must not already be a background write
// in progress.
//
// Any error is reported by the next call to PostStateUpdate.
func (h *StateHook) writeInBackground(snapshot *states.State) {
	done := make(chan struct{})
	h.background = done
	go h.writeSnapshot(snapshot, done)
}

// writeSnapshot is the body of a background write, which the caller must
// have registered in h.background as the given channel.
func (h *StateHook) writeSnapshot(snapshot *states.State, done chan struct{}) {
	start := time.Now()
	err := h.StateMgr.WriteState(snapshot)
	end := time.Now()

	// We must not hold the lock while writing or persisting, because
	// callers of PostStateUpdate wait for it while holding a lock on the
	// state.
	h.Lock()
	h.lastWrite = end
	h.lastWriteCost = end.Sub(start)
	persist := false
	mgrPersist, ok := h.StateMgr.(statemgr.Persister)
	if err == nil && ok && h.PersistInterval != 0 && h.Schemas != nil {
		if persist = h.shouldPersist(); !persist {
			log.Printf("[DEBUG] State storage %T declined to persist a state snapshot", h.StateMgr)
		}
	}
	h.Unlock()

	if persist {
		err = mgrPersist.PersistState(h.Schemas)
	}

	h.Lock()
	if persist && err == nil {
		h.intermediatePersist.LastPersist = time.Now()
	}
	h.backgroundErr = err
	h.background = nil
	if h.dirty && h.snapshot != nil && h.timer == nil && !h.finished && !h.stopping {
		// There have been more updates since we took the snapshot.
		h.scheduleBackgroundWrite()
	}
	h.Unlock()
	close(done)
}

// waitForBackground blocks until there is no background write in progress.
// The caller must hold the lock, which is released while waiting.
func (h *StateHook) waitForBackground() {
	for h.background != nil {
		done := h.background
		h.Unlock()
		<-done
		h.Lock()
	}
}

// waitForBackgroundWrite is like waitForBackground but for a caller that
// holds a lock on the state, as the callers of PostStateUpdate do.
//
// Background work that hasn't taken its snapshot yet needs a lock on the
// state too, and it can be waiting for that behind something that wants to
// modify the state, which is in turn waiting for our caller to release its
// lock. Waiting for the snapshot would then never end, so instead we tell
// the background work to discard its snapshot, which is safe because our
// caller is about to write the state it has been given.
func (h *StateHook) waitForBackgroundWrite() {
	for h.background != nil {
		if h.snapshotting {
			h.discardSnapshot = true
			return
		}
		done := h.background
		h.Unlock()
		<-done
		h.Lock()
	}
}

// Wait blocks until any snapshot that the hook is still writing in the
// background is complete, and prevents it from starting to write any more.
// The caller must call this before writing the final state to the same state
// manager, to ensure that the final state isn't then replaced by an older
// snapshot.
func (h *StateHook) Wait() {
	h.Lock()
	defer h.Unlock()
	h.finished = true
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.waitForBackground()
}

func (h *StateHook) Stopping() {
	h.Lock()
	defer h.Unlock()

	// If Terraform has been asked to stop then that might mean that a hard
	// kill signal will follow shortly in case Terraform doesn't stop
	// quickly enough, and so we'll try to persist the latest state
	// snapshot in the hope that it'll give the user less recovery work to
	// do if they _do_ subsequently hard-kill Terraform during an apply.
	h.stopping = true
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	h.waitForBackground()

	if h.dirty && h.snapshot != nil && h.StateMgr != nil {
		// There are updates to a large state that we've not written yet.
		// As elsewhere, we must not hold our lock while taking a snapshot.
		snapshot := h.snapshot
		h.Unlock()
		state := snapshot()
		h.Lock()
		// If an update arrived while we were taking the snapshot then it
		// has already been written, because we're stopping, and what we
		// have here might be older than that.
		if state != nil && h.dirty {
			h.dirty = false
			if err := h.StateMgr.WriteState(state); err != nil {
				log.Printf("[ERROR] Failed to write state after interruption: %s", err)
			}
		}
	}

	if mgrPersist, ok := h.StateMgr.(statemgr.Persister); ok && h.Schemas != nil {
		// While we're in the stopping phase we'll try to persist every
		// new state update to maximize every opportunity we get to avoid
		// losing track of objects that have been created or updated.
		// Terraform Core won't start any new operations after it's been
		// stopped, so at most we should see one more PostStateUpdate
		// call per already-active request.
		h.intermediatePersist.ForcePersist = true

		if h.shouldPersist() {
			err := mgrPersist.PersistState(h.Schemas)
			if err != nil {
				// This hook can't affect Terraform Core's ongoing behavior,
				// but it's a best effort thing anyway so we'll just emit a
				// log to aid with debugging.
				log.Printf("[ERROR] Failed to persist state after interruption: %s", err)
			}
		} else {
			log.Printf("[DEBUG] State storage %T declined to persist a state snapshot", h.StateMgr)
		}
	}

}

func (h *StateHook) shouldPersist() bool {
	if m, ok := h.StateMgr.(statemgr.IntermediateStateConditionalPersister); ok {
		return m.ShouldPersistIntermediateState(&h.intermediatePersist)
	}
	return statemgr.DefaultIntermediateStatePersistRule(&h.intermediatePersist)
}
