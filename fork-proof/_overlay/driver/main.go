// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

// Command driver is the component-level half of the fork's integrity proof.
//
// The same source is copied into a checkout of the pinned UPSTREAM commit
// and into the fork, and built in each. It only uses functions that exist,
// with the same signature, in both. Each mode generates inputs from a seed,
// passes them through one of the components that the fork reimplements, and
// prints one line per input describing the result. The two builds are given
// the same seeds, and the proof requires their output to be identical, line
// for line.
//
// This is what lets a component be compared with the real upstream
// implementation, rather than with a copy of it kept in the fork or with a
// library it's believed to match.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/zclconf/go-cty/cty"
	"golang.org/x/text/unicode/norm"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/dag"
	"github.com/hashicorp/terraform/internal/instances"
	"github.com/hashicorp/terraform/internal/lang/marks"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statefile"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

func main() {
	mode := flag.String("mode", "", "statefile, values, decode, dyntypes, dag, changes or expander")
	seed := flag.Uint64("seed", 1, "seed for the generated inputs")
	n := flag.Int("n", 100, "number of inputs")
	first := flag.Int("first", 0, "the first input, for repeating a few of them")
	flag.IntVar(&explain, "explain", -1, "print the details of this one input to standard error")
	flag.Parse()

	out := &output{}
	defer out.flush()
	for i := *first; i < *first+*n; i++ {
		// Each input has its own generator, so that the inputs don't depend
		// on how much randomness an implementation happens to consume, and
		// so that any one of them can be repeated on its own.
		r := rand.New(rand.NewPCG(*seed, uint64(i)))
		explaining = i == explain
		var line string
		switch *mode {
		case "statefile":
			line = stateFileCase(r, i)
		case "values":
			line = valuesCase(r)
		case "decode":
			line = decodeCase(r)
		case "dyntypes":
			line = dynTypesCase(r)
		case "dag":
			line = dagCase(r)
		case "changes":
			line = changesCase(r)
		case "expander":
			line = expanderCase(r)
		default:
			fmt.Fprintln(os.Stderr, "unknown mode")
			os.Exit(2)
		}
		out.printf("%s %d %s\n", *mode, i, line)
	}
}

// explain is the input whose details are wanted, for finding out why the
// two builds disagree about it, and explaining is true while that input is
// being dealt with.
var (
	explain    int
	explaining bool
)

func explainf(format string, args ...any) {
	if explaining {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

type output struct {
	buf bytes.Buffer
}

func (o *output) printf(format string, args ...any) {
	fmt.Fprintf(&o.buf, format, args...)
	if o.buf.Len() > 1<<16 {
		o.flush()
	}
}

func (o *output) flush() {
	os.Stdout.Write(o.buf.Bytes())
	o.buf.Reset()
}

func hash(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil)[:10])
}

// protect runs fn and describes a panic as a result, so that a crash in one
// build is reported as a difference rather than ending the comparison.
func protect(fn func() string) (ret string) {
	defer func() {
		if r := recover(); r != nil {
			ret = "PANIC"
		}
	}()
	return fn()
}

// ---------------------------------------------------------------------------
// Generated strings, numbers, types and values
// ---------------------------------------------------------------------------

var oddStrings = []string{
	"", " ", "plain", "with space", "a\"quote", "back\\slash", "sl/ash", "<html>&amp;</html>",
	"tab\there", "new\nline", "cr\rlf", "nul\x00byte", "unit\x1fsep", "del\x7f", "é", "é",
	"日本語", "😀", "\u2028line\u2029para", "\ufeffbom", "\xff\xfe invalid", "\xe2\x80", "${interp}",
	"%{if}", "[0]", "a.b[\"c\"]", "0", "-1", "1e5", "true", "null", "{}", "[]", strings.Repeat("x", 300),
	strings.Repeat("é<&>", 40), "\\u0041", "\\", "\"", "\n    },\n    {",
}

func genString(r *rand.Rand) string {
	if r.IntN(3) == 0 {
		return oddStrings[r.IntN(len(oddStrings))]
	}
	n := r.IntN(24)
	var b strings.Builder
	for i := 0; i < n; i++ {
		switch r.IntN(12) {
		case 0:
			b.WriteRune(rune(r.IntN(0x20)))
		case 1:
			b.WriteRune(rune(0x80 + r.IntN(0x2000)))
		case 2:
			b.WriteByte("\"\\/<>&'"[r.IntN(7)])
		case 3:
			b.WriteRune(rune(0x1F600 + r.IntN(40)))
		default:
			b.WriteByte(byte('a' + r.IntN(26)))
		}
	}
	return b.String()
}

// distinctKeys hands out keys for one map, refusing any that is a different
// sequence of bytes from a key it has already given but is the same string
// once normalized, such as an "é" written as one character and as two.
//
// Map keys are normalized, so two keys like that become one, and upstream
// decides which of their two values the map keeps by the order in which it
// happens to traverse a Go map. Its result for such an input varies from
// run to run, about one time in eight, so there is nothing to compare the
// fork's result with. Terraform never writes a map like that itself.
type distinctKeys map[string]string

func (d distinctKeys) ok(key string) bool {
	normalized := norm.NFC.String(key)
	if earlier, exists := d[normalized]; exists && earlier != key {
		return false
	}
	d[normalized] = key
	return true
}

func genIdent(r *rand.Rand) string {
	return string(rune('a'+r.IntN(6))) + strconv.Itoa(r.IntN(4))
}

// genNumber returns numbers that differ not only in value but in how they
// are represented, because how a number is written depends on the precision
// it's held with as well as on what it is.
func genNumber(r *rand.Rand) cty.Value {
	switch r.IntN(16) {
	case 0:
		return cty.NumberIntVal(int64(r.IntN(200)) - 100)
	case 1:
		return cty.NumberIntVal(int64(r.Uint64()))
	case 2:
		return cty.NumberUIntVal(r.Uint64())
	case 3:
		return cty.NumberFloatVal(float64(int64(1) << r.IntN(63)))
	case 4:
		return cty.NumberFloatVal(math.Pow(2, float64(r.IntN(200)-100)))
	case 5:
		return cty.NumberFloatVal(r.NormFloat64() * math.Pow(10, float64(r.IntN(40)-20)))
	case 6:
		return cty.NumberFloatVal(float64(r.IntN(1000)) / 8)
	case 7:
		return cty.MustParseNumberVal([]string{"0.1", "1e400", "-1e-400", "123456789012345678901234567890",
			"0.30000000000000004", "3.14159265358979323846264338327950288", "1.0", "100", "1e3", "9007199254740993",
			"18446744073709551616", "-9223372036854775809", "0", "-0", "0.5", "1152921504606846976"}[r.IntN(16)])
	case 8:
		f := new(big.Float).SetPrec(uint(8 + r.IntN(120))).SetFloat64(float64(r.Uint64()>>uint(r.IntN(40))) * 1.5)
		return cty.NumberVal(f)
	case 9:
		f := new(big.Float).SetPrec(53).SetInt64(int64(r.Uint64() >> 2))
		return cty.NumberVal(f)
	case 10:
		return cty.NumberFloatVal(float64(r.Uint64() >> uint(r.IntN(12))))
	case 11:
		return genNumber(r).Add(genNumber(r))
	case 12:
		return genNumber(r).Multiply(cty.NumberFloatVal(0.1))
	case 13:
		return cty.NumberFloatVal(math.Copysign(0, -1))
	case 14:
		return cty.NumberIntVal(int64(1)<<53 + int64(r.IntN(5)) - 2)
	default:
		return cty.NumberFloatVal(float64(r.IntN(1 << 20)))
	}
}

