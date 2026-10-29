// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package fastjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// check verifies that Unmarshal agrees with ctyjson.Unmarshal for the given
// input, including when the input is invalid.
func check(t *testing.T, src []byte, ty cty.Type) {
	t.Helper()
	// The cty decoder panics for some malformed input, in which case we
	// must behave the same way.
	try := func(unmarshal func([]byte, cty.Type) (cty.Value, error)) (val cty.Value, err error, panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		val, err = unmarshal(src, ty)
		return val, err, false
	}
	want, wantErr, wantPanic := try(ctyjson.Unmarshal)
	got, gotErr, gotPanic := try(Unmarshal)
	if gotPanic != wantPanic {
		t.Fatalf("wrong panic behavior for %s as %#v: got %t, want %t", src, ty, gotPanic, wantPanic)
	}
	if wantPanic {
		return
	}
	if (gotErr == nil) != (wantErr == nil) || (wantErr != nil && gotErr.Error() != wantErr.Error()) {
		t.Fatalf("wrong error for %s as %#v\ngot:  %v\nwant: %v", src, ty, gotErr, wantErr)
	}
	if wantErr == nil && !got.RawEquals(want) {
		t.Fatalf("wrong result for %s as %#v\ngot:  %#v\nwant: %#v", src, ty, got, want)
	}
}

func TestUnmarshal_generated(t *testing.T) {
	strings := []string{
		"", "simple", "with space", `quote"`, `back\slash`, "<html>&amp;", "new\nline", "tab\t", "\x00\x1f\x7f",
		"unicode ☃    ", "é not normalized", "invalid \xff utf8", "type", "value",
	}
	numbers := []cty.Value{
		cty.Zero, cty.NumberIntVal(1), cty.NumberIntVal(-1), cty.NumberIntVal(math.MaxInt64),
		cty.NumberUIntVal(math.MaxUint64), cty.NumberFloatVal(0.5), cty.NumberFloatVal(-1.25e-9), cty.NumberFloatVal(1e300),
		cty.MustParseNumberVal("0.1"), cty.MustParseNumberVal("1e25"), cty.MustParseNumberVal("12345678901234567890123.25"),
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

	handled := 0
	for i := 0; i < 5000; i++ {
		val := gen(4)
		for _, ty := range []cty.Type{val.Type(), cty.DynamicPseudoType} {
			src, err := ctyjson.Marshal(val, ty)
			if err != nil {
				t.Fatal(err)
			}
			check(t, src, ty)
			if p := (parser{src: src}); func() bool { _, ok := p.value(ty); return ok }() {
				handled++
			}

			// The same value as it would appear after being indented.
			var indented bytes.Buffer
			if err := json.Indent(&indented, src, "    ", "  "); err != nil {
				t.Fatal(err)
			}
			check(t, indented.Bytes(), ty)

			// A corrupted variant, which is usually invalid or at least
			// no longer conforms to the type.
			if len(src) > 2 {
				corrupt := append([]byte(nil), src...)
				switch rnd.Intn(3) {
				case 0:
					corrupt = corrupt[:rnd.Intn(len(corrupt))]
				case 1:
					corrupt[rnd.Intn(len(corrupt))] = "{}[],:\"x1 "[rnd.Intn(10)]
				case 2:
					j := rnd.Intn(len(corrupt))
					corrupt = append(corrupt[:j], corrupt[j+1:]...)
				}
				check(t, corrupt, ty)
			}
		}
	}
	if handled < 8000 {
		t.Fatalf("the fast path handled only %d of the inputs, so the test isn't exercising it", handled)
	}
}

func TestUnmarshal_special(t *testing.T) {
	object := cty.Object(map[string]cty.Type{
		"s": cty.String,
		"n": cty.Number,
		"b": cty.Bool,
		"l": cty.List(cty.String),
		"d": cty.DynamicPseudoType,
	})
	tests := []struct {
		src string
		ty  cty.Type
	}{
		{`null`, object},
		{`{}`, object},
		{`{"s":"a","n":1,"b":true,"l":["x"],"d":{"value":"v","type":"string"}}`, object},
		{`{"s":"a","unknown":1}`, object},
		{`{"s":1}`, object},
		{`{"s":true}`, object},
		{`{"n":"12"}`, object},
		{`{"n":"x"}`, object},
		{`{"n":01}`, object},
		{`{"n":1.}`, object},
		{`{"n":-}`, object},
		{`{"n":1e5}`, object},
		{`{"n":-0.0}`, object},
		{`{"n":1E+2}`, object},
		{`{"b":"true"}`, object},
		{`{"b":1}`, object},
		{`{"b":nulll}`, object},
		{`{"b":truex}`, object},
		{`{"l":[]}`, object},
		{`{"l":null}`, object},
		{`{"l":["a",null,"b"]}`, object},
		{`{"l":["a",]}`, object},
		{`{"l":[,"a"]}`, object},
		{`{"l":{"a":"b"}}`, object},
		{`{"s":"a","s":"b"}`, object},
		{`{"s":"a"} trailing`, object},
		{`{"s":"a"}{"s":"b"}`, object},
		{` { "s" : "a" } `, object},
		{`{"s":"é\n\"\\\/"}`, object},
		{`{"s":"😀"}`, object},
		{`{"s":"\ud83d"}`, object},
		{`{"s":"bad \x escape"}`, object},
		{"{\"s\":\"raw\nnewline\"}", object},
		{`{"d":null}`, object},
		{`{"d":{"type":"string","value":"v"}}`, object},
		{`{"d":{"value":null,"type":"string"}}`, object},
		{`{"d":{"value":{"a":[1,"x"]},"type":["object",{"a":["tuple",["number","string"]]}]}}`, object},
		{`{"d":{"value":"v"}}`, object},
		{`{"d":{"type":"string"}}`, object},
		{`{"d":{"value":"v","type":"string","extra":1}}`, object},
		{`{"d":{"value":"v","type":"string","type":"number"}}`, object},
		{`{"d":{"value":"v","type":"bogus"}}`, object},
		{`{"d":{"value":1,"type":"string"}}`, object},
		{`{"d":{"value":"v","type":"dynamic"}}`, object},
		{`{"d":"v"}`, object},
		{`[]`, cty.EmptyTuple},
		{`[1]`, cty.EmptyTuple},
		{`[]`, cty.Tuple([]cty.Type{cty.String})},
		{`["a","b"]`, cty.Tuple([]cty.Type{cty.String})},
		{`{}`, cty.EmptyObject},
		{`{}`, cty.Map(cty.String)},
		{`{"a":"b","a":"c"}`, cty.Map(cty.String)},
		{`[1,1,2]`, cty.Set(cty.Number)},
		{`[{"value":"a","type":"string"},{"value":1,"type":"number"}]`, cty.List(cty.DynamicPseudoType)},
		{`[{"value":"a","type":"string"}]`, cty.List(cty.DynamicPseudoType)},
		{``, object},
		{`   `, object},
		{`"s"`, cty.String},
		{`12`, cty.String},
		{`true`, cty.String},
		{`12`, cty.Number},
		{`12 13`, cty.Number},
		{`true`, cty.Bool},
		{`false`, cty.Bool},
		{`null`, cty.DynamicPseudoType},
		{`"x"`, cty.DynamicPseudoType},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("%s as %s", test.src, test.ty.FriendlyName()), func(t *testing.T) {
			check(t, []byte(test.src), test.ty)
		})
	}
}

