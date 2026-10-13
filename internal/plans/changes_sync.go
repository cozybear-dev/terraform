// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package plans

import (
	"iter"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/terraform/internal/addrs"
)

// ChangesSync is a wrapper around a Changes that provides a concurrency-safe
// interface to insert new changes and retrieve copies of existing changes.
//
// Each ChangesSync is independent of all others, so all concurrent writers
// to a particular Changes must share a single ChangesSync. Behavior is
// undefined if any other caller makes changes to the underlying Changes
// object or its nested objects concurrently with any of the methods of a
// particular ChangesSync.
// Once the object is closed, it is no longer writable and any further
// modifications will panic.
type ChangesSync struct {
	lock    sync.Mutex
	changes *Changes
	closed  atomic.Bool

	// decodeErrs are the errors from decoding the values of changes whose
	// decoding was deferred. See DecodeErrors.
	decodeErrs []error

	// index allows finding resource instance changes by address without
	// scanning all of the changes. It's built when first needed, and
	// must be accessed only through resourceIndex while holding the lock.
	index *resourceChangeIndex

	// outputs is the equivalent for output value changes, which must be
	// accessed only through outputIndex while holding the lock.
	outputs *outputChangeIndex
}

// resourceChangeIndex is an index of the resource instance changes in a
// Changes object. Each of the slices in the index has the changes in the
// same relative order as they appear in Changes.Resources.
type resourceChangeIndex struct {
	// byInstance has all of the changes for each resource instance, including
	// those for deposed objects, keyed by the string representation of
	// addrs.AbsResourceInstance.
	byInstance map[string][]*ResourceInstanceChange

	// byResource and byConfig have only the changes for current objects,
	// keyed by the string representations of addrs.AbsResource and
	// addrs.ConfigResource respectively.
	byResource map[string][]*ResourceInstanceChange
	byConfig   map[string][]*ResourceInstanceChange

	// indexed is the Changes.Resources slice that the index describes,
	// which we use to notice if it has been changed by anything other than
	// the ChangesSync methods.
	indexed []*ResourceInstanceChange

	// The following fields track when the changes for each resource were
	// last modified, to support [ChangesSync.ConfigResourceGeneration].
	// generation is incremented for each modification, and
	// configGenerations records the value of generation for the latest
	// modification of the changes for each resource, keyed by the string
	// representation of addrs.ConfigResource. baseGeneration is the smallest
	// number that this index reports for any resource, which is larger than
	// any number reported by an earlier index for the same Changes.
	generation        uint64
	baseGeneration    uint64
	configGenerations map[string]uint64
}

// modified records that the changes for the resource that the given change
// belongs to have been modified.
func (idx *resourceChangeIndex) modified(rc *ResourceInstanceChange) {
	idx.generation++
	idx.configGenerations[rc.Addr.ConfigResource().String()] = idx.generation
}

// ConfigResourceGeneration returns a number that is different each time
// the set of changes for instances of the given resource has potentially
// changed.
//
// A caller that derives something expensive from the changes for a resource
// can call this before reading the changes and remember the result alongside
// what it derived. The derived data is then still current for as long as
// ConfigResourceGeneration continues to return the same number.
func (cs *ChangesSync) ConfigResourceGeneration(addr addrs.ConfigResource) uint64 {
	if cs == nil {
		panic("ConfigResourceGeneration on nil ChangesSync")
	}
	cs.lock.Lock()
	defer cs.lock.Unlock()
	idx := cs.resourceIndex()
	return max(idx.baseGeneration, idx.configGenerations[addr.String()])
}

// resourceIndex returns the index of cs.changes.Resources, building it first
// if necessary. The caller must hold the lock.
func (cs *ChangesSync) resourceIndex() *resourceChangeIndex {
	resources := cs.changes.Resources
	if idx := cs.index; idx != nil && len(idx.indexed) == len(resources) {
		if len(resources) == 0 || idx.indexed[0] == resources[0] && idx.indexed[len(resources)-1] == resources[len(resources)-1] {
			return idx
		}
	}

	idx := &resourceChangeIndex{
		byInstance:        make(map[string][]*ResourceInstanceChange),
		byResource:        make(map[string][]*ResourceInstanceChange),
		byConfig:          make(map[string][]*ResourceInstanceChange),
		configGenerations: make(map[string]uint64),
	}
	for _, rc := range resources {
		idx.add(rc)
	}
	idx.indexed = resources
	if cs.index != nil {
		// The changes were modified without our involvement, so we don't
		// know which resources were affected and must treat all of them
		// as modified.
		idx.generation = cs.index.generation + 1
		idx.baseGeneration = idx.generation
	}
	cs.index = idx
	return idx
}