func genType(r *rand.Rand, depth int) cty.Type {
	max := 9
	if depth <= 0 {
		max = 3
	}
	switch r.IntN(max) {
	case 0:
		return cty.String
	case 1:
		return cty.Number
	case 2:
		return cty.Bool
	case 3:
		return cty.List(genType(r, depth-1))
	case 4:
		return cty.Set(genType(r, depth-1))
	case 5:
		return cty.Map(genType(r, depth-1))
	case 6:
		atys := make(map[string]cty.Type)
		for i, n := 0, r.IntN(4); i < n; i++ {
			atys[genIdent(r)] = genType(r, depth-1)
		}
		return cty.Object(atys)
	case 7:
		// Never nil, even when it's empty. A tuple type with no elements is
		// serialized differently depending on whether it was made from a
		// nil slice, and which of two equal types a collection ends up
		// with is decided by the order in which a map is traversed, so
		// upstream's own output would vary from run to run. The
		// "dyntypes" mode covers the two forms deliberately.
		etys := make([]cty.Type, 0, 3)
		for i, n := 0, r.IntN(3); i < n; i++ {
			etys = append(etys, genType(r, depth-1))
		}
		return cty.Tuple(etys)
	default:
		return cty.DynamicPseudoType
	}
}

// genValue returns a known value that conforms to the given type.
func genValue(r *rand.Rand, ty cty.Type, depth int) cty.Value {
	if ty == cty.DynamicPseudoType {
		if r.IntN(8) == 0 {
			return cty.NullVal(cty.DynamicPseudoType)
		}
		for {
			if cty := genType(r, depth-1); !cty.HasDynamicTypes() {
				return genValue(r, cty, depth-1)
			}
		}
	}
	if r.IntN(9) == 0 {
		return cty.NullVal(ty)
	}
	switch {
	case ty == cty.String:
		return cty.StringVal(genString(r))
	case ty == cty.Number:
		return genNumber(r)
	case ty == cty.Bool:
		return cty.BoolVal(r.IntN(2) == 0)
	case ty.IsListType(), ty.IsSetType():
		ety := ty.ElementType()
		var vals []cty.Value
		for i, n := 0, r.IntN(4); i < n; i++ {
			vals = append(vals, genValue(r, ety, depth-1))
		}
		switch {
		case len(vals) == 0 && ty.IsListType():
			return cty.ListValEmpty(ety)
		case len(vals) == 0:
			return cty.SetValEmpty(ety)
		case ty.IsListType():
			return cty.ListVal(vals)
		default:
			return cty.SetVal(vals)
		}
	case ty.IsMapType():
		ety := ty.ElementType()
		vals := make(map[string]cty.Value)
		keys := make(distinctKeys)
		for i, n := 0, r.IntN(4); i < n; i++ {
			key, val := genString(r), genValue(r, ety, depth-1)
			if keys.ok(key) {
				vals[key] = val
			}
		}
		if len(vals) == 0 {
			return cty.MapValEmpty(ety)
		}
		return cty.MapVal(vals)
	case ty.IsObjectType():
		// In a fixed order, so that the same seed always gives the same
		// value.
		vals := make(map[string]cty.Value)
		for _, name := range sortedAttributeNames(ty) {
			vals[name] = genValue(r, ty.AttributeType(name), depth-1)
		}
		return cty.ObjectVal(vals)
	case ty.IsTupleType():
		var vals []cty.Value
		for _, ety := range ty.TupleElementTypes() {
			vals = append(vals, genValue(r, ety, depth-1))
		}
		return cty.TupleVal(vals)
	}
	return cty.NullVal(ty)
}

func sortedAttributeNames(ty cty.Type) []string {
	names := make([]string, 0, len(ty.AttributeTypes()))
	for name := range ty.AttributeTypes() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// genSchema returns a schema with a few attributes of generated types, and
// a value that conforms to it.
func genSchema(r *rand.Rand) (providers.Schema, cty.Value) {
	block := &configschema.Block{Attributes: map[string]*configschema.Attribute{}}
	vals := make(map[string]cty.Value)
	for i, n := 0, 1+r.IntN(5); i < n; i++ {
		name := genIdent(r)
		ty := genType(r, 3)
		block.Attributes[name] = &configschema.Attribute{Type: ty, Optional: true}
		vals[name] = genValue(r, ty, 3)
	}
	return providers.Schema{Body: block, Version: int64(r.IntN(3))}, cty.ObjectVal(vals)
}

func describeErr(err error) string {
	if err == nil {
		return "ok"
	}
	return "ERR(" + err.Error() + ")"
}

func describeValue(v cty.Value) string {
	return hash([]byte(fmt.Sprintf("%#v", v)))
}

// ---------------------------------------------------------------------------
// values: a value written to the state, and to a plan
// ---------------------------------------------------------------------------

func compact(src []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, src); err != nil {
		return src
	}
	return buf.Bytes()
}

