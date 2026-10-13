// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package gcs

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// versionedObject serves an object that can be stored compressed and that
// can be replaced by a newer generation part way through being read, which
// are the two situations in which reading it as several ranges could go
// wrong and which the other tests of that don't cover.
type versionedObject struct {
	mu sync.Mutex

	// generations has the stored content of each generation, of which the
	// last is the live one. Only the live one can be read unless keepOld.
	generations [][]byte
	encoding    string
	keepOld     bool

	// replaceAfter, if not zero, makes a new generation appear once that
	// many requests for a range have been answered.
	replaceAfter int
	replacement  []byte

	ranges int
}

func (o *versionedObject) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	live := int64(len(o.generations))
	generation := live
	if g := r.URL.Query().Get("generation"); g != "" {
		generation, _ = strconv.ParseInt(g, 10, 64)
	}
	if generation < 1 || generation > live || (generation != live && !o.keepOld) {
		o.mu.Unlock()
		http.Error(w, "no such generation", http.StatusNotFound)
		return
	}
	data := o.generations[generation-1]
	sum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
	crc := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, sum))
	md5sum := md5.Sum(data)

	if strings.Contains(r.URL.Path, "/o/") {
		o.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"bucket":"b","name":"state","size":"%d","generation":"%d","crc32c":%q,"md5Hash":%q,"contentEncoding":%q}`,
			len(data), generation, crc, base64.StdEncoding.EncodeToString(md5sum[:]), o.encoding)
		return
	}

	start, end, ranged := 0, len(data), false
	if spec := r.Header.Get("Range"); spec != "" {
		var first, last int
		if n, _ := fmt.Sscanf(spec, "bytes=%d-%d", &first, &last); n == 2 {
			start, end, ranged = first, min(last+1, len(data)), true
		}
	}
	if ranged {
		o.ranges++
		if o.replaceAfter != 0 && o.ranges == o.replaceAfter {
			o.generations = append(o.generations, o.replacement)
		}
	}
	o.mu.Unlock()

	w.Header().Set("X-Goog-Generation", strconv.FormatInt(generation, 10))
	w.Header().Set("X-Goog-Hash", "crc32c="+crc)
	if o.encoding != "" {
		w.Header().Set("Content-Encoding", o.encoding)
	}
	w.Header().Set("Content-Length", strconv.Itoa(end-start))
	if ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
	}
	w.Write(data[start:end])
}

func TestRemoteClientGet_downloadInPartsAudit(t *testing.T) {
	defer func(minSize, minPart int64, parts int) {
		parallelDownloadMinSize, parallelDownloadMinPart, parallelDownloadParts = minSize, minPart, parts
	}(parallelDownloadMinSize, parallelDownloadMinPart, parallelDownloadParts)
	parallelDownloadMinSize, parallelDownloadMinPart, parallelDownloadParts = 256<<10, 64<<10, 6

	// Not compressible, so that its compressed form is large enough to be
	// read in parts too.
	random := func(seed int64, n int) []byte {
		b := make([]byte, n)
		rand.New(rand.NewSource(seed)).Read(b)
		return b
	}
	first, second := random(1, 900<<10+17), random(2, 700<<10+3)
	compressed := func(data []byte) []byte {
		gz, err := gzipBytes(data)
		if err != nil {
			t.Fatal(err)
		}
		return gz
	}

	tests := map[string]struct {
		object       *versionedObject
		want         [][]byte // any one of these
		wantRevision []string
		wantRanges   bool
	}{
		"stored compressed": {
			object:       &versionedObject{generations: [][]byte{compressed(first)}, encoding: "gzip"},
			want:         [][]byte{first},
			wantRevision: []string{"1"},
			wantRanges:   true,
		},
		// With versioning off, the generation being read disappears, and
		// the newer one must be read instead, whole.
		"replaced while being read": {
			object:       &versionedObject{generations: [][]byte{first}, replaceAfter: 2, replacement: second},
			want:         [][]byte{second},
			wantRevision: []string{"2"},
			wantRanges:   true,
		},
		// With versioning on, the old generation can still be read, and
		// what's returned must be all of one generation or all of the
		// other, and say which.
		"replaced while being read, with versioning": {
			object:       &versionedObject{generations: [][]byte{first}, replaceAfter: 2, replacement: second, keepOld: true},
			want:         [][]byte{first, second},
			wantRevision: []string{"1", "2"},
			wantRanges:   true,
		},
		"replaced while being read, compressed": {
			object:       &versionedObject{generations: [][]byte{compressed(first)}, encoding: "gzip", replaceAfter: 3, replacement: compressed(second), keepOld: true},
			want:         [][]byte{first, second},
			wantRevision: []string{"1", "2"},
			wantRanges:   true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(test.object)
			defer server.Close()
			storageClient, err := storage.NewClient(context.Background(),
				option.WithEndpoint(server.URL+"/storage/v1/"),
				option.WithoutAuthentication(),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer storageClient.Close()
			client := &remoteClient{storageClient: storageClient, bucketName: "b", stateFilePath: "state"}

			payload, diags := client.Get()
			if diags.HasErrors() {
				t.Fatalf("unexpected error: %v", diags.Err())
			}
			matched := -1
			for i, want := range test.want {
				if bytes.Equal(payload.Data, want) {
					matched = i
				}
			}
			if matched < 0 {
				t.Fatalf("got %d bytes that aren't the content of any one generation of the object", len(payload.Data))
			}
			if payload.Revision != test.wantRevision[matched] {
				t.Errorf("got the content of generation %s with revision %q", test.wantRevision[matched], payload.Revision)
			}
			if got := test.object.ranges > 0; got != test.wantRanges {
				t.Errorf("read in ranges: %v; want %v", got, test.wantRanges)
			}
		})
	}
}
