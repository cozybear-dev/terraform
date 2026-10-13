// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/lang/marks"
)

func TestValueWhollyKnown(t *testing.T) {
	unknown := cty.UnknownVal(cty.String)
	tests := map[string]cty.Value{
		"string":              cty.StringVal("a"),
		"unknown":             unknown,
		"dynamic":             cty.DynamicVal,
		"null":                cty.NullVal(cty.String),
		"null object":         cty.NullVal(cty.EmptyObject),
		"marked":              cty.StringVal("a").Mark(marks.Sensitive),
		"marked unknown":      unknown.Mark(marks.Sensitive),
		"empty object":        cty.EmptyObjectVal,
		"object":              cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("a"), "b": cty.NullVal(cty.Number)}),
		"object with unknown": cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("a"), "b": unknown}),
		"marked object":       cty.ObjectVal(map[string]cty.Value{"a": unknown}).Mark(marks.Sensitive),
		"list":                cty.ListVal([]cty.Value{cty.StringVal("a")}),
		"list with unknown":   cty.ListVal([]cty.Value{cty.StringVal("a"), unknown}),
		"unknown list":        cty.UnknownVal(cty.List(cty.String)),
		"empty list":          cty.ListValEmpty(cty.String),
		"set":                 cty.SetVal([]cty.Value{cty.StringVal("a")}),
		"set with unknown":    cty.SetVal([]cty.Value{unknown}),
		"map":                 cty.MapVal(map[string]cty.Value{"a": cty.StringVal("a")}),
		"map with unknown":    cty.MapVal(map[string]cty.Value{"a": unknown}),
		"tuple with unknown":  cty.TupleVal([]cty.Value{cty.True, unknown}),
		"nested":              cty.ObjectVal(map[string]cty.Value{"a": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"b": cty.MapVal(map[string]cty.Value{"c": unknown})})})}),
		"nested marked":       cty.ObjectVal(map[string]cty.Value{"a": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"b": cty.StringVal("c").Mark(marks.Sensitive)})})}),
	}
	for name, v := range tests {
		t.Run(name, func(t *testing.T) {
			if got, want := valueWhollyKnown(v), v.IsWhollyKnown(); got != want {
				t.Fatalf("valueWhollyKnown(%#v) is %t; want %t", v, got, want)
			}
		})
	}
}
