// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package local

import (
	"errors"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statemgr"
	"github.com/hashicorp/terraform/internal/terraform"
)

// testBlockingState is a state manager whose WriteState blocks until the
// test allows it to continue.
type testBlockingState struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	err     error
	written []*states.State
}

func (s *testBlockingState) WriteState(state *states.State) error {
	s.started <- struct{}{}
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, state)
	return s.err
}

func TestStateHookBackgroundWrite(t *testing.T) {
	mgr := &testBlockingState{
		started: make(chan struct{}, 10),
		release: make(chan struct{}),
	}
	hook := &StateHook{
		StateMgr: mgr,
		// The hook treats the state as large, and its previous write was
		// long enough ago that another is due.
		large:         true,
		lastWriteCost: time.Second,
		lastWrite:     time.Now().Add(-time.Hour),
	}

	s := statemgr.TestFullInitialState()
	want := s.DeepCopy()

	// The first update returns while the state manager is still writing.
	action, err := hook.PostStateUpdate(s)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if action != terraform.HookActionContinue {
		t.Fatalf("wrong action %#v", action)
	}
	<-mgr.started

	// Changes to the state after the hook returns must not affect the
	// snapshot being written.
	s.SetOutputValue(
		addrs.OutputValue{Name: "later"}.Absolute(addrs.RootModuleInstance),
		cty.StringVal("added afterwards"), false,
	)

	// Updates that arrive while the write is in progress are skipped.
	if _, err := hook.PostStateUpdate(s); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	waited := make(chan struct{})
	go func() {
		hook.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned while the write was still in progress")
	case <-time.After(50 * time.Millisecond):
	}

	close(mgr.release)
	<-waited

	if len(mgr.written) != 1 {
		t.Fatalf("state manager received %d snapshots; want 1", len(mgr.written))
	}
	if !mgr.written[0].Equal(want) {
		t.Fatalf("wrong snapshot written\ngot:  %#v\nwant: %#v", mgr.written[0], want)
	}

	// The write just completed, so the next update is too soon to write.
	if _, err := hook.PostStateUpdate(s); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	hook.Wait()
	if len(mgr.written) != 1 {
		t.Fatalf("state manager received %d snapshots; want 1", len(mgr.written))
	}

	// Once stopping, every update is written before the hook returns.
	hook.Stopping()
	if _, err := hook.PostStateUpdate(s); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if len(mgr.written) != 2 {
		t.Fatalf("state manager received %d snapshots; want 2", len(mgr.written))
	}
	if !mgr.written[1].Equal(s) {
		t.Fatalf("wrong snapshot written while stopping")
	}
}

func TestStateHookBackgroundWriteError(t *testing.T) {
	mgr := &testBlockingState{
		started: make(chan struct{}, 10),
		release: make(chan struct{}),
		err:     errors.New("disk full"),
	}
	close(mgr.release)
	hook := &StateHook{
		StateMgr:      mgr,
		large:         true,
		lastWriteCost: time.Second,
		lastWrite:     time.Now().Add(-time.Hour),
	}

	s := statemgr.TestFullInitialState()
	if _, err := hook.PostStateUpdate(s); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	hook.Wait()

	// The failure is reported by the next update.
	action, err := hook.PostStateUpdate(s)
	if err == nil || err.Error() != "disk full" {
		t.Fatalf("wrong error: %v", err)
	}
	if action != terraform.HookActionHalt {
		t.Fatalf("wrong action %#v", action)
	}
}

// testLargeState returns a state with enough resource instances for StateHook
// to treat it as large.
func testLargeState() *states.State {
	return states.BuildState(func(s *states.SyncState) {
		for i := 0; i < stateLargeInstanceCount; i++ {
			s.SetResourceInstanceCurrent(
				addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "null_resource",
					Name: "foo",
				}.Instance(addrs.IntKey(i)).Absolute(addrs.RootModuleInstance),
				&states.ResourceInstanceObjectSrc{
					AttrsJSON: []byte(`{"id":"foo"}`),
					Status:    states.ObjectReady,
				},
				addrs.AbsProviderConfig{
					Provider: addrs.NewDefaultProvider("null"),
					Module:   addrs.RootModule,
				},
			)
		}
	})
}

