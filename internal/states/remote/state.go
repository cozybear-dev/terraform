// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package remote

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sync"

	uuid "github.com/hashicorp/go-uuid"

	"github.com/hashicorp/terraform/internal/schemarepo"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statefile"
	"github.com/hashicorp/terraform/internal/states/statemgr"
)

// State implements the State interfaces in the state package to handle
// reading and writing the remote state. This State on its own does no
// local caching so every persist will go to the remote storage and local
// writes will go to memory.
type State struct {
	mu sync.Mutex

	Client Client

	// We track two pieces of meta data in addition to the state itself:
	//
	// lineage - the state's unique ID
	// serial  - the monotonic counter of "versions" of the state
	//
	// Both of these (along with state) have a sister field
	// that represents the values read in from an existing source.
	// All three of these values are used to determine if the new
	// state has changed from an existing state we read in.
	lineage, readLineage string
	serial, readSerial   uint64
	state, readState     *states.State

	// lastSize is the size in bytes of the most recent snapshot that we
	// read or persisted, if any.
	lastSize int

	// readDigest is the digest of readState, if we've had reason to
	// calculate it. It must be reset whenever readState changes.
	readDigest   *statefile.MarshalDigest
	disableLocks bool

	// readRevision identifies the snapshot that we most recently read, if
	// the client can tell us that and if nothing has been written to this
	// manager since, in which case refreshing can't change anything as long
	// as that's still the snapshot that's stored. It's empty otherwise.
	readRevision string

	// If this is set then the state manager will decline to store intermediate
	// state snapshots created while a Terraform Core apply operation is in
	// progress. Otherwise (by default) it will accept persistent snapshots
	// using the default rules defined in the local backend.
	DisableIntermediateSnapshots bool
}

var _ statemgr.Full = (*State)(nil)
var _ statemgr.Migrator = (*State)(nil)
var _ statemgr.IntermediateStateConditionalPersister = (*State)(nil)

// statemgr.Reader impl.
func (s *State) State() *states.State {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state.DeepCopy()
}

func (s *State) GetRootOutputValues(ctx context.Context) (map[string]*states.OutputValue, error) {
	if err := s.RefreshState(); err != nil {
		return nil, fmt.Errorf("Failed to load state: %s", err)
	}

	state := s.State()
	if state == nil {
		state = states.NewState()
	}

	return state.RootOutputValues, nil
}

// StateForMigration is part of our implementation of statemgr.Migrator.
func (s *State) StateForMigration() *statefile.File {
	s.mu.Lock()
	defer s.mu.Unlock()

	return statefile.New(s.state.DeepCopy(), s.lineage, s.serial)
}

// statemgr.Writer impl.
func (s *State) WriteState(state *states.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// We create a deep copy of the state here, because the caller also has
	// a reference to the given object and can potentially go on to mutate
	// it after we return, but we want the snapshot at this point in time.
	s.state = state.DeepCopy()
	s.readRevision = ""

	return nil
}

// WriteStateForMigration is part of our implementation of statemgr.Migrator.
func (s *State) WriteStateForMigration(f *statefile.File, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !force {
		checkFile := statefile.New(s.state, s.lineage, s.serial)
		if err := statemgr.CheckValidImport(f, checkFile); err != nil {
			return err
		}
	}

	// The remote backend needs to pass the `force` flag through to its client.
	// For backends that support such operations, inform the client
	// that a force push has been requested
	c, isForcePusher := s.Client.(ClientForcePusher)
	if force && isForcePusher {
		c.EnableForcePush()
	}

	// We create a deep copy of the state here, because the caller also has
	// a reference to the given object and can potentially go on to mutate
	// it after we return, but we want the snapshot at this point in time.
	s.state = f.State.DeepCopy()
	s.readRevision = ""
	s.lineage = f.Lineage
	s.serial = f.Serial

	return nil
}

// statemgr.Refresher impl.
func (s *State) RefreshState() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshState()
}

// refreshState is the main implementation of RefreshState, but split out so
// that we can make internal calls to it from methods that are already holding
// the s.mu lock.
func (s *State) refreshState() error {
	if rc, ok := s.Client.(ClientRevisioner); ok && s.readRevision != "" {
		// If what's stored is still the snapshot we read last time, and
		// nothing has been written here since, then reading it again would
		// leave everything exactly as it is now.
		if current, err := rc.CurrentRevision(); err == nil && current == s.readRevision {
			return nil
		}
	}
	s.readRevision = ""

	payload, diags := s.Client.Get()
	if diags.HasErrors() {
		return diags.Err()
	}

	// no remote state is OK
	if payload == nil {
		s.readState = nil
		s.readDigest = nil
		s.lineage = ""
		s.serial = 0
		return nil
	}

	stateFile, err := statefile.ReadBytes(payload.Data)
	if err != nil {
		return err
	}
	s.lastSize = len(payload.Data)

	s.lineage = stateFile.Lineage
	s.serial = stateFile.Serial
	s.state = stateFile.State

	// Properties from the remote must be separate so we can
	// track changes as lineage, serial and/or state are mutated
	s.readLineage = stateFile.Lineage
	s.readSerial = stateFile.Serial
	s.readState = s.state.DeepCopy()
	s.readDigest = nil
	s.readRevision = payload.Revision
	return nil
}

