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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// fakeDownloads is just enough of the download API to serve one object,
// whole or in ranges, and to misbehave for some of the ranges.
type fakeDownloads struct {
	mu sync.Mutex

	data       []byte
	generation int64

	// failRanges makes requests for a range that doesn't start at zero
	// fail with this status, if it's not zero.
	failRanges int
	// corruptRanges makes those requests return the wrong content.
	corruptRanges bool
	// noChecksum leaves out the checksum of the object.
	noChecksum bool

	wholeRequests int
	rangeRequests int
	// otherGenerations counts the requests for a range that didn't ask
	// for the generation of the object.
	otherGenerations int
}

func (f *fakeDownloads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// We write the response without holding the lock, because a client
	// can stop reading one response while it waits for others.
	respond := f.respond(r)
	respond(w)
}

// respond decides how to respond to a request and returns a function that
// does so.
func (f *fakeDownloads) respond(r *http.Request) func(w http.ResponseWriter) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Method != http.MethodGet {
		return func(w http.ResponseWriter) {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}
	sum := crc32.Checksum(f.data, crc32.MakeTable(crc32.Castagnoli))
	crc := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, sum))
	md5sum := md5.Sum(f.data)
	size, generation := len(f.data), f.generation
	if strings.Contains(r.URL.Path, "/o/") {
		// The metadata of the object.
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"bucket":"b","name":"state","size":"%d","generation":"%d","crc32c":%q,"md5Hash":%q}`,
				size, generation, crc, base64.StdEncoding.EncodeToString(md5sum[:]))
		}
	}

	start, end := 0, len(f.data)
	ranged := false
	if spec := r.Header.Get("Range"); spec != "" {
		var first, last int
		if n, _ := fmt.Sscanf(spec, "bytes=%d-%d", &first, &last); n == 2 {
			start, end, ranged = first, min(last+1, len(f.data)), true
		} else if n == 1 {
			start, ranged = first, first != 0
		}
	}
	if ranged {
		f.rangeRequests++
		if r.URL.Query().Get("generation") != strconv.FormatInt(f.generation, 10) {
			f.otherGenerations++
		}
		if status := f.failRanges; start != 0 && status != 0 {
			return func(w http.ResponseWriter) {
				http.Error(w, "no such object", status)
			}
		}
	} else {
		f.wholeRequests++
	}

	body := f.data[start:end]
	if ranged && start != 0 && f.corruptRanges {
		body = bytes.Repeat([]byte{'x'}, len(body))
	}
	noChecksum := f.noChecksum
	return func(w http.ResponseWriter) {
		w.Header().Set("X-Goog-Generation", strconv.FormatInt(generation, 10))
		w.Header().Set("X-Goog-Stored-Content-Length", strconv.Itoa(size))
		if !noChecksum {
			w.Header().Set("X-Goog-Hash", "crc32c="+crc)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if ranged {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Write(body)
	}
}

// A large state is read as several ranges of one generation of the object,
// and is read whole instead if that doesn't give what the object's checksum
// says it should.
func TestRemoteClientGet_download(t *testing.T) {
	defer func(minSize, minPart int64, parts int) {
		parallelDownloadMinSize, parallelDownloadMinPart, parallelDownloadParts = minSize, minPart, parts
	}(parallelDownloadMinSize, parallelDownloadMinPart, parallelDownloadParts)
	parallelDownloadMinSize, parallelDownloadMinPart, parallelDownloadParts = 1<<20, 256<<10, 4

	small := bytes.Repeat([]byte("small state "), 1000)
	// Not a multiple of the size of a part, and with no two parts alike.
	large := make([]byte, 3<<20+12345)
	for i := range large {
		large[i] = byte(i ^ i>>8 ^ i>>16)
	}

	tests := map[string]struct {
		fake *fakeDownloads

		wantRanges int
		wantWhole  int
	}{
		"small": {fake: &fakeDownloads{data: small}, wantWhole: 1},
		// The request that finds out about the object asks for all of it.
		"large":              {fake: &fakeDownloads{data: large}, wantRanges: 4, wantWhole: 1},
		"large, no checksum": {fake: &fakeDownloads{data: large, noChecksum: true}, wantWhole: 1},
		"large, a range of it has gone": {
			fake: &fakeDownloads{data: large, failRanges: http.StatusNotFound}, wantRanges: 4, wantWhole: 2,
		},
		"large, a range of it is wrong": {
			fake: &fakeDownloads{data: large, corruptRanges: true}, wantRanges: 4, wantWhole: 2,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fake := test.fake
			fake.generation = 1234
			server := httptest.NewServer(fake)
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
			if !bytes.Equal(payload.Data, fake.data) {
				t.Errorf("got %d bytes that aren't the %d bytes of the object", len(payload.Data), len(fake.data))
			}
			if payload.Revision != "1234" {
				t.Errorf("revision is %q; want the generation of the object", payload.Revision)
			}
			if fake.rangeRequests != test.wantRanges {
				t.Errorf("made %d requests for a range; want %d", fake.rangeRequests, test.wantRanges)
			}
			if fake.wholeRequests != test.wantWhole {
				t.Errorf("made %d requests for the whole object; want %d", fake.wholeRequests, test.wantWhole)
			}
			if fake.otherGenerations != 0 {
				t.Errorf("%d requests for a range didn't ask for the generation being read", fake.otherGenerations)
			}
		})
	}
}
