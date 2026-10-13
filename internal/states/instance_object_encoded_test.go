// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/lang/marks"
	"github.com/hashicorp/terraform/internal/providers"
)

// An object that's decoded and then encoded again can keep its serialized
// attributes, but only when that gives the same result as serializing it.
func TestResourceInstanceObjectEncodeAfterDecode(t *testing.T) {
	schema := providers.Schema{
		Body: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"a": {Type: cty.String, Optional: true},
				"b": {Type: cty.String, Optional: true, Sensitive: true},
				"n": {Type: cty.Number, Optional: true},
			},
		},
	}
	otherSchema := providers.Schema{
		Body: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"a": {Type: cty.String, Optional: true},
				"b": {Type: cty.String, Optional: true},
				"n": {Type: cty.Number, Optional: true},
				"z": {Type: cty.String, Optional: true},
			},
		},
	}
	sensitiveB := []cty.Path{cty.GetAttrPath("b")}

	tests := map[string]struct {
		src            string
		sensitivePaths []cty.Path
		want           string
		shared         bool
	}{
		"canonical":            {`{"a":"x","b":"y","n":1}`, nil, `{"a":"x","b":"y","n":1}`, true},
		"indented":             {"{\n  \"a\": \"x\",\n  \"b\": \"y\",\n  \"n\": 1\n}", nil, `{"a":"x","b":"y","n":1}`, true},
		"sensitive":            {`{"a":"x","b":"y","n":1}`, sensitiveB, `{"a":"x","b":"y","n":1}`, true},
		"different order":      {`{"n":1,"b":"y","a":"x"}`, nil, `{"a":"x","b":"y","n":1}`, false},
		"missing attribute":    {`{"a":"x","n":1}`, nil, `{"a":"x","b":null,"n":1}`, false},
		"different number":     {`{"a":"x","b":"y","n":1.0}`, nil, `{"a":"x","b":"y","n":1}`, false},
		"different escaping":   {`{"a":"` + "\\" + `u0078","b":"y","n":1}`, nil, `{"a":"x","b":"y","n":1}`, false},
		"whitespace in string": {`{"a":"x y","b":" ","n":1}`, nil, `{"a":"x y","b":" ","n":1}`, true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			src := &ResourceInstanceObjectSrc{
				AttrsJSON:          []byte(test.src),
				AttrSensitivePaths: test.sensitivePaths,
				Status:             ObjectReady,
			}
			obj, err := src.Decode(schema)
			if err != nil {
				t.Fatal(err)
			}

			// Encoding repeatedly, and encoding copies, must all agree.
			for i, obj := range []*ResourceInstanceObject{obj, obj, obj.DeepCopy()} {
				got, err := obj.Encode(schema)
				if err != nil {
					t.Fatal(err)
				}
				if !equivalentJSON(got.AttrsJSON, []byte(test.want)) {
					t.Errorf("encoding %d: wrong result\ngot:  %s\nwant: %s", i, got.AttrsJSON, test.want)
				}
				if shared := &got.AttrsJSON[0] == &src.AttrsJSON[0]; shared != test.shared {
					t.Errorf("encoding %d: shares the original serialization = %t; want %t", i, shared, test.shared)
				}
				if len(got.AttrSensitivePaths) != len(test.sensitivePaths) {
					t.Errorf("encoding %d: wrong sensitive paths %#v", i, got.AttrSensitivePaths)
				}

				// What we produced must itself decode and encode consistently.
				again, err := got.Decode(schema)
				if err != nil {
					t.Fatal(err)
				}
				if !again.Value.RawEquals(obj.Value) {
					t.Errorf("encoding %d: decodes to a different value", i)
				}
				gotAgain, err := again.Encode(schema)
				if err != nil {
					t.Fatal(err)
				}
				if !equivalentJSON(gotAgain.AttrsJSON, []byte(test.want)) {
					t.Errorf("encoding %d: wrong result after another round trip: %s", i, gotAgain.AttrsJSON)
				}
			}

			// A different value must never get the old serialization, whether
			// it's assigned to the same object or to another that was told
			// about it.
			changed := obj.DeepCopy()
			changed.Value = cty.ObjectVal(map[string]cty.Value{
				"a": cty.StringVal("changed"),
				"b": cty.NullVal(cty.String).Mark(marks.Sensitive),
				"n": cty.NumberIntVal(2),
			})
			other := &ResourceInstanceObject{Value: changed.Value, Status: ObjectReady}
			other.ShareEncodingFrom(obj)
			for _, obj := range []*ResourceInstanceObject{changed, other} {
				got, err := obj.Encode(schema)
				if err != nil {
					t.Fatal(err)
				}
				if want := `{"a":"changed","b":null,"n":2}`; string(got.AttrsJSON) != want {
					t.Errorf("wrong result for changed value\ngot:  %s\nwant: %s", got.AttrsJSON, want)
				}
				if len(got.AttrSensitivePaths) != 1 {
					t.Errorf("wrong sensitive paths for changed value %#v", got.AttrSensitivePaths)
				}
			}

			// The same value under a different schema must be serialized
			// for that schema.
			unmarked, _ := obj.Value.UnmarkDeep()
			attrs := unmarked.AsValueMap()
			attrs["z"] = cty.NullVal(cty.String)
			wider := obj.DeepCopy()
			wider.Value = cty.ObjectVal(attrs)
			got, err := wider.Encode(otherSchema)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.AttrsJSON) == 0 || &got.AttrsJSON[0] == &src.AttrsJSON[0] {
				t.Errorf("object encoded with another schema shares the original serialization")
			}
		})
	}
}

func TestSameValue(t *testing.T) {
	if !valueIsFourWords {
		t.Fatal("cty.Value doesn't have the expected layout, so unchanged values can't be recognized")
	}
	obj := func() cty.Value {
		return cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("a")})
	}
	a := obj()
	marked := a.Mark(marks.Sensitive)
	tests := map[string]struct {
		a, b cty.Value
		same bool
	}{
		"copy":                 {a, a, true},
		"nil":                  {cty.NilVal, cty.NilVal, true},
		"nil and not":          {cty.NilVal, a, false},
		"equal but separate":   {a, obj(), false},
		"marked copy":          {marked, marked, true},
		"marked and not":       {marked, a, false},
		"marked separately":    {marked, a.Mark(marks.Sensitive), false},
		"different primitives": {cty.StringVal("a"), cty.StringVal("b"), false},
		"null of other type":   {cty.NullVal(cty.String), cty.NullVal(cty.Number), false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := sameValue(test.a, test.b); got != test.same {
				t.Errorf("wrong result %t; want %t", got, test.same)
			}
			if test.same && !test.a.RawEquals(test.b) {
				t.Errorf("values reported as the same are not equal")
			}
		})
	}
}
