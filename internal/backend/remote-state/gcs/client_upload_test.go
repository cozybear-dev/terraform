// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package gcs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// fakeUploads is just enough of the upload API to tell us how a state was
// uploaded, and to fail some of the requests.
type fakeUploads struct {
	mu sync.Mutex

	// failSingle is how many single-request uploads to fail, with failStatus.
	failSingle int
	failStatus int

	metadata       []byte // the object metadata of the last single-request upload
	singleRequests int    // single-request uploads attempted
	chunkRequests  int    // requests that sent part of a chunked upload
	stored         []byte
	pending        []byte
}

func (f *fakeUploads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	done := func(data []byte) {
		f.stored = data
		w.Header().Set("Content-Type", "application/json")
		// The client checks what it sent against this checksum.
		sum := crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli))
		crc := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint32(nil, sum))
		fmt.Fprintf(w, `{"bucket":"b","name":"state","size":"%d","generation":"1","crc32c":%q}`, len(data), crc)
	}
	switch uploadType := r.URL.Query().Get("uploadType"); {
	case r.Method == http.MethodPost && uploadType == "multipart":
		f.singleRequests++
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		var parts [][]byte
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(part)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			parts = append(parts, data)
		}
		if f.failSingle > 0 {
			f.failSingle--
			http.Error(w, "injected failure", f.failStatus)
			return
		}
		if len(parts) != 2 {
			http.Error(w, "expected metadata and content", http.StatusBadRequest)
			return
		}
		f.metadata = parts[0]
		done(parts[1])
	case r.Method == http.MethodPost && uploadType == "resumable":
		f.pending = nil
		w.Header().Set("Location", "http://"+r.Host+"/session")
		w.WriteHeader(http.StatusOK)
	case (r.Method == http.MethodPut || r.Method == http.MethodPost) && r.URL.Path == "/session":
		f.chunkRequests++
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.pending = append(f.pending, data...)
		// The total is only given with the final chunk.
		contentRange := r.Header.Get("Content-Range")
		if total := contentRange[strings.LastIndex(contentRange, "/")+1:]; total != "*" {
			if n, _ := strconv.Atoi(total); n != len(f.pending) {
				http.Error(w, fmt.Sprintf("have %d bytes but the total is %s", len(f.pending), total), http.StatusBadRequest)
				return
			}
			done(f.pending)
			return
		}
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(f.pending)-1))
		w.Header().Set("X-HTTP-Status-Code-Override", "308")
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.String(), http.StatusNotImplemented)
	}
}

// A large state is uploaded in one request, retried as a whole if that
// fails in a way that's worth retrying, and finally uploaded in chunks if
// it keeps failing.
func TestRemoteClientPut_upload(t *testing.T) {
	defer func(attempts int, backoff time.Duration) {
		singleRequestUploadAttempts, singleRequestUploadBackoff = attempts, backoff
	}(singleRequestUploadAttempts, singleRequestUploadBackoff)
	singleRequestUploadBackoff = time.Millisecond

	small := bytes.Repeat([]byte("small state "), 1000)
	// A little more than two of the default chunks.
	large := bytes.Repeat([]byte("0123456789abcdef"), (2*16*1024*1024+4096)/16)

	tests := map[string]struct {
		data       []byte
		failSingle int
		failStatus int

		wantSingle int
		wantChunks int
		wantErr    bool
	}{
		// Something that fits in one chunk is a single request anyway.
		"small":                        {data: small, wantSingle: 1},
		"large":                        {data: large, wantSingle: 1},
		"large, one transient failure": {data: large, failSingle: 1, failStatus: 503, wantSingle: 2},
		"large, two transient failures": {
			data: large, failSingle: 2, failStatus: 503, wantSingle: 3,
		},
		"large, persistent transient failure": {
			// Falls back on a chunked upload, which is three requests here.
			data: large, failSingle: 100, failStatus: 503, wantSingle: 3, wantChunks: 3,
		},
		"large, failure that isn't worth retrying": {
			data: large, failSingle: 100, failStatus: 403, wantSingle: 1, wantErr: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeUploads{failSingle: test.failSingle, failStatus: test.failStatus}
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

			diags := client.Put(test.data)
			if diags.HasErrors() != test.wantErr {
				t.Fatalf("failed = %t; want %t: %v", diags.HasErrors(), test.wantErr, diags.Err())
			}
			if fake.singleRequests != test.wantSingle {
				t.Errorf("made %d single-request uploads; want %d", fake.singleRequests, test.wantSingle)
			}
			if fake.chunkRequests != test.wantChunks {
				t.Errorf("made %d requests of a chunked upload; want %d", fake.chunkRequests, test.wantChunks)
			}
			if !test.wantErr && !bytes.Equal(fake.stored, test.data) {
				t.Errorf("stored %d bytes that aren't the %d bytes that were given", len(fake.stored), len(test.data))
			}
			if test.wantErr && fake.stored != nil {
				t.Errorf("stored %d bytes despite failing", len(fake.stored))
			}
		})
	}
}

// When it's asked to, the client stores a state compressed, described in a
// way that lets the service serve it decompressed to anything that doesn't
// ask for it as stored.
func TestRemoteClientPut_compressed(t *testing.T) {
	state := bytes.Repeat([]byte(`{"resources": [{"type": "test_thing", "name": "a"}]}`+"\n"), 20000)

	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			if enabled {
				t.Setenv(compressStateEnvVar, "1")
			} else {
				t.Setenv(compressStateEnvVar, "")
			}
			fake := &fakeUploads{}
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

			if diags := client.Put(state); diags.HasErrors() {
				t.Fatal(diags.Err())
			}
			if got := bytes.Contains(fake.metadata, []byte(`"contentEncoding":"gzip"`)); got != enabled {
				t.Errorf("described as gzip = %t; want %t\nmetadata: %s", got, enabled, fake.metadata)
			}
			if !enabled {
				if !bytes.Equal(fake.stored, state) {
					t.Fatal("didn't store the state as it was given")
				}
				return
			}
			if len(fake.stored) >= len(state)/10 {
				t.Errorf("stored %d bytes for a state of %d bytes, which isn't much smaller", len(fake.stored), len(state))
			}
			got, err := gunzip(fake.stored)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, state) {
				t.Fatal("what was stored doesn't decompress to the state that was given")
			}
		})
	}

	// Corrupt data must be an error rather than a wrong result.
	if _, err := gunzip([]byte("not gzip")); err == nil {
		t.Error("decompressed something that isn't compressed")
	}
	gz, err := gzipBytes(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gunzip(gz[:len(gz)/2]); err == nil {
		t.Error("decompressed truncated data without an error")
	}
}
