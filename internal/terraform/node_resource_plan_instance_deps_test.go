// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/hashicorp/terraform/internal/addrs"
)

func TestMergeDepsAndDepsEqual(t *testing.T) {
	var all []addrs.ConfigResource
	for _, module := range []addrs.Module{nil, {"a"}, {"a", "b"}, {"b"}} {
		for _, mode := range []addrs.ResourceMode{addrs.ManagedResourceMode, addrs.DataResourceMode} {
			for i := 0; i < 4; i++ {
				all = append(all, addrs.ConfigResource{
					Module:   module,
					Resource: addrs.Resource{Mode: mode, Type: "test_thing", Name: fmt.Sprintf("r%d", i)},
				})
			}
		}
	}

	rnd := rand.New(rand.NewSource(1))
	pick := func(sorted bool) []addrs.ConfigResource {
		var ret []addrs.ConfigResource
		for _, addr := range all {
			if rnd.Intn(3) == 0 {
				ret = append(ret, addr)
			}
		}
		if sorted {
			sort.Slice(ret, func(i, j int) bool { return ret[i].String() < ret[j].String() })
		} else {
			rnd.Shuffle(len(ret), func(i, j int) { ret[i], ret[j] = ret[j], ret[i] })
		}
		return ret
	}
	asSet := func(deps []addrs.ConfigResource) map[string]bool {
		ret := make(map[string]bool)
		for _, dep := range deps {
			if ret[dep.String()] {
				t.Fatalf("duplicate %s in %s", dep, deps)
			}
			ret[dep.String()] = true
		}
		return ret
	}

	for i := 0; i < 2000; i++ {
		a := pick(rnd.Intn(4) != 0)
		b := pick(rnd.Intn(4) != 0)
		if rnd.Intn(5) == 0 {
			b = append([]addrs.ConfigResource(nil), a...)
		}
		aSet, bSet := asSet(a), asSet(b)

		wantUnion := make(map[string]bool)
		for k := range aSet {
			wantUnion[k] = true
		}
		for k := range bSet {
			wantUnion[k] = true
		}
		gotUnion := asSet(mergeDeps(a, b))
		if len(gotUnion) != len(wantUnion) {
			t.Fatalf("wrong union of %s and %s: %v", a, b, gotUnion)
		}
		for k := range wantUnion {
			if !gotUnion[k] {
				t.Fatalf("union of %s and %s is missing %s", a, b, k)
			}
		}

		wantEqual := len(aSet) == len(bSet)
		for k := range aSet {
			if !bSet[k] {
				wantEqual = false
			}
		}
		if got := depsEqual(a, b); got != wantEqual {
			t.Fatalf("depsEqual(%s, %s) is %t; want %t", a, b, got, wantEqual)
		}
	}

	// When one argument already is the union, the result shares its
	// backing array rather than being a new slice.
	sorted := append([]addrs.ConfigResource(nil), all...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	a := sorted[:6]
	b := sorted[2:5]
	if got := mergeDeps(a, b); &got[0] != &a[0] || len(got) != len(a) {
		t.Fatal("union with a subset did not return the superset itself")
	}
	if got := mergeDeps(b, a); &got[0] != &a[0] || len(got) != len(a) {
		t.Fatal("union with a superset did not return the superset itself")
	}
}
