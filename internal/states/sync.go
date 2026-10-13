// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"iter"
	"log"
	"sync"
	"sync/atomic"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/checks"
)

// SyncState is a wrapper around State that provides concurrency-safe access to
// various common operations that occur during a Terraform graph walk, or other
// similar concurrent contexts.
//
// When a SyncState wrapper is in use, no concurrent direct access to the
// underlying objects is permitted unless the caller first acquires an explicit
// lock, using the Lock and Unlock methods. Most callers should _not_
// explicitly lock, and should instead use the other methods of this type that
// handle locking automatically.
//
// Since SyncState is able to safely consolidate multiple updates into a single
// atomic operation, many of its methods are at a higher level than those
// of the underlying types, and operate on the state as a whole rather than
// on individual sub-structures of the state.
//
// SyncState can only protect against races within its own methods. It cannot
// provide any guarantees about the order in which concurrent operations will
// be processed, so callers may still need to employ higher-level techniques
// for ensuring correct operation sequencing, such as building and walking
// a dependency graph.
type SyncState struct {
	state    *State
	writable bool
	lock     sync.RWMutex

	// The following fields track when the resources in the state were last
	// modified, to support [SyncState.ResourceGeneration]. generation is
	// incremented for each modification. resourceGenerations records the
	// value of generation for the latest modification that affected only a
	// single resource, keyed by the string representation of the resource's
	// address, and allGeneration records it for the latest modification
	// that could have affected any resource.
	generation          uint64
	allGeneration       uint64
	resourceGenerations map[string]uint64

	// moduleSetGeneration is incremented each time the set of modules in
	// the state might have changed, which is whenever a caller could have
	// modified anything and whenever a narrower modification left the
	// state with a different number of modules. Like the fields above, it
	// must only be accessed while holding the lock.
	moduleSetGeneration uint64

	// moduleIndex is an index of the modules in the state that describes
	// them as of moduleIndexGeneration, for [SyncState.ResourcesWhileRLocked].
	// These two have their own lock because they are updated by callers
	// that hold the main one only for reading.
	moduleIndexLock       sync.Mutex
	moduleIndex           *ModuleInstancesIndex
	moduleIndexGeneration uint64
}

// ResourcesWhileRLocked returns the same as [State.Resources] would for the
// state returned by [SyncState.RLock], which the caller must have called and
// not yet released. The result is subject to the same rules as that state.
//
// This is for a caller that asks about many resources, such as once for
// each resource in the configuration. State.Resources looks through every
// module in the state to find the ones a resource could belong to, whereas
// this keeps an index of them for as long as the set of modules is
// unchanged.
func (s *SyncState) ResourcesWhileRLocked(addr addrs.ConfigResource) []*Resource {
	if !s.writable {
		// The state can now be modified without our involvement, so we
		// can't know whether an index is still current.
		return s.state.Resources(addr)
	}

	s.moduleIndexLock.Lock()
	if s.moduleIndex == nil || s.moduleIndexGeneration != s.moduleSetGeneration {
		s.moduleIndex = s.state.IndexModuleInstances()
		s.moduleIndexGeneration = s.moduleSetGeneration
	}
	idx := s.moduleIndex
	s.moduleIndexLock.Unlock()

	var ret []*Resource
	for _, m := range idx.ModuleInstances(addr.Module) {
		r := m.Resource(addr.Resource)
		if r != nil {
			ret = append(ret, r)
		}
	}
	return ret
}

// ResourceGeneration returns a number that is different each time anything
// about the resource with the given address has potentially changed in the
// state.
//
// A caller that derives something expensive from the state of a resource can
// call this before reading the resource and remember the result alongside
// what it derived. The derived data is then still current for as long as
// ResourceGeneration continues to return the same number.
//
// Once [SyncState.Close] has been called the state can be modified without
// the SyncState being involved, and so from then on every call returns a
// different number.
func (s *SyncState) ResourceGeneration(addr addrs.AbsResource) uint64 {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if !s.writable {
		return untrackedGenerations.Add(1)
	}
	return max(s.allGeneration, s.resourceGenerations[addr.String()])
}

