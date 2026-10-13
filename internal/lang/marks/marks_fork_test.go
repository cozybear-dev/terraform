// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package marks

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestContainsMarked(t *testing.T) {
	marked := cty.StringVal("a").Mark(Sensitive)
	tests := map[string]cty.Value{
		"string":              cty.StringVal("a"),
		"marked string":       marked,
		"null":                cty.NullVal(cty.String),
		"marked null":         cty.NullVal(cty.String).Mark(Sensitive),
		"unknown":             cty.UnknownVal(cty.String),
		"marked unknown":      cty.UnknownVal(cty.String).Mark(Sensitive),
		"dynamic":             cty.DynamicVal,
		"empty object":        cty.EmptyObjectVal,
		"object":              cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("a"), "b": cty.NumberIntVal(1)}),
		"object with marked":  cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("a"), "b": marked}),
		"marked object":       cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("a")}).Mark(Sensitive),
		"nested":              cty.ObjectVal(map[string]cty.Value{"a": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"b": cty.MapVal(map[string]cty.Value{"c": cty.StringVal("d")})})})}),
		"nested marked":       cty.ObjectVal(map[string]cty.Value{"a": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"b": cty.MapVal(map[string]cty.Value{"c": marked})})})}),
		"empty list":          cty.ListValEmpty(cty.String),
		"list":                cty.ListVal([]cty.Value{cty.StringVal("a")}),
		"list with marked":    cty.ListVal([]cty.Value{cty.StringVal("b"), marked}),
		"marked list":         cty.ListVal([]cty.Value{cty.StringVal("b")}).Mark(Sensitive),
		"set":                 cty.SetVal([]cty.Value{cty.StringVal("a")}),
		"set with marked":     cty.SetVal([]cty.Value{cty.StringVal("b"), marked}),
		"map":                 cty.MapVal(map[string]cty.Value{"a": cty.StringVal("a")}),
		"map with marked":     cty.MapVal(map[string]cty.Value{"a": marked}),
		"tuple":               cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.True}),
		"tuple with marked":   cty.TupleVal([]cty.Value{cty.True, marked}),
		"unknown list":        cty.UnknownVal(cty.List(cty.String)),
		"null object":         cty.NullVal(cty.EmptyObject),
		"list of unknown":     cty.ListVal([]cty.Value{cty.UnknownVal(cty.String)}),
		"object deprecation":  cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("a").Mark("other")}),
		"empty set of object": cty.SetValEmpty(cty.EmptyObject),
	}
	for name, v := range tests {
		t.Run(name, func(t *testing.T) {
			if got, want := ContainsMarked(v), v.ContainsMarked(); got != want {
				t.Fatalf("ContainsMarked(%#v) is %t; want %t", v, got, want)
			}
		})
	}
}
