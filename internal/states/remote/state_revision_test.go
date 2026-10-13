// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package remote

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statefile"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

// revisionedClient is a client that stores one snapshot in memory and
// numbers each one it's given.
type revisionedClient struct {
	data     []byte
	revision int

	gets, revisionChecks int
	revisionErr          error
	omitRevision         bool
}

var _ ClientRevisioner = (*revisionedClient)(nil)

func (c *revisionedClient) Get() (*Payload, tfdiags.Diagnostics) {
	c.gets++
	if c.data == nil {
		return nil, nil
	}
	payload := &Payload{Data: bytes.Clone(c.data)}
	if !c.omitRevision {
		payload.Revision = fmt.Sprint(c.revision)
	}
	return payload, nil
}

func (c *revisionedClient) Put(data []byte) tfdiags.Diagnostics {
	c.data = bytes.Clone(data)
	c.revision++
	return nil
}

func (c *revisionedClient) Delete() tfdiags.Diagnostics {
	c.data = nil
	c.revision++
	return nil
}

func (c *revisionedClient) CurrentRevision() (string, error) {
	c.revisionChecks++
	if c.revisionErr != nil {
		return "", c.revisionErr
	}
	if c.data == nil {
		return "", nil
	}
	return fmt.Sprint(c.revision), nil
}

// Refreshing must only skip reading the snapshot when doing so would leave
// the manager exactly as it already is.
func TestState_refreshWithRevisions(t *testing.T) {
	newState := func(value string) *states.State {
		return states.BuildState(func(s *states.SyncState) {
			s.SetOutputValue(addrs.OutputValue{Name: "v"}.Absolute(addrs.RootModuleInstance), cty.StringVal(value), false)
		})
	}
	stored := func(t *testing.T, client *revisionedClient) *statefile.File {
		t.Helper()
		f, err := statefile.Read(bytes.NewReader(client.data))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	value := func(s *states.State) string {
		if s == nil {
			return "<nil>"
		}
		return s.RootOutputValues["v"].Value.AsString()
	}
	refresh := func(t *testing.T, mgr *State, client *revisionedClient, wantGet bool, wantValue string) {
		t.Helper()
		before := client.gets
		if err := mgr.RefreshState(); err != nil {
			t.Fatal(err)
		}
		if got := client.gets > before; got != wantGet {
			t.Fatalf("read the snapshot = %t; want %t", got, wantGet)
		}
		if got := value(mgr.State()); got != wantValue {
			t.Fatalf("state has value %q; want %q", got, wantValue)
		}
	}

	client := &revisionedClient{}
	mgr := &State{Client: client}

	// There's nothing stored yet, so there's nothing to skip.
	if err := mgr.RefreshState(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RefreshState(); err != nil {
		t.Fatal(err)
	}
	if client.gets != 2 {
		t.Fatalf("read %d times with nothing stored; want 2", client.gets)
	}

	// Our own write means that what we last read is out of date.
	if err := mgr.WriteState(newState("a")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.PersistState(nil); err != nil {
		t.Fatal(err)
	}
	refresh(t, mgr, client, true, "a")
	serial := stored(t, client).Serial

	// Nothing has changed, so there's nothing to read, however many times.
	refresh(t, mgr, client, false, "a")
	refresh(t, mgr, client, false, "a")
	if client.revisionChecks == 0 {
		t.Fatal("never asked for the current revision")
	}

	// Something else replaces the snapshot.
	other := &State{Client: client}
	if err := other.RefreshState(); err != nil {
		t.Fatal(err)
	}
	if err := other.WriteState(newState("b")); err != nil {
		t.Fatal(err)
	}
	if err := other.PersistState(nil); err != nil {
		t.Fatal(err)
	}
	refresh(t, mgr, client, true, "b")
	refresh(t, mgr, client, false, "b")

	// A local write that hasn't been persisted is discarded by refreshing,
	// which means reading the snapshot again.
	if err := mgr.WriteState(newState("local")); err != nil {
		t.Fatal(err)
	}
	refresh(t, mgr, client, true, "b")
	refresh(t, mgr, client, false, "b")

	// Persisting after skipped refreshes must still work as usual.
	if err := mgr.WriteState(newState("c")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.PersistState(nil); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, client); value(got.State) != "c" || got.Serial <= serial {
		t.Fatalf("stored value %q with serial %d; want \"c\" with a serial greater than %d", value(got.State), got.Serial, serial)
	}
	refresh(t, mgr, client, true, "c")
	refresh(t, mgr, client, false, "c")

	// If the revision can't be determined then we read the snapshot.
	client.revisionErr = errors.New("unavailable")
	refresh(t, mgr, client, true, "c")
	client.revisionErr = nil
	refresh(t, mgr, client, false, "c")

	// A snapshot that comes without a revision is always read again.
	client.omitRevision = true
	if err := mgr.WriteState(newState("d")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.PersistState(nil); err != nil {
		t.Fatal(err)
	}
	refresh(t, mgr, client, true, "d")
	refresh(t, mgr, client, true, "d")
	client.omitRevision = false

	// The snapshot being deleted is a change like any other.
	refresh(t, mgr, client, true, "d")
	refresh(t, mgr, client, false, "d")
	client.Delete()
	before := client.gets
	if err := mgr.RefreshState(); err != nil {
		t.Fatal(err)
	}
	if client.gets == before {
		t.Fatal("didn't read after the snapshot was deleted")
	}
}
