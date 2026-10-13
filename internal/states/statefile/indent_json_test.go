// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform/internal/addrs"
)

// appendIndentedJSON must produce exactly what the standard library does
// for everything it accepts, and must accept nothing that the standard
// library rejects.
func TestAppendIndentedJSON(t *testing.T) {
	bs := string(rune(92)) // a backslash
	check := func(t *testing.T, src []byte, mustAccept bool) {
		t.Helper()
		for _, style := range [][2]string{{"          ", "  "}, {"", "\t"}, {"", ""}} {
			prefix, indent := style[0], style[1]
			want, wantErr := json.MarshalIndent(json.RawMessage(src), prefix, indent)
			got, ok := appendIndentedJSON([]byte("existing"), src, prefix, indent)
			switch {
			case ok && wantErr != nil:
				t.Fatalf("accepted input that the standard library rejects\ninput: %q\nerror: %s", src, wantErr)
			case ok && !bytes.Equal(got, append([]byte("existing"), want...)):
				t.Fatalf("wrong result\ninput: %q\ngot:   %q\nwant:  %q", src, got, want)
			case !ok && mustAccept:
				t.Fatalf("rejected input that it should accept\ninput: %q", src)
			case !ok && wantErr == nil && len(src) < 200:
				// It's allowed to reject valid input, but it's not expected
				// to for anything other than excessive nesting.
				t.Errorf("rejected valid input\ninput: %q", src)
			}
		}
	}

	valid := []string{
		`{}`, `[]`, `""`, `0`, `-0`, `null`, `true`, `false`, `1.5e+10`, `-12.0E-3`, `1e5`, `"a"`,
		` { } `, "\t[\r\n]\n", `[[]]`, `[{}]`, `{"a":{}}`, `{"a":[]}`, `[[],[]]`, `[{},{}]`,
		`{"a":1}`, `{"a":1,"b":[1,2,{"c":null}],"d":"e"}`, `[1, "two", [3], {"four": 4}]`,
		`{ "a" : [ 1 , 2 ] , "b" : { "c" : true , "d" : false } }`,
		`"<script>&amp;</script>"`, `{"<&>":"<&>"}`,
		`"` + "\u2028" + `x` + "\u2029" + `"`, `"` + "\u2027\u202aâ€" + `"`, "\"\xe2\x80\"", "\"\xe2\"", "\"\xe2\x80\xa7\xe2\x81\xa8\"",
		`"` + bs + `u2028` + bs + `u003c` + bs + `uABcd"`,
		`"` + bs + `"` + bs + bs + bs + `/` + bs + `b` + bs + `f` + bs + `n` + bs + `r` + bs + `t"`,
		`"` + bs + bs + `"`, `["` + bs + bs + `","` + bs + `""]`,
		`"spaces  and ` + bs + `ttabs : , [ ] { }"`,
		"\"invalid utf-8 \xff\xfe\xc0\"", `"héllo wörld ☃ 🎉"`,
		`[0,-1,10,1.0,0.1,1e1,1E-1,1e+1,123456789012345678901234567890.123456789e-100]`,
		`{"":""}`, `{"a":"b","a":"c"}`,
		strings.Repeat("[", 500) + strings.Repeat("]", 500),
		strings.Repeat(`{"a":`, 500) + `1` + strings.Repeat("}", 500),
	}
	for _, src := range valid {
		t.Run("valid", func(t *testing.T) {
			check(t, []byte(src), true)
		})
	}

	invalid := []string{
		``, ` `, `{`, `}`, `[`, `]`, `{]`, `[}`, `[1,]`, `{"a":1,}`, `{,}`, `[,]`, `{"a"}`, `{"a":}`, `{a:1}`, `{1:1}`,
		`{"a" 1}`, `{"a":1 "b":2}`, `[1 2]`, `1 2`, `{} {}`, `[]]`, `{}}`, `"a`, `"a` + bs + `"`, `"` + bs + `x"`, `"` + bs + `u12"`,
		`"` + bs + `u12g4"`, `"` + bs, "\"a\nb\"", "\"a\x00b\"", "\"\x1f\"", `01`, `-`, `--1`, `+1`, `1.`, `.5`, `1e`, `1e+`, `1.e1`,
		`0x10`, `1_000`, `tru`, `truee`, `nul`, `falsy`, `True`, `NaN`, `Infinity`, `'a'`, `[1,2`, `{"a":[1,2}`, `{"a":{"b":1]}`,
		`<`, `[<]`, "\xe2\x80\xa8", `/* c */ 1`, `1 // c`, `{"a":1}x`, `x{"a":1}`, "\xef\xbb\xbf{}", "{}\x00", "\v1", "\f1",
		strings.Repeat("[", 1001) + strings.Repeat("]", 1001), strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
	}
	for _, src := range invalid {
		t.Run("invalid", func(t *testing.T) {
			check(t, []byte(src), false)
		})
	}

	// Random documents, with random formatting and random corruption.
	rnd := rand.New(rand.NewSource(1))
	var gen func(depth int) string
	space := func() string {
		return []string{"", "", "", " ", "\n", "\t", "\r\n", "  \n  "}[rnd.Intn(8)]
	}
	genString := func() string {
		var sb strings.Builder
		sb.WriteByte('"')
		for range rnd.Intn(12) {
			switch rnd.Intn(16) {
			case 0:
				sb.WriteString(bs + string((`"` + bs + `/bfnrt`)[rnd.Intn(8)]))
			case 1:
				fmt.Fprintf(&sb, bs+"u%04x", rnd.Intn(0x10000))
			case 2:
				sb.WriteString([]string{"<", ">", "&", "\u2028", "\u2029", "\u2027", "é", "☃", "🎉", "\xe2", "\xe2\x80", "\xff"}[rnd.Intn(12)])
			case 3:
				sb.WriteString([]string{" ", ":", ",", "[", "]", "{", "}", "'"}[rnd.Intn(8)])
			default:
				sb.WriteByte(byte('a' + rnd.Intn(26)))
			}
		}
		sb.WriteByte('"')
		return sb.String()
	}
	gen = func(depth int) string {
		kind := rnd.Intn(8)
		if depth > 5 && kind < 2 {
			kind += 2
		}
		switch kind {
		case 0:
			var sb strings.Builder
			sb.WriteString("{" + space())
			for i := range rnd.Intn(5) {
				if i > 0 {
					sb.WriteString(space() + "," + space())
				}
				sb.WriteString(genString() + space() + ":" + space() + gen(depth+1))
			}
			sb.WriteString(space() + "}")
			return sb.String()
		case 1:
			var sb strings.Builder
			sb.WriteString("[" + space())
			for i := range rnd.Intn(5) {
				if i > 0 {
					sb.WriteString(space() + "," + space())
				}
				sb.WriteString(gen(depth + 1))
			}
			sb.WriteString(space() + "]")
			return sb.String()
		case 2, 3:
			return genString()
		case 4:
			return []string{"0", "-0", "1", "-17", "3.25", "1e9", "-2.5E-7", "1E+2", "100000000000000000000000000001"}[rnd.Intn(9)]
		case 5:
			return fmt.Sprint(rnd.Int63())
		default:
			return []string{"true", "false", "null"}[rnd.Intn(3)]
		}
	}
	for range 5000 {
		src := space() + gen(0) + space()
		check(t, []byte(src), true)

		// Corrupting a valid document usually makes it invalid, and
		// whichever it is we must agree with the standard library.
		for range 4 {
			corrupt := []byte(src)
			if len(corrupt) == 0 {
				continue
			}
			at := rnd.Intn(len(corrupt))
			switch rnd.Intn(4) {
			case 0:
				corrupt = append(corrupt[:at], corrupt[at+1:]...)
			case 1:
				replacements := `{}[]",:` + bs + ` 019.eE-+tfnu<&` + "\x00\n\xe2\x80\xa8"
				corrupt[at] = replacements[rnd.Intn(len(replacements))]
			case 2:
				corrupt = corrupt[:at]
			default:
				corrupt = append(corrupt[:at:at], append([]byte{src[rnd.Intn(len(src))]}, corrupt[at:]...)...)
			}
			check(t, corrupt, false)
		}
	}
}