// untrackedGenerations provides the results of [SyncState.ResourceGeneration]
// for states whose modifications can no longer be tracked. It starts well
// beyond the range of the numbers used for tracked modifications.
var untrackedGenerations = func() *atomic.Uint64 {
	var ret atomic.Uint64
	ret.Store(1 << 62)
	return &ret
}()

// Module returns a snapshot of the state of the module instance with the given
// address, or nil if no such module is tracked.
//
// The return value is a pointer to a copy of the module state, which the
// caller may then freely access and mutate. However, since the module state
// tends to be a large data structure with many child objects, where possible
// callers should prefer to use a more granular accessor to access a child
// module directly, and thus reduce the amount of copying required.
func (s *SyncState) Module(addr addrs.ModuleInstance) *Module {
	s.lock.RLock()
	ret := s.state.Module(addr).DeepCopy()
	s.lock.RUnlock()
	return ret
}

// ModuleInstances returns the addresses of the module instances currently
// store within state for the given module.
func (s *SyncState) ModuleInstances(addr addrs.Module) []addrs.ModuleInstance {
	s.lock.RLock()
	ret := s.state.ModuleInstances(addr)
	s.lock.RUnlock()

	insts := make([]addrs.ModuleInstance, len(ret))
	for i, inst := range ret {
		insts[i] = inst.Addr
	}
	return insts
}

// RemoveModule removes the entire state for the given module, taking with
// it any resources associated with the module. This should generally be
// called only for modules whose resources have all been destroyed, but
// that is not enforced by this method.
func (s *SyncState) RemoveModule(addr addrs.ModuleInstance) {
	defer s.beginWrite()()

	s.state.RemoveModule(addr)
}

// OutputValue returns a snapshot of the state of the output value with the
// given address, or nil if no such output value is tracked.
//
// The return value is a pointer to a copy of the output value state, which the
// caller may then freely access and mutate.
func (s *SyncState) OutputValue(addr addrs.AbsOutputValue) *OutputValue {
	s.lock.RLock()
	ret := s.state.OutputValue(addr).DeepCopy()
	s.lock.RUnlock()
	return ret
}

// SetOutputValue writes a given output value into the state, overwriting
// any existing value of the same name.
//
// The state only tracks output values for the root module, so attempts to
// write output values for any other module will be silently ignored.
func (s *SyncState) SetOutputValue(addr addrs.AbsOutputValue, value cty.Value, sensitive bool) {
	if !addr.Module.IsRoot() {
		return
	}

	defer s.beginOutputWrite()()
	s.state.SetOutputValue(addr, value, sensitive)
}

// RemoveOutputValue removes the stored value for the output value with the
// given address.
//
// The state only tracks output values for the root module, so attempts to
// remove output values for any other module will be silently ignored.
func (s *SyncState) RemoveOutputValue(addr addrs.AbsOutputValue) {
	if !addr.Module.IsRoot() {
		return
	}

	defer s.beginOutputWrite()()
	s.state.RemoveOutputValue(addr)
}

// Resource returns a snapshot of the state of the resource with the given
// address, or nil if no such resource is tracked.
//
// The return value is a pointer to a copy of the resource state, which the
// caller may then freely access and mutate.
func (s *SyncState) Resource(addr addrs.AbsResource) *Resource {
	s.lock.RLock()
	ret := s.state.Resource(addr).DeepCopy()
	s.lock.RUnlock()
	return ret
}

// ResourceInstance returns a snapshot of the state the resource instance with
// the given address, or nil if no such instance is tracked.
//
// The return value is a pointer to a copy of the instance state, which the
// caller may then freely access and mutate.
func (s *SyncState) ResourceInstance(addr addrs.AbsResourceInstance) *ResourceInstance {
	s.lock.RLock()
	ret := s.state.ResourceInstance(addr).DeepCopy()
	s.lock.RUnlock()
	return ret
}

