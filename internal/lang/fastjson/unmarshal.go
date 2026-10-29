// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

// Package fastjson contains a faster equivalent of the JSON decoder from
// the cty library, for the hot paths that decode a very large number of
// values such as when reading the objects in a large state.
package fastjson

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// Unmarshal is equivalent to ctyjson.Unmarshal, producing exactly the same
// result, but is considerably faster for typical values.
//
// The cty decoder re-parses the source of each nested value once for each
// level of nesting above it, and allocates a new buffered decoder each time.
// This instead parses the source in a single pass. It directly handles only
// well-formed input whose JSON types exactly match the given type, and
// delegates to ctyjson.Unmarshal for everything else, so that all of the
// type conversions and error messages come from there.
func Unmarshal(buf []byte, ty cty.Type) (cty.Value, error) {
	p := parser{src: buf}
	if val, ok := p.value(ty); ok {
		if p.skipSpace(); p.pos == len(p.src) {
			return val, nil
		}
	}
	return ctyjson.Unmarshal(buf, ty)
}

type parser struct {
	src []byte
	pos int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		default:
			return
		}
	}
}

// literal consumes the given keyword if it's next in the input.
func (p *parser) literal(word string) bool {
	end := p.pos + len(word)
	if end > len(p.src) || string(p.src[p.pos:end]) != word {
		return false
	}
	// A keyword must not be immediately followed by something that would
	// make it part of a longer token.
	if end < len(p.src) {
		switch c := p.src[end]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			return false
		}
	}
	p.pos = end
	return true
}

// delim consumes the given delimiter if it's the next thing in the input
// after any whitespace.
func (p *parser) delim(c byte) bool {
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == c {
		p.pos++
		return true
	}
	return false
}

// value parses a value of the given type starting at the current position,
// or returns false if the input isn't something it can deal with.
func (p *parser) value(ty cty.Type) (cty.Value, bool) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return cty.NilVal, false
	}
	if p.src[p.pos] == 'n' {
		if !p.literal("null") {
			return cty.NilVal, false
		}
		return cty.NullVal(ty), true
	}

	switch {
	case ty == cty.DynamicPseudoType:
		return p.dynamicValue()
	case ty == cty.String:
		s, ok := p.stringToken()
		if !ok {
			return cty.NilVal, false
		}
		return cty.StringVal(s), true
	case ty == cty.Number:
		start := p.pos
		if !p.numberToken() {
			return cty.NilVal, false
		}
		val, err := cty.ParseNumberVal(string(p.src[start:p.pos]))
		if err != nil {
			return cty.NilVal, false
		}
		return val, true
	case ty == cty.Bool:
		switch {
		case p.literal("true"):
			return cty.True, true
		case p.literal("false"):
			return cty.False, true
		default:
			return cty.NilVal, false
		}
	case ty.IsListType(), ty.IsSetType():
		ety := ty.ElementType()
		if ety.HasDynamicTypes() {
			// The elements could turn out to have different types.
			return cty.NilVal, false
		}
		vals, ok := p.elements(func(int) (cty.Type, bool) { return ety, true })
		switch {
		case !ok:
			return cty.NilVal, false
		case ty.IsListType() && len(vals) == 0:
			return cty.ListValEmpty(ety), true
		case ty.IsListType():
			return cty.ListVal(vals), true
		case len(vals) == 0:
			return cty.SetValEmpty(ety), true
		default:
			return cty.SetVal(vals), true
		}
	case ty.IsTupleType():
		etys := ty.TupleElementTypes()
		vals, ok := p.elements(func(i int) (cty.Type, bool) {
			if i >= len(etys) {
				return cty.NilType, false
			}
			return etys[i], true
		})
		switch {
		case !ok || len(vals) != len(etys):
			return cty.NilVal, false
		case len(vals) == 0:
			return cty.EmptyTupleVal, true
		default:
			return cty.TupleVal(vals), true
		}
	case ty.IsMapType():
		ety := ty.ElementType()
		if ety.HasDynamicTypes() {
			return cty.NilVal, false
		}
		vals, ok := p.members(func(string) (cty.Type, bool) { return ety, true })
		switch {
		case !ok:
			return cty.NilVal, false
		case len(vals) == 0:
			return cty.MapValEmpty(ety), true
		default:
			return cty.MapVal(vals), true
		}
	case ty.IsObjectType():
		atys := ty.AttributeTypes()
		vals, ok := p.members(func(name string) (cty.Type, bool) {
			aty, ok := atys[name]
			return aty, ok
		})
		if !ok {
			return cty.NilVal, false
		}
		if len(vals) != len(atys) {
			if vals == nil {
				vals = make(map[string]cty.Value, len(atys))
			}
			for name, aty := range atys {
				if _, exists := vals[name]; !exists {
					vals[name] = cty.NullVal(aty)
				}
			}
		}
		if len(vals) == 0 {
			return cty.EmptyObjectVal, true
		}
		return cty.ObjectVal(vals), true
	default:
		// Capsule types, and anything else we don't expect to see.
		return cty.NilVal, false
	}
}

