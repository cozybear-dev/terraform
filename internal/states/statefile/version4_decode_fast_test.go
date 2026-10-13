// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// unmarshalResourceV4 must give exactly the result that the standard library
// gives for everything it accepts, and must accept nothing that the standard
// library rejects.
func TestUnmarshalResourceV4(t *testing.T) {
	check := func(t *testing.T, src []byte, mustAccept bool) {
		t.Helper()
		var want resourceStateV4
		wantErr := json.Unmarshal(src, &want)
		var got resourceStateV4
		ok := unmarshalResourceV4(src, &got)
		switch {
		case ok && wantErr != nil:
			t.Fatalf("accepted input that the standard library rejects\ninput: %s\nerror: %s", src, wantErr)
		case ok && !reflect.DeepEqual(got, want):
			t.Fatalf("wrong result\ninput: %s\ngot:   %#v\nwant:  %#v", src, got, want)
		case !ok && mustAccept:
			t.Fatalf("rejected input that it should accept\ninput: %s", src)
		}
	}

	bs := string(rune(92)) // a backslash
	fixed := map[string]bool{
		`{}`:                 true,
		` { } `:              true,
		`{"instances":[]}`:   true,
		`{"instances":null}`: false,
		`{"instances":[{}]}`: true,
		`{"mode":"managed","type":"t","name":"n","provider":"p","instances":[{"schema_version":0,"attributes":{"id":"x"}}]}`:    true,
		`{"module":"module.a","mode":"data","each":"map","instances":[{"index_key":"k","schema_version":3,"attributes":null}]}`: true,
		`{"instances":[{"index_key":0},{"index_key":12},{"index_key":999999999999999}]}`:                                        true,
		`{"instances":[{"index_key":1234567890123456}]}`:                                                                        false,
		`{"instances":[{"index_key":-1}]}`:   false,
		`{"instances":[{"index_key":1.5}]}`:  false,
		`{"instances":[{"index_key":1e3}]}`:  false,
		`{"instances":[{"index_key":01}]}`:   false,
		`{"instances":[{"index_key":null}]}`: false,
		`{"instances":[{"index_key":"é"}]}`:  false,
		`{"provider":"provider[` + bs + `"registry.terraform.io/hashicorp/test` + bs + `"]","module":"module.m[` + bs + `"` + bs + `u003c` + bs + `u0026` + bs + `u003E` + bs + bs + bs + `/` + bs + `"]"}`: true,
		`{"instances":[{"index_key":"` + bs + `u00e9"}]}`:                                false,
		`{"instances":[{"index_key":"` + bs + `u001f"}]}`:                                false,
		`{"instances":[{"index_key":"` + bs + `u007f"}]}`:                                false,
		`{"instances":[{"index_key":"` + bs + `u0041b"}]}`:                               true,
		`{"instances":[{"index_key":"` + bs + `u00"}]}`:                                  false,
		`{"instances":[{"index_key":"a` + bs + `"}]}`:                                    false,
		`{"mo` + bs + `u0064e":"managed"}`:                                               true,
		`{"instances":[{"index_key":"a` + bs + `nb"}]}`:                                  false,
		`{"instances":[{"schema_version":1.0}]}`:                                         false,
		`{"instances":[{"schema_version":"1"}]}`:                                         false,
		`{"instances":[{"private":"cHJpdmF0ZQ=="}]}`:                                     true,
		`{"instances":[{"private":""}]}`:                                                 true,
		`{"instances":[{"private":"not base64!"}]}`:                                      false,
		`{"instances":[{"private":null}]}`:                                               false,
		`{"instances":[{"dependencies":["a","b"]}]}`:                                     true,
		`{"instances":[{"dependencies":[]}]}`:                                            true,
		`{"instances":[{"dependencies":null}]}`:                                          false,
		`{"instances":[{"dependencies":"unusual"}]}`:                                     true,
		`{"instances":[{"create_before_destroy":true},{"create_before_destroy":false}]}`: true,
		`{"instances":[{"create_before_destroy":null}]}`:                                 false,
		`{"instances":[{"attributes_flat":{"id":"x"}}]}`:                                 false,
		`{"instances":[{"attributes":{"a":1},"attributes":{"a":2}}]}`:                    false,
		`{"instances":[{"Attributes":{"a":1}}]}`:                                         false,
		`{"instances":[{"unknown":1}]}`:                                                  false,
		`{"unknown":1}`:                                                                  false,
		`{"MODE":"managed"}`:                                                             false,
		`{"mode":"managed","mode":"data"}`:                                               false,
		`{"instances":[{"sensitive_attributes":[[{"type":"get_attr","value":"id"}]],"identity_schema_version":2,"identity":{"id":"i"},"status":"tainted","deposed":"00000001"}]}`: true,
		`{"instances":[{"attributes":{"nested":[1,2.5e-3,true,false,null,[],{},"<&>` + bs + `u2028` + bs + `"é"]}}]}`:                                                             true,
		`{"instances":[{"attributes":  {"a" : [ 1 , 2 ] }  } ] }`:                                                                                                                 true,
		// Invalid in various ways.
		``:                    false,
		`[]`:                  false,
		`{"instances":[{}]}x`: false,
		`{"instances":[{},]}`: false,
		`{"instances":[{"attributes":{invalid}}]}`:            false,
		`{"instances":[{"attributes":{"a":01}}]}`:             false,
		`{"instances":[{"attributes":{"a":"` + "\n" + `"}}]}`: false,
		`{"instances":[{"attributes":{"a":1,}}]}`:             false,
		`{"instances":[{"attributes":}]}`:                     false,
		`{"instances":[{"attributes":{"a":tru}}]}`:            false,
		`{"instances":[{"schema_version":1,}]}`:               false,
		`{"mode":"managed",}`:                                 false,
		`{"mode":"managed" "type":"t"}`:                       false,
		`{"mode"}`:                                            false,
		`{"instances":[{"attributes":` + strings.Repeat("[", 2000) + strings.Repeat("]", 2000) + `}]}`: false,
	}
	for src, mustAccept := range fixed {
		check(t, []byte(src), mustAccept)
	}

	// Resources in the form we write them, and corrupted versions of those.
	rnd := rand.New(rand.NewSource(3))
	sV4 := testStateV4(rnd, 600)
	accepted, total := 0, 0
	for i := range sV4.Resources {
		rs := &sV4.Resources[i]
		indented, err := json.MarshalIndent(rs, "    ", "  ")
		if err != nil {
			t.Fatal(err)
		}
		compact, err := json.Marshal(rs)
		if err != nil {
			t.Fatal(err)
		}
		for _, src := range [][]byte{indented, compact} {
			unusual := false
			for _, is := range rs.Instances {
				if is.AttributesFlat != nil {
					unusual = true
				}
			}
			total++
			var got resourceStateV4
			if unmarshalResourceV4(src, &got) {
				accepted++
			}
			check(t, src, !unusual && rs.Instances != nil)

			for range 6 {
				corrupt := bytes.Clone(src)
				at := rnd.Intn(len(corrupt))
				switch rnd.Intn(4) {
				case 0:
					corrupt = append(corrupt[:at], corrupt[at+1:]...)
				case 1:
					replacements := `{}[]",:` + bs + ` 019.eE-+tfnuA` + "\x00\n\xe2"
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
	if accepted < total/2 {
		t.Fatalf("accepted only %d of %d resources in the form we write them", accepted, total)
	}
}

// validJSONValue must agree with the standard library about what's valid,
// except that it's allowed to reject deeply nested values.
func TestValidJSONValue(t *testing.T) {
	bs := string(rune(92)) // a backslash
	check := func(src string) {
		t.Helper()
		want := json.Valid([]byte(src))
		end, ok := validJSONValue([]byte(src), 0)
		// Whitespace is allowed before a value, but what follows it is
		// for the caller to deal with.
		got := ok && len(strings.TrimRight(src[end:], " \t\r\n")) == 0
		if got != want {
			t.Fatalf("wrong result %t; want %t\ninput: %q", got, want, src)
		}
		if ok {
			// It must stop exactly at the end of the value.
			if !json.Valid([]byte(src[:end])) {
				t.Fatalf("reports that an invalid value ends at %d\ninput: %q", end, src)
			}
			if end > 0 && strings.ContainsRune(" \t\r\n", rune(src[end-1])) {
				t.Fatalf("includes trailing whitespace in the value\ninput: %q", src)
			}
		}
	}
	for _, src := range []string{
		`{}`, `[]`, `""`, `0`, `-0`, `null`, `true`, `false`, `1.5e+10`, `-12.0E-3`, ` { } `, "\t[\r\n]\n", `[[]]`, `[{}]`, `{"a":{}}`,
		`{"a":1,"b":[1,2,{"c":null}],"d":"e"}`, `[1, "two", [3], {"four": 4}]`, `"<&>"`, `"` + bs + `u12aB` + bs + `n` + bs + bs + bs + `""`,
		"\"\xff\xfe\"", `"héllo"`,
		``, ` `, `{`, `}`, `[`, `]`, `{]`, `[}`, `[1,]`, `{"a":1,}`, `{,}`, `[,]`, `{"a"}`, `{"a":}`, `{a:1}`, `{1:1}`, `{"a" 1}`, `[1 2]`, `1 2`,
		`"a`, `"` + bs + `x"`, `"` + bs + `u12"`, `"` + bs + `u12g4"`, `"` + bs, "\"a\nb\"", "\"a\x00b\"", `01`, `-`, `--1`, `+1`, `1.`, `.5`, `1e`, `1e+`,
		`tru`, `truee`, `nul`, `True`, `NaN`, `'a'`, `[1,2`, `{"a":[1,2}`, `{"a":{"b":1]}`, `{"a":1}x`, `{} {}`, `[]]`,
	} {
		check(src)
	}
}

// Finding the elements of an array concurrently must give exactly what
// finding them sequentially gives, including when the places it guesses
// that elements start are wrong.
func TestSplitJSONObjectArray(t *testing.T) {
	defer func(old int) { parallelSplitMinSize = old }(parallelSplitMinSize)
	parallelSplitMinSize = 1

	sV4 := testStateV4(rand.New(rand.NewSource(4)), 400)
	docs := map[string][]byte{}
	for name, indent := range map[string]string{"standard": "  ", "one space": " ", "four spaces": "    ", "tabs": "\t"} {
		src, err := json.MarshalIndent(sV4.Resources, "  ", indent)
		if err != nil {
			t.Fatal(err)
		}
		docs[name] = src
	}
	compact, err := json.Marshal(sV4.Resources)
	if err != nil {
		t.Fatal(err)
	}
	docs["compact"] = compact
	docs["empty"] = []byte(`[]`)
	docs["empty with space"] = []byte("[ \n ]")
	docs["one element"] = []byte(`[{"a":[{},{}]}]`)
	// What separates elements can also appear after the end of the array.
	docs["followed by another array"] = append(bytes.Clone(docs["standard"]), ",\n  \"more\": [\n    {\n      \"a\": 1\n    },\n    {\n      \"a\": 2\n    },\n    {\n      \"a\": 3\n    }\n  ]"...)
	docs["one element followed by another array"] = []byte("[\n    {\n      \"a\": 1\n    }\n  ],\n  \"more\": [\n    {\n      \"a\": 1\n    },\n    {\n      \"a\": 2\n    }\n  ]")
	// Invalid in ways that the scan is expected to notice.
	docs["trailing comma"] = []byte("[\n    {\n      \"a\": 1\n    },\n    {\n      \"a\": 2\n    },\n  ]")
	docs["not objects"] = []byte("[\n    {\n      \"a\": 1\n    },\n    {\n      \"a\": 2\n    },\n    3\n  ]")
	docs["unterminated"] = docs["standard"][:len(docs["standard"])-1]
	docs["truncated"] = docs["standard"][:len(docs["standard"])/2]
	docs["missing comma"] = bytes.Replace(docs["standard"], []byte("\n    },\n    {"), []byte("\n    }\n    {"), 1)

	for name, src := range docs {
		t.Run(name, func(t *testing.T) {
			// Leading content, as there is before the array in a state.
			for _, lead := range []string{"", `{"resources": `} {
				doc := append([]byte(lead), src...)
				at := len(lead)

				var want [][]byte
				wantEnd, wantOK := 0, true
				if first := skipJSONSpace(doc, at+1); first < len(doc) && doc[first] == ']' {
					wantEnd = first + 1
				} else {
					want, wantEnd, wantOK = scanJSONObjectArray(doc, first, -1)
				}

				for range 20 {
					got, gotEnd, gotOK := splitJSONObjectArray(doc, at)
					if gotOK != wantOK {
						t.Fatalf("succeeded = %t; want %t", gotOK, wantOK)
					}
					if !gotOK {
						continue
					}
					if gotEnd != wantEnd {
						t.Fatalf("wrong end %d; want %d", gotEnd, wantEnd)
					}
					if len(got) != len(want) {
						t.Fatalf("found %d elements; want %d", len(got), len(want))
					}
					for i := range got {
						if !bytes.Equal(got[i], want[i]) || (len(got[i]) != 0 && &got[i][0] != &want[i][0]) {
							t.Fatalf("wrong element %d\ngot:  %s\nwant: %s", i, got[i], want[i])
						}
					}
				}
				if wantOK && !json.Valid(doc[at:wantEnd]) {
					t.Fatalf("the array isn't valid JSON, so the test is wrong")
				}
				if strings.HasPrefix(name, "standard") || name == "compact" || name == "one space" {
					if !wantOK || len(want) != len(sV4.Resources) {
						t.Fatalf("found %d elements; want %d", len(want), len(sV4.Resources))
					}
				}
			}
		})
	}

	// A whole state in each of those forms must be read just as it is by
	// the sequential reader.
	standard := testStateV4Src(t, 1500)
	for name, indent := range map[string]string{"standard": "  ", "one space": " ", "four spaces": "    "} {
		t.Run("state "+name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := json.Indent(&buf, standard, "", indent); err != nil {
				t.Fatal(err)
			}
			src := buf.Bytes()

			got, ok := readStateV4Parallel(src)
			if !ok {
				t.Fatal("rejected a valid state file")
			}
			want, diags := readStateV4(src)
			if diags.HasErrors() {
				t.Fatal(diags.Err())
			}
			var gotBuf, wantBuf bytes.Buffer
			if err := Write(got, &gotBuf); err != nil {
				t.Fatal(err)
			}
			if err := Write(want, &wantBuf); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotBuf.Bytes(), wantBuf.Bytes()) {
				t.Fatal("states differ")
			}
		})
	}
}

// The source given to ReadBytes can be reused by the caller afterwards, so
// nothing in the result may refer to it, however it was decoded.
func TestReadBytesDoesNotRetainSource(t *testing.T) {
	standard := testStateV4Src(t, 800)
	var compact, oneSpace bytes.Buffer
	if err := json.Compact(&compact, standard); err != nil {
		t.Fatal(err)
	}
	if err := json.Indent(&oneSpace, standard, "", " "); err != nil {
		t.Fatal(err)
	}
	// A state that the concurrent reader declines, so that the sequential
	// one is covered too.
	small := testStateV4Src(t, 800)[:0]
	small = append(small, `{"version":4,"terraform_version":"1.2.3","serial":3,"lineage":"l","outputs":{"o":{"value":"v","type":"string"}},"resources":[{"mode":"managed","type":"test_thing","name":"a","provider":"provider[\"registry.terraform.io/hashicorp/test\"]","instances":[{"schema_version":0,"attributes":{"id":"x"},"private":"cHJpdmF0ZQ==","dependencies":["test_thing.b"],"sensitive_attributes":[[{"type":"get_attr","value":"id"}]]}]}]}`...)

	for name, src := range map[string][]byte{"standard": standard, "compact": compact.Bytes(), "one space": oneSpace.Bytes(), "small": small} {
		t.Run(name, func(t *testing.T) {
			write := func(f *File) []byte {
				var buf bytes.Buffer
				if err := Write(f, &buf); err != nil {
					t.Fatal(err)
				}
				return buf.Bytes()
			}
			reference, err := ReadBytes(bytes.Clone(src))
			if err != nil {
				t.Fatal(err)
			}
			want := write(reference)

			scratch := bytes.Clone(src)
			got, err := ReadBytes(scratch)
			if err != nil {
				t.Fatal(err)
			}
			for i := range scratch {
				scratch[i] = 'X'
			}
			if !bytes.Equal(write(got), want) {
				t.Fatal("the state changed when the source it was read from was overwritten")
			}

			// A recycled buffer is used for the next read of a similar
			// size, which must give the same result while leaving states
			// that were read earlier alone.
			if len(src) >= minRecycledReadBuffer {
				RecycleReadBuffer(scratch)
				again, err := Read(bytes.NewReader(src))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(write(again), want) {
					t.Fatal("wrong result when reading into a recycled buffer")
				}
				if !bytes.Equal(write(got), want) {
					t.Fatal("the state changed when the buffer it was read from was reused")
				}
			}
		})
	}
	if len(standard) < minRecycledReadBuffer {
		t.Fatalf("test state is only %d bytes, which is too small to be recycled", len(standard))
	}
}