func TestStateHookLargeState(t *testing.T) {
	mgr := &testBlockingState{
		started: make(chan struct{}, 10),
		release: make(chan struct{}, 10),
	}
	hook := &StateHook{StateMgr: mgr}

	// The state that the hook can take snapshots of, as an operation would
	// provide through its synchronized state.
	var mu sync.Mutex
	current := testLargeState()
	closed := false
	snapshots := 0
	hook.BindStateSnapshots(func() *states.State {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return nil
		}
		snapshots++
		return current.DeepCopy()
	})
	update := func(name string) {
		t.Helper()
		mu.Lock()
		current.SetOutputValue(
			addrs.OutputValue{Name: name}.Absolute(addrs.RootModuleInstance),
			cty.StringVal(name), false,
		)
		state := current
		mu.Unlock()
		action, err := hook.PostStateUpdate(state)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if action != terraform.HookActionContinue {
			t.Fatalf("wrong action %#v", action)
		}
	}
	outputs := func(state *states.State) []string {
		var names []string
		for name := range state.RootOutputValues {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}

	// The first updates don't write anything before returning. A snapshot
	// follows shortly afterwards, even though nothing else happens, and
	// includes everything so far.
	update("a")
	update("b")
	<-mgr.started
	// These arrive while the first snapshot is still being written, so
	// they must be included in another snapshot afterwards, again without
	// anything else needing to happen.
	update("c")
	update("d")
	mgr.release <- struct{}{}
	<-mgr.started
	mgr.release <- struct{}{}
	hook.Wait()

	mgr.mu.Lock()
	written := mgr.written
	mgr.mu.Unlock()
	if len(written) != 2 {
		t.Fatalf("state manager received %d snapshots; want 2", len(written))
	}
	if got, want := outputs(written[0]), []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("first snapshot has outputs %q; want %q", got, want)
	}
	if got, want := outputs(written[1]), []string{"a", "b", "c", "d"}; !slices.Equal(got, want) {
		t.Errorf("second snapshot has outputs %q; want %q", got, want)
	}

	// After Wait, nothing more is written in the background.
	update("e")
	time.Sleep(50 * time.Millisecond)
	hook.Wait()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.written) != 2 {
		t.Fatalf("state manager received %d snapshots after Wait; want 2", len(mgr.written))
	}
}

func TestStateHookLargeStateShortOperation(t *testing.T) {
	mgr := &testBlockingState{
		started: make(chan struct{}, 10),
		release: make(chan struct{}, 10),
	}
	hook := &StateHook{StateMgr: mgr}
	state := testLargeState()
	snapshots := 0
	var mu sync.Mutex
	hook.BindStateSnapshots(func() *states.State {
		mu.Lock()
		defer mu.Unlock()
		snapshots++
		return state.DeepCopy()
	})

	// An operation that finishes sooner than a snapshot would take to write
	// doesn't write any intermediate snapshots at all, because the caller
	// is about to write the final state anyway.
	if _, err := hook.PostStateUpdate(state); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	hook.Wait()
	time.Sleep(2 * time.Duration(stateLargeInstanceCount) * stateWriteCostPerInstance)

	mu.Lock()
	defer mu.Unlock()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if snapshots != 0 || len(mgr.written) != 0 {
		t.Fatalf("took %d snapshots and wrote %d; want none", snapshots, len(mgr.written))
	}
}

func TestStateHookLargeStateStopping(t *testing.T) {
	mgr := &testBlockingState{
		started: make(chan struct{}, 10),
		release: make(chan struct{}),
	}
	close(mgr.release)
	hook := &StateHook{StateMgr: mgr}
	state := testLargeState()
	hook.BindStateSnapshots(func() *states.State {
		return state.DeepCopy()
	})

	// If we're asked to stop before the first snapshot was due, then the
	// updates so far are written immediately.
	if _, err := hook.PostStateUpdate(state); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	hook.Stopping()

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.written) != 1 {
		t.Fatalf("state manager received %d snapshots; want 1", len(mgr.written))
	}
	if !mgr.written[0].Equal(state) {
		t.Fatal("wrong snapshot written")
	}
}

// testRecordingState is a state manager that just remembers every snapshot
// it's given, and is safe to use from several goroutines.
type testRecordingState struct {
	mu      sync.Mutex
	written []*states.State
}

func (s *testRecordingState) WriteState(state *states.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, state.DeepCopy())
	return nil
}

func (s *testRecordingState) snapshots() []*states.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.written)
}

