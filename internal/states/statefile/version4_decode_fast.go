// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bytes"
	"encoding/base64"
)

// unmarshalResourceV4 decodes the JSON representation of a resource into rs,
// giving exactly the result that json.Unmarshal would give for a zero-valued
// rs, or returns false if the source is anything other than the
// straightforward kind of thing that we write ourselves. In that case rs
// might have been partially populated, and the caller must reset it and use
// json.Unmarshal instead, which will also produce the appropriate error if
// the source is invalid.
//
// Nearly all of a large state is the serialized attributes of resource
// instance objects, which we keep as they are rather than decoding them.
// The standard library validates the whole of its input before it starts,
// and then scans each of those values again in order to find its extent,
// using a scanner that's general rather than fast. This instead validates
// each value as it finds the end of it.
func unmarshalResourceV4(src []byte, rs *resourceStateV4) bool {
	d := fastDecoderV4{src: src}
	ok := d.object(func(key string) bool {
		switch key {
		case "module":
			return d.str(&rs.Module)
		case "mode":
			return d.str(&rs.Mode)
		case "type":
			return d.str(&rs.Type)
		case "name":
			return d.str(&rs.Name)
		case "each":
			return d.str(&rs.EachMode)
		case "provider":
			return d.str(&rs.ProviderConfig)
		case "instances":
			return d.instances(&rs.Instances)
		default:
			return false
		}
	})
	if !ok {
		return false
	}
	d.space()
	return d.i == len(d.src)
}

// fastDecoderV4 is the state of unmarshalResourceV4. Each of its methods
// that returns a boolean returns false to mean that the source is invalid
// or is something that it doesn't handle.
type fastDecoderV4 struct {
	src []byte
	i   int
}

func (d *fastDecoderV4) space() {
	d.i = skipJSONSpace(d.src, d.i)
}

// object decodes an object, calling member to decode the value of each of
// its properties. It doesn't accept an object that has more than one
// property of the same name, because which of them takes effect depends on
// the type of the value.
func (d *fastDecoderV4) object(member func(key string) bool) bool {
	d.space()
	if d.i >= len(d.src) || d.src[d.i] != '{' {
		return false
	}
	d.i++
	d.space()
	if d.i < len(d.src) && d.src[d.i] == '}' {
		d.i++
		return true
	}
	// There are few enough distinct properties that it's cheapest just to
	// compare each one with all of those before it.
	var seenBuf [16]string
	seen := seenBuf[:0]
	for {
		d.space()
		var key string
		if !d.simpleString(&key) {
			return false
		}
		for _, other := range seen {
			if other == key {
				return false
			}
		}
		seen = append(seen, key)
		d.space()
		if d.i >= len(d.src) || d.src[d.i] != ':' {
			return false
		}
		d.i++
		d.space()
		if !member(key) {
			return false
		}
		d.space()
		if d.i >= len(d.src) {
			return false
		}
		switch d.src[d.i] {
		case ',':
			d.i++
		case '}':
			d.i++
			return true
		default:
			return false
		}
	}
}

// simpleString decodes a string that consists only of printable ASCII
// characters, either as themselves or written as one of the escape sequences
// that we produce when writing such a string.
func (d *fastDecoderV4) simpleString(to *string) bool {
	if d.i >= len(d.src) || d.src[d.i] != '"' {
		return false
	}
	start := d.i + 1
	for j := start; j < len(d.src); j++ {
		c := d.src[j]
		switch {
		case c == '"':
			*to = string(d.src[start:j])
			d.i = j + 1
			return true
		case c == '\\':
			// Addresses routinely contain quotes, so this isn't unusual.
			return d.escapedString(to, start, j)
		case c < 0x20 || c >= 0x7f:
			return false
		}
	}
	return false
}

// escapedString is the continuation of simpleString for a string that
// starts at d.src[start] and has its first escape sequence at d.src[j].
func (d *fastDecoderV4) escapedString(to *string, start, j int) bool {
	buf := make([]byte, 0, 2*(j-start)+16)
	buf = append(buf, d.src[start:j]...)
	for j < len(d.src) {
		c := d.src[j]
		switch {
		case c == '"':
			*to = string(buf)
			d.i = j + 1
			return true
		case c == '\\':
			if j+1 >= len(d.src) {
				return false
			}
			switch e := d.src[j+1]; e {
			case '"', '\\', '/':
				buf = append(buf, e)
				j += 2
			case 'u':
				// Only the characters that are escaped for the benefit of
				// HTML, which are written as "\\u00" and two hex digits.
				if j+6 > len(d.src) || d.src[j+2] != '0' || d.src[j+3] != '0' {
					return false
				}
				hi, lo := unhexJSON(d.src[j+4]), unhexJSON(d.src[j+5])
				if hi < 0 || lo < 0 {
					return false
				}
				r := byte(hi<<4 | lo)
				if r < 0x20 || r >= 0x7f {
					return false
				}
				buf = append(buf, r)
				j += 6
			default:
				return false
			}
		case c < 0x20 || c >= 0x7f:
			return false
		default:
			buf = append(buf, c)
			j++
		}
	}
	return false
}

