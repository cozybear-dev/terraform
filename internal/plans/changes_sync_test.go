// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package plans

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/lang/marks"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/schemarepo"
)

// TestChangesSyncResourceIndex verifies that the lookups ChangesSync answers
// using its index always agree with scanning the underlying Changes directly.
func TestChangesSyncResourceIndex(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))

	var instAddrs []addrs.AbsResourceInstance
	for _, module := range []addrs.ModuleInstance{
		addrs.RootModuleInstance,
		addrs.RootModuleInstance.Child("a", addrs.NoKey),
		addrs.RootModuleInstance.Child("a", addrs.IntKey(1)),
		addrs.RootModuleInstance.Child("b", addrs.StringKey("x")),
	} {
		for _, name := range []string{"a", "b"} {
			for _, key := range []addrs.InstanceKey{addrs.NoKey, addrs.IntKey(0), addrs.IntKey(1), addrs.StringKey("k")} {
				instAddrs = append(instAddrs, addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "test_thing",
					Name: name,
				}.Instance(key).Absolute(module))
			}
		}
	}
	deposedKeys := []addrs.DeposedKey{addrs.NotDeposed, addrs.NotDeposed, "00000001", "00000002"}

	changes := NewChanges()
	cs := changes.SyncWrapper()

	describe := func(rcs []*ResourceInstanceChange) []string {
		ret := make([]string, len(rcs))
		for i, rc := range rcs {
			ret[i] = fmt.Sprintf("%s %q %s", rc.Addr, rc.DeposedKey, rc.Action)
		}
		return ret
	}
	check := func(step int) {
		t.Helper()
		for _, addr := range instAddrs {
			for _, dk := range deposedKeys {
				var want *ResourceInstanceChange
				if dk == addrs.NotDeposed {
					want = changes.ResourceInstance(addr)
				} else {
					want = changes.ResourceInstanceDeposed(addr, dk)
				}
				got := cs.GetResourceInstanceChange(addr, dk)
				if (got == nil) != (want == nil) || (got != nil && (got.Action != want.Action || !got.Addr.Equal(want.Addr) || got.DeposedKey != want.DeposedKey)) {
					t.Fatalf("step %d: wrong change for %s %q", step, addr, dk)
				}
			}

			got := describe(cs.GetChangesForAbsResource(addr.ContainingResource()))
			want := describe(changes.InstancesForAbsResource(addr.ContainingResource()))
			if !slices.Equal(got, want) {
				t.Fatalf("step %d: wrong changes for %s\ngot:  %q\nwant: %q", step, addr.ContainingResource(), got, want)
			}

			got = describe(cs.GetChangesForConfigResource(addr.ConfigResource()))
			want = describe(slices.Collect(changes.InstancesForConfigResource(addr.ConfigResource())))
			if !slices.Equal(got, want) {
				t.Fatalf("step %d: wrong changes for %s\ngot:  %q\nwant: %q", step, addr.ConfigResource(), got, want)
			}
		}
	}

	check(0)
	for step := 1; step <= 300; step++ {
		addr := instAddrs[rnd.Intn(len(instAddrs))]
		dk := deposedKeys[rnd.Intn(len(deposedKeys))]
		switch rnd.Intn(4) {
		case 0:
			cs.RemoveResourceInstanceChange(addr, dk)
		case 1:
			// Changes made without going through the ChangesSync must
			// be noticed too.
			changes.Resources = append(changes.Resources, &ResourceInstanceChange{
				Addr:       addr,
				DeposedKey: dk,
				Change:     Change{Action: Delete},
			})
		default:
			cs.AppendResourceInstanceChange(&ResourceInstanceChange{
				Addr:       addr,
				DeposedKey: dk,
				Change:     Change{Action: Action(rnd.Intn(3))},
			})
		}
		check(step)
	}

	cs.Close()
	for _, addr := range instAddrs {
		got := describe(slices.Collect(ReadInstancesForConfigResource(cs, addr.ConfigResource())))
		want := describe(slices.Collect(changes.InstancesForConfigResource(addr.ConfigResource())))
		if !slices.Equal(got, want) {
			t.Fatalf("wrong changes for %s after closing\ngot:  %q\nwant: %q", addr.ConfigResource(), got, want)
		}
	}
}

