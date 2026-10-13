// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package addrs

import (
	"math/rand"
	"testing"
)

func TestConfigResourceStringLess(t *testing.T) {
	names := []string{"", "a", "b", "ab", "a_b", "data", "module", "a.b", "z"}
	var all []ConfigResource
	for _, mode := range []ResourceMode{ManagedResourceMode, DataResourceMode, EphemeralResourceMode, ListResourceMode, InvalidResourceMode} {
		for _, typeName := range names {
			for _, name := range names {
				res := Resource{Mode: mode, Type: typeName, Name: name}
				all = append(all, ConfigResource{Resource: res})
				for _, m1 := range names[:5] {
					all = append(all, ConfigResource{Module: Module{m1}, Resource: res})
					for _, m2 := range names[:4] {
						all = append(all, ConfigResource{Module: Module{m1, m2}, Resource: res})
					}
				}
			}
		}
	}

	rnd := rand.New(rand.NewSource(1))
	check := func(a, b ConfigResource) {
		if got, want := a.StringLess(b), a.String() < b.String(); got != want {
			t.Fatalf("%q StringLess %q is %t; want %t", a.String(), b.String(), got, want)
		}
	}
	for i := 0; i < 200000; i++ {
		check(all[rnd.Intn(len(all))], all[rnd.Intn(len(all))])
	}
	for i := range all {
		check(all[i], all[i])
		check(all[i], all[(i+1)%len(all)])

		// AppendString must agree with String too, including for a module
		// path that's longer than its initial buffer allows for.
		for _, addr := range []ConfigResource{all[i], {Module: Module{"a", "b", "c", "d", "e", "f", "g"}, Resource: all[i].Resource}} {
			if got, want := string(addr.AppendString([]byte("existing"))), "existing"+addr.String(); got != want {
				t.Fatalf("AppendString produced %q; want %q", got, want)
			}
		}
	}
}