func (idx *resourceChangeIndex) add(rc *ResourceInstanceChange) {
	instKey := rc.Addr.String()
	idx.byInstance[instKey] = append(idx.byInstance[instKey], rc)
	if rc.DeposedKey == addrs.NotDeposed {
		resKey := rc.Addr.ContainingResource().String()
		idx.byResource[resKey] = append(idx.byResource[resKey], rc)
		configKey := rc.Addr.ConfigResource().String()
		idx.byConfig[configKey] = append(idx.byConfig[configKey], rc)
	}
}

func (idx *resourceChangeIndex) remove(rc *ResourceInstanceChange) {
	remove := func(m map[string][]*ResourceInstanceChange, key string) {
		// We always build a new slice here, because a caller might still be
		// iterating over the old one.
		old := m[key]
		if len(old) <= 1 {
			delete(m, key)
			return
		}
		updated := make([]*ResourceInstanceChange, 0, len(old)-1)
		for _, other := range old {
			if other != rc {
				updated = append(updated, other)
			}
		}
		m[key] = updated
	}
	remove(idx.byInstance, rc.Addr.String())
	if rc.DeposedKey == addrs.NotDeposed {
		remove(idx.byResource, rc.Addr.ContainingResource().String())
		remove(idx.byConfig, rc.Addr.ConfigResource().String())
	}
}

// instance returns the change for the given object of the given resource
// instance, or nil if there isn't one.
func (idx *resourceChangeIndex) instance(addr addrs.AbsResourceInstance, dk addrs.DeposedKey) *ResourceInstanceChange {
	for _, rc := range idx.byInstance[addr.String()] {
		if rc.DeposedKey == dk {
			return rc
		}
	}
	return nil
}

// AppendResourceInstanceChange records the given resource instance change in
// the set of planned resource changes.
//
// The caller must ensure that there are no concurrent writes to the given
// change while this method is running, but it is safe to resume mutating
// it after this method returns without affecting the saved change.
func (cs *ChangesSync) AppendResourceInstanceChange(change *ResourceInstanceChange) {
	if cs == nil {
		panic("AppendResourceInstanceChange on nil ChangesSync")
	}
	defer cs.beginWrite()()

	s := change.DeepCopy()
	idx := cs.resourceIndex()
	cs.changes.Resources = append(cs.changes.Resources, s)
	idx.add(s)
	idx.modified(s)
	idx.indexed = cs.changes.Resources
}

func (cs *ChangesSync) AppendQueryInstance(query *QueryInstance) {
	if cs == nil {
		panic("AppendQueryInstance on nil ChangesSync")
	}
	defer cs.beginWrite()()

	s := query.DeepCopy() // TODO do we need to deep copy here?
	cs.changes.Queries = append(cs.changes.Queries, s)
}

// GetResourceInstanceChange searches the set of resource instance changes for
// one matching the given address and deposed key, returning it if it exists.
// Use [addrs.NotDeposed] as the deposed key to represent the "current"
// object for the given resource instance.
//
// If no such change exists, nil is returned.
//
// The returned object is a deep copy of the change recorded in the plan, so
// callers may mutate it although it's generally better (less confusing) to
// treat planned changes as immutable after they've been initially constructed.
func (cs *ChangesSync) GetResourceInstanceChange(addr addrs.AbsResourceInstance, dk addrs.DeposedKey) *ResourceInstanceChange {
	if cs == nil {
		panic("GetResourceInstanceChange on nil ChangesSync")
	}
	cs.lock.Lock()
	rc := cs.resourceIndex().instance(addr, dk)
	cs.lock.Unlock()

	return cs.decoded(rc)[0]
}

