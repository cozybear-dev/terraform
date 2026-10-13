// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/hashicorp/terraform/internal/lang/marks"
)

func TestMarshalJSON(t *testing.T) {
	bigNumber, _, err := big.ParseFloat("123456789012345678901234567890.5", 10, 512, big.ToNearestEven)
	if err != nil {
		t.Fatal(err)
	}
	strings := []string{
		"", "simple", "with space", `quote"`, `back\slash`, "<html>&amp;", "new\nline", "tab\t", "\x00\x1f\x7f",
		"unicode ☃    ", "invalid \xff utf8", "~!@#$%^*()_+-=[]{}|;':,./?",
	}
	numbers := []cty.Value{
		cty.Zero, cty.NumberIntVal(1), cty.NumberIntVal(-1), cty.NumberIntVal(math.MaxInt64), cty.NumberIntVal(math.MinInt64),
		cty.NumberUIntVal(math.MaxUint64), cty.NumberFloatVal(0.5), cty.NumberFloatVal(-1.25e-9), cty.NumberFloatVal(1e300),
		cty.NumberFloatVal(math.Copysign(0, -1)), cty.NumberVal(bigNumber), cty.MustParseNumberVal("0.1"),
		cty.MustParseNumberVal("-0"), cty.MustParseNumberVal("1e25"), cty.MustParseNumberVal("9223372036854775808"),
		cty.MustParseNumberVal("3.0"), cty.MustParseNumberVal("12345678901234567890123"),
	}

	rnd := rand.New(rand.NewSource(1))
	var gen func(depth int) cty.Value
	gen = func(depth int) cty.Value {
		n := 12
		if depth <= 0 {
			n = 6
		}
		switch rnd.Intn(n) {
		case 0:
			return cty.StringVal(strings[rnd.Intn(len(strings))])
		case 1:
			return numbers[rnd.Intn(len(numbers))]
		case 2:
			return cty.BoolVal(rnd.Intn(2) == 0)
		case 3:
			return cty.NullVal(cty.String)
		case 4:
			return cty.NullVal(cty.DynamicPseudoType)
		case 5:
			return cty.NullVal(cty.Map(cty.Number))
		case 6:
			count := rnd.Intn(4)
			if count == 0 {
				return cty.ListValEmpty(cty.String)
			}
			elems := make([]cty.Value, count)
			for i := range elems {
				elems[i] = cty.StringVal(strings[rnd.Intn(len(strings))])
			}
			return cty.ListVal(elems)
		case 7:
			count := rnd.Intn(4)
			if count == 0 {
				return cty.SetValEmpty(cty.Number)
			}
			elems := make([]cty.Value, count)
			for i := range elems {
				elems[i] = numbers[rnd.Intn(len(numbers))]
			}
			return cty.SetVal(elems)
		case 8:
			count := rnd.Intn(4)
			if count == 0 {
				return cty.MapValEmpty(cty.Bool)
			}
			elems := make(map[string]cty.Value, count)
			for i := 0; i < count; i++ {
				elems[strings[rnd.Intn(len(strings))]] = cty.BoolVal(i%2 == 0)
			}
			return cty.MapVal(elems)
		case 9:
			elems := make([]cty.Value, rnd.Intn(4))
			for i := range elems {
				elems[i] = gen(depth - 1)
			}
			return cty.TupleVal(elems)
		default:
			attrs := make(map[string]cty.Value)
			for i, count := 0, rnd.Intn(5); i < count; i++ {
				attrs[strings[rnd.Intn(len(strings))]] = gen(depth - 1)
			}
			return cty.ObjectVal(attrs)
		}
	}

	exactCount := 0
	check := func(t *testing.T, val cty.Value, ty cty.Type) {
		t.Helper()
		want, wantErr := ctyjson.Marshal(val, ty)
		got, gotErr := marshalJSON(val, ty)
		if (gotErr == nil) != (wantErr == nil) || (wantErr != nil && gotErr.Error() != wantErr.Error()) {
			t.Fatalf("wrong error for %#v as %#v\ngot:  %v\nwant: %v", val, ty, gotErr, wantErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("wrong result for %#v as %#v\ngot:  %s\nwant: %s", val, ty, got, want)
		}

		// When the result is reported as exact, decoding it must give back
		// exactly the value we started with.
		src, exact, err := marshalJSONExact(val, ty)
		if (err == nil) != (wantErr == nil) || !bytes.Equal(src, want) {
			t.Fatalf("marshalJSONExact disagrees with ctyjson.Marshal for %#v as %#v", val, ty)
		}
		if exact {
			exactCount++
			decoded, err := ctyjson.Unmarshal(src, ty)
			if err != nil {
				t.Fatalf("failed to decode supposedly-exact encoding of %#v as %#v: %s", val, ty, err)
			}
			if !decoded.RawEquals(val) {
				t.Fatalf("supposedly-exact encoding of %#v as %#v decodes as %#v", val, ty, decoded)
			}
		}
	}

	t.Run("generated", func(t *testing.T) {
		for i := 0; i < 5000; i++ {
			val := gen(4)
			check(t, val, val.Type())
			check(t, val, cty.DynamicPseudoType)
			// A dynamically-typed attribute nested inside a statically-typed
			// object, as for the "input" attribute of terraform_data.
			check(t, cty.ObjectVal(map[string]cty.Value{"v": val}), cty.Object(map[string]cty.Type{"v": cty.DynamicPseudoType}))
		}
	})
	if exactCount < 1000 {
		t.Fatalf("only %d of the generated encodings were exact, so the test isn't exercising that", exactCount)
	}

	special := map[string]struct {
		val cty.Value
		ty  cty.Type
	}{
		"unknown":          {cty.UnknownVal(cty.String), cty.String},
		"nested unknown":   {cty.ListVal([]cty.Value{cty.UnknownVal(cty.String)}), cty.List(cty.String)},
		"marked":           {cty.StringVal("a").Mark(marks.Sensitive), cty.String},
		"nested marked":    {cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("a").Mark(marks.Sensitive)}), cty.Object(map[string]cty.Type{"a": cty.String})},
		"infinity":         {cty.PositiveInfinity, cty.Number},
		"negative inf":     {cty.NegativeInfinity, cty.Number},
		"needs conversion": {cty.NumberIntVal(5), cty.String},
		"cannot convert":   {cty.StringVal("a"), cty.Number},
		"tuple as list":    {cty.TupleVal([]cty.Value{cty.StringVal("a")}), cty.List(cty.String)},
		"object as map":    {cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("a")}), cty.Map(cty.String)},
		"dynamic null":     {cty.NullVal(cty.DynamicPseudoType), cty.DynamicPseudoType},
		"typed null":       {cty.NullVal(cty.List(cty.String)), cty.List(cty.String)},
		"dynamic unknown":  {cty.DynamicVal, cty.DynamicPseudoType},
	}
	for name, test := range special {
		t.Run(name, func(t *testing.T) {
			check(t, test.val, test.ty)
		})
	}
}