func valuesCase(r *rand.Rand) string {
	return protect(func() string {
		schema, val := genSchema(r)
		var b strings.Builder

		// As it's sent to a provider and recorded in a plan.
		dv, err := plans.NewDynamicValue(val, schema.Body.ImpliedType())
		fmt.Fprintf(&b, "msgpack=%s:%s ", describeErr(err), hash(dv))
		explainf("msgpack: %x", []byte(dv))
		dv, err = plans.NewDynamicValue(val, cty.DynamicPseudoType)
		fmt.Fprintf(&b, "msgpackdyn=%s:%s ", describeErr(err), hash(dv))

		// With some of it marked as sensitive, as it's written to the state.
		// Which parts are marked is decided by where they are, rather than
		// by the generator, because the order in which a value is traversed
		// isn't fixed.
		marked := val
		if salt := r.Uint64(); salt%2 == 0 {
			marked, _ = cty.Transform(val, func(p cty.Path, v cty.Value) (cty.Value, error) {
				if len(p) == 0 {
					return v, nil
				}
				sum := sha256.Sum256([]byte(fmt.Sprintf("%d %#v", salt, p)))
				if sum[0]%5 == 0 {
					return v.Mark(marks.Sensitive), nil
				}
				return v, nil
			})
		}
		obj := &states.ResourceInstanceObject{
			Value:        marked,
			Status:       states.ObjectReady,
			Private:      []byte(genString(r)),
			Dependencies: genDeps(r),
		}
		src, err := obj.Encode(schema)
		if err != nil {
			fmt.Fprintf(&b, "encode=%s", describeErr(err))
			return b.String()
		}
		// Upstream itself lists the sensitive paths in an order that varies
		// from one run to the next, so they are compared as a set.
		paths := make([]string, len(src.AttrSensitivePaths))
		for i, path := range src.AttrSensitivePaths {
			paths[i] = fmt.Sprintf("%#v", path)
		}
		sort.Strings(paths)
		explainf("value: %#v\ntype: %#v\nencoded: %s", marked, schema.Body.ImpliedType(), src.AttrsJSON)
		fmt.Fprintf(&b, "encode=%s paths=%s deps=%s ", hash(src.AttrsJSON), hash([]byte(strings.Join(paths, "\n"))), describeDeps(src.Dependencies))

		// Read back, and written again without having changed.
		dec, err := src.Decode(schema)
		if err != nil {
			fmt.Fprintf(&b, "decode=%s", describeErr(err))
			return b.String()
		}
		fmt.Fprintf(&b, "decode=%s ", describeValue(dec.Value))
		again, err := dec.Encode(schema)
		if err != nil {
			fmt.Fprintf(&b, "reencode=%s", describeErr(err))
			return b.String()
		}
		fmt.Fprintf(&b, "reencode=%s ", hash(again.AttrsJSON))

		// The same after a trip through a state file, which indents the
		// attributes. The fork keeps the indented form in memory where
		// upstream replaces it with the compact form, which makes no
		// difference to anything that's written, so here the two are
		// compared without their insignificant whitespace.
		var indented bytes.Buffer
		json.Indent(&indented, src.AttrsJSON, "            ", "  ")
		fromFile := &states.ResourceInstanceObjectSrc{
			SchemaVersion:      src.SchemaVersion,
			AttrsJSON:          indented.Bytes(),
			AttrSensitivePaths: src.AttrSensitivePaths,
			Status:             src.Status,
			Dependencies:       src.Dependencies,
		}
		dec, err = fromFile.Decode(schema)
		if err != nil {
			fmt.Fprintf(&b, "filedecode=%s", describeErr(err))
			return b.String()
		}
		again, err = dec.Encode(schema)
		if err != nil {
			fmt.Fprintf(&b, "fileencode=%s", describeErr(err))
			return b.String()
		}
		explainf("decoded from file: %#v\nencoded again: %s", dec.Value, again.AttrsJSON)
		fmt.Fprintf(&b, "file=%s:%s", describeValue(dec.Value), hash(compact(again.AttrsJSON)))
		return b.String()
	})
}

func genDeps(r *rand.Rand) []addrs.ConfigResource {
	var deps []addrs.ConfigResource
	for i, n := 0, r.IntN(5); i < n; i++ {
		var mod addrs.Module
		for j, m := 0, r.IntN(3); j < m; j++ {
			mod = append(mod, genIdent(r))
		}
		mode := addrs.ManagedResourceMode
		if r.IntN(4) == 0 {
			mode = addrs.DataResourceMode
		}
		deps = append(deps, addrs.Resource{Mode: mode, Type: "fp_" + genIdent(r), Name: genIdent(r)}.InModule(mod))
	}
	return deps
}

func describeDeps(deps []addrs.ConfigResource) string {
	strs := make([]string, len(deps))
	for i, dep := range deps {
		strs[i] = dep.String()
	}
	return hash([]byte(strings.Join(strs, "\n")))
}

// ---------------------------------------------------------------------------
// dyntypes: values of dynamic type whose types are equal but written differently
// ---------------------------------------------------------------------------

// dynTypesCase encodes a series of dynamically-typed values whose types
// are equal to one another as types, but are not all serialized alike,
// because a tuple type with no elements is written as [] or as null
// depending on how it was constructed. What's written for each value must
// depend only on that value, and not on which were encoded before it.
func dynTypesCase(r *rand.Rand) string {
	return protect(func() string {
		schema := providers.Schema{Body: &configschema.Block{Attributes: map[string]*configschema.Attribute{
			"dyn": {Type: cty.DynamicPseudoType, Optional: true},
		}}}
		nilTuple, emptyTuple := cty.Tuple(nil), cty.Tuple([]cty.Type{})
		wrap := func(ty cty.Type, depth int) cty.Value {
			val := cty.ListValEmpty(ty)
			for i := 0; i < depth; i++ {
				val = cty.ListVal([]cty.Value{val})
			}
			return val
		}
		var b strings.Builder
		depth := r.IntN(3)
		for i, n := 0, 2+r.IntN(6); i < n; i++ {
			ty := emptyTuple
			if r.IntN(2) == 0 {
				ty = nilTuple
			}
			obj := &states.ResourceInstanceObject{
				Value:  cty.ObjectVal(map[string]cty.Value{"dyn": wrap(ty, depth)}),
				Status: states.ObjectReady,
			}
			src, err := obj.Encode(schema)
			if err != nil {
				return "encode=" + describeErr(err)
			}
			explainf("encoded: %s", src.AttrsJSON)
			dv, err := plans.NewDynamicValue(obj.Value, cty.DynamicPseudoType)
			fmt.Fprintf(&b, "%s:%s:%s ", hash(src.AttrsJSON), describeErr(err), hash(dv))
		}
		return b.String()
	})
}

// ---------------------------------------------------------------------------
// decode: serialized attributes, written every way JSON allows
// ---------------------------------------------------------------------------

