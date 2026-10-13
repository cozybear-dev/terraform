// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package planfile

import (
	"bytes"
	"compress/flate"
	"io"
	"runtime"
	"sync"
)

// parallelDeflateChunkSize is the amount of uncompressed data that
// parallelDeflateWriter compresses as a single unit.
const parallelDeflateChunkSize = 1 << 20

// newParallelDeflateWriter is a zip.Compressor that produces a standard
// DEFLATE stream but, for large inputs, compresses separate chunks of the
// input concurrently.
//
// A plan file contains two full snapshots of the state, so for a large
// state the compression would otherwise dominate the time taken to save the
// plan. Each chunk is compressed independently of the others, which costs a
// little of the compression ratio at the boundaries between chunks, but the
// result is an ordinary DEFLATE stream that any implementation can read.
func newParallelDeflateWriter(w io.Writer) (io.WriteCloser, error) {
	pw := &parallelDeflateWriter{
		w:       w,
		pending: make(chan chan *bytes.Buffer, runtime.GOMAXPROCS(0)),
		done:    make(chan struct{}),
	}
	go pw.writeChunks()
	return pw, nil
}

type parallelDeflateWriter struct {
	w io.Writer

	// chunk is the uncompressed data that we've not yet started compressing.
	chunk []byte

	// pending has one channel for each chunk whose compression has been
	// started, in the order the chunks must be written. Each channel
	// delivers the compressed form of its chunk. The capacity of pending
	// limits how many chunks can be in progress at once.
	pending chan chan *bytes.Buffer

	// done is closed once writeChunks has written everything from pending,
	// after which err is the first error it encountered, if any.
	done   chan struct{}
	err    error
	closed bool
}

var (
	flateWriters = sync.Pool{
		New: func() any {
			// The error is always nil for a valid compression level.
			fw, _ := flate.NewWriter(io.Discard, flate.DefaultCompression)
			return fw
		},
	}
	// deflateChunks are the buffers that accumulate uncompressed data,
	// which are reused because a plan file has many members and most of
	// them are far smaller than a chunk.
	deflateChunks = sync.Pool{
		New: func() any {
			chunk := make([]byte, 0, parallelDeflateChunkSize)
			return &chunk
		},
	}
	deflateBuffers = sync.Pool{
		New: func() any {
			return new(bytes.Buffer)
		},
	}
)

func (pw *parallelDeflateWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if pw.chunk == nil {
			pw.chunk = (*deflateChunks.Get().(*[]byte))[:0]
		}
		n := copy(pw.chunk[len(pw.chunk):cap(pw.chunk)], p)
		pw.chunk = pw.chunk[:len(pw.chunk)+n]
		p = p[n:]
		written += n

		// We only start compressing a chunk once we know there's more data
		// to follow it, because the final chunk must be terminated
		// differently.
		if len(pw.chunk) == cap(pw.chunk) && len(p) > 0 {
			pw.compressChunk(false)
		}
	}
	// If writing to the underlying writer fails then we report that from
	// Close, because the writes happen in the background.
	return written, nil
}

func (pw *parallelDeflateWriter) Close() error {
	if pw.closed {
		return pw.err
	}
	pw.closed = true
	pw.compressChunk(true)
	close(pw.pending)
	<-pw.done
	return pw.err
}

// compressChunk starts compressing the current chunk in the background.
func (pw *parallelDeflateWriter) compressChunk(final bool) {
	chunk := pw.chunk
	pw.chunk = nil
	result := make(chan *bytes.Buffer, 1)
	pw.pending <- result
	go func() {
		buf := deflateBuffers.Get().(*bytes.Buffer)
		buf.Reset()
		fw := flateWriters.Get().(*flate.Writer)
		fw.Reset(buf)
		// Writing to a bytes.Buffer cannot fail.
		fw.Write(chunk)
		if final {
			// This writes the block that marks the end of the stream.
			fw.Close()
		} else {
			// This ends the current block and aligns the output to a byte
			// boundary without ending the stream, so that the output for
			// the next chunk can directly follow it.
			fw.Flush()
		}
		flateWriters.Put(fw)
		if chunk != nil {
			deflateChunks.Put(&chunk)
		}
		result <- buf
	}()
}

// writeChunks writes the compressed chunks to the underlying writer in
// the order they were started.
func (pw *parallelDeflateWriter) writeChunks() {
	defer close(pw.done)
	for result := range pw.pending {
		buf := <-result
		if pw.err == nil {
			_, pw.err = pw.w.Write(buf.Bytes())
		}
		deflateBuffers.Put(buf)
	}
}