// GetCurrentResourceInstanceChanges is like GetResourceInstanceChange for
// the current object of each of the given resource instances, returning the
// results in the same order, with nil for any instance that has no change.
//
// This is preferable when the caller needs the changes for many instances,
// because the changes are a major point of lock contention.
func (cs *ChangesSync) GetCurrentResourceInstanceChanges(instAddrs []addrs.AbsResourceInstance) []*ResourceInstanceChange {
	if cs == nil {
		panic("GetCurrentResourceInstanceChanges on nil ChangesSync")
	}
	if len(instAddrs) == 0 {
		return nil
	}
	// Building the keys doesn't need the lock.
	keys := make([]string, len(instAddrs))
	for i, addr := range instAddrs {
		keys[i] = addr.String()
	}
	ret := make([]*ResourceInstanceChange, len(instAddrs))

	cs.lock.Lock()
	idx := cs.resourceIndex()
	for i, key := range keys {
		for _, rc := range idx.byInstance[key] {
			if rc.DeposedKey == addrs.NotDeposed {
				ret[i] = rc
				break
			}
		}
	}
	cs.lock.Unlock()

	return cs.decoded(ret...)
}

// decoded returns a copy of each of the given changes, which can be nil,
// after making sure that its values have been decoded if their decoding
// was deferred. The caller must not hold the lock: decoding can be a
// significant amount of work, and the changes are a major point of lock
// contention, so we don't hold the lock while doing it. A change is never
// modified once it has been recorded, other than by this.
func (cs *ChangesSync) decoded(rcs ...*ResourceInstanceChange) []*ResourceInstanceChange {
	ret := make([]*ResourceInstanceChange, len(rcs))
	var errs []error
	for i, rc := range rcs {
		if rc == nil {
			continue
		}
		if rc.DeferredValues != nil {
			if err := rc.decodeValues(); err != nil {
				errs = append(errs, err)
			}
		}
		ret[i] = rc.DeepCopy()
	}
	if len(errs) != 0 {
		cs.lock.Lock()
		for _, err := range errs {
			if !slices.Contains(cs.decodeErrs, err) {
				cs.decodeErrs = append(cs.decodeErrs, err)
			}
		}
		cs.lock.Unlock()
	}
	return ret
}

// DecodeErrors returns any errors encountered so far when decoding the
// values of changes whose decoding was deferred by
// [ChangesSrc.DecodeDeferringNoOps]. The values of an affected change are
// unknown, so results derived from them are not reliable.
func (cs *ChangesSync) DecodeErrors() []error {
	cs.lock.Lock()
	defer cs.lock.Unlock()
	return slices.Clone(cs.decodeErrs)
}

// ResourceInstanceChangeAction describes the action planned for the current
// object of a resource instance, without the values involved.
type ResourceInstanceChangeAction struct {
	Addr   addrs.AbsResourceInstance
	Action Action
}

// GetChangeActionsForConfigResource is like GetChangesForConfigResource but
// returns only the address and action of each change. That's much cheaper,
// both because it doesn't copy the changes and because it doesn't need the
// values of any change whose decoding has been deferred.
func (cs *ChangesSync) GetChangeActionsForConfigResource(addr addrs.ConfigResource) []ResourceInstanceChangeAction {
	if cs == nil {
		panic("GetChangeActionsForConfigResource on nil ChangesSync")
	}
	cs.lock.Lock()
	defer cs.lock.Unlock()
	changes := cs.resourceIndex().byConfig[addr.String()]
	if len(changes) == 0 {
		return nil
	}
	ret := make([]ResourceInstanceChangeAction, len(changes))
	for i, rc := range changes {
		ret[i] = ResourceInstanceChangeAction{rc.Addr, rc.Action}
	}
	return ret
}

// GetChangesForConfigResource searches the set of resource instance
// changes and returns all changes related to a given configuration address.
// This is be used to find possible changes related to a configuration
// reference.
//
// If no such changes exist, nil is returned.
//
// The returned objects are a deep copy of the change recorded in the plan, so
// callers may mutate them although it's generally better (less confusing) to
// treat planned changes as immutable after they've been initially constructed.
func (cs *ChangesSync) GetChangesForConfigResource(addr addrs.ConfigResource) []*ResourceInstanceChange {
	if cs == nil {
		panic("GetChangesForConfigResource on nil ChangesSync")
	}
	cs.lock.Lock()
	// The index never modifies a slice once something could have seen it.
	changes := cs.resourceIndex().byConfig[addr.String()]
	cs.lock.Unlock()
	if len(changes) == 0 {
		return nil
	}
	return cs.decoded(changes...)
}