// ReadEachConfigResourceInstance returns an iterator over the resource instances
// of the given config resource, applying the given selector function to each
// instance and yielding the results.
func ReadEachConfigResourceInstance[T any](syncState *SyncState, addr addrs.ConfigResource, selector func(*ResourceInstance) (T, bool)) iter.Seq[T] {
	if syncState == nil {
		panic("ReadEachConfigResourceInstance on nil state")
	}

	if syncState.writable {
		panic("ReadEachConfigResourceInstance on writable state")
	}

	addrs := syncState.state.allResourceInstanceObjectAddrs(func(objAddr addrs.AbsResourceInstanceObject) bool {
		return objAddr.ResourceInstance.ConfigResource().Equal(addr)
	})

	return func(yield func(T) bool) {
		for _, addr := range addrs {
			val := syncState.state.ResourceInstance(addr.ResourceInstance)
			selected, ok := selector(val)
			if !ok {
				continue
			}
			if !yield(selected) {
				return
			}
		}
	}
}

// ResourceInstanceObject returns a snapshot of the resource instance object
// of the given deposed key belonging to the given resource instance. Use
// [addrs.NotDeposed] for the deposed key to retrieve the "current" object,
// if any. Returns nil if there is no such object.
//
// The return value is a pointer to a copy of the object, which the caller may
// then freely access and mutate.
func (s *SyncState) ResourceInstanceObject(addr addrs.AbsResourceInstance, dk DeposedKey) *ResourceInstanceObjectSrc {
	s.lock.RLock()
	defer s.lock.RUnlock()

	inst := s.state.ResourceInstance(addr)
	if inst == nil {
		return nil
	}
	return inst.Object(dk).DeepCopy()
}

// RememberDecodedResourceInstanceObject records that decoding the given
// source object, which must be a snapshot previously returned for the given
// address by one of the methods of this SyncState, produced the given result,
// so that later snapshots of the same object can be decoded without parsing
// the serialized attributes again.
//
// Decoding the same object repeatedly is otherwise a significant cost when
// evaluating references to resources whose instances are not themselves
// being changed, because nothing else causes the decoded value to be
// retained. This does nothing if the object in the state is no longer the
// one that the snapshot was taken from.
func (s *SyncState) RememberDecodedResourceInstanceObject(addr addrs.AbsResourceInstance, dk DeposedKey, src *ResourceInstanceObjectSrc, decoded *ResourceInstanceObject) {
	if src == nil || decoded == nil || decoded.Value == cty.NilVal {
		return
	}

	s.lock.Lock()
	defer s.lock.Unlock()
	if !s.writable {
		// Once the state is closed it can be read without holding the lock.
		return
	}

	inst := s.state.ResourceInstance(addr)
	if inst == nil {
		return
	}
	current := inst.Object(dk)
	if current == nil || current.decodeValueCache != cty.NilVal || !current.sameSource(src) {
		return
	}
	current.decodeValueCache = decoded.Value
	if current.decodeIdentityCache == cty.NilVal {
		current.decodeIdentityCache = decoded.Identity
	}
}

// SetResourceMeta updates the resource-level metadata for the resource at
// the given address, creating the containing module state and resource state
// as a side-effect if not already present.
func (s *SyncState) SetResourceProvider(addr addrs.AbsResource, provider addrs.AbsProviderConfig) {
	defer s.beginResourceWrite(addr)()

	ms := s.state.EnsureModule(addr.Module)
	ms.SetResourceProvider(addr.Resource, provider)
}

// RemoveResource removes the entire state for the given resource, taking with
// it any instances associated with the resource. This should generally be
// called only for resource objects whose instances have all been destroyed,
// but that is not enforced by this method. (Use RemoveResourceIfEmpty instead
// to safely check first.)
func (s *SyncState) RemoveResource(addr addrs.AbsResource) {
	defer s.beginWrite()()

	ms := s.state.EnsureModule(addr.Module)
	ms.RemoveResource(addr.Resource)
	s.maybePruneModule(addr.Module)
}