// genJSON writes a JSON value that's meant for the given type but is
// formatted unusually, and sometimes isn't right for the type at all.
func genJSON(r *rand.Rand, b *strings.Builder, ty cty.Type, depth int) {
	space := func() {
		if r.IntN(4) == 0 {
			b.WriteString([]string{" ", "\n", "\t", "\r\n", "  "}[r.IntN(5)])
		}
	}
	str := func(s string) {
		b.WriteByte('"')
		for _, c := range []byte(s) {
			switch {
			case c == '"' || c == '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			case c < 0x20:
				fmt.Fprintf(b, "\\u%04x", c)
			case c < 0x7f && r.IntN(10) == 0:
				fmt.Fprintf(b, "\\u00%02X", c)
			default:
				b.WriteByte(c)
			}
		}
		b.WriteByte('"')
	}
	space()
	defer space()
	switch r.IntN(40) {
	case 0:
		b.WriteString("null")
		return
	case 1:
		// Something of the wrong kind.
		b.WriteString([]string{`"12"`, `12`, `true`, `"true"`, `[]`, `{}`, `[1,"a"]`, `{"x":1}`, `1e2`, `"😀"`, `"\ud83d"`, `-0`, `0.10`, `1E+2`, `01`, `tru`, `"a" "b"`, `[1,]`}[r.IntN(18)])
		return
	}
	switch {
	case ty == cty.DynamicPseudoType:
		inner := genType(r, depth-1)
		for inner.HasDynamicTypes() {
			inner = genType(r, depth-1)
		}
		tyJSON, _ := inner.MarshalJSON()
		if r.IntN(2) == 0 {
			b.WriteString(`{"value":`)
			genJSON(r, b, inner, depth-1)
			b.WriteString(`,"type":` + string(tyJSON) + `}`)
		} else {
			b.WriteString(`{ "type" : ` + string(tyJSON) + ` , "value" : `)
			genJSON(r, b, inner, depth-1)
			b.WriteString(`}`)
		}
	case ty == cty.String:
		str(genString(r))
	case ty == cty.Number:
		b.WriteString([]string{"0", "-0", "1", "-17", "1.5", "0.1", "1e5", "1E-5", "1.0e+2", "123456789012345678901234567890", "9007199254740993", "1152921504606846976", "0.30000000000000004", "1e400", "3.14159265358979323846264338327950288"}[r.IntN(15)])
	case ty == cty.Bool:
		b.WriteString([]string{"true", "false"}[r.IntN(2)])
	case ty.IsListType(), ty.IsSetType():
		b.WriteByte('[')
		for i, n := 0, r.IntN(4); i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			genJSON(r, b, ty.ElementType(), depth-1)
		}
		b.WriteByte(']')
	case ty.IsTupleType():
		b.WriteByte('[')
		etys := ty.TupleElementTypes()
		if r.IntN(12) == 0 && len(etys) > 0 {
			etys = etys[:len(etys)-1]
		}
		for i, ety := range etys {
			if i > 0 {
				b.WriteByte(',')
			}
			genJSON(r, b, ety, depth-1)
		}
		b.WriteByte(']')
	case ty.IsMapType():
		b.WriteByte('{')
		keys := make(distinctKeys)
		for i, n := 0, r.IntN(4); i < n; i++ {
			// The same key twice is fine, and is what "dup" is for: the
			// later one wins. See distinctKeys for what isn't.
			key := "dup"
			if r.IntN(6) != 0 {
				key = genString(r)
			}
			if !keys.ok(key) {
				key = "dup"
			}
			if i > 0 {
				b.WriteByte(',')
			}
			str(key)
			space()
			b.WriteByte(':')
			genJSON(r, b, ty.ElementType(), depth-1)
		}
		b.WriteByte('}')
	case ty.IsObjectType():
		b.WriteByte('{')
		names := make([]string, 0, len(ty.AttributeTypes()))
		for name := range ty.AttributeTypes() {
			names = append(names, name)
		}
		sort.Strings(names)
		r.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
		first := true
		for _, name := range names {
			switch r.IntN(14) {
			case 0:
				continue // left out
			case 1:
				// Present twice.
				if !first {
					b.WriteByte(',')
				}
				first = false
				str(name)
				b.WriteByte(':')
				genJSON(r, b, ty.AttributeType(name), depth-1)
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			str(name)
			space()
			b.WriteByte(':')
			genJSON(r, b, ty.AttributeType(name), depth-1)
		}
		if r.IntN(20) == 0 {
			if !first {
				b.WriteByte(',')
			}
			b.WriteString(`"removed_attribute":{"x":[1,2,{"y":null}]}`)
		}
		b.WriteByte('}')
	}
}

func decodeCase(r *rand.Rand) string {
	return protect(func() string {
		schema, _ := genSchema(r)
		var b strings.Builder
		genJSON(r, &b, schema.Body.ImpliedType(), 4)
		src := &states.ResourceInstanceObjectSrc{
			SchemaVersion: uint64(schema.Version),
			AttrsJSON:     []byte(b.String()),
			Status:        states.ObjectReady,
		}
		explainf("type: %#v\nsource: %s", schema.Body.ImpliedType(), src.AttrsJSON)
		obj, err := src.Decode(schema)
		if err != nil {
			return "decode=" + describeErr(err)
		}
		explainf("decoded: %#v", obj.Value)
		again, err := obj.Encode(schema)
		if err != nil {
			return "decode=" + describeValue(obj.Value) + " encode=" + describeErr(err)
		}
		return "decode=" + describeValue(obj.Value) + " encode=" + hash(compact(again.AttrsJSON))
	})
}

// ---------------------------------------------------------------------------
// statefile: reading and writing whole state files
// ---------------------------------------------------------------------------

// jsonWriter writes JSON text in one of several layouts.
type jsonWriter struct {
	b      strings.Builder
	r      *rand.Rand
	style  int // 0: as Terraform writes it, 1: compact, 2: erratic, 3: misleading
	depth  int
	indent string
}

func (w *jsonWriter) newline() {
	switch w.style {
	case 0:
		w.b.WriteByte('\n')
		for i := 0; i < w.depth; i++ {
			w.b.WriteString("  ")
		}
	case 2:
		w.b.WriteString([]string{"", " ", "\n", "\t", "\r\n ", "\n    "}[w.r.IntN(6)])
	case 3:
		w.b.WriteByte('\n')
		w.b.WriteString(w.indent)
	}
}

func (w *jsonWriter) sep() {
	if w.style != 1 {
		w.b.WriteByte(' ')
	}
}

func (w *jsonWriter) str(s string) {
	enc, _ := json.Marshal(s)
	w.b.Write(enc)
}

