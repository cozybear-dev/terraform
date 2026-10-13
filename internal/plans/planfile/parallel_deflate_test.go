// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package planfile

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"
)

func TestParallelDeflateWriter(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	// Data that is partly compressible, like a state snapshot.
	data := make([]byte, 5*parallelDeflateChunkSize+12345)
	for i := 0; i < len(data); {
		word := []byte(fmt.Sprintf("%d-", rnd.Intn(5000)))
		i += copy(data[i:], word)
	}

	sizes := []int{
		0, 1, 100,
		parallelDeflateChunkSize - 1, parallelDeflateChunkSize, parallelDeflateChunkSize + 1,
		2 * parallelDeflateChunkSize, 2*parallelDeflateChunkSize + 1,
		len(data),
	}
	for _, size := range sizes {
		for _, writeSize := range []int{1 << 30, 4096, 1000003} {
			t.Run(fmt.Sprintf("%d bytes in writes of %d", size, writeSize), func(t *testing.T) {
				want := data[:size]

				// First as a raw stream, read with the standard library.
				var compressed bytes.Buffer
				w, err := newParallelDeflateWriter(&compressed)
				if err != nil {
					t.Fatal(err)
				}
				for rest := want; len(rest) > 0; {
					n := min(writeSize, len(rest))
					if written, err := w.Write(rest[:n]); err != nil || written != n {
						t.Fatalf("wrote %d bytes of %d: %v", written, n, err)
					}
					rest = rest[n:]
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(flate.NewReader(&compressed))
				if err != nil {
					t.Fatalf("failed to decompress: %s", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatal("wrong data after decompressing")
				}

				// Then as a member of a zip archive, which also verifies
				// the checksum and the recorded sizes.
				var archive bytes.Buffer
				zw := zip.NewWriter(&archive)
				zw.RegisterCompressor(zip.Deflate, newParallelDeflateWriter)
				fw, err := zw.Create("data")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fw.Write(want); err != nil {
					t.Fatal(err)
				}
				if err := zw.Close(); err != nil {
					t.Fatal(err)
				}
				zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
				if err != nil {
					t.Fatal(err)
				}
				fr, err := zr.File[0].Open()
				if err != nil {
					t.Fatal(err)
				}
				got, err = io.ReadAll(fr)
				if err != nil {
					t.Fatalf("failed to read from archive: %s", err)
				}
				if !bytes.Equal(got, want) {
					t.Fatal("wrong data after reading from archive")
				}
			})
		}
	}
}

type failingWriter struct {
	remaining int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.remaining -= len(p); w.remaining < 0 {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func TestParallelDeflateWriter_error(t *testing.T) {
	data := make([]byte, 4*parallelDeflateChunkSize)
	rand.New(rand.NewSource(1)).Read(data)

	w, err := newParallelDeflateWriter(&failingWriter{remaining: parallelDeflateChunkSize})
	if err != nil {
		t.Fatal(err)
	}
	// The writes to the underlying writer happen in the background, so
	// the error is reported when closing.
	if _, err := w.Write(data); err != nil {
		t.Fatalf("unexpected error from Write: %s", err)
	}
	closeErr := w.Close()
	if closeErr == nil || closeErr.Error() != "disk full" {
		t.Fatalf("wrong error from Close: %v", closeErr)
	}
}