// RemoveResourceIfEmpty is similar to RemoveResource but first checks to
// make sure there are no instances or objects left in the resource.
//
// Returns true if the resource was removed, or false if remaining child
// objects prevented its removal. Returns true also if the resource was
// already absent, and thus no action needed to be taken.
func (s *SyncState) RemoveResourceIfEmpty(addr addrs.AbsResource) bool {
	defer s.beginWrite()()

	ms := s.state.Module(addr.Module)
	if ms == nil {
		return true // nothing to do
	}
	rs := ms.Resource(addr.Resource)
	if rs == nil {
		return true // nothing to do
	}
	if len(rs.Instances) != 0 {
		// We don't check here for the possibility of instances that exist
		// but don't have any objects because it's the responsibility of the
		// instance-mutation methods to prune those away automatically.
		return false
	}
	ms.RemoveResource(addr.Resource)
	s.maybePruneModule(addr.Module)
	return true
}

// SetResourceInstanceCurrent saves the given instance object as the current
// generation of the resource instance with the given address, simultaneously
// updating the recorded provider configuration address, dependencies, and
// resource EachMode.
//
// Any existing current instance object for the given resource is overwritten.
// Set obj to nil to remove the primary generation object altogether. If there
// are no deposed objects then the instance as a whole will be removed, which
// may in turn also remove the containing module if it becomes empty.
//
// The caller must ensure that the given ResourceInstanceObject is not
// concurrently mutated during this call, but may be freely used again once
// this function returns.
//
// The provider address is a resource-wide settings and is updated
// for all other instances of the same resource as a side-effect of this call.
//
// If the containing module for this resource or the resource itself are not
// already tracked in state then they will be added as a side-effect.
func (s *SyncState) SetResourceInstanceCurrent(addr addrs.AbsResourceInstance, obj *ResourceInstanceObjectSrc, provider addrs.AbsProviderConfig) {
	defer s.beginResourceWrite(addr.ContainingResource())()

	ms := s.state.EnsureModule(addr.Module)
	ms.SetResourceInstanceCurrent(addr.Resource, obj.DeepCopy(), provider)
	s.maybePruneModule(addr.Module)
}

// SetResourceInstanceDeposed saves the given instance object as a deposed
// generation of the resource instance with the given address and deposed key.
//
// Call this method only for pre-existing deposed objects that already have
// a known DeposedKey. For example, this method is useful if reloading objects
// that were persisted to a state file. To mark the current object as deposed,
// use DeposeResourceInstanceObject instead.
//
// The caller must ensure that the given ResourceInstanceObject is not
// concurrently mutated during this call, but may be freely used again once
// this function returns.
//
// The resource that contains the given instance must already exist in the
// state, or this method will panic. Use Resource to check first if its
// presence is not already guaranteed.
//
// Any existing current instance object for the given resource and deposed key
// is overwritten. Set obj to nil to remove the deposed object altogether. If
// the instance is left with no objects after this operation then it will
// be removed from its containing resource altogether.
//
// If the containing module for this resource or the resource itself are not
// already tracked in state then they will be added as a side-effect.
func (s *SyncState) SetResourceInstanceDeposed(addr addrs.AbsResourceInstance, key DeposedKey, obj *ResourceInstanceObjectSrc, provider addrs.AbsProviderConfig) {
	defer s.beginResourceWrite(addr.ContainingResource())()

	ms := s.state.EnsureModule(addr.Module)
	ms.SetResourceInstanceDeposed(addr.Resource, key, obj.DeepCopy(), provider)
	s.maybePruneModule(addr.Module)
}

// DeposeResourceInstanceObject moves the current instance object for the
// given resource instance address into the deposed set, leaving the instance
// without a current object.
//
// The return value is the newly-allocated deposed key, or NotDeposed if the
// given instance is already lacking a current object.
//
// If the containing module for this resource or the resource itself are not
// already tracked in state then there cannot be a current object for the
// given instance, and so NotDeposed will be returned without modifying the
// state at all.
func (s *SyncState) DeposeResourceInstanceObject(addr addrs.AbsResourceInstance) DeposedKey {
	defer s.beginResourceWrite(addr.ContainingResource())()

	ms := s.state.Module(addr.Module)
	if ms == nil {
		return NotDeposed
	}

	return ms.deposeResourceInstanceObject(addr.Resource, NotDeposed)
}