// object writes an object with the given properties, each of which is a
// name followed by a function that writes its value. A nil function means
// that the property is left out.
func (w *jsonWriter) object(members ...any) {
	w.b.WriteByte('{')
	w.depth++
	n := 0
	for i := 0; i+1 < len(members); i += 2 {
		fn, _ := members[i+1].(func())
		if fn == nil {
			continue
		}
		if n > 0 {
			w.b.WriteByte(',')
		}
		n++
		w.newline()
		w.str(members[i].(string))
		w.b.WriteByte(':')
		w.sep()
		fn()
	}
	w.depth--
	if n > 0 {
		w.newline()
	}
	w.b.WriteByte('}')
}

func (w *jsonWriter) array(n int, elem func(i int)) {
	w.b.WriteByte('[')
	w.depth++
	for i := 0; i < n; i++ {
		if i > 0 {
			w.b.WriteByte(',')
		}
		w.newline()
		elem(i)
	}
	w.depth--
	if n > 0 {
		w.newline()
	}
	w.b.WriteByte(']')
}

func (w *jsonWriter) raw(s string) func() {
	return func() { w.b.WriteString(s) }
}

func (w *jsonWriter) strFn(s string) func() {
	return func() { w.str(s) }
}

// anyJSON writes an arbitrary JSON value, as the attributes of an object.
func (w *jsonWriter) anyJSON(depth int) {
	r := w.r
	max := 8
	if depth <= 0 {
		max = 5
	}
	switch r.IntN(max) {
	case 0:
		w.str(genString(r))
	case 1:
		w.b.WriteString([]string{"0", "-0", "1", "-17", "1.5", "0.1", "1e5", "1E-5", "1.0e+2", "123456789012345678901234567890", "9007199254740993", "1e400"}[r.IntN(12)])
	case 2:
		w.b.WriteString([]string{"true", "false", "null"}[r.IntN(3)])
	case 3:
		w.b.WriteString([]string{"[]", "{}", "[ ]", "{ }", `""`}[r.IntN(5)])
	case 4:
		w.str(genIdent(r))
	case 5, 6:
		var members []any
		for i, n := 0, r.IntN(5); i < n; i++ {
			members = append(members, genString(r), func() { w.anyJSON(depth - 1) })
		}
		w.object(members...)
	default:
		w.array(r.IntN(4), func(int) { w.anyJSON(depth - 1) })
	}
}

var providerAddrs = []string{
	`provider["registry.terraform.io/hashicorp/fp"]`,
	`provider["registry.terraform.io/hashicorp/fp"].west`,
	`module.a.provider["registry.terraform.io/hashicorp/fp"]`,
	`provider["terraform.io/builtin/terraform"]`,
	`provider.fp`,
}

var moduleAddrs = []string{"", "", "module.a", "module.a[0]", `module.a["k"]`, `module.a.module.b["x y"]`, `module.a[1].module.b`, `module.c["é"]`, "module.9bad"}

var dependencyAddrs = []string{"fp_thing.a", "fp_thing.b", "module.a.fp_thing.c", "data.fp_data.d", "module.a.module.b.fp_thing.e", "fp_thing.with-dash", "not an address", "fp_thing.a[0]", "fp_thing.é", ""}

func genStateFile(r *rand.Rand, style, resources, attrSize int) []byte {
	w := &jsonWriter{r: r, style: style}
	if style == 3 {
		w.indent = "      "
	}
	resource := func(int) {
		mod := moduleAddrs[r.IntN(len(moduleAddrs))]
		var modFn func()
		if mod != "" {
			modFn = w.strFn(mod)
		}
		var eachFn func()
		each := r.IntN(4)
		if each == 1 {
			eachFn = w.strFn("list")
		} else if each == 2 {
			eachFn = w.strFn("map")
		}
		mode := "managed"
		if r.IntN(5) == 0 {
			mode = "data"
		}
		instances := 1 + r.IntN(3)
		if each == 0 {
			instances = 1
		}
		if r.IntN(15) == 0 {
			instances = 0
		}
		// Usually every object of a resource has the same dependencies.
		var deps []string
		for i, n := 0, r.IntN(4); i < n; i++ {
			deps = append(deps, dependencyAddrs[r.IntN(8)])
		}
		if r.IntN(25) == 0 {
			deps = append(deps, dependencyAddrs[8+r.IntN(2)])
		}
		w.object(
			"module", modFn,
			"mode", w.strFn(mode),
			"type", w.strFn("fp_"+genIdent(r)),
			"name", w.strFn(genIdent(r)+strconv.Itoa(r.IntN(100000))),
			"each", eachFn,
			"provider", w.strFn(providerAddrs[r.IntN(len(providerAddrs)-r.IntN(2))]),
			"instances", func() {
				w.array(instances, func(i int) {
					var indexFn, statusFn, deposedFn, sensitiveFn, privateFn, depsFn, cbdFn, identityFn func()
					switch each {
					case 1:
						indexFn = w.raw(strconv.Itoa(i))
					case 2:
						indexFn = w.strFn(genString(r) + strconv.Itoa(i))
					}
					if r.IntN(8) == 0 {
						statusFn = w.strFn("tainted")
					}
					if r.IntN(10) == 0 {
						deposedFn = w.strFn(fmt.Sprintf("%08x", r.Uint32()))
					}
					if r.IntN(4) == 0 {
						sensitiveFn = w.raw(`[[{"type":"get_attr","value":"secret"}],[{"type":"get_attr","value":"tags"},{"type":"index","value":{"value":"k","type":"string"}}]]`)
					}
					if r.IntN(4) == 0 {
						privateFn = w.strFn(base64.StdEncoding.EncodeToString([]byte(genString(r))))
					}
					if len(deps) > 0 && r.IntN(8) != 0 {
						depsFn = func() { w.array(len(deps), func(i int) { w.str(deps[i]) }) }
					} else if r.IntN(12) == 0 {
						depsFn = w.raw([]string{"[]", "null", `["fp_thing.a", "fp_thing.a"]`, `"fp_thing.a"`, `[1]`}[r.IntN(5)])
					}
					if r.IntN(6) == 0 {
						cbdFn = w.raw("true")
					}
					if r.IntN(12) == 0 {
						identityFn = w.raw(`{"id":"x"}`)
					}
					w.object(
						"index_key", indexFn,
						"status", statusFn,
						"deposed", deposedFn,
						"schema_version", w.raw(strconv.Itoa(r.IntN(3))),
						"attributes", func() {
							w.object(
								"id", w.strFn("id-"+genIdent(r)),
								"value", w.strFn(genString(r)),
								"blob", w.strFn(strings.Repeat("x", attrSize)),
								"extra", func() { w.anyJSON(4) },
							)
						},
						"sensitive_attributes", sensitiveFn,
						"identity_schema_version", w.raw("0"),
						"identity", identityFn,
						"private", privateFn,
						"dependencies", depsFn,
						"create_before_destroy", cbdFn,
					)
				})
			},
		)
	}
	members := []any{
		"version", w.raw("4"),
		"terraform_version", w.strFn([]string{"1.5.7", "1.9.0", "1.14.0"}[r.IntN(3)]),
		"serial", w.raw(strconv.Itoa(r.IntN(1000))),
		"lineage", w.strFn("11111111-2222-3333-4444-555555555555"),
		"outputs", func() {
			var outs []any
			for i, n := 0, r.IntN(3); i < n; i++ {
				outs = append(outs, genIdent(r), func() {
					var sensitiveFn func()
					if r.IntN(3) == 0 {
						sensitiveFn = w.raw("true")
					}
					switch r.IntN(3) {
					case 0:
						w.object("value", w.strFn(genString(r)), "type", w.strFn("string"), "sensitive", sensitiveFn)
					case 1:
						w.object("value", w.raw("[1,2.5,1e3]"), "type", w.raw(`["list","number"]`), "sensitive", sensitiveFn)
					default:
						w.object("value", w.raw(`{"a":"x","b":true}`), "type", w.raw(`["object",{"a":"string","b":"bool"}]`), "sensitive", sensitiveFn)
					}
				})
			}
			w.object(outs...)
		},
		"resources", func() {
			if style == 3 {
				// The elements are separated in a way that also occurs
				// inside them, which the fork's search for the boundaries
				// between resources must not be misled by.
				w.b.WriteByte('[')
				for i := 0; i < resources; i++ {
					if i > 0 {
						w.b.WriteString(",")
					}
					w.b.WriteString("\n      ")
					w.b.WriteString(`{"mode":"managed","type":"fp_m","name":"n` + strconv.Itoa(i) + `","provider":"provider[\"registry.terraform.io/hashicorp/fp\"]","instances":[{"schema_version":0,"attributes":{"id":"x","list":[` + "\n    {\n      \"a\": \"" + strings.Repeat("y", attrSize) + "\"\n    },\n    {\n      \"a\": 2\n    }\n  ]}}]}")
				}
				w.b.WriteString("\n    ]")
				return
			}
			w.array(resources, resource)
		},
		"check_results", w.raw("null"),
	}
	if r.IntN(10) == 0 {
		// Properties in an unusual order.
		members = append(members[10:], members[:10]...)
	}
	w.object(members...)
	w.b.WriteByte('\n')
	return []byte(w.b.String())
}

