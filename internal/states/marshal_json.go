// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"encoding/json"
	"math/big"
	"math/bits"
	"sort"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// marshalJSON is equivalent to ctyjson.Marshal, producing exactly the same
// result, but is considerably faster for the values we typically find in
// resource instance objects.
//
// It directly handles only the situations where serialization succeeds
// without needing any type conversion, and delegates to ctyjson.Marshal
// for everything else so that the handling of unusual situations and all of
// the error messages come from there.
func marshalJSON(val cty.Value, ty cty.Type) ([]byte, error) {
	src, _, err := marshalJSONExact(val, ty)
	return src, err
}

// marshalJSONExact is like marshalJSON but additionally reports whether
// decoding the result using the same type is certain to produce a value
// that is indistinguishable from the one given, in which case a caller that
// would otherwise decode the result can just use the original value.
//
// That isn't true in general. In particular, a number that isn't an integer
// is written in decimal and then parsed with a fixed precision, which can
// give a slightly different number than we started with. The result is
// reported as exact only when the value consists entirely of things that
// are known to survive unchanged.
func marshalJSONExact(val cty.Value, ty cty.Type) (src []byte, exact bool, err error) {
	if errs := val.Type().TestConformance(ty); errs == nil {
		// We encode into a reusable scratch buffer and then copy the result
		// into a buffer of exactly the right size, rather than letting the
		// result buffer grow by repeated reallocation, because this is
		// called for every object in the state and the discarded smaller
		// buffers would otherwise add up to more garbage than the results.
		scratch := jsonScratch.Get().(*[]byte)
		enc := jsonEncoder{exact: true}
		buf, ok := enc.appendJSON((*scratch)[:0], val, ty)
		var result []byte
		if ok {
			result = make([]byte, len(buf))
			copy(result, buf)
		}
		if cap(buf) <= maxJSONScratch {
			*scratch = buf[:0]
		}
		jsonScratch.Put(scratch)
		if ok {
			return result, enc.exact, nil
		}
	}
	src, err = ctyjson.Marshal(val, ty)
	return src, false, err
}

// jsonScratch is a pool of buffers for marshalJSONExact to encode into.
var jsonScratch = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 4096)
		return &buf
	},
}

// maxJSONScratch is the largest buffer that we'll return to jsonScratch.
const maxJSONScratch = 1 << 20

// jsonEncoder is the state for a single call to marshalJSONExact.
type jsonEncoder struct {
	// exact remains true for as long as everything we've encoded is certain
	// to decode as exactly the value we were given.
	exact bool
}

// appendJSON appends the JSON serialization of the given value to buf,
// or returns false if the value is not one that it can serialize.
func (enc *jsonEncoder) appendJSON(buf []byte, val cty.Value, ty cty.Type) ([]byte, bool) {
	if val.IsMarked() || !val.IsKnown() {
		return nil, false
	}

	// If the value will be decoded as DynamicPseudoType then we need to save
	// dynamic type information to recover the real type.
	if ty == cty.DynamicPseudoType && val.Type() != cty.DynamicPseudoType {
		typeJSON, ok := dynamicTypeJSON(val.Type())
		if !ok {
			return nil, false
		}
		buf = append(buf, `{"value":`...)
		buf, ok = enc.appendJSON(buf, val, val.Type())
		if !ok {
			return nil, false
		}
		buf = append(buf, `,"type":`...)
		buf = append(buf, typeJSON...)
		buf = append(buf, '}')
		return buf, true
	}

	if val.IsNull() {
		return append(buf, "null"...), true
	}

	var ok bool
	switch {
	case ty == cty.String:
		return enc.appendJSONString(buf, val.AsString())
	case ty == cty.Number:
		return enc.appendJSONNumber(buf, val.AsBigFloat())
	case ty == cty.Bool:
		if val.True() {
			return append(buf, "true"...), true
		}
		return append(buf, "false"...), true
	case ty.IsListType(), ty.IsSetType():
		buf = append(buf, '[')
		ety := ty.ElementType()
		first := true
		for it := val.ElementIterator(); it.Next(); {
			if !first {
				buf = append(buf, ',')
			}
			first = false
			_, ev := it.Element()
			if buf, ok = enc.appendJSON(buf, ev, ety); !ok {
				return nil, false
			}
		}
		return append(buf, ']'), true
	case ty.IsMapType():
		buf = append(buf, '{')
		ety := ty.ElementType()
		first := true
		for it := val.ElementIterator(); it.Next(); {
			if !first {
				buf = append(buf, ',')
			}
			first = false
			ek, ev := it.Element()
			if buf, ok = enc.appendJSON(buf, ek, ek.Type()); !ok {
				return nil, false
			}
			buf = append(buf, ':')
			if buf, ok = enc.appendJSON(buf, ev, ety); !ok {
				return nil, false
			}
		}
		return append(buf, '}'), true
	case ty.IsTupleType():
		buf = append(buf, '[')
		etys := ty.TupleElementTypes()
		i := 0
		for it := val.ElementIterator(); it.Next(); i++ {
			if i > 0 {
				buf = append(buf, ',')
			}
			_, ev := it.Element()
			if buf, ok = enc.appendJSON(buf, ev, etys[i]); !ok {
				return nil, false
			}
		}
		return append(buf, ']'), true
	case ty.IsObjectType():
		buf = append(buf, '{')
		atys := ty.AttributeTypes()
		names := make([]string, 0, len(atys))
		for name := range atys {
			names = append(names, name)
		}
		sort.Strings(names)
		for i, name := range names {
			if i > 0 {
				buf = append(buf, ',')
			}
			if buf, ok = enc.appendJSONString(buf, name); !ok {
				return nil, false
			}
			buf = append(buf, ':')
			if buf, ok = enc.appendJSON(buf, val.GetAttr(name), atys[name]); !ok {
				return nil, false
			}
		}
		return append(buf, '}'), true
	default:
		// Capsule types, and anything else we don't expect to see.
		return nil, false
	}
}