func unhexJSON(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func (d *fastDecoderV4) str(to *string) bool {
	return d.simpleString(to)
}

// uint decodes a non-negative integer that's small enough for there to be
// no question about how to represent it.
func (d *fastDecoderV4) uint(to *uint64) bool {
	start := d.i
	var n uint64
	for d.i < len(d.src) && d.src[d.i] >= '0' && d.src[d.i] <= '9' {
		n = n*10 + uint64(d.src[d.i]-'0')
		d.i++
	}
	digits := d.i - start
	if digits == 0 || digits > 15 || (digits > 1 && d.src[start] == '0') {
		return false
	}
	if d.i < len(d.src) {
		// Anything else that could be part of a number means that this
		// isn't just an integer.
		switch d.src[d.i] {
		case '.', 'e', 'E', '+', '-':
			return false
		}
	}
	*to = n
	return true
}

func (d *fastDecoderV4) boolean(to *bool) bool {
	switch {
	case bytes.HasPrefix(d.src[d.i:], []byte("true")):
		*to = true
		d.i += 4
	case bytes.HasPrefix(d.src[d.i:], []byte("false")):
		*to = false
		d.i += 5
	default:
		return false
	}
	return true
}

// raw validates the value at the current position and returns a copy of
// its source.
func (d *fastDecoderV4) raw() ([]byte, bool) {
	end, ok := validJSONValue(d.src, d.i)
	if !ok {
		return nil, false
	}
	ret := bytes.Clone(d.src[d.i:end])
	d.i = end
	return ret, true
}

func (d *fastDecoderV4) instances(to *[]instanceObjectStateV4) bool {
	if d.i >= len(d.src) || d.src[d.i] != '[' {
		return false
	}
	d.i++
	d.space()
	instances := []instanceObjectStateV4{}
	if d.i < len(d.src) && d.src[d.i] == ']' {
		d.i++
		*to = instances
		return true
	}
	for {
		instances = append(instances, instanceObjectStateV4{})
		if !d.instance(&instances[len(instances)-1]) {
			return false
		}
		d.space()
		if d.i >= len(d.src) {
			return false
		}
		switch d.src[d.i] {
		case ',':
			d.i++
		case ']':
			d.i++
			*to = instances
			return true
		default:
			return false
		}
	}
}

func (d *fastDecoderV4) instance(is *instanceObjectStateV4) bool {
	return d.object(func(key string) bool {
		switch key {
		case "index_key":
			// This is either a string or a number, and a number is
			// represented as a float64.
			if d.i < len(d.src) && d.src[d.i] == '"' {
				var s string
				if !d.simpleString(&s) {
					return false
				}
				is.IndexKey = s
				return true
			}
			var n uint64
			if !d.uint(&n) {
				return false
			}
			// An integer of at most 15 digits is exactly representable.
			is.IndexKey = float64(n)
			return true
		case "status":
			return d.str(&is.Status)
		case "deposed":
			return d.str(&is.Deposed)
		case "schema_version":
			return d.uint(&is.SchemaVersion)
		case "attributes":
			raw, ok := d.raw()
			is.AttributesRaw = raw
			return ok
		case "sensitive_attributes":
			raw, ok := d.raw()
			is.AttributeSensitivePaths = raw
			return ok
		case "identity_schema_version":
			return d.uint(&is.IdentitySchemaVersion)
		case "identity":
			raw, ok := d.raw()
			is.IdentityRaw = raw
			return ok
		case "private":
			var encoded string
			if !d.simpleString(&encoded) {
				return false
			}
			private := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
			n, err := base64.StdEncoding.Decode(private, []byte(encoded))
			if err != nil {
				return false
			}
			is.PrivateRaw = private[:n]
			return true
		case "dependencies":
			if d.i < len(d.src) && d.src[d.i] == 'n' {
				// A null leaves the pointer unset, which we leave to the
				// standard library along with everything else unusual.
				return false
			}
			raw, ok := d.raw()
			if !ok {
				return false
			}
			is.Dependencies = &dependenciesV4{src: raw}
			return true
		case "create_before_destroy":
			return d.boolean(&is.CreateBeforeDestroy)
		default:
			// This includes attributes_flat, which only very old states
			// have, and anything we don't recognize.
			return false
		}
	})
}

// validJSONValue returns the index just after the end of the JSON value
// that starts at src[i] if it's valid, or false if it isn't. It also returns
// false for a value that's nested deeply enough that the standard library
// might consider it to be invalid for that reason.
func validJSONValue(src []byte, i int) (int, bool) {
	const maxDepth = 1000

	// containers has one entry for each array or object we're currently
	// inside, which is true for an object.
	var containersBuf [32]bool
	containers := containersBuf[:0]

	// str returns the index just after the string that starts at src[j],
	// which must be a quote.
	str := func(j int) (int, bool) {
		j++
		for {
			// Strings are most of the content, and most of a string is
			// ordinary characters, which we pass over as a block.
			for j < len(src) && !jsonStringInvalidOrEnd[src[j]] {
				j++
			}
			if j >= len(src) {
				return 0, false
			}
			switch c := src[j]; {
			case c == '"':
				return j + 1, true
			case c == '\\':
				if j+1 >= len(src) {
					return 0, false
				}
				switch src[j+1] {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					j += 2
				case 'u':
					if j+6 > len(src) {
						return 0, false
					}
					for _, h := range src[j+2 : j+6] {
						switch {
						case h >= '0' && h <= '9', h >= 'a' && h <= 'f', h >= 'A' && h <= 'F':
						default:
							return 0, false
						}
					}
					j += 6
				default:
					return 0, false
				}
			default:
				// A control character.
				return 0, false
			}
		}
	}

	digits := func(j int) (int, bool) {
		from := j
		for j < len(src) && src[j] >= '0' && src[j] <= '9' {
			j++
		}
		return j, j > from
	}

	var ok bool
	for {
		i = skipJSONSpace(src, i)
		if i >= len(src) {
			return 0, false
		}
		// The start of a value.
		switch c := src[i]; {
		case c == '{' || c == '[':
			if len(containers) >= maxDepth {
				return 0, false
			}
			containers = append(containers, c == '{')
			i = skipJSONSpace(src, i+1)
			if i < len(src) && src[i] == c+2 {
				// An empty container, which we deal with below as though
				// it were the end of a value inside the container.
				// ('}' and ']' each follow their opening delimiter by two.)
				break
			}
			if c == '[' {
				continue // with the first element
			}
			if i >= len(src) || src[i] != '"' {
				return 0, false
			}
			if i, ok = str(i); !ok {
				return 0, false
			}
			i = skipJSONSpace(src, i)
			if i >= len(src) || src[i] != ':' {
				return 0, false
			}
			i++
			continue // with the value of the first property
		case c == '"':
			if i, ok = str(i); !ok {
				return 0, false
			}
		case c == '-' || (c >= '0' && c <= '9'):
			if c == '-' {
				i++
			}
			if i < len(src) && src[i] == '0' {
				i++
			} else if i, ok = digits(i); !ok {
				return 0, false
			}
			if i < len(src) && src[i] == '.' {
				if i, ok = digits(i + 1); !ok {
					return 0, false
				}
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				i++
				if i < len(src) && (src[i] == '+' || src[i] == '-') {
					i++
				}
				if i, ok = digits(i); !ok {
					return 0, false
				}
			}
		case c == 't' && bytes.HasPrefix(src[i:], []byte("true")):
			i += 4
		case c == 'f' && bytes.HasPrefix(src[i:], []byte("false")):
			i += 5
		case c == 'n' && bytes.HasPrefix(src[i:], []byte("null")):
			i += 4
		default:
			return 0, false
		}

		// We've completed a value, so now we close any containers that it
		// was the last member of, and then expect another member of the
		// innermost one that remains.
		for {
			if len(containers) == 0 {
				return i, true
			}
			i = skipJSONSpace(src, i)
			if i >= len(src) {
				return 0, false
			}
			inObject := containers[len(containers)-1]
			c := src[i]
			if c == ',' {
				i++
				if !inObject {
					break
				}
				i = skipJSONSpace(src, i)
				if i >= len(src) || src[i] != '"' {
					return 0, false
				}
				if i, ok = str(i); !ok {
					return 0, false
				}
				i = skipJSONSpace(src, i)
				if i >= len(src) || src[i] != ':' {
					return 0, false
				}
				i++
				break
			}
			if (inObject && c != '}') || (!inObject && c != ']') {
				return 0, false
			}
			i++
			containers = containers[:len(containers)-1]
		}
	}
}

// jsonStringInvalidOrEnd is true for each byte that can't appear as it is
// inside a JSON string, along with those that end a string or begin an
// escape sequence.
var jsonStringInvalidOrEnd = func() (special [256]bool) {
	for c := range 0x20 {
		special[c] = true
	}
	special['"'] = true
	special['\\'] = true
	return special
}()
