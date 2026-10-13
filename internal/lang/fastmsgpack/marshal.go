// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

// Package fastmsgpack is an alternative to the msgpack package of cty which
// produces exactly the same results with less work.
//
// Values are serialized as msgpack whenever they're sent to a provider and
// whenever they're recorded in a plan, so for a configuration with very
// many resource instances the cost of doing that is significant.
package fastmsgpack

import (
	"encoding/binary"
	"math"
	"math/big"
	"slices"
	"sync"

	"github.com/zclconf/go-cty/cty"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"
)

// Unmarshal is the Unmarshal function of the msgpack package of cty.
func Unmarshal(b []byte, ty cty.Type) (cty.Value, error) {
	return ctymsgpack.Unmarshal(b, ty)
}

// ImpliedType is the ImpliedType function of the msgpack package of cty.
func ImpliedType(b []byte) (cty.Type, error) {
	return ctymsgpack.ImpliedType(b)
}

// Marshal returns exactly what the Marshal function of the msgpack package
// of cty returns for the same arguments.
//
// It deals directly with the kinds of value that make up nearly all of what
// we serialize, writing the result into a reused buffer and returning a copy
// of exactly the right size. Anything else is left to the other
// implementation: values that don't already conform to the given type,
// unknown values, and anything that can't be serialized at all, for which
// that implementation produces the appropriate error.
func Marshal(val cty.Value, ty cty.Type) ([]byte, error) {
	if errs := val.Type().TestConformance(ty); errs != nil {
		// The other implementation attempts a conversion.
		return ctymsgpack.Marshal(val, ty)
	}

	scratch := scratchBuffers.Get().(*[]byte)
	buf, ok := appendValue((*scratch)[:0], val, ty)
	var ret []byte
	if ok {
		ret = slices.Clone(buf)
	}
	// A buffer that grew very large for one unusual value isn't worth
	// holding on to.
	if cap(buf) <= maxScratchBuffer {
		*scratch = buf
		scratchBuffers.Put(scratch)
	}
	if !ok {
		return ctymsgpack.Marshal(val, ty)
	}
	return ret, nil
}

const maxScratchBuffer = 4 << 20

var scratchBuffers = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 16*1024)
		return &buf
	},
}

// appendValue appends the serialization of the given value, which must
// conform to the given type, and returns the extended buffer. It returns
// false if the value includes anything that this implementation doesn't
// deal with, in which case what's been appended is meaningless.
func appendValue(buf []byte, val cty.Value, ty cty.Type) ([]byte, bool) {
	if val.IsMarked() {
		return buf, false
	}

	// If the value will be decoded as DynamicPseudoType then its real type
	// is recorded along with it, as the JSON serialization of the type.
	if ty == cty.DynamicPseudoType && val.Type() != cty.DynamicPseudoType {
		typeJSON, err := val.Type().MarshalJSON()
		if err != nil || typeJSON == nil {
			return buf, false
		}
		buf = append(buf, 0x92) // an array of two elements
		buf = appendBinLen(buf, len(typeJSON))
		buf = append(buf, typeJSON...)
		return appendValue(buf, val, val.Type())
	}

	if !val.IsKnown() {
		return buf, false
	}
	if val.IsNull() {
		return append(buf, 0xc0), true
	}

	switch {
	case ty == cty.String:
		return appendString(buf, val.AsString()), true
	case ty == cty.Number:
		return appendNumber(buf, val)
	case ty == cty.Bool:
		if val.True() {
			return append(buf, 0xc3), true
		}
		return append(buf, 0xc2), true
	case ty.IsPrimitiveType():
		return buf, false
	case ty.IsListType(), ty.IsSetType():
		buf = appendArrayLen(buf, val.LengthInt())
		ety := ty.ElementType()
		ok := true
		for it := val.ElementIterator(); it.Next(); {
			_, ev := it.Element()
			if buf, ok = appendValue(buf, ev, ety); !ok {
				return buf, false
			}
		}
		return buf, true
	case ty.IsMapType():
		buf = appendMapLen(buf, val.LengthInt())
		ety := ty.ElementType()
		ok := true
		for it := val.ElementIterator(); it.Next(); {
			ek, ev := it.Element()
			if buf, ok = appendValue(buf, ek, ek.Type()); !ok {
				return buf, false
			}
			if buf, ok = appendValue(buf, ev, ety); !ok {
				return buf, false
			}
		}
		return buf, true
	case ty.IsTupleType():
		etys := ty.TupleElementTypes()
		buf = appendArrayLen(buf, len(etys))
		i := 0
		ok := true
		for it := val.ElementIterator(); it.Next(); i++ {
			if i >= len(etys) {
				return buf, false
			}
			_, ev := it.Element()
			if buf, ok = appendValue(buf, ev, etys[i]); !ok {
				return buf, false
			}
		}
		return buf, i == len(etys)
	case ty.IsObjectType():
		atys := ty.AttributeTypes()
		// The attributes are written in order of their names.
		var namesBuf [24]string
		names := namesBuf[:0]
		for name := range atys {
			names = append(names, name)
		}
		slices.Sort(names)
		buf = appendMapLen(buf, len(names))
		ok := true
		for _, name := range names {
			buf = appendString(buf, name)
			if buf, ok = appendValue(buf, val.GetAttr(name), atys[name]); !ok {
				return buf, false
			}
		}
		return buf, true
	default:
		// Capsule types, and anything else unexpected.
		return buf, false
	}
}