// appendIndentedSimpleStrings must produce exactly what the standard library
// does for everything it accepts.
func TestAppendIndentedSimpleStrings(t *testing.T) {
	bs := string(rune(92)) // a backslash
	tests := map[string]struct {
		strs   []string
		accept bool
	}{
		"empty":            {[]string{}, false},
		"nil":              {nil, false},
		"one":              {[]string{"test_thing.a"}, true},
		"several":          {[]string{"test_thing.a", "module.m.test_thing.b", "data.test_thing.c", ""}, true},
		"all printable":    {[]string{" !#$%'()*+,-./0123456789:;=?@ABCXYZ[]^_`abcxyz{|}~"}, true},
		"quote":            {[]string{"a", `module.m["k"].test_thing.a`}, false},
		"backslash":        {[]string{"a" + bs + "b"}, false},
		"html":             {[]string{"a<b"}, false},
		"html 2":           {[]string{"a>b"}, false},
		"html 3":           {[]string{"a&b"}, false},
		"control":          {[]string{"a\tb"}, false},
		"newline":          {[]string{"a\nb"}, false},
		"delete":           {[]string{"a\x7fb"}, false},
		"non-ASCII":        {[]string{"café"}, false},
		"invalid UTF-8":    {[]string{"a\xffb"}, false},
		"line separator":   {[]string{"a\u2028b"}, false},
		"unusual at start": {[]string{`"`, "a"}, false},
		"unusual at end":   {[]string{"a", "b", "c<"}, false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for _, style := range [][2]string{{"          ", "  "}, {"", "\t"}, {"", ""}} {
				prefix, indent := style[0], style[1]
				got, ok := appendIndentedSimpleStrings([]byte("existing"), test.strs, prefix, indent)
				if ok != test.accept {
					t.Fatalf("accepted = %t; want %t", ok, test.accept)
				}
				if !ok {
					continue
				}
				want, err := json.MarshalIndent(test.strs, prefix, indent)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, append([]byte("existing"), want...)) {
					t.Fatalf("wrong result\ngot:  %q\nwant: %q", got, want)
				}
			}
		})
	}
}