// ReadInstancesForConfigResource returns an iterator over the changes for a given
// configuration resource, applying the given selector function to each change.
// The changes must no longer be writable otherwise a panic will occur.
func ReadInstancesForConfigResource(changesSync *ChangesSync, addr addrs.ConfigResource) iter.Seq[*ResourceInstanceChange] {
	if changesSync == nil {
		panic("ReadInstancesForConfigResource on nil ChangesSync")
	}
	if !changesSync.closed.Load() {
		panic("ReadEachConfigResourceChange requires closed ChangesSync")
	}

	changesSync.lock.Lock()
	changes := changesSync.resourceIndex().byConfig[addr.String()]
	changesSync.lock.Unlock()
	// This is for its effect on the changes themselves, which we return.
	changesSync.decoded(changes...)
	return slices.Values(changes)
}

// GetChangesForAbsResource searches the set of resource instance
// changes and returns all changes related to a given configuration address.
//
// If no such changes exist, nil is returned.
//
// The returned objects are a deep copy of the change recorded in the plan, so
// callers may mutate them although it's generally better (less confusing) to
// treat planned changes as immutable after they've been initially constructed.
func (cs *ChangesSync) GetChangesForAbsResource(addr addrs.AbsResource) []*ResourceInstanceChange {
	if cs == nil {
		panic("GetChangesForAbsResource on nil ChangesSync")
	}
	cs.lock.Lock()
	// The index never modifies a slice once something could have seen it.
	changes := cs.resourceIndex().byResource[addr.String()]
	cs.lock.Unlock()
	if len(changes) == 0 {
		return nil
	}
	return cs.decoded(changes...)
}

func (cs *ChangesSync) GetQueryInstancesForAbsResource(addr addrs.AbsResource) []*QueryInstance {
	if cs == nil {
		panic("GetQueryInstancesForAbsResource on nil ChangesSync")
	}
	cs.lock.Lock()
	defer cs.lock.Unlock()
	var queries []*QueryInstance
	for _, q := range cs.changes.QueriesForAbsResource(addr) {
		queries = append(queries, q.DeepCopy())
	}
	return queries
}

// RemoveResourceInstanceChange searches the set of resource instance changes
// for one matching the given address and deposed key, and removes it from the
// set if it exists.
func (cs *ChangesSync) RemoveResourceInstanceChange(addr addrs.AbsResourceInstance, dk addrs.DeposedKey) {
	if cs == nil {
		panic("RemoveResourceInstanceChange on nil ChangesSync")
	}
	defer cs.beginWrite()()

	idx := cs.resourceIndex()
	rc := idx.instance(addr, dk)
	if rc == nil {
		return
	}
	i := slices.Index(cs.changes.Resources, rc)
	copy(cs.changes.Resources[i:], cs.changes.Resources[i+1:])
	cs.changes.Resources = cs.changes.Resources[:len(cs.changes.Resources)-1]
	idx.remove(rc)
	idx.modified(rc)
	idx.indexed = cs.changes.Resources
}

// AppendOutputChange records the given output value change in the set of
// planned value changes.
//
// The caller must ensure that there are no concurrent writes to the given
// change while this method is running, but it is safe to resume mutating
// it after this method returns without affecting the saved change.
func (cs *ChangesSync) AppendOutputChange(changeSrc *OutputChange) {
	if cs == nil {
		panic("AppendOutputChange on nil ChangesSync")
	}
	defer cs.beginWrite()()

	idx := cs.outputIndex()
	cs.changes.Outputs = append(cs.changes.Outputs, changeSrc)
	idx.add(changeSrc)
	idx.indexed = cs.changes.Outputs
}

