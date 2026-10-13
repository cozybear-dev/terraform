// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// The serialization of a type is remembered so that it needn't be produced
// for every value, but a type that's equal to a remembered one can still be
// written differently, and each value must get its own.
func TestMarshalJSON_equalTypesWrittenDifferently(t *testing.T) {
	vals := []cty.Value{
		cty.ListValEmpty(cty.Tuple(nil)),
		cty.ListValEmpty(cty.Tuple([]cty.Type{})),
		cty.ListValEmpty(cty.Tuple(nil)),
		cty.ObjectVal(map[string]cty.Value{"a": cty.ListValEmpty(cty.Tuple([]cty.Type{}))}),
		cty.ObjectVal(map[string]cty.Value{"a": cty.ListValEmpty(cty.Tuple(nil))}),
		cty.EmptyTupleVal,
	}
	for range 3 {
		for _, val := range vals {
			want, err := ctyjson.Marshal(val, cty.DynamicPseudoType)
			if err != nil {
				t.Fatal(err)
			}
			got, err := marshalJSON(val, cty.DynamicPseudoType)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("for %#v\n got %s\nwant %s", val, got, want)
			}
		}
	}
}