// elements parses a JSON array whose element at each index has the type
// that typeOf returns.
func (p *parser) elements(typeOf func(i int) (cty.Type, bool)) ([]cty.Value, bool) {
	if !p.delim('[') {
		return nil, false
	}
	if p.delim(']') {
		return nil, true
	}
	var vals []cty.Value
	for {
		ety, ok := typeOf(len(vals))
		if !ok {
			return nil, false
		}
		val, ok := p.value(ety)
		if !ok {
			return nil, false
		}
		vals = append(vals, val)
		if p.delim(']') {
			return vals, true
		}
		if !p.delim(',') {
			return nil, false
		}
	}
}

// members parses a JSON object whose property of each name has the type
// that typeOf returns.
func (p *parser) members(typeOf func(name string) (cty.Type, bool)) (map[string]cty.Value, bool) {
	if !p.delim('{') {
		return nil, false
	}
	if p.delim('}') {
		return nil, true
	}
	vals := make(map[string]cty.Value)
	for {
		p.skipSpace()
		name, ok := p.stringToken()
		if !ok || !p.delim(':') {
			return nil, false
		}
		ty, ok := typeOf(name)
		if !ok {
			return nil, false
		}
		val, ok := p.value(ty)
		if !ok {
			return nil, false
		}
		vals[name] = val
		if p.delim('}') {
			return vals, true
		}
		if !p.delim(',') {
			return nil, false
		}
	}
}

// dynamicValue parses the wrapper object that represents a value whose type
// isn't known statically, which records the type alongside the value.
func (p *parser) dynamicValue() (cty.Value, bool) {
	if !p.delim('{') {
		return cty.NilVal, false
	}
	var typeSrc, valueSrc []byte
	for {
		p.skipSpace()
		name, ok := p.stringToken()
		if !ok || !p.delim(':') {
			return cty.NilVal, false
		}
		p.skipSpace()
		start := p.pos
		if !p.skipValue(0) {
			return cty.NilVal, false
		}
		switch name {
		case "type":
			if typeSrc != nil {
				return cty.NilVal, false
			}
			typeSrc = p.src[start:p.pos]
		case "value":
			if valueSrc != nil {
				return cty.NilVal, false
			}
			valueSrc = p.src[start:p.pos]
		default:
			return cty.NilVal, false
		}
		if p.delim('}') {
			break
		}
		if !p.delim(',') {
			return cty.NilVal, false
		}
	}
	if typeSrc == nil || valueSrc == nil {
		return cty.NilVal, false
	}

	ty, ok := dynamicType(typeSrc)
	if !ok {
		return cty.NilVal, false
	}
	if ty == cty.DynamicPseudoType {
		// Not something that the encoder produces.
		return cty.NilVal, false
	}
	inner := parser{src: valueSrc}
	val, ok := inner.value(ty)
	if !ok {
		return cty.NilVal, false
	}
	if inner.skipSpace(); inner.pos != len(inner.src) {
		return cty.NilVal, false
	}
	return val, true
}

// maxSkipDepth limits how deeply nested a value skipValue will skip over.
const maxSkipDepth = 200

// skipValue moves past the JSON value at the current position, checking
// only as much of its syntax as is needed to find where it ends. The value
// is always parsed properly afterwards.
func (p *parser) skipValue(depth int) bool {
	if p.pos >= len(p.src) || depth > maxSkipDepth {
		return false
	}
	switch p.src[p.pos] {
	case '"':
		_, ok := p.stringToken()
		return ok
	case '{', '[':
		closer := byte('}')
		if p.src[p.pos] == '[' {
			closer = ']'
		}
		isObject := closer == '}'
		p.pos++
		if p.delim(closer) {
			return true
		}
		for {
			p.skipSpace()
			if isObject {
				if _, ok := p.stringToken(); !ok || !p.delim(':') {
					return false
				}
				p.skipSpace()
			}
			if !p.skipValue(depth + 1) {
				return false
			}
			if p.delim(closer) {
				return true
			}
			if !p.delim(',') {
				return false
			}
		}
	case 't':
		return p.literal("true")
	case 'f':
		return p.literal("false")
	case 'n':
		return p.literal("null")
	default:
		return p.numberToken()
	}
}

// stringToken parses the JSON string at the current position.
func (p *parser) stringToken() (string, bool) {
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", false
	}
	start := p.pos + 1
	simple := true
	for i := start; i < len(p.src); i++ {
		switch c := p.src[i]; {
		case c == '"':
			p.pos = i + 1
			if simple {
				return string(p.src[start:i]), true
			}
			// Anything involving escape sequences or non-ASCII characters
			// goes through the standard library, so that we interpret it
			// in exactly the same way.
			var s string
			if err := json.Unmarshal(p.src[start-1:i+1], &s); err != nil {
				return "", false
			}
			return s, true
		case c == '\\':
			simple = false
			i++ // whatever follows can't end the string
		case c < 0x20:
			return "", false
		case c >= 0x80:
			simple = false
		}
	}
	return "", false
}

