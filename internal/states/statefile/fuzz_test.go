// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The fuzz targets in this file compare each hand-written routine with the
// standard library routine it stands in for. Each routine is allowed to
// decline an input, which leaves it to the standard library, so the property
// is always that whatever it does accept, it treats identically.
//
// Run one with, for example:
//
//	go test ./internal/states/statefile -run '^$' -fuzz FuzzAppendIndentedJSON -fuzztime 60s

var fuzzJSONSeeds = []string{
	`{}`, `[]`, `null`, `true`, `0`, `-0`, `1.5e+10`, `""`, `"a\"b\\c\/d\b\f\n\r\t"`, `"é😀"`,
	`{"a":1,"b":[true,false,null],"c":{"d":"<&>"}}`, ` { "a" : [ 1 , 2 ] } `, `[[[[[[]]]]]]`,
	"\"  \"", `"\ud800"`, `{"a":{}, "a":[]}`, `[1,]`, `{"a"}`, `01`, `1.`, `.5`, `tru`, `"unterminated`,
	"{\n  \"id\": \"x\",\n  \"tags\": {\n    \"k\": \"v\"\n  }\n}", "\"\xff\xfe\"", "\"\xe2\x80\xa8\"", "\"\xe2\x80\"",
}

func FuzzAppendIndentedJSON(f *testing.F) {
	for _, seed := range fuzzJSONSeeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		got, ok := appendIndentedJSON([]byte("prefix"), src, "          ", "  ")
		if !ok {
			return
		}
		want, err := json.MarshalIndent(json.RawMessage(src), "          ", "  ")
		if err != nil {
			t.Fatalf("accepted %q, which the standard library rejects: %s", src, err)
		}
		if !bytes.Equal(got, append([]byte("prefix"), want...)) {
			t.Fatalf("for %q\n got %q\nwant %q", src, got[len("prefix"):], want)
		}
	})
}

func FuzzValidJSONValue(f *testing.F) {
	for _, seed := range fuzzJSONSeeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		end, ok := validJSONValue(src, 0)
		if ok && !json.Valid(src[:end]) {
			t.Fatalf("accepted %q as a value, which the standard library rejects", src[:end])
		}
		if ok && end < len(src) {
			// Nothing that could continue the value may follow it.
			if next := src[end]; json.Valid(src[:end+1]) && next != ' ' && next != '\t' && next != '\r' && next != '\n' {
				t.Fatalf("stopped at %d in %q, but the value continues", end, src)
			}
		}
		if !ok && json.Valid(src) && bytes.Count(src, []byte("[")) < 900 && bytes.Count(src, []byte("{")) < 900 {
			t.Fatalf("rejected %q, which is valid", src)
		}
	})
}

func FuzzUnmarshalResourceV4(f *testing.F) {
	for _, seed := range []string{
		`{"mode":"managed","type":"t","name":"n","provider":"provider[\"registry.terraform.io/hashicorp/fp\"]","instances":[]}`,
		`{"module":"module.a[\"k\"]","mode":"data","type":"t","name":"n","each":"map","provider":"p","instances":[{"index_key":"a","schema_version":1,"attributes":{"id":"x"},"sensitive_attributes":[],"private":"YQ==","dependencies":["a.b"],"create_before_destroy":true}]}`,
		`{"mode":"managed","type":"t","name":"n","provider":"p","instances":[{"index_key":0,"status":"tainted","deposed":"abcd1234","schema_version":0,"attributes":null,"identity_schema_version":2,"identity":{"a":1},"dependencies":[]}]}`,
		`{"mode":"managed","type":"t","name":"n","provider":"p","instances":[{"schema_version":0,"attributes_flat":{"a":"b"}}]}`,
		`{"mode":"managed","type":"t","name":"n","provider":"p","instances":[{"schema_version":1e0,"dependencies":null}],"instances":[]}`,
		`{"Mode":"managed","TYPE":"t","name":"n","provider":"p","instances":[{"index_key":-1}]}`,
		`{"mode":"managed","type":"<&>","name":"n<","provider":"p","instances":[{"index_key":123456789012345678,"private":"!"}]}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		var got resourceStateV4
		if !unmarshalResourceV4(src, &got) {
			return
		}
		var want resourceStateV4
		if err := json.Unmarshal(src, &want); err != nil {
			t.Fatalf("accepted %q, which the standard library rejects: %s", src, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("for %q\n got %#v\nwant %#v", src, got, want)
		}
	})
}

func FuzzParseSimpleStringArray(f *testing.F) {
	for _, seed := range []string{`["a","b"]`, `[]`, `[""]`, ` [ "a.b" , "module.c.d[0]" ] `, `["a\"b"]`, `["é"]`, `["a",1]`, `["a"`, `"a"`, `["a",]`, `null`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		got, ok := parseSimpleStringArray(src)
		if !ok {
			return
		}
		var want []string
		if err := json.Unmarshal(src, &want); err != nil {
			t.Fatalf("accepted %q, which the standard library rejects: %s", src, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("for %q\n got %q\nwant %q", src, got, want)
		}
	})
}