func mutate(r *rand.Rand, src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	src = bytes.Clone(src)
	const junk = "{}[],:\"\\ \n0aE.-tfn\x00\xff"
	for i, n := 0, 1+r.IntN(3); i < n; i++ {
		at := r.IntN(len(src))
		switch r.IntN(6) {
		case 0:
			src = append(src[:at], src[at+1:]...)
		case 1:
			src[at] = junk[r.IntN(len(junk))]
		case 2:
			src = append(src[:at], append([]byte{junk[r.IntN(len(junk))]}, src[at:]...)...)
		case 3:
			src = src[:at]
		case 4:
			end := min(len(src), at+1+r.IntN(200))
			src = append(src[:end], append(bytes.Clone(src[at:end]), src[end:]...)...)
		default:
			end := min(len(src), at+1+r.IntN(200))
			src = append(src[:at], src[end:]...)
		}
		if len(src) == 0 {
			break
		}
	}
	return src
}

func describeState(f *statefile.File) string {
	var lines []string
	add := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	add("meta %s %d %s", f.TerraformVersion, f.Serial, f.Lineage)
	for name, ov := range f.State.RootOutputValues {
		add("output %s %v %#v", name, ov.Sensitive, ov.Value)
	}
	object := func(addr string, obj *states.ResourceInstanceObjectSrc) {
		deps := make([]string, len(obj.Dependencies))
		for i, dep := range obj.Dependencies {
			deps[i] = dep.String()
		}
		add("object %s status=%c schema=%d/%d cbd=%v attrs=%s flat=%v sensitive=%#v identity=%q private=%q nilprivate=%v deps=%q",
			addr, obj.Status, obj.SchemaVersion, obj.IdentitySchemaVersion, obj.CreateBeforeDestroy,
			hash(obj.AttrsJSON), obj.AttrsFlat, obj.AttrSensitivePaths, obj.IdentityJSON, obj.Private, obj.Private == nil, deps)
	}
	for _, ms := range f.State.Modules {
		add("module %s", ms.Addr)
		for _, rs := range ms.Resources {
			add("resource %s provider=%s instances=%d", rs.Addr, rs.ProviderConfig, len(rs.Instances))
			for key, is := range rs.Instances {
				addr := rs.Addr.Instance(key).String()
				if is.Current != nil {
					object(addr, is.Current)
				}
				for dk, obj := range is.Deposed {
					object(addr+" deposed "+string(dk), obj)
				}
			}
		}
	}
	sort.Strings(lines)
	return hash([]byte(strings.Join(lines, "\n")))
}

func stateFileCase(r *rand.Rand, i int) string {
	return protect(func() string {
		style := r.IntN(4)
		resources, attrSize := r.IntN(12), r.IntN(40)
		switch {
		case i%40 == 7:
			// Large enough for the fork to look for the boundaries between
			// resources concurrently.
			resources, attrSize = 1500+r.IntN(500), 2600
			style = []int{0, 0, 1, 3}[r.IntN(4)]
		case i%10 == 3:
			// Large enough for the fork to decode the resources concurrently.
			resources, attrSize = 300+r.IntN(300), 900
		}
		src := genStateFile(r, style, resources, attrSize)
		mutated := false
		if r.IntN(3) == 0 {
			src, mutated = mutate(r, src), true
		}

		f, err := statefile.Read(bytes.NewReader(src))
		if err != nil {
			// The wording of some errors differs, which the fork documents
			// (difference 8), so only the fact of the error is compared.
			return fmt.Sprintf("style=%d len=%d mutated=%v read=ERR", style, len(src), mutated)
		}
		var first bytes.Buffer
		if err := statefile.Write(f, &first); err != nil {
			return fmt.Sprintf("style=%d len=%d mutated=%v read=ok state=%s write=ERR", style, len(src), mutated, describeState(f))
		}
		// What was written must read back as the same state, and be written
		// identically the second time.
		f2, err := statefile.Read(bytes.NewReader(first.Bytes()))
		if err != nil {
			return fmt.Sprintf("style=%d len=%d mutated=%v read=ok state=%s write=%s reread=ERR", style, len(src), mutated, describeState(f), hash(first.Bytes()))
		}
		var second bytes.Buffer
		statefile.Write(f2, &second)
		return fmt.Sprintf("style=%d len=%d mutated=%v read=ok state=%s write=%s:%d reread=%s rewrite=%s equal=%v",
			style, len(src), mutated, describeState(f), hash(first.Bytes()), first.Len(), describeState(f2), hash(second.Bytes()),
			statefile.StatesMarshalEqual(f.State, f2.State))
	})
}