func (enc *jsonEncoder) appendJSONString(buf []byte, s string) ([]byte, bool) {
	simple := true
	for i := 0; i < len(s); i++ {
		// These are the only characters that encoding/json writes without
		// any escaping.
		if c := s[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
			simple = false
			break
		}
	}
	if simple {
		buf = append(buf, '"')
		buf = append(buf, s...)
		return append(buf, '"'), true
	}

	if !utf8.ValidString(s) {
		// The encoder replaces anything that isn't valid UTF-8.
		enc.exact = false
	}
	src, err := json.Marshal(s)
	if err != nil {
		return nil, false
	}
	return append(buf, src...), true
}

func (enc *jsonEncoder) appendJSONNumber(buf []byte, f *big.Float) ([]byte, bool) {
	if f.IsInf() {
		return nil, false
	}
	// Numbers are stored with far more precision than most values need, which
	// makes finding their shortest decimal representation expensive, but
	// in practice most numbers are relatively small integers that we can
	// format directly. Negative zero is formatted as "-0", so it must take
	// the general path.
	if f.IsInt() && f.Sign() != 0 {
		if i, accuracy := f.Int64(); accuracy == big.Exact {
			// The general path writes the shortest decimal number that
			// identifies the value among those that can be held with the
			// same precision, which is only certain to be the integer
			// itself when that precision distinguishes it from the integers
			// either side of it. A large integer held as a 64-bit float,
			// such as the result of pow(2, 60), is written with its last
			// digits as zeros instead, and we must write the same thing.
			magnitude := uint64(i)
			if i < 0 {
				magnitude = -magnitude
			}
			if uint(bits.Len64(magnitude)) <= f.Prec() {
				return strconv.AppendInt(buf, i, 10), true
			}
		}
	}
	enc.exact = false
	return f.Append(buf, 'f', -1), true
}

// recentTypes remembers the serialized forms of the types most recently
// passed to dynamicTypeJSON.
//
// Serializing a type is expensive relative to serializing a typical value,
// and the values of a dynamically-typed attribute across the instances of a
// resource usually all have the same type. Types can't be used as map keys,
// so we just compare against a small number of recent ones.
var recentTypes struct {
	sync.Mutex
	entries [8]struct {
		ty   cty.Type
		json []byte
	}
	next int
}

// dynamicTypeJSON returns the serialized form of the given type, as
// recorded alongside a dynamically-typed value. The result must not be
// modified.
func dynamicTypeJSON(ty cty.Type) ([]byte, bool) {
	if hasEmptyTuple(ty) {
		// A tuple type with no elements is serialized as either
		// ["tuple",[]] or ["tuple",null], depending on how the type was
		// constructed, and the two are equal as types. Recognizing a type
		// by its being equal to an earlier one would give this one the
		// serialization of the other, so these are never remembered.
		src, err := ty.MarshalJSON()
		return src, err == nil
	}

	recentTypes.Lock()
	for i := range recentTypes.entries {
		if entry := &recentTypes.entries[i]; entry.json != nil && entry.ty.Equals(ty) {
			src := entry.json
			recentTypes.Unlock()
			return src, true
		}
	}
	recentTypes.Unlock()

	src, err := ty.MarshalJSON()
	if err != nil {
		return nil, false
	}

	recentTypes.Lock()
	entry := &recentTypes.entries[recentTypes.next]
	entry.ty, entry.json = ty, src
	recentTypes.next = (recentTypes.next + 1) % len(recentTypes.entries)
	recentTypes.Unlock()
	return src, true
}

// hasEmptyTuple returns true if the given type is, or contains, a tuple type
// with no elements.
func hasEmptyTuple(ty cty.Type) bool {
	switch {
	case ty.IsTupleType():
		etys := ty.TupleElementTypes()
		if len(etys) == 0 {
			return true
		}
		for _, ety := range etys {
			if hasEmptyTuple(ety) {
				return true
			}
		}
	case ty.IsCollectionType():
		return hasEmptyTuple(ty.ElementType())
	case ty.IsObjectType():
		for _, aty := range ty.AttributeTypes() {
			if hasEmptyTuple(aty) {
				return true
			}
		}
	}
	return false
}
