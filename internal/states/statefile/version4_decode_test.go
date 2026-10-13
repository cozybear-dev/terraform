// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// testStateV4Src returns the source of a state file that is large enough
// to be eligible for parallel decoding.
func testStateV4Src(t testing.TB, resources int) []byte {
	t.Helper()
	sV4 := testStateV4(rand.New(rand.NewSource(2)), resources)
	for i := range sV4.Resources {
		rs := &sV4.Resources[i]
		rs.Module = ""
		rs.EachMode = ""
		for j := range rs.Instances {
			// The generated instances aren't all distinct, and use legacy
			// forms that the reader would complain about.
			is := &rs.Instances[j]
			is.IndexKey = j
			is.Deposed = ""
			is.AttributesFlat = nil
			is.AttributesRaw = json.RawMessage(`{"id":"` + strings.Repeat("x", 200) + `","escaped":"\"}]\\"}`)
		}
	}
	var buf bytes.Buffer
	if err := encodeStateV4(sV4, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < parallelDecodeMinSize {
		t.Fatalf("test state is only %d bytes", buf.Len())
	}
	return buf.Bytes()
}

func TestReadStateV4Parallel(t *testing.T) {
	src := testStateV4Src(t, 500)

	got, ok := readStateV4Parallel(src)
	if !ok {
		t.Fatal("parallel reader rejected a valid state file")
	}
	want, diags := readStateV4(src)
	if diags.HasErrors() {
		t.Fatal(diags.Err())
	}

	// Comparing the serialized forms covers everything that can be
	// represented in a state file.
	var gotBuf, wantBuf bytes.Buffer
	if err := Write(got, &gotBuf); err != nil {
		t.Fatal(err)
	}
	if err := Write(want, &wantBuf); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(wantBuf.String(), gotBuf.String()); diff != "" {
		t.Fatalf("wrong result\n%s", diff)
	}
	if !got.State.Equal(want.State) {
		t.Fatal("states are not equal")
	}

	// Also with all of the insignificant whitespace removed.
	var compact bytes.Buffer
	if err := json.Compact(&compact, src); err != nil {
		t.Fatal(err)
	}
	got, ok = readStateV4Parallel(compact.Bytes())
	if !ok {
		t.Fatal("parallel reader rejected a valid compact state file")
	}
	gotBuf.Reset()
	if err := Write(got, &gotBuf); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(wantBuf.String(), gotBuf.String()); diff != "" {
		t.Fatalf("wrong result after compacting\n%s", diff)
	}
}

func TestReadStateV4Parallel_fallback(t *testing.T) {
	src := string(testStateV4Src(t, 500))
	if _, err := Read(strings.NewReader(src)); err != nil {
		t.Fatalf("test state is invalid: %s", err)
	}

	replaceOnce := func(old, new string) string {
		t.Helper()
		if strings.Count(src, old) < 1 {
			t.Fatalf("test state doesn't contain %q", old)
		}
		return strings.Replace(src, old, new, 1)
	}
	replaceLast := func(old, new string) string {
		t.Helper()
		i := strings.LastIndex(src, old)
		if i < 0 {
			t.Fatalf("test state doesn't contain %q", old)
		}
		return src[:i] + new + src[i+len(old):]
	}

	tests := map[string]struct {
		src     string
		wantErr bool
	}{
		"truncated":                {src[:len(src)/2], true},
		"trailing garbage":         {src + "{}", true},
		"wrong version":            {replaceOnce(`"version": 4`, `"version": 5`), true},
		"missing element comma":    {replaceOnce("    },\n    {", "    }\n    {"), true},
		"trailing element comma":   {replaceLast("    }\n  ],", "    },\n  ],"), true},
		"wrong array close":        {replaceLast("    }\n  ],", "    }\n  },"), true},
		"wrong element close":      {replaceOnce("    },\n    {", "    ],\n    {"), true},
		"invalid element":          {replaceOnce(`"mode": "managed"`, `"mode": managed`), true},
		"invalid outside elements": {replaceOnce(`"serial": `, `"serial": x`), true},
		"non-object element":       {replaceLast("\"resources\": [\n", "\"resources\": [\n    null,\n"), true},
		"capitalized property":     {replaceLast(`"resources": [`, `"Resources": [`), false},
		"escaped property":         {replaceLast(`"resources": [`, `"resource`+"\\"+`u0073": [`), false},
		"duplicate property":       {replaceLast(`"check_results"`, `"resources": [], "check_results"`), false},
		"unusable content":         {replaceOnce(`"mode": "managed"`, `"mode": "bogus"`), true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, ok := readStateV4Parallel([]byte(test.src)); ok {
				t.Fatal("parallel reader accepted the input")
			}

			// The overall result must be whatever the sequential reader
			// would have produced.
			_, err := Read(strings.NewReader(test.src))
			if (err != nil) != test.wantErr {
				t.Fatalf("wrong error: %v", err)
			}
		})
	}
}

func BenchmarkReadStateV4(b *testing.B) {
	src := testStateV4Src(b, 20000)
	b.Run("parallel", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		for i := 0; i < b.N; i++ {
			if _, ok := readStateV4Parallel(src); !ok {
				b.Fatal("failed")
			}
		}
	})
	b.Run("sequential", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		for i := 0; i < b.N; i++ {
			if _, diags := sniffJSONStateVersion(src); diags.HasErrors() {
				b.Fatal(diags.Err())
			}
			if _, diags := readStateV4(src); diags.HasErrors() {
				b.Fatal(diags.Err())
			}
		}
	})
}
