// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// BenchmarkStateFile measures reading and writing the state file named in
// the TF_BENCH_STATE_FILE environment variable, for investigating the
// performance of these operations with a real-world state snapshot.
func BenchmarkStateFile(b *testing.B) {
	filename := os.Getenv("TF_BENCH_STATE_FILE")
	if filename == "" {
		b.Skip("TF_BENCH_STATE_FILE is not set")
	}
	src, err := os.ReadFile(filename)
	if err != nil {
		b.Fatal(err)
	}
	file, err := Read(bytes.NewReader(src))
	if err != nil {
		b.Fatal(err)
	}

	b.Run("Read", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		for i := 0; i < b.N; i++ {
			if _, err := Read(bytes.NewReader(src)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Write", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		for i := 0; i < b.N; i++ {
			if err := Write(file, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Digest", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		for i := 0; i < b.N; i++ {
			StateMarshalDigest(file.State)
		}
	})
	b.Run("DeepCopy", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			file.State.DeepCopy()
		}
	})
}