func BenchmarkUnmarshal(b *testing.B) {
	attrs := map[string]cty.Value{}
	for i := 0; i < 40; i++ {
		attrs[fmt.Sprintf("string_%d", i)] = cty.StringVal(fmt.Sprintf("arn:aws:iam::123456789012:role/app-%d", i))
		attrs[fmt.Sprintf("number_%d", i)] = cty.NumberIntVal(int64(1000 + i))
		attrs[fmt.Sprintf("list_%d", i)] = cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{"port": cty.NumberIntVal(80), "cidr": cty.StringVal("10.0.0.0/8")})})
	}
	val := cty.ObjectVal(attrs)
	ty := val.Type()
	src, err := ctyjson.Marshal(val, ty)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("fastjson", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := Unmarshal(src, ty); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ctyjson", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := ctyjson.Unmarshal(src, ty); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestHasOnlySupportedAttributes(t *testing.T) {
	ty := cty.Object(map[string]cty.Type{
		"s":   cty.String,
		"d":   cty.DynamicPseudoType,
		"obj": cty.Object(map[string]cty.Type{"a": cty.Number}),
		"l":   cty.List(cty.Object(map[string]cty.Type{"a": cty.String})),
		"set": cty.Set(cty.Object(map[string]cty.Type{"a": cty.String})),
		"m":   cty.Map(cty.Object(map[string]cty.Type{"a": cty.String})),
		"t":   cty.Tuple([]cty.Type{cty.String, cty.Object(map[string]cty.Type{"a": cty.String})}),
	})
	tests := map[string]bool{
		`{}`:                                    true,
		`null`:                                  true,
		`{"s":"x"}`:                             true,
		` { "s" : "x" , "obj" : { "a" : 1 } } `: true,
		`{"s":null,"obj":null,"l":null}`:        true,
		`{"d":{"anything":{"goes":[1,{"x":2}]}}}`:                    true,
		`{"l":[{"a":"x"},{}],"set":[{"a":"y"}],"m":{"k":{"a":"z"}}}`: true,
		`{"t":["x",{"a":"y"}]}`:                                      true,
		`{"s":12}`:                                                   true,
		`{"removed":"x"}`:                                            false,
		`{"obj":{"removed":1}}`:                                      false,
		`{"l":[{"a":"x"},{"removed":"y"}]}`:                          false,
		`{"set":[{"removed":"y"}]}`:                                  false,
		`{"m":{"k":{"removed":"z"}}}`:                                false,
		`{"t":["x",{"removed":"y"}]}`:                                false,
		`{"t":["x",{"a":"y"},"extra"]}`:                              false,
		`{"obj":[1]}`:                                                false,
		`{"l":{"a":"x"}}`:                                            false,
		`{"s":"x"`:                                                   false,
		`{"s":"x"} trailing`:                                         false,
		`{"s" "x"}`:                                                  false,
		``:                                                           false,
	}
	for src, want := range tests {
		if got := HasOnlySupportedAttributes([]byte(src), ty); got != want {
			t.Errorf("HasOnlySupportedAttributes(%s) is %t; want %t", src, got, want)
		}
	}
}
