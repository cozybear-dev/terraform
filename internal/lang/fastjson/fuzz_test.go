// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package fastjson

import (
	"fmt"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

var fuzzTypes = []cty.Type{
	cty.String, cty.Number, cty.Bool, cty.DynamicPseudoType,
	cty.List(cty.String), cty.Set(cty.Number), cty.Map(cty.Bool), cty.List(cty.DynamicPseudoType),
	cty.Tuple([]cty.Type{cty.String, cty.Number}), cty.EmptyTuple, cty.EmptyObject,
	cty.Object(map[string]cty.Type{"a": cty.String, "b": cty.Number, "c": cty.List(cty.Bool)}),
	cty.Object(map[string]cty.Type{"id": cty.String, "tags": cty.Map(cty.String), "dyn": cty.DynamicPseudoType, "rule": cty.List(cty.Object(map[string]cty.Type{"port": cty.Number}))}),
	cty.Map(cty.Object(map[string]cty.Type{"é": cty.Set(cty.String)})),
}

// FuzzUnmarshal compares Unmarshal with the library function it replaces,
// for any input at all and a selection of types. They must agree on whether
// the input is acceptable, on the error if it isn't, and on the value,
// down to how each number in it is represented.
func FuzzUnmarshal(f *testing.F) {
	for _, seed := range []string{
		`null`, `"a"`, `1`, `-0`, `1e400`, `0.1`, `true`, `"true"`, `"12"`, `[]`, `{}`, `["a","b"]`, `[1,2.5]`, `{"k":true}`,
		`["x",1]`, `{"a":"s","b":2,"c":[true]}`, `{"a":"s","a":"t","b":null}`, `{"value":"x","type":"string"}`,
		`{"type":["list","number"],"value":[1,2]}`, `{"value":null,"type":"dynamic"}`, `{"value":[],"type":["tuple",[]]}`,
		`{"id":"i","tags":{"<":"&"},"dyn":{"value":{"a":1},"type":["object",{"a":"number"}]},"rule":[{"port":80}]}`,
		`"😀"`, `"\ud83d"`, "\"\xff\"", `{"a" : "s" , "b" : 1E2 , "c" : [ ] }`, `[1,]`, `{"a":}`, `"a" "b"`, `01`, `{"unknown":1}`,
	} {
		for i := range fuzzTypes {
			f.Add([]byte(seed), uint8(i))
		}
	}
	f.Fuzz(func(t *testing.T, src []byte, which uint8) {
		ty := fuzzTypes[int(which)%len(fuzzTypes)]
		// The library panics for a few inputs, such as a tuple with too
		// many elements. Those are inputs that Unmarshal leaves to it, so
		// the same must happen.
		call := func(unmarshal func([]byte, cty.Type) (cty.Value, error)) (val cty.Value, err error, panicked bool) {
			defer func() {
				if recover() != nil {
					panicked = true
				}
			}()
			val, err = unmarshal(src, ty)
			return val, err, false
		}
		got, gotErr, gotPanic := call(Unmarshal)
		want, wantErr, wantPanic := call(ctyjson.Unmarshal)
		if gotPanic != wantPanic {
			t.Fatalf("for %q as %#v: panicked=%v, and the library panicked=%v", src, ty, gotPanic, wantPanic)
		}
		if gotPanic {
			return
		}
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("for %q as %#v\n got error %v\nwant error %v", src, ty, gotErr, wantErr)
		}
		if gotErr != nil {
			if gotErr.Error() != wantErr.Error() {
				t.Fatalf("for %q as %#v\n got error %v\nwant error %v", src, ty, gotErr, wantErr)
			}
			return
		}
		if !got.RawEquals(want) || fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
			t.Fatalf("for %q as %#v\n got %#v\nwant %#v", src, ty, got, want)
		}
	})
}
