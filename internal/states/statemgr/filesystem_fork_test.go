// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statemgr

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statefile"
)

// Each write must leave the file with exactly the new snapshot, whether
// that's longer or shorter than what the file held before.
func TestFilesystem_writeReplacesContent(t *testing.T) {
	newState := func(resources int) *states.State {
		return states.BuildState(func(s *states.SyncState) {
			for i := range resources {
				s.SetResourceInstanceCurrent(
					addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_thing", Name: fmt.Sprintf("r%d", i)}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance),
					&states.ResourceInstanceObjectSrc{
						Status:    states.ObjectReady,
						AttrsJSON: []byte(fmt.Sprintf(`{"id":"object-%d"}`, i)),
					},
					addrs.AbsProviderConfig{Provider: addrs.NewDefaultProvider("test"), Module: addrs.RootModule},
				)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "terraform.tfstate")
	ls := NewFilesystem(path)
	for _, resources := range []int{50, 500, 3, 0, 200, 199, 1} {
		state := newState(resources)
		if err := ls.WriteState(state); err != nil {
			t.Fatal(err)
		}
		if err := ls.PersistState(nil); err != nil {
			t.Fatal(err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// The file must be exactly what writing its content again gives,
		// with nothing left over from an earlier, longer snapshot.
		file, err := statefile.Read(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("after writing %d resources: %s", resources, err)
		}
		var want bytes.Buffer
		if err := statefile.Write(file, &want); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("after writing %d resources the file has %d bytes, but its content should take %d", resources, len(got), want.Len())
		}
		if !statefile.StatesMarshalEqual(file.State, state) {
			t.Fatalf("after writing %d resources the file doesn't have the state that was written", resources)
		}
	}
}

// If Terraform is killed part way through writing a snapshot then whatever
// is left in the file must not be readable as a state, because it isn't one
// that ever existed. In particular the unwritten part of the new snapshot
// must not be made up for by what remains of the previous one, and the file
// must not be left empty while the new snapshot is still being prepared.
func TestFilesystem_interruptedWrite(t *testing.T) {
	newState := func(lastID string) *states.State {
		return states.BuildState(func(s *states.SyncState) {
			for i := range 100 {
				id := fmt.Sprintf("object-%03d", i)
				if i == 99 {
					id = lastID
				}
				s.SetResourceInstanceCurrent(
					addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_thing", Name: fmt.Sprintf("r%03d", i)}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance),
					&states.ResourceInstanceObjectSrc{
						Status:    states.ObjectReady,
						AttrsJSON: []byte(fmt.Sprintf(`{"id":%q}`, id)),
					},
					addrs.AbsProviderConfig{Provider: addrs.NewDefaultProvider("test"), Module: addrs.RootModule},
				)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "terraform.tfstate")
	ls := NewFilesystem(path)
	if err := ls.WriteState(newState("object-099")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The second snapshot is the same size as the first and differs from it
	// only near the end, so everything else in the two lines up exactly.
	// We capture what the file contains at several points while it's being
	// written, which is what would be left if Terraform were killed then.
	var partial [][]byte
	defer func(original func(*statefile.File, io.Writer) error) {
		writeStateFile = original
	}(writeStateFile)
	writeStateFile = func(file *statefile.File, w io.Writer) error {
		// Until there's something to write, the file must still hold the
		// previous snapshot. An empty file would be read as there being
		// no state at all, without any error.
		if content, err := os.ReadFile(path); err != nil || !bytes.Equal(content, before) {
			t.Errorf("the file has %d bytes before anything has been written to it; want the previous snapshot of %d bytes (error: %v)", len(content), len(before), err)
		}
		return statefile.Write(file, &testInterruptedWriter{
			w:  w,
			at: []int{1, 100, len(before) / 2, len(before) - 200},
			interrupted: func() {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				partial = append(partial, content)
			},
		})
	}
	if err := ls.WriteState(newState("object-XYZ")); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || bytes.Equal(after, before) {
		t.Fatalf("the two snapshots should be different but the same size; got %d and %d bytes", len(before), len(after))
	}

	if len(partial) != 4 {
		t.Fatalf("captured the file %d times while it was being written; want 4", len(partial))
	}
	for i, content := range partial {
		if _, err := statefile.Read(bytes.NewReader(content)); err == nil {
			t.Errorf("interruption %d left a file of %d bytes that can be read as a state", i, len(content))
		}
	}
}

// testInterruptedWriter passes everything written to it on to w, calling
// interrupted each time the total written reaches one of the offsets in at,
// which must be in ascending order.
type testInterruptedWriter struct {
	w           io.Writer
	at          []int
	interrupted func()
	written     int
}

func (w *testInterruptedWriter) Write(p []byte) (int, error) {
	total := 0
	for len(w.at) > 0 && w.written+len(p) >= w.at[0] {
		n, err := w.w.Write(p[:w.at[0]-w.written])
		w.written += n
		total += n
		if err != nil {
			return total, err
		}
		p = p[n:]
		w.at = w.at[1:]
		w.interrupted()
	}
	n, err := w.w.Write(p)
	w.written += n
	return total + n, err
}