// numberToken moves past the JSON number at the current position, returning
// false if there isn't a valid one there.
func (p *parser) numberToken() bool {
	i := p.pos
	digits := func() bool {
		start := i
		for i < len(p.src) && p.src[i] >= '0' && p.src[i] <= '9' {
			i++
		}
		return i > start
	}
	if i < len(p.src) && p.src[i] == '-' {
		i++
	}
	switch {
	case i < len(p.src) && p.src[i] == '0':
		i++
	case !digits():
		return false
	}
	if i < len(p.src) && p.src[i] == '.' {
		i++
		if !digits() {
			return false
		}
	}
	if i < len(p.src) && (p.src[i] == 'e' || p.src[i] == 'E') {
		i++
		if i < len(p.src) && (p.src[i] == '+' || p.src[i] == '-') {
			i++
		}
		if !digits() {
			return false
		}
	}
	p.pos = i
	return true
}

// dynamicTypes remembers the result of parsing the serialized form of a type
// recorded alongside a dynamically-typed value, as a map from the source
// string to cty.Type.
//
// Many values typically have exactly the same type, and parsing a type is
// expensive relative to parsing the value itself. Types are immutable, so
// the values can also then all share a single type object. We stop adding
// entries once there are a lot of them, so that an input with an unbounded
// variety of types can't make this grow without limit.
var (
	dynamicTypes     sync.Map
	dynamicTypeCount atomic.Int64
)

const maxDynamicTypes = 4096

func dynamicType(src []byte) (cty.Type, bool) {
	if cached, ok := dynamicTypes.Load(string(src)); ok {
		return cached.(cty.Type), true
	}
	var ty cty.Type
	if err := ty.UnmarshalJSON(src); err != nil {
		return cty.NilType, false
	}
	if dynamicTypeCount.Load() < maxDynamicTypes {
		if _, loaded := dynamicTypes.LoadOrStore(string(src), ty); !loaded {
			dynamicTypeCount.Add(1)
		}
	}
	return ty, true
}

// HasOnlySupportedAttributes returns true if the given JSON value can be
// established to have no object properties other than those that are
// attributes of the corresponding object types within the given type.
//
// A false result doesn't mean that there are unsupported attributes: it's
// also the result for anything this can't quickly make sense of, including
// invalid syntax and values whose structure doesn't match the type. Values of
// primitive types and of dynamic type aren't examined at all.
func HasOnlySupportedAttributes(buf []byte, ty cty.Type) bool {
	p := parser{src: buf}
	if !p.onlySupported(ty, 0) {
		return false
	}
	p.skipSpace()
	return p.pos == len(p.src)
}

func (p *parser) onlySupported(ty cty.Type, depth int) bool {
	p.skipSpace()
	if p.pos >= len(p.src) || depth > maxSkipDepth {
		return false
	}
	if p.src[p.pos] == 'n' {
		return p.literal("null")
	}
	switch {
	case ty.IsObjectType():
		atys := ty.AttributeTypes()
		return p.eachMember(func(name string) bool {
			aty, ok := atys[name]
			return ok && p.onlySupported(aty, depth+1)
		})
	case ty.IsMapType():
		ety := ty.ElementType()
		return p.eachMember(func(string) bool {
			return p.onlySupported(ety, depth+1)
		})
	case ty.IsListType(), ty.IsSetType():
		ety := ty.ElementType()
		return p.eachElement(func(int) bool {
			return p.onlySupported(ety, depth+1)
		})
	case ty.IsTupleType():
		etys := ty.TupleElementTypes()
		return p.eachElement(func(i int) bool {
			return i < len(etys) && p.onlySupported(etys[i], depth+1)
		})
	default:
		return p.skipValue(depth)
	}
}

// eachMember calls fn for each property of the JSON object at the current
// position, with the parser positioned at the property's value, which fn
// must consume.
func (p *parser) eachMember(fn func(name string) bool) bool {
	if !p.delim('{') {
		return false
	}
	if p.delim('}') {
		return true
	}
	for {
		p.skipSpace()
		name, ok := p.stringToken()
		if !ok || !p.delim(':') || !fn(name) {
			return false
		}
		if p.delim('}') {
			return true
		}
		if !p.delim(',') {
			return false
		}
	}
}

// eachElement calls fn for each element of the JSON array at the current
// position, with the parser positioned at the element, which fn must
// consume.
func (p *parser) eachElement(fn func(i int) bool) bool {
	if !p.delim('[') {
		return false
	}
	if p.delim(']') {
		return true
	}
	for i := 0; ; i++ {
		if !fn(i) {
			return false
		}
		if p.delim(']') {
			return true
		}
		if !p.delim(',') {
			return false
		}
	}
}
