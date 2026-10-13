// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package fastmsgpack

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"
)

// Marshal must return exactly what the other implementation returns,
// including when that's an error.
func TestMarshal(t *testing.T) {
	check := func(t *testing.T, val cty.Value, ty cty.Type) {
		t.Helper()
		want, wantErr := ctymsgpack.Marshal(val, ty)
		// Twice, so that a reused buffer is covered too.
		for range 2 {
			got, err := Marshal(val, ty)
			// Errors come from the other implementation either way, but the
			// message for a failed conversion can name any one of several
			// problems, so we only compare whether there was one.
			if (err != nil) != (wantErr != nil) {
				t.Fatalf("wrong error for %#v as %#v\ngot:  %v\nwant: %v", val, ty, err, wantErr)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("wrong result for %#v as %#v\ngot:  %x\nwant: %x", val, ty, got, want)
			}
		}
	}

	bigNum := func(s string) cty.Value {
		f, _, err := big.ParseFloat(s, 10, 512, big.ToNearestEven)
		if err != nil {
			t.Fatal(err)
		}
		return cty.NumberVal(f)
	}
	var values []cty.Value
	for _, n := range []int64{
		0, 1, 126, 127, 128, 254, 255, 256, 65534, 65535, 65536, math.MaxUint32 - 1, math.MaxUint32, math.MaxUint32 + 1, math.MaxInt64 - 1, math.MaxInt64,
		-1, -31, -32, -33, -127, -128, -129, -32767, -32768, -32769, math.MinInt32 + 1, math.MinInt32, math.MinInt32 - 1, math.MinInt64 + 1, math.MinInt64,
	} {
		values = append(values, cty.NumberIntVal(n))
	}
	for _, f := range []float64{0.5, -0.5, 1.25, 1e100, -1e100, 1e-100, math.MaxFloat64, math.SmallestNonzeroFloat64, 3.141592653589793, 1e15, 1e16, 12345678.5} {
		values = append(values, cty.NumberFloatVal(f))
	}
	values = append(values,
		cty.PositiveInfinity, cty.NegativeInfinity,
		cty.NumberUIntVal(math.MaxUint64), cty.NumberUIntVal(math.MaxInt64+1),
		bigNum("18446744073709551616"), bigNum("-9223372036854775809"), bigNum("123456789012345678901234567890"),
		bigNum("0.1"), bigNum("1.000000000000000000000000000001"), bigNum("123456789012345678901234567890.123456789"), bigNum("1e400"), bigNum("-1e-400"),
		cty.True, cty.False,
		cty.NullVal(cty.String), cty.NullVal(cty.Number), cty.NullVal(cty.Bool), cty.NullVal(cty.DynamicPseudoType),
		cty.NullVal(cty.List(cty.String)), cty.NullVal(cty.EmptyObject), cty.NullVal(cty.Map(cty.Number)),
		cty.UnknownVal(cty.String), cty.DynamicVal, cty.UnknownVal(cty.List(cty.String)),
		cty.UnknownVal(cty.String).RefineNotNull(), cty.UnknownVal(cty.String).Refine().StringPrefix("foo").NewValue(),
		cty.StringVal("secret").Mark("sensitive"),
	)
	for _, l := range []int{0, 1, 31, 32, 33, 255, 256, 257, 65535, 65536, 70000} {
		values = append(values, cty.StringVal(strings.Repeat("x", l)))
	}
	values = append(values, cty.StringVal("héllo wörld ☃ 🎉\x00\n\"\\"))
	for _, l := range []int{0, 1, 15, 16, 17, 300} {
		strs := make([]cty.Value, l)
		nums := make(map[string]cty.Value, l)
		for i := range strs {
			strs[i] = cty.StringVal(fmt.Sprintf("element-%d", i))
			nums[fmt.Sprintf("key-%03d", i)] = cty.NumberIntVal(int64(i * 1000))
		}
		if l == 0 {
			values = append(values, cty.ListValEmpty(cty.String), cty.SetValEmpty(cty.String), cty.MapValEmpty(cty.Number), cty.EmptyTupleVal, cty.EmptyObjectVal)
			continue
		}
		values = append(values, cty.ListVal(strs), cty.SetVal(strs), cty.MapVal(nums), cty.TupleVal(strs), cty.ObjectVal(nums))
	}
	nested := cty.ObjectVal(map[string]cty.Value{
		"id":      cty.StringVal("i-123"),
		"count":   cty.NumberIntVal(3),
		"ratio":   cty.NumberFloatVal(0.25),
		"enabled": cty.True,
		"tags":    cty.MapVal(map[string]cty.Value{"Name": cty.StringVal("a"), "Tier": cty.StringVal("web")}),
		"rules": cty.ListVal([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{"port": cty.NumberIntVal(80), "cidr": cty.StringVal("10.0.0.0/8"), "extra": cty.NullVal(cty.String)}),
			cty.ObjectVal(map[string]cty.Value{"port": cty.NumberIntVal(443), "cidr": cty.NullVal(cty.String), "extra": cty.StringVal("x")}),
		}),
		"set":     cty.SetVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("z")}), cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("b")})}),
		"tuple":   cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.NumberIntVal(1), cty.NullVal(cty.Bool), cty.EmptyObjectVal}),
		"nothing": cty.NullVal(cty.List(cty.Number)),
		"empty":   cty.ListValEmpty(cty.EmptyObject),
	})
	values = append(values, nested)
	values = append(values,
		cty.ObjectVal(map[string]cty.Value{"a": cty.UnknownVal(cty.String), "b": cty.StringVal("known")}),
		cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("x").Mark("sensitive"), "b": cty.StringVal("known")}),
		cty.ListVal([]cty.Value{cty.StringVal("a"), cty.UnknownVal(cty.String)}),
		cty.MapVal(map[string]cty.Value{"a": cty.StringVal("x").Mark("sensitive")}),
	)

	for i, val := range values {
		t.Run(fmt.Sprintf("value %d", i), func(t *testing.T) {
			// As its own type.
			check(t, val, val.Type())
			// As a dynamically-typed value, alone and inside something else.
			check(t, val, cty.DynamicPseudoType)
			wrapped := cty.ObjectVal(map[string]cty.Value{"dynamic": val, "static": cty.StringVal("s")})
			check(t, wrapped, cty.Object(map[string]cty.Type{"dynamic": cty.DynamicPseudoType, "static": cty.String}))
			check(t, cty.TupleVal([]cty.Value{val, val}), cty.Tuple([]cty.Type{cty.DynamicPseudoType, val.Type()}))
			check(t, cty.MapVal(map[string]cty.Value{"k": val}), cty.Map(cty.DynamicPseudoType))
			check(t, cty.ListVal([]cty.Value{val}), cty.List(cty.DynamicPseudoType))
			// As types that it doesn't conform to, some of which it can be
			// converted to and some not.
			for _, ty := range []cty.Type{cty.String, cty.Number, cty.Bool, cty.List(cty.String), cty.Set(cty.String), cty.Map(cty.String), cty.EmptyObject,
				cty.Object(map[string]cty.Type{"id": cty.String}), cty.ObjectWithOptionalAttrs(map[string]cty.Type{"id": cty.String, "opt": cty.Number}, []string{"opt"})} {
				check(t, val, ty)
			}
		})
	}

	// Random values of random types.
	rnd := rand.New(rand.NewSource(1))
	var genType func(depth int) cty.Type
	genType = func(depth int) cty.Type {
		n := rnd.Intn(10)
		if depth > 3 {
			n = rnd.Intn(4)
		}
		switch n {
		case 0, 1:
			return cty.String
		case 2:
			return cty.Number
		case 3:
			return cty.Bool
		case 4:
			return cty.List(genType(depth + 1))
		case 5:
			return cty.Set(genType(depth + 1))
		case 6:
			return cty.Map(genType(depth + 1))
		case 7:
			etys := make([]cty.Type, rnd.Intn(4))
			for i := range etys {
				etys[i] = genType(depth + 1)
			}
			return cty.Tuple(etys)
		case 8:
			return cty.DynamicPseudoType
		default:
			atys := make(map[string]cty.Type)
			for i := range rnd.Intn(20) {
				atys[fmt.Sprintf("attr_%d_%d", rnd.Intn(100), i)] = genType(depth + 1)
			}
			return cty.Object(atys)
		}
	}
	var genValue func(ty cty.Type, depth int) cty.Value
	genValue = func(ty cty.Type, depth int) cty.Value {
		switch rnd.Intn(40) {
		case 0:
			return cty.NullVal(ty)
		case 1:
			if depth > 0 {
				return cty.UnknownVal(ty)
			}
		}
		switch {
		case ty == cty.DynamicPseudoType:
			concrete := genType(depth + 2)
			for concrete == cty.DynamicPseudoType {
				concrete = genType(depth + 2)
			}
			return genValue(concrete, depth+1)
		case ty == cty.String:
			return cty.StringVal(strings.Repeat("s", rnd.Intn(40)))
		case ty == cty.Number:
			switch rnd.Intn(3) {
			case 0:
				return cty.NumberIntVal(rnd.Int63n(1<<uint(rnd.Intn(62))) - rnd.Int63n(1<<uint(rnd.Intn(62))))
			case 1:
				return cty.NumberFloatVal(rnd.NormFloat64() * 1000)
			default:
				return cty.NumberIntVal(int64(rnd.Intn(300) - 150))
			}
		case ty == cty.Bool:
			return cty.BoolVal(rnd.Intn(2) == 0)
		case ty.IsListType(), ty.IsSetType():
			elems := make([]cty.Value, rnd.Intn(5))
			for i := range elems {
				elems[i] = genValue(ty.ElementType(), depth+1)
			}
			if len(elems) == 0 {
				if ty.IsListType() {
					return cty.ListValEmpty(ty.ElementType())
				}
				return cty.SetValEmpty(ty.ElementType())
			}
			// Elements of dynamic type can't be combined unless they all
			// turned out to have the same type.
			for _, elem := range elems[1:] {
				if !elem.Type().Equals(elems[0].Type()) {
					elems = elems[:1]
					break
				}
			}
			if ty.IsListType() {
				return cty.ListVal(elems)
			}
			return cty.SetVal(elems)
		case ty.IsMapType():
			elems := make(map[string]cty.Value)
			var first cty.Value
			for i := range rnd.Intn(5) {
				elem := genValue(ty.ElementType(), depth+1)
				if first == cty.NilVal {
					first = elem
				}
				if elem.Type().Equals(first.Type()) {
					elems[fmt.Sprintf("key%d", i)] = elem
				}
			}
			if len(elems) == 0 {
				return cty.MapValEmpty(ty.ElementType())
			}
			return cty.MapVal(elems)
		case ty.IsTupleType():
			etys := ty.TupleElementTypes()
			elems := make([]cty.Value, len(etys))
			for i := range elems {
				elems[i] = genValue(etys[i], depth+1)
			}
			return cty.TupleVal(elems)
		default:
			attrs := make(map[string]cty.Value)
			for name, aty := range ty.AttributeTypes() {
				attrs[name] = genValue(aty, depth+1)
			}
			return cty.ObjectVal(attrs)
		}
	}
	for range 3000 {
		ty := genType(0)
		val := genValue(ty, 0)
		check(t, val, ty)
	}
}

func BenchmarkMarshal(b *testing.B) {
	rules := make([]cty.Value, 6)
	for i := range rules {
		rules[i] = cty.ObjectVal(map[string]cty.Value{"port": cty.NumberIntVal(int64(1000 + i)), "cidr": cty.StringVal("10.0.0.0/24"), "proto": cty.StringVal("tcp")})
	}
	attrs := map[string]cty.Value{
		"id":   cty.StringVal("id-app-0004-b-57"),
		"name": cty.StringVal("app-0004-k0057"),
		"blob": cty.StringVal(strings.Repeat("policy ", 1500)),
		"tags": cty.MapVal(map[string]cty.Value{"Name": cty.StringVal("a"), "Module": cty.StringVal("m"), "Tier": cty.StringVal("web")}),
		"rule": cty.ListVal(rules),
	}
	for i := range 12 {
		attrs[fmt.Sprintf("attr%d", i)] = cty.NullVal(cty.String)
	}
	val := cty.ObjectVal(attrs)
	ty := val.Type()
	b.Run("fast", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := Marshal(val, ty); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cty", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := ctymsgpack.Marshal(val, ty); err != nil {
				b.Fatal(err)
			}
		}
	})
}