// statemgr.Persister impl.
func (s *State) PersistState(schemas *schemarepo.Schemas) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("[DEBUG] states/remote: state read serial is: %d; serial is: %d", s.readSerial, s.serial)
	log.Printf("[DEBUG] states/remote: state read lineage is: %s; lineage is: %s", s.readLineage, s.lineage)

	// stateDigest is the digest of s.state, if we calculate it below.
	var stateDigest *statefile.MarshalDigest

	if s.readState != nil {
		lineageUnchanged := s.readLineage != "" && s.lineage == s.readLineage
		serialUnchanged := s.readSerial != 0 && s.serial == s.readSerial
		stateUnchanged := false
		// We can usually tell that the state has changed without needing to
		// serialize it for comparison.
		if s.state != nil && !statefile.StatesDefinitelyDiffer(s.state, s.readState) {
			// We need the digest of the snapshot we read only the first
			// time we get here, and then we calculate it at the same time
			// as the digest of the new snapshot.
			var wg sync.WaitGroup
			if s.readDigest == nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					digest := statefile.StateMarshalDigest(s.readState)
					s.readDigest = &digest
				}()
			}
			digest := statefile.StateMarshalDigest(s.state)
			wg.Wait()
			stateDigest = &digest
			stateUnchanged = *stateDigest == *s.readDigest
		}
		if stateUnchanged && lineageUnchanged && serialUnchanged {
			// If the state, lineage or serial haven't changed at all then we have nothing to do.
			return nil
		}
		s.serial++
	} else {
		// We might be writing a new state altogether, but before we do that
		// we'll check to make sure there isn't already a snapshot present
		// that we ought to be updating.
		err := s.refreshState()
		if err != nil {
			return fmt.Errorf("failed checking for existing remote state: %s", err)
		}
		log.Printf("[DEBUG] states/remote: after refresh, state read serial is: %d; serial is: %d", s.readSerial, s.serial)
		log.Printf("[DEBUG] states/remote: after refresh, state read lineage is: %s; lineage is: %s", s.readLineage, s.lineage)
		if s.lineage == "" { // indicates that no state snapshot is present yet
			lineage, err := uuid.GenerateUUID()
			if err != nil {
				return fmt.Errorf("failed to generate initial lineage: %v", err)
			}
			s.lineage = lineage
			s.serial++
		}
	}

	f := statefile.New(s.state, s.lineage, s.serial)

	// The new snapshot is likely to be a similar size to the previous one,
	// so we allocate about that much space up front rather than repeatedly
	// growing and copying a smaller buffer, which is significant for a
	// large state.
	var buf bytes.Buffer
	buf.Grow(s.lastSize + s.lastSize/16 + 512)
	err := statefile.Write(f, &buf)
	if err != nil {
		return err
	}
	s.lastSize = buf.Len()

	// Whatever happens now, what's stored may no longer be what we read.
	s.readRevision = ""
	diags := s.Client.Put(buf.Bytes())
	if diags.HasErrors() {
		return diags.Err()
	}

	// After we've successfully persisted, what we just wrote is our new
	// reference state until someone calls RefreshState again.
	// We've potentially overwritten (via force) the state, lineage
	// and / or serial (and serial was incremented) so we copy over all
	// three fields so everything matches the new state and a subsequent
	// operation would correctly detect no changes to the lineage, serial or state.
	s.readState = s.state.DeepCopy()
	s.readDigest = stateDigest
	s.readLineage = s.lineage
	s.readSerial = s.serial
	return nil
}

// ShouldPersistIntermediateState implements statemgr.IntermediateStateConditionalPersister
func (s *State) ShouldPersistIntermediateState(info *statemgr.IntermediateStatePersistInfo) bool {
	if s.DisableIntermediateSnapshots {
		return false
	}
	return statemgr.DefaultIntermediateStatePersistRule(info)
}

// Lock calls the Client's Lock method if it's implemented.
func (s *State) Lock(info *statemgr.LockInfo) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.disableLocks {
		return "", nil
	}

	if c, ok := s.Client.(ClientLocker); ok {
		return c.Lock(info)
	}
	return "", nil
}

// Unlock calls the Client's Unlock method if it's implemented.
func (s *State) Unlock(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.disableLocks {
		return nil
	}

	if c, ok := s.Client.(ClientLocker); ok {
		return c.Unlock(id)
	}
	return nil
}

// DisableLocks turns the Lock and Unlock methods into no-ops. This is intended
// to be called during initialization of a state manager and should not be
// called after any of the statemgr.Full interface methods have been called.
func (s *State) DisableLocks() {
	s.disableLocks = true
}

// StateSnapshotMeta returns the metadata from the most recently persisted
// or refreshed persistent state snapshot.
//
// This is an implementation of statemgr.PersistentMeta.
func (s *State) StateSnapshotMeta() statemgr.SnapshotMeta {
	return statemgr.SnapshotMeta{
		Lineage: s.lineage,
		Serial:  s.serial,
	}
}