// parseSimpleStringArray must give exactly what the standard library gives
// for everything it accepts.
func TestParseSimpleStringArray(t *testing.T) {
	bs := string(rune(92)) // a backslash
	tests := map[string]bool{
		`["a"]`: true,
		`["test_thing.a","module.m.test_thing.b"]`:              true,
		" [ \"a\" ,\n\t\"b\" , \"\" ] \n":                       true,
		"[\n            \"a\",\n            \"b\"\n          ]": true,
		`[" !#$%'()*+,-./09:;<=>?@AZ[]^_az{|}~&"]`:              true,
		`[]`:                   false,
		`[ ]`:                  false,
		`null`:                 false,
		`"a"`:                  false,
		`{}`:                   false,
		``:                     false,
		`[`:                    false,
		`["a"`:                 false,
		`["a",]`:               false,
		`[,"a"]`:               false,
		`["a" "b"]`:            false,
		`["a"]x`:               false,
		`["a"],`:               false,
		`["a`:                  false,
		`[1]`:                  false,
		`[null]`:               false,
		`["a",null]`:           false,
		`[["a"]]`:              false,
		`["a` + bs + `"b"]`:    false,
		`["a` + bs + `nb"]`:    false,
		`["a` + bs + `u0041"]`: false,
		`["café"]`:             false,
		"[\"a\x7f\"]":          false,
		"[\"a\tb\"]":           false,
		"[\"a\nb\"]":           false,
		"[\"a\xff\"]":          false,
	}
	for src, accept := range tests {
		got, ok := parseSimpleStringArray([]byte(src))
		if ok != accept {
			t.Fatalf("accepted = %t; want %t\ninput: %q", ok, accept, src)
		}
		if !ok {
			continue
		}
		var want []string
		if err := json.Unmarshal([]byte(src), &want); err != nil {
			t.Fatalf("accepted input that the standard library rejects\ninput: %q\nerror: %s", src, err)
		}
		if len(got) != len(want) {
			t.Fatalf("wrong result\ninput: %q\ngot:  %#v\nwant: %#v", src, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("wrong result\ninput: %q\ngot:  %#v\nwant: %#v", src, got, want)
			}
		}
	}
}

// appendIndentedSimpleAddrs must produce exactly what the standard library
// does for the string representations of the addresses it accepts.
func TestAppendIndentedSimpleAddrs(t *testing.T) {
	addr := func(module []string, mode addrs.ResourceMode, typeName, name string) addrs.ConfigResource {
		return addrs.ConfigResource{
			Module:   addrs.Module(module),
			Resource: addrs.Resource{Mode: mode, Type: typeName, Name: name},
		}
	}
	a := addr(nil, addrs.ManagedResourceMode, "test_thing", "a")
	b := addr([]string{"m", "n"}, addrs.DataResourceMode, "test_thing", "b")
	tests := map[string]struct {
		deps   []addrs.ConfigResource
		accept bool
	}{
		"empty":             {nil, false},
		"one":               {[]addrs.ConfigResource{a}, true},
		"several":           {[]addrs.ConfigResource{a, b, a, addr([]string{"x"}, addrs.EphemeralResourceMode, "t", "n")}, true},
		"many":              {slices.Repeat([]addrs.ConfigResource{a, b}, 500), true},
		"unusual name":      {[]addrs.ConfigResource{a, addr(nil, addrs.ManagedResourceMode, "test_thing", "café")}, false},
		"unusual module":    {[]addrs.ConfigResource{addr([]string{`"`}, addrs.ManagedResourceMode, "test_thing", "a"), a}, false},
		"invalid mode":      {[]addrs.ConfigResource{addr(nil, addrs.InvalidResourceMode, "test_thing", "a")}, false},
		"needs html escape": {[]addrs.ConfigResource{addr(nil, addrs.ManagedResourceMode, "a&b", "a")}, false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for _, style := range [][2]string{{"          ", "  "}, {"", "\t"}, {"", ""}} {
				prefix, indent := style[0], style[1]
				got, ok := appendIndentedSimpleAddrs([]byte("existing"), test.deps, prefix, indent)
				if ok != test.accept {
					t.Fatalf("accepted = %t; want %t", ok, test.accept)
				}
				strs := make([]string, len(test.deps))
				for i, dep := range test.deps {
					strs[i] = dep.String()
				}
				// It must agree with the equivalent for strings about
				// what's acceptable, and with the standard library about
				// the result.
				if _, strsOK := appendIndentedSimpleStrings(nil, strs, prefix, indent); strsOK != ok {
					t.Fatalf("accepted = %t, but for the same addresses as strings it's %t", ok, strsOK)
				}
				if !ok {
					continue
				}
				want, err := json.MarshalIndent(strs, prefix, indent)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, append([]byte("existing"), want...)) {
					t.Fatalf("wrong result\ngot:  %q\nwant: %q", got, want)
				}
			}
		})
	}
}