func BenchmarkMarshalJSON(b *testing.B) {
	attrs := map[string]cty.Value{}
	for i := 0; i < 40; i++ {
		attrs[fmt.Sprintf("string_%d", i)] = cty.StringVal(fmt.Sprintf("arn:aws:iam::123456789012:role/app-%d", i))
		attrs[fmt.Sprintf("number_%d", i)] = cty.NumberIntVal(int64(1000 + i))
		attrs[fmt.Sprintf("bool_%d", i)] = cty.BoolVal(i%2 == 0)
	}
	val := cty.ObjectVal(attrs)
	ty := val.Type()

	b.Run("marshalJSON", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := marshalJSON(val, ty); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ctyjson", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := ctyjson.Marshal(val, ty); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestEquivalentJSON(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{`{}`, `{}`, true},
		{`{"a":1}`, "{\n  \"a\": 1\n}", true},
		{`{"a":[1,2,{"b":null}]}`, "{ \"a\" : [ 1 ,\t2 , { \"b\" : null } ] }\r\n", true},
		{`{"a":"x y"}`, `{"a":"xy"}`, false},
		{`{"a":"x y"}`, `{ "a": "x y" }`, true},
		{`{"a":"x  y"}`, `{"a":"x y"}`, false},
		{`{"a":"\" "}`, `{"a": "\" "}`, true},
		{`{"a":"\\"}`, `{ "a" : "\\" }`, true},
		{`{"a":"\\","b":" "}`, `{"a":"\\","b":""}`, false},
		{`[1,2]`, `[12]`, false},
		{`[1,2]`, `[1,2,3]`, false},
		{`[1,2,3]`, `[1,2]`, false},
		{`{"a":1}`, `{"a":2}`, false},
		{`{"a":1}`, `{"b":1}`, false},
		{`"a"`, ` "a" `, true},
		{``, ` `, true},
	}
	for _, test := range tests {
		if got := equivalentJSON([]byte(test.a), []byte(test.b)); got != test.want {
			t.Errorf("equivalentJSON(%q, %q) is %t; want %t", test.a, test.b, got, test.want)
		}
		if got := equivalentJSON([]byte(test.b), []byte(test.a)); got != test.want {
			t.Errorf("equivalentJSON(%q, %q) is %t; want %t", test.b, test.a, got, test.want)
		}
	}
}