// outputChangeIndex is an index of the output value changes in a Changes
// object, which allows finding them without scanning all of them. A
// configuration with many modules has a great many output values, each of
// which is looked up at least once, so scanning for each one takes time in
// proportion to the square of how many there are.
//
// Each of the slices in the index has the changes in the same relative
// order as they appear in Changes.Outputs.
type outputChangeIndex struct {
	// byAddr has the changes for each output value, keyed by the string
	// representation of its address.
	byAddr map[string][]*OutputChange

	// byCall has the changes for the output values of all instances of
	// each module call, keyed by outputCallKey.
	byCall map[string][]*OutputChange

	// root has the changes for the output values of the root module.
	root []*OutputChange

	// indexed is the Changes.Outputs slice that the index describes, which
	// we use to detect that the changes were modified without our
	// involvement.
	indexed []*OutputChange
}

// outputCallKey returns the key in outputChangeIndex.byCall for the given
// module call inside the given module instance.
func outputCallKey(parent addrs.ModuleInstance, call string) string {
	return parent.String() + "\x00" + call
}

// outputIndex returns the index of cs.changes.Outputs, building it first
// if necessary. The caller must hold the lock.
func (cs *ChangesSync) outputIndex() *outputChangeIndex {
	outputs := cs.changes.Outputs
	if idx := cs.outputs; idx != nil && len(idx.indexed) == len(outputs) {
		if len(outputs) == 0 || idx.indexed[0] == outputs[0] && idx.indexed[len(outputs)-1] == outputs[len(outputs)-1] {
			return idx
		}
	}
	idx := &outputChangeIndex{
		byAddr: make(map[string][]*OutputChange),
		byCall: make(map[string][]*OutputChange),
	}
	for _, oc := range outputs {
		idx.add(oc)
	}
	idx.indexed = outputs
	cs.outputs = idx
	return idx
}

func (idx *outputChangeIndex) add(oc *OutputChange) {
	addrKey := oc.Addr.String()
	idx.byAddr[addrKey] = append(idx.byAddr[addrKey], oc)
	if oc.Addr.Module.IsRoot() {
		idx.root = append(idx.root, oc)
		return
	}
	parent, call := oc.Addr.Module.Call()
	callKey := outputCallKey(parent, call.Name)
	idx.byCall[callKey] = append(idx.byCall[callKey], oc)
}

func (idx *outputChangeIndex) remove(oc *OutputChange) {
	// We always build a new slice here, because a caller might still be
	// using the old one. Only the first occurrence goes, which is what
	// happens to Changes.Outputs, so that the two still agree if the same
	// change was somehow recorded more than once.
	without := func(old []*OutputChange) []*OutputChange {
		at := slices.Index(old, oc)
		if at < 0 {
			return old
		}
		if len(old) == 1 {
			return nil
		}
		updated := make([]*OutputChange, 0, len(old)-1)
		updated = append(updated, old[:at]...)
		return append(updated, old[at+1:]...)
	}
	addrKey := oc.Addr.String()
	if updated := without(idx.byAddr[addrKey]); len(updated) != 0 {
		idx.byAddr[addrKey] = updated
	} else {
		delete(idx.byAddr, addrKey)
	}
	if oc.Addr.Module.IsRoot() {
		idx.root = without(idx.root)
		return
	}
	parent, call := oc.Addr.Module.Call()
	callKey := outputCallKey(parent, call.Name)
	if updated := without(idx.byCall[callKey]); len(updated) != 0 {
		idx.byCall[callKey] = updated
	} else {
		delete(idx.byCall, callKey)
	}
}

// GetOutputChange searches the set of output value changes for one matching
// the given address, returning it if it exists.
//
// If no such change exists, nil is returned.
//
// The returned object is a deep copy of the change recorded in the plan, so
// callers may mutate it although it's generally better (less confusing) to
// treat planned changes as immutable after they've been initially constructed.
func (cs *ChangesSync) GetOutputChange(addr addrs.AbsOutputValue) *OutputChange {
	if cs == nil {
		panic("GetOutputChange on nil ChangesSync")
	}
	cs.lock.Lock()
	defer cs.lock.Unlock()

	if found := cs.outputIndex().byAddr[addr.String()]; len(found) != 0 {
		return found[0]
	}
	return nil
}

