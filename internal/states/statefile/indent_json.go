// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

// appendIndentedJSON appends to dst the same thing that
//
//	json.MarshalIndent(json.RawMessage(src), prefix, indent)
//
// returns, and returns the extended buffer. If src isn't valid JSON, or is
// something that this function prefers not to deal with, then it returns
// false instead, in which case the caller must use the standard library and
// must disregard the returned buffer.
//
// The serialized attributes of resource instance objects make up nearly all
// of a large state, and they're already JSON when we come to write them.
// The standard library deals with a value like that by validating it,
// then producing a compact copy of it with some characters escaped, and
// then producing an indented copy of that, which between them account for
// most of the time taken to write a state. This does the same in one pass.
func appendIndentedJSON(dst, src []byte, prefix, indent string) ([]byte, bool) {
	const maxDepth = 1000
	const hex = "0123456789abcdef"

	// containers has one entry for each array or object we're currently
	// inside, which is true for an object.
	var containersBuf [32]bool
	containers := containersBuf[:0]

	newline := func(dst []byte, depth int) []byte {
		dst = append(dst, '\n')
		dst = append(dst, prefix...)
		for range depth {
			dst = append(dst, indent...)
		}
		return dst
	}

	i := 0
	skipSpace := func() {
		for i < len(src) {
			switch src[i] {
			case ' ', '\t', '\r', '\n':
				i++
			default:
				return
			}
		}
	}

	// str copies the string that starts at src[i], which must be a quote,
	// escaping the characters that the standard library escapes.
	str := func(dst []byte) ([]byte, bool) {
		// Strings are most of the content, so this uses its own position
		// rather than the shared one while it's scanning.
		start, j := i, i+1
		for {
			if j >= len(src) {
				return dst, false
			}
			c := src[j]
			if !jsonStringSpecial[c] {
				j++
				continue
			}
			switch {
			case c == '"':
				j++
				i = j
				return append(dst, src[start:j]...), true
			case c == '\\':
				if j+1 >= len(src) {
					return dst, false
				}
				switch src[j+1] {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					j += 2
				case 'u':
					if j+6 > len(src) {
						return dst, false
					}
					for _, h := range src[j+2 : j+6] {
						switch {
						case h >= '0' && h <= '9', h >= 'a' && h <= 'f', h >= 'A' && h <= 'F':
						default:
							return dst, false
						}
					}
					j += 6
				default:
					return dst, false
				}
			case c < 0x20:
				return dst, false
			case c == '<' || c == '>' || c == '&':
				dst = append(dst, src[start:j]...)
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
				j++
				start = j
			default:
				// 0xE2, which might begin U+2028 or U+2029.
				if j+2 < len(src) && src[j+1] == 0x80 && src[j+2]&^1 == 0xA8 {
					dst = append(dst, src[start:j]...)
					dst = append(dst, '\\', 'u', '2', '0', '2', hex[src[j+2]&0xF])
					j += 3
					start = j
				} else {
					j++
				}
			}
		}
	}

	// number copies the number that starts at src[i].
	number := func(dst []byte) ([]byte, bool) {
		start := i
		digits := func() bool {
			from := i
			for i < len(src) && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			return i > from
		}
		if src[i] == '-' {
			i++
		}
		switch {
		case i < len(src) && src[i] == '0':
			i++
		case !digits():
			return dst, false
		}
		if i < len(src) && src[i] == '.' {
			i++
			if !digits() {
				return dst, false
			}
		}
		if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
			i++
			if i < len(src) && (src[i] == '+' || src[i] == '-') {
				i++
			}
			if !digits() {
				return dst, false
			}
		}
		return append(dst, src[start:i]...), true
	}

	literal := func(dst []byte, word string) ([]byte, bool) {
		if len(src)-i < len(word) || string(src[i:i+len(word)]) != word {
			return dst, false
		}
		i += len(word)
		return append(dst, word...), true
	}

	// Each iteration deals with one value, and then with whatever follows
	// it in the containers it belongs to.
	var ok bool
	for {
		skipSpace()
		if i >= len(src) {
			return dst, false
		}
		// The start of a value.
		switch c := src[i]; {
		case c == '{' || c == '[':
			i++
			skipSpace()
			if i < len(src) && src[i] == c+2 {
				// An empty container has nothing between its delimiters.
				// ('}' and ']' each follow their opening delimiter by two.)
				dst = append(dst, c, c+2)
				i++
				break
			}
			if len(containers) >= maxDepth {
				return dst, false
			}
			containers = append(containers, c == '{')
			dst = append(dst, c)
			dst = newline(dst, len(containers))
			if c == '[' {
				continue // with the first element
			}
			if i >= len(src) || src[i] != '"' {
				return dst, false
			}
			if dst, ok = str(dst); !ok {
				return dst, false
			}
			skipSpace()
			if i >= len(src) || src[i] != ':' {
				return dst, false
			}
			i++
			dst = append(dst, ':', ' ')
			continue // with the value of the first property
		case c == '"':
			if dst, ok = str(dst); !ok {
				return dst, false
			}
		case c == '-' || (c >= '0' && c <= '9'):
			if dst, ok = number(dst); !ok {
				return dst, false
			}
		case c == 't':
			if dst, ok = literal(dst, "true"); !ok {
				return dst, false
			}
		case c == 'f':
			if dst, ok = literal(dst, "false"); !ok {
				return dst, false
			}
		case c == 'n':
			if dst, ok = literal(dst, "null"); !ok {
				return dst, false
			}
		default:
			return dst, false
		}

		// We've completed a value, so now we close any containers that it
		// was the last member of, and then expect another member of the
		// innermost one that remains.
		for {
			skipSpace()
			if len(containers) == 0 {
				// Nothing can follow the outermost value.
				return dst, i == len(src)
			}
			if i >= len(src) {
				return dst, false
			}
			inObject := containers[len(containers)-1]
			c := src[i]
			if c == ',' {
				i++
				dst = append(dst, ',')
				dst = newline(dst, len(containers))
				if !inObject {
					break
				}
				skipSpace()
				if i >= len(src) || src[i] != '"' {
					return dst, false
				}
				if dst, ok = str(dst); !ok {
					return dst, false
				}
				skipSpace()
				if i >= len(src) || src[i] != ':' {
					return dst, false
				}
				i++
				dst = append(dst, ':', ' ')
				break
			}
			if (inObject && c != '}') || (!inObject && c != ']') {
				return dst, false
			}
			i++
			containers = containers[:len(containers)-1]
			dst = newline(dst, len(containers))
			dst = append(dst, c)
		}
	}
}

// jsonStringSpecial is true for each byte that needs attention when it
// appears inside a JSON string, rather than just being copied.
var jsonStringSpecial = func() (special [256]bool) {
	for c := range 0x20 {
		special[c] = true
	}
	for _, c := range []byte{'"', '\\', '<', '>', '&', 0xE2} {
		special[c] = true
	}
	return special
}()
