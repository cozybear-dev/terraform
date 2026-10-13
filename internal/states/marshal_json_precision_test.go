// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"math"
	"math/big"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// How the library writes a number depends on the precision it's held with,
// and marshalJSON must write exactly what the library does.
func TestMarshalJSON_numberPrecision(t *testing.T) {
	withPrec := func(prec uint, f float64) cty.Value {
		return cty.NumberVal(new(big.Float).SetPrec(prec).SetFloat64(f))
	}
	for name, val := range map[string]cty.Value{
		"pow(2, 60) as a float":   cty.NumberFloatVal(math.Pow(2, 60)),
		"-pow(2, 61) as a float":  cty.NumberFloatVal(-math.Pow(2, 61)),
		"pow(10, 18) as a float":  cty.NumberFloatVal(1e18),
		"pow(2, 53) as a float":   cty.NumberFloatVal(math.Pow(2, 53)),
		"pow(2, 53) + 2":          cty.NumberFloatVal(math.Pow(2, 53) + 2),
		"pow(2, 60) as an int":    cty.NumberIntVal(1 << 60),
		"the largest int":         cty.NumberIntVal(math.MaxInt64),
		"the smallest int":        cty.NumberIntVal(math.MinInt64),
		"the smallest as a float": cty.NumberFloatVal(-math.Pow(2, 63)),
		"24 bits of precision":    withPrec(24, 1<<30+1<<7),
		"one bit of precision":    withPrec(1, 4096),
		"a small int":             cty.NumberIntVal(42),
		"a small float":           cty.NumberFloatVal(42),
		"a parsed number":         cty.MustParseNumberVal("1152921504606846976"),
	} {
		t.Run(name, func(t *testing.T) {
			want, err := ctyjson.Marshal(val, cty.Number)
			if err != nil {
				t.Fatal(err)
			}
			got, err := marshalJSON(val, cty.Number)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("got %s; the library writes %s", got, want)
			}
		})
	}
}
