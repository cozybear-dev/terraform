// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// FuzzMarshalJSONNumber compares how marshalJSON writes a number with how
// the library it replaces does. How a number is written depends on the
// precision it's held with as well as on what it is, so both are generated.
func FuzzMarshalJSONNumber(f *testing.F) {
	f.Add(int64(1), int16(0), uint8(53))
	f.Add(int64(1), int16(60), uint8(53))
	f.Add(int64(-1), int16(61), uint8(53))
	f.Add(int64(1<<53+1), int16(0), uint8(64))
	f.Add(int64(1<<62), int16(0), uint8(24))
	f.Add(int64(3), int16(-2), uint8(200))
	f.Add(int64(0), int16(0), uint8(1))
	f.Fuzz(func(t *testing.T, mantissa int64, exponent int16, precision uint8) {
		if precision == 0 {
			precision = 1
		}
		num := new(big.Float).SetPrec(uint(precision)).SetInt64(mantissa)
		num.SetMantExp(num, int(exponent)%1100)
		val := cty.NumberVal(num)
		want, wantErr := ctyjson.Marshal(val, cty.Number)
		got, gotErr := marshalJSON(val, cty.Number)
		if (wantErr == nil) != (gotErr == nil) || !bytes.Equal(got, want) {
			t.Fatalf("for %s held with %d bits of precision\n got %s (%v)\nwant %s (%v)", num.Text('g', 40), precision, got, gotErr, want, wantErr)
		}
	})
}

// FuzzMarshalJSONString does the same for strings, including invalid ones.
func FuzzMarshalJSONString(f *testing.F) {
	for _, seed := range []string{"", "plain", "a\"b\\c", "<&>", "  ", "\x00\x1f\x7f", "é", "é", "\xff\xfe", "\xe2\x80", "😀"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, val := range []cty.Value{cty.StringVal(s), cty.MapVal(map[string]cty.Value{s: cty.StringVal(s)}), cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal(s)})} {
			want, wantErr := ctyjson.Marshal(val, val.Type())
			got, gotErr := marshalJSON(val, val.Type())
			if (wantErr == nil) != (gotErr == nil) || !bytes.Equal(got, want) {
				t.Fatalf("for %#v\n got %s (%v)\nwant %s (%v)", val, got, gotErr, want, wantErr)
			}
			// As a value of unknown type, which records the type too.
			want, wantErr = ctyjson.Marshal(val, cty.DynamicPseudoType)
			got, gotErr = marshalJSON(val, cty.DynamicPseudoType)
			if (wantErr == nil) != (gotErr == nil) || !bytes.Equal(got, want) {
				t.Fatalf("for %#v as a dynamic value\n got %s (%v)\nwant %s (%v)", val, got, gotErr, want, wantErr)
			}
		}
	})
}

// FuzzEquivalentJSON checks that two valid documents are considered the same
// exactly when they differ only in insignificant whitespace.
func FuzzEquivalentJSON(f *testing.F) {
	f.Add([]byte(`{"a": [1, 2]}`), []byte(`{"a":[1,2]}`))
	f.Add([]byte(`[1, 23]`), []byte(`[12, 3]`))
	f.Add([]byte(`"a b"`), []byte(`"ab"`))
	f.Add([]byte(`{"a":"<"}`), []byte(`{"a":"<"}`))
	f.Add([]byte(" [ ] "), []byte("[]"))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		if !json.Valid(a) || !json.Valid(b) {
			return
		}
		var compactA, compactB bytes.Buffer
		json.Compact(&compactA, a)
		json.Compact(&compactB, b)
		want := bytes.Equal(compactA.Bytes(), compactB.Bytes())
		if got := equivalentJSON(a, b); got != want {
			t.Fatalf("equivalentJSON(%q, %q) = %v; want %v", a, b, got, want)
		}
	})
}