// DeposeResourceInstanceObjectForceKey is like DeposeResourceInstanceObject
// but uses a pre-allocated key. It's the caller's responsibility to ensure
// that there aren't any races to use a particular key; this method will panic
// if the given key is already in use.
func (s *SyncState) DeposeResourceInstanceObjectForceKey(addr addrs.AbsResourceInstance, forcedKey DeposedKey) {
	defer s.beginResourceWrite(addr.ContainingResource())()

	if forcedKey == NotDeposed {
		// Usage error: should use DeposeResourceInstanceObject in this case
		panic("DeposeResourceInstanceObjectForceKey called without forced key")
	}

	ms := s.state.Module(addr.Module)
	if ms == nil {
		return // Nothing to do, since there can't be any current object either.
	}

	ms.deposeResourceInstanceObject(addr.Resource, forcedKey)
}

// ForgetResourceInstanceAll removes the record of all objects associated with
// the specified resource instance, if present. If not present, this is a no-op.
func (s *SyncState) ForgetResourceInstanceAll(addr addrs.AbsResourceInstance) {
	defer s.beginResourceWrite(addr.ContainingResource())()

	ms := s.state.Module(addr.Module)
	if ms == nil {
		return
	}
	ms.ForgetResourceInstanceAll(addr.Resource)
	s.maybePruneModule(addr.Module)
}

// ForgetResourceInstanceCurrent removes the record of the current object with
// the given address, if present. If not present, this is a no-op.
func (s *SyncState) ForgetResourceInstanceCurrent(addr addrs.AbsResourceInstance) {
	defer s.beginResourceWrite(addr.ContainingResource())()

	ms := s.state.Module(addr.Module)
	if ms == nil {
		return
	}
	ms.ForgetResourceInstanceCurrent(addr.Resource)
	s.maybePruneModule(addr.Module)
}

// ForgetResourceInstanceDeposed removes the record of the deposed object with
// the given address and key, if present. If not present, this is a no-op.
func (s *SyncState) ForgetResourceInstanceDeposed(addr addrs.AbsResourceInstance, key DeposedKey) {
	defer s.beginResourceWrite(addr.ContainingResource())()

	ms := s.state.Module(addr.Module)
	if ms == nil {
		return
	}
	ms.ForgetResourceInstanceDeposed(addr.Resource, key)
	s.maybePruneModule(addr.Module)
}

// MaybeRestoreResourceInstanceDeposed will restore the deposed object with the
// given key on the specified resource as the current object for that instance
// if and only if that would not cause us to forget an existing current
// object for that instance.
//
// Returns true if the object was restored to current, or false if no change
// was made at all.
func (s *SyncState) MaybeRestoreResourceInstanceDeposed(addr addrs.AbsResourceInstance, key DeposedKey) bool {
	defer s.beginResourceWrite(addr.ContainingResource())()

	if key == NotDeposed {
		panic("MaybeRestoreResourceInstanceDeposed called without DeposedKey")
	}

	ms := s.state.Module(addr.Module)
	if ms == nil {
		// Nothing to do, since the specified deposed object cannot exist.
		return false
	}

	return ms.maybeRestoreResourceInstanceDeposed(addr.Resource, key)
}