func TestChangesSrcDecodeDeferringNoOps(t *testing.T) {
	schema := providers.Schema{
		Body: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"id":     {Type: cty.String, Computed: true},
				"secret": {Type: cty.String, Optional: true, Sensitive: true},
			},
		},
	}
	schemas := &schemarepo.Schemas{
		Providers: map[addrs.Provider]providers.ProviderSchema{
			addrs.NewDefaultProvider("test"): {
				ResourceTypes: map[string]providers.Schema{"test_thing": schema},
			},
		},
	}
	providerAddr := addrs.AbsProviderConfig{Provider: addrs.NewDefaultProvider("test"), Module: addrs.RootModule}
	value := func(id string) cty.Value {
		return cty.ObjectVal(map[string]cty.Value{
			"id":     cty.StringVal(id),
			"secret": cty.StringVal("s-" + id).Mark(marks.Sensitive),
		})
	}

	original := NewChanges()
	var instAddrs []addrs.AbsResourceInstance
	for i, action := range []Action{NoOp, Update, NoOp, NoOp} {
		addr := addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_thing", Name: "a"}.Instance(addrs.IntKey(i)).Absolute(addrs.RootModuleInstance)
		instAddrs = append(instAddrs, addr)
		after := value(fmt.Sprintf("before-%d", i))
		if action != NoOp {
			after = value(fmt.Sprintf("after-%d", i))
		}
		original.Resources = append(original.Resources, &ResourceInstanceChange{
			Addr:         addr,
			PrevRunAddr:  addr,
			ProviderAddr: providerAddr,
			Change:       Change{Action: action, Before: value(fmt.Sprintf("before-%d", i)), After: after},
		})
	}
	src, err := original.Encode(schemas)
	if err != nil {
		t.Fatal(err)
	}

	eager, err := src.Decode(schemas)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := src.DecodeDeferringNoOps(schemas)
	if err != nil {
		t.Fatal(err)
	}
	for i, rc := range deferred.Resources {
		if got, want := rc.DeferredValues != nil, rc.Action == NoOp; got != want {
			t.Fatalf("change %d: deferred is %t; want %t", i, got, want)
		}
	}

	cs := deferred.SyncWrapper()

	// Asking only for the actions doesn't decode anything.
	actions := cs.GetChangeActionsForConfigResource(instAddrs[0].ConfigResource())
	if len(actions) != len(instAddrs) {
		t.Fatalf("got %d actions; want %d", len(actions), len(instAddrs))
	}
	for i, got := range actions {
		if !got.Addr.Equal(instAddrs[i]) || got.Action != eager.Resources[i].Action {
			t.Fatalf("wrong action %d: %#v", i, got)
		}
	}
	if deferred.Resources[0].Before != cty.NilVal {
		t.Fatal("asking for actions decoded the values")
	}

	// Each way of getting a change returns it fully decoded.
	check := func(got *ResourceInstanceChange, i int) {
		t.Helper()
		want := eager.Resources[i]
		if got == nil || !got.Before.RawEquals(want.Before) || !got.After.RawEquals(want.After) || got.Action != want.Action {
			t.Fatalf("wrong change %d\ngot:  %#v\nwant: %#v", i, got, want)
		}
	}
	check(cs.GetResourceInstanceChange(instAddrs[2], addrs.NotDeposed), 2)
	for i, got := range cs.GetChangesForConfigResource(instAddrs[0].ConfigResource()) {
		check(got, i)
	}
	for i, got := range cs.GetChangesForAbsResource(instAddrs[0].ContainingResource()) {
		check(got, i)
	}
	if errs := cs.DecodeErrors(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// A change that can't be decoded is reported, with unknown values.
	broken, err := src.DecodeDeferringNoOps(schemas)
	if err != nil {
		t.Fatal(err)
	}
	broken.Resources[0].DeferredValues.src = &ResourceInstanceChangeSrc{
		Addr:      instAddrs[0],
		ChangeSrc: ChangeSrc{Action: NoOp, Before: DynamicValue("not msgpack"), After: DynamicValue("nor this")},
	}
	cs = broken.SyncWrapper()
	got := cs.GetResourceInstanceChange(instAddrs[0], addrs.NotDeposed)
	if got.After.IsKnown() {
		t.Fatalf("undecodable change has known value %#v", got.After)
	}
	if errs := cs.DecodeErrors(); len(errs) != 1 {
		t.Fatalf("got %d errors; want 1: %v", len(errs), errs)
	}
}

// The actions of changes can be read while the deferred values of the same
// changes are being decoded for something else.
func TestChangesSyncDeferredValuesConcurrentActions(t *testing.T) {
	schema := providers.Schema{
		Body: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"id": {Type: cty.String, Computed: true},
			},
		},
	}
	schemas := &schemarepo.Schemas{
		Providers: map[addrs.Provider]providers.ProviderSchema{
			addrs.NewDefaultProvider("test"): {
				ResourceTypes: map[string]providers.Schema{"test_thing": schema},
			},
		},
	}
	providerAddr := addrs.AbsProviderConfig{Provider: addrs.NewDefaultProvider("test"), Module: addrs.RootModule}

	original := NewChanges()
	resource := addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_thing", Name: "a"}
	for i := range 2000 {
		addr := resource.Instance(addrs.IntKey(i)).Absolute(addrs.RootModuleInstance)
		value := cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal(fmt.Sprintf("object-%d", i))})
		original.Resources = append(original.Resources, &ResourceInstanceChange{
			Addr:         addr,
			PrevRunAddr:  addr,
			ProviderAddr: providerAddr,
			Change:       Change{Action: NoOp, Before: value, After: value},
		})
	}
	src, err := original.Encode(schemas)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := src.DecodeDeferringNoOps(schemas)
	if err != nil {
		t.Fatal(err)
	}
	cs := deferred.SyncWrapper()
	configAddr := resource.InModule(addrs.RootModule)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, rc := range cs.GetChangesForConfigResource(configAddr) {
			if !rc.After.IsKnown() {
				t.Errorf("change for %s wasn't decoded", rc.Addr)
			}
		}
	}()
	for range 50 {
		for _, action := range cs.GetChangeActionsForConfigResource(configAddr) {
			if action.Action != NoOp {
				t.Fatalf("wrong action %s for %s", action.Action, action.Addr)
			}
		}
	}
	<-done
}