// ---------------------------------------------------------------------------
// dag: transitive reduction, cycle detection and the walk
// ---------------------------------------------------------------------------

type vertex int

func (v vertex) Name() string { return "v" + strconv.Itoa(int(v)) }

func genGraph(r *rand.Rand) (*dag.AcyclicGraph, int, bool) {
	var g dag.AcyclicGraph
	n := 1 + r.IntN(60)
	if r.IntN(10) == 0 {
		n = 200 + r.IntN(400)
	}
	for i := 0; i < n; i++ {
		g.Add(vertex(i))
	}
	cyclic := false
	switch r.IntN(6) {
	case 0:
		// A chain, with shortcuts.
		for i := 1; i < n; i++ {
			g.Connect(vertex(i), vertex(i-1))
			if i > 2 && r.IntN(3) == 0 {
				g.Connect(vertex(i), vertex(r.IntN(i-1)))
			}
		}
	case 1:
		// Everything connected to everything before it.
		for i := 0; i < min(n, 40); i++ {
			for j := 0; j < i; j++ {
				g.Connect(vertex(i), vertex(j))
			}
		}
	case 2:
		// Layers, like modules whose resources depend on the layer before.
		width := 1 + r.IntN(8)
		for i := width; i < n; i++ {
			for j := 0; j < 1+r.IntN(width); j++ {
				g.Connect(vertex(i), vertex((i/width-1)*width+r.IntN(width)))
			}
		}
	default:
		edges := r.IntN(4 * n)
		for i := 0; i < edges; i++ {
			a, b := r.IntN(n), r.IntN(n)
			if a == b {
				continue
			}
			if a < b {
				a, b = b, a
			}
			g.Connect(vertex(a), vertex(b))
		}
	}
	if r.IntN(12) == 0 && n > 2 {
		// An edge in the other direction, which usually makes a cycle.
		a, b := r.IntN(n), r.IntN(n)
		if a > b {
			a, b = b, a
		}
		g.Connect(vertex(a), vertex(b))
		cyclic = true
	}
	return &g, n, cyclic
}

func describeEdges(g *dag.AcyclicGraph) string {
	edges := g.Edges()
	strs := make([]string, len(edges))
	for i, e := range edges {
		strs[i] = e.Source.Name() + ">" + e.Target.Name()
	}
	sort.Strings(strs)
	return fmt.Sprintf("%d:%s", len(strs), hash([]byte(strings.Join(strs, ","))))
}

func dagCase(r *rand.Rand) string {
	return protect(func() string {
		g, n, _ := genGraph(r)
		var b strings.Builder
		fmt.Fprintf(&b, "n=%d edges=%s ", n, describeEdges(g))

		valid := g.Validate() == nil
		var cycles []string
		for _, cycle := range g.Cycles() {
			var names []string
			for _, v := range cycle {
				names = append(names, v.Name())
			}
			sort.Strings(names)
			cycles = append(cycles, strings.Join(names, "+"))
		}
		sort.Strings(cycles)
		fmt.Fprintf(&b, "valid=%v cycles=%s ", valid, hash([]byte(strings.Join(cycles, ";"))))

		g.TransitiveReduction()
		if !valid {
			// A graph with a cycle is never walked, and what upstream's
			// reduction leaves of one depends on the order in which it
			// happens to visit the vertices, so there is nothing to compare
			// beyond its having completed.
			return b.String() + "reduced=completed"
		}
		fmt.Fprintf(&b, "reduced=%s ", describeEdges(g))

		// Walk it with some of the vertices failing. Which vertices are
		// visited and which are skipped is decided by the graph alone.
		failing := make(map[vertex]bool)
		for i := 0; i < n; i++ {
			if r.IntN(15) == 0 {
				failing[vertex(i)] = true
			}
		}
		var mu sync.Mutex
		done := make(map[vertex]bool)
		var visited []string
		violations := 0
		diags := g.Walk(func(v dag.Vertex) tfdiags.Diagnostics {
			var diags tfdiags.Diagnostics
			mu.Lock()
			for dep := range g.EdgesFrom(v).All() {
				if !done[dep.(vertex)] {
					violations++
				}
			}
			visited = append(visited, v.Name())
			mu.Unlock()
			if failing[v.(vertex)] {
				diags = diags.Append(fmt.Errorf("%s failed", v.Name()))
			}
			mu.Lock()
			done[v.(vertex)] = true
			mu.Unlock()
			return diags
		})
		sort.Strings(visited)
		var messages []string
		for _, diag := range diags {
			messages = append(messages, diag.Description().Summary)
		}
		sort.Strings(messages)
		fmt.Fprintf(&b, "visited=%d:%s violations=%d diags=%d:%s", len(visited), hash([]byte(strings.Join(visited, ","))), violations, len(messages), hash([]byte(strings.Join(messages, ","))))
		return b.String()
	})
}

// ---------------------------------------------------------------------------
// changes: the planned changes, as the graph walk records and finds them
// ---------------------------------------------------------------------------

func genInstanceAddr(r *rand.Rand) addrs.AbsResourceInstance {
	mod := addrs.RootModuleInstance
	for i, n := 0, r.IntN(3); i < n; i++ {
		switch r.IntN(3) {
		case 0:
			mod = mod.Child("m"+strconv.Itoa(r.IntN(2)), addrs.NoKey)
		case 1:
			mod = mod.Child("m"+strconv.Itoa(r.IntN(2)), addrs.IntKey(r.IntN(2)))
		default:
			mod = mod.Child("m"+strconv.Itoa(r.IntN(2)), addrs.StringKey([]string{"a", "b c"}[r.IntN(2)]))
		}
	}
	res := addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "fp_thing", Name: "r" + strconv.Itoa(r.IntN(3))}
	var key addrs.InstanceKey = addrs.NoKey
	switch r.IntN(3) {
	case 1:
		key = addrs.IntKey(r.IntN(3))
	case 2:
		key = addrs.StringKey([]string{"x", "1"}[r.IntN(2)])
	}
	return res.Instance(key).Absolute(mod)
}