// RemovePlannedResourceInstanceObjects removes from the state any resource
// instance objects that have the status ObjectPlanned, indiciating that they
// are just transient placeholders created during planning.
//
// Note that this does not restore any "ready" or "tainted" object that might
// have been present before the planned object was written. The only real use
// for this method is in preparing the state created during a refresh walk,
// where we run the planning step for certain instances just to create enough
// information to allow correct expression evaluation within provider and
// data resource blocks. Discarding planned instances in that case is okay
// because the refresh phase only creates planned objects to stand in for
// objects that don't exist yet, and thus the planned object must have been
// absent before by definition.
func (s *SyncState) RemovePlannedResourceInstanceObjects() {
	// TODO: Merge together the refresh and plan phases into a single walk,
	// so we can remove the need to create this "partial plan" during refresh
	// that we then need to clean up before proceeding.

	defer s.beginWrite()()

	for _, ms := range s.state.Modules {
		moduleAddr := ms.Addr

		for _, rs := range ms.Resources {
			resAddr := rs.Addr.Resource

			for ik, is := range rs.Instances {
				instAddr := resAddr.Instance(ik)

				if is.Current != nil && is.Current.Status == ObjectPlanned {
					// Setting the current instance to nil removes it from the
					// state altogether if there are not also deposed instances.
					ms.SetResourceInstanceCurrent(instAddr, nil, rs.ProviderConfig)
				}

				for dk, obj := range is.Deposed {
					// Deposed objects should never be "planned", but we'll
					// do this anyway for the sake of completeness.
					if obj.Status == ObjectPlanned {
						ms.ForgetResourceInstanceDeposed(instAddr, dk)
					}
				}
			}
		}

		// We may have deleted some objects, which means that we may have
		// left a module empty, and so we must prune to preserve the invariant
		// that only the root module is allowed to be empty.
		s.maybePruneModule(moduleAddr)
	}
}

// DiscardCheckResults discards any previously-recorded check results, with
// the intent of preventing any references to them after they have become
// stale due to starting (but possibly not completing) an update.
func (s *SyncState) DiscardCheckResults() {
	defer s.beginWrite()()
	s.state.CheckResults = nil
}

// RecordCheckResults replaces any check results already recorded in the state
// with a new set taken from the given check state object.
func (s *SyncState) RecordCheckResults(checkState *checks.State) {
	defer s.beginWrite()()
	s.state.RecordCheckResults(checkState)
}

// Lock acquires an explicit lock on the state, allowing direct read and write
// access to the returned state object. The caller must call Unlock once
// access is no longer needed, and then immediately discard the state pointer
// pointer.
//
// If the [SyncState.Close] method was previously called on this object then
// the caller must treat the result as read-only, since it now has shared
// ownership with whoever called Close.
//
// Most callers should not use this. Instead, use the concurrency-safe
// accessors and mutators provided directly on SyncState.
func (s *SyncState) Lock() *State {
	s.lock.Lock()
	// We can't know what the caller will do with the state, so we must
	// assume that it will modify it.
	s.generation++
	s.allGeneration = s.generation
	s.moduleSetGeneration++
	return s.state
}

// Snapshot returns a copy of the entire state as it is right now, or nil
// if [SyncState.Close] has already been called.
//
// This is for callers that need to capture the state at a time of their own
// choosing while it's still being modified, such as to periodically save
// the progress of a long-running operation. It returns nil once the state
// is closed because the state can then be modified without the involvement
// of the SyncState, and so can no longer be read safely from here.
func (s *SyncState) Snapshot() *State {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if !s.writable {
		return nil
	}
	return s.state.DeepCopy()
}

// RLock acquires an explicit read lock on the state, allowing direct read
// access to the returned state object while preventing concurrent
// modifications. The caller must not modify the state, must call RUnlock
// once access is no longer needed, and must then immediately discard the
// state pointer.
//
// Use this instead of [SyncState.Lock] when only reading, because other
// readers can then proceed at the same time.
func (s *SyncState) RLock() *State {
	s.lock.RLock()
	return s.state
}

// RUnlock releases a lock previously acquired by RLock, at which point the
// caller must cease all use of the state pointer that was returned.
func (s *SyncState) RUnlock() {
	s.lock.RUnlock()
}

// Unlock releases a lock previously acquired by Lock, at which point the
// caller must cease all use of the state pointer that was returned.
//
// Do not call this method except to end an explicit lock acquired by
// Lock. If a caller calls Unlock without first holding the lock, behavior
// is undefined.
func (s *SyncState) Unlock() {
	s.lock.Unlock()
}