func appendNumber(buf []byte, val cty.Value) ([]byte, bool) {
	bf := val.AsBigFloat()
	if bf.IsInf() {
		if bf.Signbit() {
			return appendFloat64(buf, math.Inf(-1)), true
		}
		return appendFloat64(buf, math.Inf(1)), true
	}
	if iv, acc := bf.Int64(); acc == big.Exact {
		return appendInt(buf, iv), true
	}
	if fv, acc := bf.Float64(); acc == big.Exact && !bf.IsInt() {
		return appendFloat64(buf, fv), true
	}
	return appendString(buf, bf.Text('f', -1)), true
}

func appendFloat64(buf []byte, n float64) []byte {
	buf = append(buf, 0xcb)
	return binary.BigEndian.AppendUint64(buf, math.Float64bits(n))
}

// appendInt appends an integer in the smallest form that can represent it.
func appendInt(buf []byte, n int64) []byte {
	switch {
	case n >= 0 && n <= math.MaxInt8:
		return append(buf, byte(n))
	case n >= 0 && n <= math.MaxUint8:
		return append(buf, 0xcc, byte(n))
	case n >= 0 && n <= math.MaxUint16:
		return binary.BigEndian.AppendUint16(append(buf, 0xcd), uint16(n))
	case n >= 0 && n <= math.MaxUint32:
		return binary.BigEndian.AppendUint32(append(buf, 0xce), uint32(n))
	case n >= 0:
		return binary.BigEndian.AppendUint64(append(buf, 0xcf), uint64(n))
	case n >= -32:
		return append(buf, byte(n))
	case n >= math.MinInt8:
		return append(buf, 0xd0, byte(n))
	case n >= math.MinInt16:
		return binary.BigEndian.AppendUint16(append(buf, 0xd1), uint16(n))
	case n >= math.MinInt32:
		return binary.BigEndian.AppendUint32(append(buf, 0xd2), uint32(n))
	default:
		return binary.BigEndian.AppendUint64(append(buf, 0xd3), uint64(n))
	}
}

func appendString(buf []byte, s string) []byte {
	switch l := len(s); {
	case l < 32:
		buf = append(buf, 0xa0|byte(l))
	case l < 256:
		buf = append(buf, 0xd9, byte(l))
	case l <= math.MaxUint16:
		buf = binary.BigEndian.AppendUint16(append(buf, 0xda), uint16(l))
	default:
		buf = binary.BigEndian.AppendUint32(append(buf, 0xdb), uint32(l))
	}
	return append(buf, s...)
}

func appendBinLen(buf []byte, l int) []byte {
	switch {
	case l < 256:
		return append(buf, 0xc4, byte(l))
	case l <= math.MaxUint16:
		return binary.BigEndian.AppendUint16(append(buf, 0xc5), uint16(l))
	default:
		return binary.BigEndian.AppendUint32(append(buf, 0xc6), uint32(l))
	}
}

func appendArrayLen(buf []byte, l int) []byte {
	switch {
	case l < 16:
		return append(buf, 0x90|byte(l))
	case l <= math.MaxUint16:
		return binary.BigEndian.AppendUint16(append(buf, 0xdc), uint16(l))
	default:
		return binary.BigEndian.AppendUint32(append(buf, 0xdd), uint32(l))
	}
}

func appendMapLen(buf []byte, l int) []byte {
	switch {
	case l < 16:
		return append(buf, 0x80|byte(l))
	case l <= math.MaxUint16:
		return binary.BigEndian.AppendUint16(append(buf, 0xde), uint16(l))
	default:
		return binary.BigEndian.AppendUint32(append(buf, 0xdf), uint32(l))
	}
}