// GetRootOutputChanges searches the set of output changes for any that reside
// the root module. If no such changes exist, nil is returned.
//
// The returned objects are a deep copy of the change recorded in the plan, so
// callers may mutate them although it's generally better (less confusing) to
// treat planned changes as immutable after they've been initially constructed.
func (cs *ChangesSync) GetRootOutputChanges() []*OutputChange {
	if cs == nil {
		panic("GetRootOutputChanges on nil ChangesSync")
	}
	cs.lock.Lock()
	defer cs.lock.Unlock()

	return slices.Clone(cs.outputIndex().root)
}

// GetOutputChanges searches the set of output changes for any that reside in
// module instances beneath the given module. If no changes exist, nil
// is returned.
//
// The returned objects are a deep copy of the change recorded in the plan, so
// callers may mutate them although it's generally better (less confusing) to
// treat planned changes as immutable after they've been initially constructed.
func (cs *ChangesSync) GetOutputChanges(parent addrs.ModuleInstance, module addrs.ModuleCall) []*OutputChange {
	if cs == nil {
		panic("GetOutputChange on nil ChangesSync")
	}
	cs.lock.Lock()
	defer cs.lock.Unlock()

	return slices.Clone(cs.outputIndex().byCall[outputCallKey(parent, module.Name)])
}

// RemoveOutputChange searches the set of output value changes for one matching
// the given address, and removes it from the set if it exists.
func (cs *ChangesSync) RemoveOutputChange(addr addrs.AbsOutputValue) {
	if cs == nil {
		panic("RemoveOutputChange on nil ChangesSync")
	}
	defer cs.beginWrite()()

	idx := cs.outputIndex()
	found := idx.byAddr[addr.String()]
	if len(found) == 0 {
		// The usual case while planning, where this is called for every
		// output value before its change is recorded.
		return
	}
	remove := found[0]
	for i, o := range cs.changes.Outputs {
		if o != remove {
			continue
		}
		copy(cs.changes.Outputs[i:], cs.changes.Outputs[i+1:])
		cs.changes.Outputs = cs.changes.Outputs[:len(cs.changes.Outputs)-1]
		idx.remove(remove)
		idx.indexed = cs.changes.Outputs
		return
	}
}

// GetActionInvocation gets an action invocation based on the action address, the triggering
// resource address, the action trigger block index, and the action list index.
func (cs *ChangesSync) GetActionInvocation(addr addrs.AbsActionInstance, actionTrigger ActionTrigger) *ActionInvocationInstance {
	if cs == nil {
		panic("GetActionInvocation on nil ChangesSync")
	}
	cs.lock.Lock()
	defer cs.lock.Unlock()

	for _, a := range cs.changes.ActionInvocations {
		if a.Addr.Equal(addr) {
			if a.ActionTrigger.Equals(actionTrigger) {
				return a
			}
		}
	}
	return nil
}

// AppendActionInvocation
func (cs *ChangesSync) AppendActionInvocation(action *ActionInvocationInstance) {
	if cs == nil {
		panic("AppendActionInvocation on nil ChangesSync")
	}
	defer cs.beginWrite()()

	cs.changes.ActionInvocations = append(cs.changes.ActionInvocations, action)
}

// RemoveActionInvocation searches the set of action invocations for one
// matching the given address, and removes it from the set if it exists.
func (cs *ChangesSync) RemoveActionInvocation(addr addrs.AbsActionInstance) {
	if cs == nil {
		panic("RemoveActionInvocation on nil ChangesSync")
	}
	defer cs.beginWrite()()

	addrStr := addr.String()
	for i, a := range cs.changes.ActionInvocations {
		if a.Addr.String() != addrStr {
			continue
		}
		copy(cs.changes.ActionInvocations[i:], cs.changes.ActionInvocations[i+1:])
		cs.changes.ActionInvocations = cs.changes.ActionInvocations[:len(cs.changes.ActionInvocations)-1]
		return
	}
}

// Close marks the wrapper as closed, so any further modifications will panic.
func (cs *ChangesSync) Close() {
	cs.lock.Lock()
	defer cs.lock.Unlock()
	cs.closed.Store(true) // only reading is allowed after close
}

func (cs *ChangesSync) beginWrite() func() {
	cs.lock.Lock()
	if cs.closed.Load() {
		cs.lock.Unlock()
		panic("write on closed ChangesSync")
	}
	return func() {
		cs.lock.Unlock()
	}
}
