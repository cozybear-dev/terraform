// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"testing"

	"github.com/hashicorp/terraform/internal/addrs"
)

// TestSyncStateResourcesWhileRLocked modifies a state in every way that
// SyncState allows, at random, and checks after each modification that
// looking for resources through the index of modules gives the same result
// as looking through all of the modules.
func TestSyncStateResourcesWhileRLocked(t *testing.T) {
	provider := addrs.AbsProviderConfig{
		Provider: addrs.NewDefaultProvider("test"),
		Module:   addrs.RootModule,
	}
	moduleNames := []string{"a", "b", "c"}
	keys := []addrs.InstanceKey{addrs.NoKey, addrs.IntKey(0), addrs.IntKey(1), addrs.StringKey("x")}
	resourceNames := []string{"one", "two"}

	rnd := rand.New(rand.NewSource(1))
	randomModule := func() addrs.ModuleInstance {
		inst := addrs.RootModuleInstance
		for depth := rnd.Intn(3); depth > 0; depth-- {
			inst = inst.Child(moduleNames[rnd.Intn(len(moduleNames))], keys[rnd.Intn(len(keys))])
		}
		return inst
	}
	randomResource := func() addrs.AbsResource {
		return addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "test_thing",
			Name: resourceNames[rnd.Intn(len(resourceNames))],
		}.Absolute(randomModule())
	}
	object := &ResourceInstanceObjectSrc{
		Status:    ObjectReady,
		AttrsJSON: []byte(`{}`),
	}

	describe := func(resources []*Resource) []string {
		ret := make([]string, 0, len(resources))
		for _, r := range resources {
			ret = append(ret, fmt.Sprintf("%s %p", r.Addr, r))
		}
		sort.Strings(ret)
		return ret
	}

	for round := range 30 {
		ss := NewState().SyncWrapper()
		for step := range 400 {
			var did string
			switch op := rnd.Intn(11); op {
			case 0, 1, 2, 3:
				addr := randomResource().Instance(keys[rnd.Intn(len(keys))])
				did = fmt.Sprintf("set %s", addr)
				ss.SetResourceInstanceCurrent(addr, object, provider)
			case 4, 5:
				addr := randomResource().Instance(keys[rnd.Intn(len(keys))])
				did = fmt.Sprintf("forget %s", addr)
				ss.ForgetResourceInstanceAll(addr)
			case 6:
				addr := randomResource()
				did = fmt.Sprintf("remove %s", addr)
				ss.RemoveResource(addr)
			case 7:
				addr := randomModule()
				if addr.IsRoot() {
					continue
				}
				did = fmt.Sprintf("remove module %s", addr)
				ss.RemoveModule(addr)
			case 8:
				src, dst := randomModule(), randomModule()
				if src.IsRoot() || dst.IsRoot() || ss.Module(src) == nil || ss.Module(dst) != nil {
					// Only something that exists can move, and only
					// to where nothing does.
					continue
				}
				did = fmt.Sprintf("move module %s to %s", src, dst)
				ss.MaybeMoveModuleInstance(src, dst)
			case 9:
				src, dst := randomResource(), randomResource()
				if ss.Resource(src) == nil || ss.Resource(dst) != nil {
					// Only something that exists can move, and only
					// to where nothing does.
					continue
				}
				did = fmt.Sprintf("move %s to %s", src, dst)
				ss.MaybeMoveAbsResource(src, dst)
			case 10:
				// A caller with the whole state can do anything to it,
				// including replacing a module without changing how many
				// there are.
				state := ss.Lock()
				for key, ms := range state.Modules {
					if ms.Addr.IsRoot() {
						continue
					}
					did = fmt.Sprintf("replace module %s directly", ms.Addr)
					delete(state.Modules, key)
					replacement := randomModule()
					if !replacement.IsRoot() {
						state.EnsureModule(replacement)
					}
					break
				}
				ss.Unlock()
			}

			state := ss.RLock()
			for _, moduleName := range append([]string{""}, moduleNames...) {
				for _, parentName := range append([]string{""}, moduleNames...) {
					var module addrs.Module
					if parentName != "" {
						module = append(module, parentName)
					}
					if moduleName != "" {
						module = append(module, moduleName)
					}
					for _, resourceName := range resourceNames {
						addr := addrs.ConfigResource{
							Module: module,
							Resource: addrs.Resource{
								Mode: addrs.ManagedResourceMode,
								Type: "test_thing",
								Name: resourceName,
							},
						}
						got := describe(ss.ResourcesWhileRLocked(addr))
						want := describe(state.Resources(addr))
						if !slices.Equal(got, want) {
							ss.RUnlock()
							t.Fatalf("round %d step %d, after %q: wrong result for %s\ngot:  %v\nwant: %v", round, step, did, addr, got, want)
						}
					}
				}
			}
			ss.RUnlock()
		}
	}
}

func TestStateIndexModuleInstances(t *testing.T) {
	state := NewState()
	for _, name := range []string{"a", "b"} {
		for _, key := range []addrs.InstanceKey{addrs.NoKey, addrs.IntKey(1), addrs.StringKey("a.b")} {
			inst := addrs.RootModuleInstance.Child(name, key)
			state.EnsureModule(inst)
			state.EnsureModule(inst.Child("a", addrs.NoKey))
		}
	}
	idx := state.IndexModuleInstances()
	for _, module := range []addrs.Module{nil, {"a"}, {"b"}, {"c"}, {"a", "a"}, {"b", "a"}, {"a", "b"}, {"a", "a", "a"}} {
		describe := func(modules []*Module) []string {
			ret := make([]string, 0, len(modules))
			for _, m := range modules {
				ret = append(ret, fmt.Sprintf("%s %p", m.Addr, m))
			}
			sort.Strings(ret)
			return ret
		}
		got, want := describe(idx.ModuleInstances(module)), describe(state.ModuleInstances(module))
		if !slices.Equal(got, want) {
			t.Errorf("wrong result for %s\ngot:  %v\nwant: %v", module, got, want)
		}
	}
}