func TestStateHookStoppingNewerUpdate(t *testing.T) {
	mgr := &testRecordingState{}
	hook := &StateHook{
		StateMgr: mgr,
		// The hook treats the state as large, and its previous write was
		// slow enough and recent enough that another isn't due.
		large:         true,
		lastWriteCost: time.Hour,
		lastWrite:     time.Now(),
	}

	current := statemgr.TestFullInitialState()
	stopping := false
	hook.BindStateSnapshots(func() *states.State {
		snapshot := current.DeepCopy()
		if stopping {
			// Another update completes just after the hook takes the
			// snapshot that it writes when asked to stop. That update is
			// written before it returns, because the hook is stopping.
			stopping = false
			current.SetOutputValue(
				addrs.OutputValue{Name: "newer"}.Absolute(addrs.RootModuleInstance),
				cty.StringVal("newer"), false,
			)
			if _, err := hook.PostStateUpdate(current); err != nil {
				t.Errorf("unexpected error: %s", err)
			}
		}
		return snapshot
	})

	if _, err := hook.PostStateUpdate(current); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if got := len(mgr.snapshots()); got != 0 {
		t.Fatalf("state manager received %d snapshots before stopping; want none", got)
	}
	stopping = true
	hook.Stopping()

	// The older snapshot must not have replaced the newer state.
	written := mgr.snapshots()
	if len(written) == 0 {
		t.Fatal("nothing was written")
	}
	if last := written[len(written)-1]; !last.Equal(current) {
		t.Fatalf("the last of the %d snapshots written is not the latest state", len(written))
	}
}

func TestStateHookStoppingDuringBackgroundSnapshot(t *testing.T) {
	mgr := &testRecordingState{}
	hook := &StateHook{
		StateMgr: mgr,
		// The hook treats the state as large, and its previous write was
		// long enough ago that another is due.
		large:         true,
		lastWriteCost: time.Second,
		lastWrite:     time.Now().Add(-time.Hour),
	}

	// The hook takes its snapshots through a real synchronized state here,
	// because this is about how the two interact.
	ss := statemgr.TestFullInitialState().SyncWrapper()
	snapshotStarted := make(chan struct{})
	snapshotAllowed := make(chan struct{})
	var startedOnce sync.Once
	hook.BindStateSnapshots(func() *states.State {
		startedOnce.Do(func() { close(snapshotStarted) })
		<-snapshotAllowed
		return ss.Snapshot()
	})

	// The first update makes the hook start to take a snapshot in the
	// background, which we hold up just before it locks the state.
	if _, err := hook.PostStateUpdate(ss.RLock()); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ss.RUnlock()
	<-snapshotStarted

	// Then Terraform is asked to stop.
	stopped := make(chan struct{})
	go func() {
		hook.Stopping()
		close(stopped)
	}()
	for {
		hook.Lock()
		stopping := hook.stopping
		hook.Unlock()
		if stopping {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Another update arrives, whose caller holds a lock on the state while
	// it calls the hook, and something else is waiting to modify the state,
	// which means that the background snapshot can't lock the state until
	// that caller has released it.
	state := ss.RLock()
	modified := make(chan struct{})
	go func() {
		ss.SetOutputValue(
			addrs.OutputValue{Name: "later"}.Absolute(addrs.RootModuleInstance),
			cty.StringVal("later"), false,
		)
		close(modified)
	}()
	time.Sleep(100 * time.Millisecond) // for the modification to begin waiting
	close(snapshotAllowed)
	time.Sleep(100 * time.Millisecond) // for the snapshot to begin waiting
	want := state.DeepCopy()

	// The hook must therefore not make that caller wait for the snapshot.
	updated := make(chan error, 1)
	go func() {
		_, err := hook.PostStateUpdate(state)
		updated <- err
	}()
	select {
	case err := <-updated:
		ss.RUnlock()
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	case <-time.After(10 * time.Second):
		ss.RUnlock() // lets everything finish
		t.Fatal("the update is waiting for a snapshot that is waiting for the update's lock on the state")
	}
	<-modified
	<-stopped
	hook.Wait()

	// The update was written, and the snapshot that had not been taken by
	// then was not written afterwards, because it would have been of an
	// older state as far as the hook can tell.
	written := mgr.snapshots()
	if len(written) != 1 {
		t.Fatalf("state manager received %d snapshots; want 1", len(written))
	}
	if !written[0].Equal(want) {
		t.Fatal("wrong snapshot written")
	}
}