// Close extracts the underlying state from inside this wrapper.
//
// After this function returns, the [State] object is shared between the
// caller and this [SyncState] object, but the [SyncState] will only allow
// read access. Callers must avoid accessing the [SyncState] concurrently
// with any subsequent modifications to the state.
func (s *SyncState) Close() *State {
	s.lock.Lock()
	ret := s.state
	s.writable = false // only reading is allowed after close
	s.lock.Unlock()
	return ret
}

// maybePruneModule will remove a module from the state altogether if it is
// empty, unless it's the root module which must always be present.
//
// This helper method is not concurrency-safe on its own, so must only be
// called while the caller is already holding the lock for writing.
func (s *SyncState) maybePruneModule(addr addrs.ModuleInstance) {
	if addr.IsRoot() {
		// We never prune the root.
		return
	}

	ms := s.state.Module(addr)
	if ms == nil {
		return
	}

	if ms.empty() {
		log.Printf("[TRACE] states.SyncState: pruning %s because it is empty", addr)
		s.state.RemoveModule(addr)
	}
}

func (s *SyncState) MoveAbsResource(src, dst addrs.AbsResource) {
	defer s.beginWrite()()

	s.state.MoveAbsResource(src, dst)
}

func (s *SyncState) MaybeMoveAbsResource(src, dst addrs.AbsResource) bool {
	defer s.beginWrite()()

	return s.state.MaybeMoveAbsResource(src, dst)
}

func (s *SyncState) MoveResourceInstance(src, dst addrs.AbsResourceInstance) {
	defer s.beginWrite()()

	s.state.MoveAbsResourceInstance(src, dst)
}

func (s *SyncState) MaybeMoveResourceInstance(src, dst addrs.AbsResourceInstance) bool {
	defer s.beginWrite()()

	return s.state.MaybeMoveAbsResourceInstance(src, dst)
}

func (s *SyncState) MoveModuleInstance(src, dst addrs.ModuleInstance) {
	defer s.beginWrite()()

	s.state.MoveModuleInstance(src, dst)
}

func (s *SyncState) MaybeMoveModuleInstance(src, dst addrs.ModuleInstance) bool {
	defer s.beginWrite()()

	return s.state.MaybeMoveModuleInstance(src, dst)
}

func (s *SyncState) beginWrite() func() {
	unlock := s.lockForWrite()
	// Callers of this function can modify anything in the state.
	s.generation++
	s.allGeneration = s.generation
	s.moduleSetGeneration++
	return unlock
}

// beginResourceWrite is like beginWrite but for a caller that will modify
// only the given resource and its instances.
func (s *SyncState) beginResourceWrite(addr addrs.AbsResource) func() {
	unlock := s.lockForWrite()
	s.generation++
	if s.resourceGenerations == nil {
		s.resourceGenerations = make(map[string]uint64)
	}
	s.resourceGenerations[addr.String()] = s.generation
	return s.noteModuleSetChange(unlock)
}

// noteModuleSetChange wraps the given function, which releases the lock
// after a modification that can add or remove at most the one module that
// the modified object belongs to, so that it first records whether that
// happened. Such a modification changes the set of modules exactly when it
// changes how many there are.
func (s *SyncState) noteModuleSetChange(unlock func()) func() {
	before := len(s.state.Modules)
	return func() {
		if len(s.state.Modules) != before {
			s.moduleSetGeneration++
		}
		unlock()
	}
}

// beginOutputWrite is like beginWrite but for a caller that will modify only
// output values, and so cannot affect any resource.
func (s *SyncState) beginOutputWrite() func() {
	return s.noteModuleSetChange(s.lockForWrite())
}

func (s *SyncState) lockForWrite() func() {
	s.lock.Lock()
	if !s.writable {
		s.lock.Unlock()
		panic("attempt to write through non-writable SyncState")
	}
	return func() {
		s.lock.Unlock()
	}
}
