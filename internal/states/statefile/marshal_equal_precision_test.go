// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"math"
	"math/big"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/states"
)

// Two states can hold output values that are equal and still not be written
// identically, in which case they must not be reported as certain to be.
func TestStatesDefinitelyMarshalEqual_valuesWrittenDifferently(t *testing.T) {
	state := func(val cty.Value) *states.State {
		s := states.NewState()
		s.SetOutputValue(addrs.OutputValue{Name: "out"}.Absolute(addrs.RootModuleInstance), val, false)
		return s
	}
	exact, _ := new(big.Float).SetPrec(512).SetString("1152921504606846976")
	tests := map[string]struct {
		a, b cty.Value
	}{
		// What pow(2, 60) returns, and the same number as it's read back
		// from a state file.
		"number held with different precision": {cty.NumberFloatVal(math.Pow(2, 60)), cty.NumberVal(exact)},
		"number nested in a list":              {cty.ListVal([]cty.Value{cty.NumberFloatVal(math.Pow(2, 60))}), cty.ListVal([]cty.Value{cty.NumberVal(exact)})},
		// A tuple type with no elements is written as [] or as null.
		"equal types written differently": {cty.ListValEmpty(cty.Tuple(nil)), cty.ListValEmpty(cty.Tuple([]cty.Type{}))},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			a, b := state(test.a), state(test.b)
			if !test.a.RawEquals(test.b) {
				t.Fatalf("the values are meant to be equal")
			}
			if StatesMarshalEqual(a, b) {
				t.Fatalf("the states are meant to be written differently")
			}
			if StatesDefinitelyMarshalEqual(a, b) {
				t.Errorf("reported as certain to be written identically")
			}
			if !StatesDefinitelyMarshalEqual(a, state(test.a)) {
				t.Errorf("a state isn't reported as written identically to another with the very same value")
			}
		})
	}
}
