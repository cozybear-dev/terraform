// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package fastmsgpack

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"
)

// FuzzMarshal compares Marshal with the library function it replaces, for
// numbers held with every precision and strings of every length, on their
// own, inside collections, and as values of unknown type.
func FuzzMarshal(f *testing.F) {
	f.Add(int64(1), int16(0), uint8(53), "")
	f.Add(int64(1), int16(60), uint8(53), "a")
	f.Add(int64(-1), int16(63), uint8(64), "é")
	f.Add(int64(1<<53+1), int16(0), uint8(64), "\xff")
	f.Add(int64(3), int16(-2), uint8(200), "0123456789012345678901234567890123456789")
	f.Add(int64(-33), int16(0), uint8(8), "x")
	f.Fuzz(func(t *testing.T, mantissa int64, exponent int16, precision uint8, s string) {
		if precision == 0 {
			precision = 1
		}
		num := new(big.Float).SetPrec(uint(precision)).SetInt64(mantissa)
		num.SetMantExp(num, int(exponent)%1100)
		number, str := cty.NumberVal(num), cty.StringVal(s)
		for _, val := range []cty.Value{
			number, str,
			cty.ListVal([]cty.Value{number, cty.NumberIntVal(mantissa)}),
			cty.SetVal([]cty.Value{str, cty.StringVal("other")}),
			cty.MapVal(map[string]cty.Value{s: number}),
			cty.ObjectVal(map[string]cty.Value{"n": number, "s": str, "z": cty.NullVal(cty.Bool)}),
			cty.TupleVal([]cty.Value{str, number, cty.True}),
		} {
			for _, ty := range []cty.Type{val.Type(), cty.DynamicPseudoType} {
				want, wantErr := ctymsgpack.Marshal(val, ty)
				got, gotErr := Marshal(val, ty)
				if (wantErr == nil) != (gotErr == nil) || !bytes.Equal(got, want) {
					t.Fatalf("for %#v as %#v\n got %x (%v)\nwant %x (%v)", val, ty, got, gotErr, want, wantErr)
				}
			}
		}
	})
}