func changesCase(r *rand.Rand) string {
	return protect(func() string {
		cs := plans.NewChanges().SyncWrapper()
		provider := addrs.AbsProviderConfig{Module: addrs.RootModule, Provider: addrs.NewDefaultProvider("fp")}
		var b strings.Builder
		describe := func(rcs ...*plans.ResourceInstanceChange) {
			for _, rc := range rcs {
				if rc == nil {
					b.WriteString("nil,")
					continue
				}
				fmt.Fprintf(&b, "%s/%s/%c/%s,", rc.Addr, rc.DeposedKey, rc.Action, rc.After.AsString())
			}
			b.WriteString(";")
		}
		outputs := func(ocs ...*plans.OutputChange) {
			for _, oc := range ocs {
				if oc == nil {
					b.WriteString("nil,")
					continue
				}
				fmt.Fprintf(&b, "%s/%s,", oc.Addr, oc.After.AsString())
			}
			b.WriteString(";")
		}
		genOutput := func() addrs.AbsOutputValue {
			return addrs.AbsOutputValue{Module: genInstanceAddr(r).Module, OutputValue: addrs.OutputValue{Name: "o" + strconv.Itoa(r.IntN(3))}}
		}
		for step, steps := 0, 20+r.IntN(200); step < steps; step++ {
			addr := genInstanceAddr(r)
			dk := addrs.NotDeposed
			if r.IntN(6) == 0 {
				dk = addrs.DeposedKey([]string{"00000001", "00000002"}[r.IntN(2)])
			}
			switch r.IntN(12) {
			case 0, 1, 2:
				cs.AppendResourceInstanceChange(&plans.ResourceInstanceChange{
					Addr:         addr,
					PrevRunAddr:  addr,
					DeposedKey:   dk,
					ProviderAddr: provider,
					Change: plans.Change{
						Action: []plans.Action{plans.NoOp, plans.Create, plans.Update, plans.Delete}[r.IntN(4)],
						Before: cty.NullVal(cty.String),
						After:  cty.StringVal(strconv.Itoa(step)),
					},
				})
			case 3:
				cs.RemoveResourceInstanceChange(addr, dk)
			case 4:
				describe(cs.GetResourceInstanceChange(addr, dk))
			case 5:
				describe(cs.GetChangesForConfigResource(addr.ConfigResource())...)
			case 6:
				describe(cs.GetChangesForAbsResource(addr.ContainingResource())...)
			case 7, 8:
				cs.AppendOutputChange(&plans.OutputChange{
					Addr:   genOutput(),
					Change: plans.Change{Action: plans.Create, Before: cty.NullVal(cty.String), After: cty.StringVal(strconv.Itoa(step))},
				})
			case 9:
				cs.RemoveOutputChange(genOutput())
			case 10:
				outputs(cs.GetOutputChange(genOutput()))
				outputs(cs.GetRootOutputChanges()...)
			default:
				out := genOutput()
				if !out.Module.IsRoot() {
					parent, call := out.Module.Call()
					outputs(cs.GetOutputChanges(parent, call)...)
				}
			}
		}
		return hash([]byte(b.String()))
	})
}

// ---------------------------------------------------------------------------
// expander: the instances of modules and resources
// ---------------------------------------------------------------------------

type expansionTree struct {
	name     string
	children []*expansionTree
}

func expanderCase(r *rand.Rand) string {
	return protect(func() string {
		exp := instances.NewExpander(nil)
		var b strings.Builder

		// A tree of module calls, three deep.
		var build func(depth int) []*expansionTree
		build = func(depth int) []*expansionTree {
			if depth == 0 {
				return nil
			}
			var ret []*expansionTree
			for i, n := 0, r.IntN(4); i < n; i++ {
				ret = append(ret, &expansionTree{name: "m" + strconv.Itoa(i), children: build(depth - 1)})
			}
			return ret
		}
		tree := build(3)

		resource := addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "fp_thing", Name: "r"}
		forEach := func(n int) map[string]cty.Value {
			m := make(map[string]cty.Value)
			for i := 0; i < n; i++ {
				m[[]string{"a", "b c", "é", "10", "9"}[i]] = cty.True
			}
			return m
		}
		// Register the calls in each module instance, parents first, as the
		// graph walk does, asking about each module once it's registered.
		var register func(path addrs.Module, parents []addrs.ModuleInstance, calls []*expansionTree)
		register = func(path addrs.Module, parents []addrs.ModuleInstance, calls []*expansionTree) {
			for _, parent := range parents {
				switch r.IntN(4) {
				case 0:
					exp.SetResourceSingle(parent, resource)
				case 1:
					exp.SetResourceCount(parent, resource, r.IntN(4))
				case 2:
					exp.SetResourceForEach(parent, resource, forEach(r.IntN(4)))
				default:
					exp.SetResourceCountUnknown(parent, resource)
				}
			}
			fmt.Fprintf(&b, "resource %s:", path)
			for _, inst := range exp.ExpandModuleResource(path, resource) {
				b.WriteString(inst.String() + ",")
			}
			var unknown []string
			for _, per := range exp.UnknownResourceInstances(resource.InModule(path)) {
				unknown = append(unknown, per.String())
			}
			sort.Strings(unknown)
			fmt.Fprintf(&b, " unknown=%q\n", unknown)

			for _, call := range calls {
				callAddr := addrs.ModuleCall{Name: call.name}
				for _, parent := range parents {
					switch r.IntN(6) {
					case 0:
						exp.SetModuleSingle(parent, callAddr)
					case 1, 2:
						exp.SetModuleCount(parent, callAddr, r.IntN(4))
					case 3, 4:
						exp.SetModuleForEach(parent, callAddr, forEach(r.IntN(4)))
					default:
						exp.SetModuleCountUnknown(parent, callAddr)
					}
				}
				childPath := path.Child(call.name)
				// Asked for twice, since the fork remembers the answer.
				for range 2 {
					insts := exp.ExpandModule(childPath, false)
					fmt.Fprintf(&b, "module %s:", childPath)
					for _, inst := range insts {
						b.WriteString(inst.String() + ",")
					}
					// A caller is free to modify what it's given.
					for i := range insts {
						insts[i] = addrs.RootModuleInstance
					}
					var unknown []string
					for _, pem := range exp.UnknownModuleInstances(childPath, false) {
						unknown = append(unknown, pem.String())
					}
					sort.Strings(unknown)
					fmt.Fprintf(&b, " unknown=%q\n", unknown)
				}
				register(childPath, exp.ExpandModule(childPath, false), call.children)
			}
		}
		register(addrs.RootModule, []addrs.ModuleInstance{addrs.RootModuleInstance}, tree)
		return hash([]byte(b.String()))
	})
}
