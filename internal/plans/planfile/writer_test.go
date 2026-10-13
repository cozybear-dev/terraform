// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package planfile

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/configload"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statefile"
)

// The two state snapshots in a plan file are written only once when they're
// the same, and in either case each must contain what writing that snapshot
// on its own would produce.
func TestCreateStateSnapshots(t *testing.T) {
	// The state is large enough to be compressed as several chunks, and to
	// be written in more than one piece.
	newState := func(id string) *states.State {
		return states.BuildState(func(s *states.SyncState) {
			for i := range 6000 {
				s.SetResourceInstanceCurrent(
					addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_thing", Name: "a"}.Instance(addrs.IntKey(i)).Absolute(addrs.RootModuleInstance),
					&states.ResourceInstanceObjectSrc{
						Status:    states.ObjectReady,
						AttrsJSON: []byte(fmt.Sprintf(`{"id":%q,"index":%d,"padding":%q}`, id, i, strings.Repeat("x", 200+i%50))),
					},
					addrs.AbsProviderConfig{Provider: addrs.NewDefaultProvider("test"), Module: addrs.RootModule},
				)
			}
		})
	}
	prior := newState("prior")

	tests := map[string]struct {
		prevRun      *statefile.File
		prior        *statefile.File
		wantSameData bool
		// wantShared is whether the two are expected to be stored using
		// the same compressed data for everything but their metadata.
		wantShared bool
	}{
		"unchanged": {
			statefile.New(prior.DeepCopy(), "lineage", 4),
			statefile.New(prior, "lineage", 4),
			true,
			true,
		},
		"same file": {
			statefile.New(prior, "lineage", 4),
			nil, // set to the same as prevRun below
			true,
			true,
		},
		// This is how the two are related in a real plan file when nothing
		// changed during refresh.
		"unchanged without metadata": {
			&statefile.File{State: prior.DeepCopy()},
			statefile.New(prior, "lineage", 4),
			false,
			true,
		},
		"changed without metadata": {
			&statefile.File{State: newState("previous")},
			statefile.New(prior, "lineage", 4),
			false,
			false,
		},
		"changed": {
			statefile.New(newState("previous"), "lineage", 4),
			statefile.New(prior, "lineage", 4),
			false,
			false,
		},
		"different serial": {
			statefile.New(prior.DeepCopy(), "lineage", 3),
			statefile.New(prior, "lineage", 4),
			false,
			true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.prior == nil {
				test.prior = test.prevRun
			}
			filename := filepath.Join(t.TempDir(), "tfplan")
			err := Create(filename, CreateArgs{
				ConfigSnapshot: &configload.Snapshot{
					Modules: map[string]*configload.SnapshotModule{
						"": {Dir: ".", Files: map[string][]byte{"main.tf": nil}},
					},
				},
				PreviousRunStateFile: test.prevRun,
				StateFile:            test.prior,
				Plan: &plans.Plan{
					Changes:      plans.NewChangesSrc(),
					PriorState:   test.prior.State,
					PrevRunState: test.prevRun.State,
					VariableValues: map[string]plans.DynamicValue{
						"unused": mustDynamicValue(t, cty.StringVal("unused")),
					},
					Backend: &plans.Backend{
						Type:      "local",
						Config:    mustDynamicValue(t, cty.EmptyObjectVal),
						Workspace: "default",
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			want := func(f *statefile.File) []byte {
				var buf bytes.Buffer
				if err := statefile.Write(f, &buf); err != nil {
					t.Fatal(err)
				}
				return buf.Bytes()
			}

			// The archive itself must be valid and have both members, with
			// the content that each of them should have.
			zr, err := zip.OpenReader(filename)
			if err != nil {
				t.Fatal(err)
			}
			defer zr.Close()
			members := map[string][]byte{}
			for _, zf := range zr.File {
				if age := time.Since(zf.Modified); age < -time.Minute || age > time.Hour {
					t.Errorf("%s has modification time %s, which isn't the current time", zf.Name, zf.Modified)
				}
				r, err := zf.Open()
				if err != nil {
					t.Fatalf("can't open %s: %s", zf.Name, err)
				}
				src, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					// This includes verifying the checksum.
					t.Fatalf("can't read %s: %s", zf.Name, err)
				}
				members[zf.Name] = src
			}
			if got, want := members[tfstateFilename], want(test.prior); !bytes.Equal(got, want) {
				t.Errorf("wrong state snapshot\ngot:  %s\nwant: %s", got, want)
			}
			if got, want := members[tfstatePreviousFilename], want(test.prevRun); !bytes.Equal(got, want) {
				t.Errorf("wrong previous run state snapshot\ngot:  %s\nwant: %s", got, want)
			}
			if got := bytes.Equal(members[tfstateFilename], members[tfstatePreviousFilename]); got != test.wantSameData {
				t.Errorf("snapshots have the same content = %t; want %t", got, test.wantSameData)
			}
			compressedSizes := map[string]int64{}
			for _, zf := range zr.File {
				compressedSizes[zf.Name] = int64(zf.CompressedSize64)
			}
			// When the data is shared the two differ only by a few bytes of
			// metadata, whereas separately compressed snapshots of different
			// states are very unlikely to have nearly the same size.
			difference := compressedSizes[tfstateFilename] - compressedSizes[tfstatePreviousFilename]
			if got := difference > -64 && difference < 64; got != test.wantShared {
				t.Errorf("snapshots have compressed sizes %d and %d, so shared = %t; want %t", compressedSizes[tfstateFilename], compressedSizes[tfstatePreviousFilename], got, test.wantShared)
			}

			// Each must be exactly what reading that member alone gives,
			// in whichever order they're read.
			for _, order := range []string{"prior first", "previous first"} {
				pr, err := Open(filename)
				if err != nil {
					t.Fatal(err)
				}
				var gotPrior, gotPrevRun *statefile.File
				if order == "prior first" {
					gotPrior, err = pr.ReadStateFile()
					if err != nil {
						t.Fatal(err)
					}
				}
				gotPrevRun, err = pr.ReadPrevStateFile()
				if err != nil {
					t.Fatal(err)
				}
				if gotPrior == nil {
					gotPrior, err = pr.ReadStateFile()
					if err != nil {
						t.Fatal(err)
					}
				}
				pr.Close()
				for name, got := range map[string]*statefile.File{tfstateFilename: gotPrior, tfstatePreviousFilename: gotPrevRun} {
					want, err := statefile.Read(bytes.NewReader(members[name]))
					if err != nil {
						t.Fatal(err)
					}
					if !statefile.StatesMarshalEqual(got.State, want.State) {
						t.Errorf("%s: wrong state in %s", order, name)
					}
					if got.Serial != want.Serial || got.Lineage != want.Lineage || got.TerraformVersion.String() != want.TerraformVersion.String() {
						t.Errorf(
							"%s: wrong metadata in %s\ngot:  serial %d, lineage %q, version %s\nwant: serial %d, lineage %q, version %s",
							order, name,
							got.Serial, got.Lineage, got.TerraformVersion,
							want.Serial, want.Lineage, want.TerraformVersion,
						)
					}
				}
				if gotPrior.State == gotPrevRun.State {
					t.Errorf("%s: both snapshots are the same object", order)
				}
			}
		})
	}
}

func mustDynamicValue(t *testing.T, val cty.Value) plans.DynamicValue {
	t.Helper()
	ret, err := plans.NewDynamicValue(val, val.Type())
	if err != nil {
		t.Fatal(err)
	}
	return ret
}

func TestCRC32Combine(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	data := make([]byte, 300000)
	rnd.Read(data)
	for _, total := range []int{0, 1, 2, 3, 7, 8, 9, 255, 256, 4097, 65536, len(data)} {
		for _, at := range []int{0, 1, total / 3, total / 2, total - 1, total} {
			if at < 0 || at > total {
				continue
			}
			a, b := data[:at], data[at:total]
			got := crc32Combine(crc32.ChecksumIEEE(a), crc32.ChecksumIEEE(b), int64(len(b)))
			if want := crc32.ChecksumIEEE(data[:total]); got != want {
				t.Errorf("wrong result %#08x for %d bytes followed by %d bytes; want %#08x", got, len(a), len(b), want)
			}
		}
	}
}

// ReadPlan reads the two state snapshots concurrently. When they're the same
// the second is a copy of the first, which must not be taken while the first
// is being made to share its data with the state given to ShareStateDataFrom.
func TestReadPlanSharingStateData(t *testing.T) {
	newState := func() *states.State {
		return states.BuildState(func(s *states.SyncState) {
			for i := range 4000 {
				s.SetResourceInstanceCurrent(
					addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_thing", Name: "a"}.Instance(addrs.IntKey(i)).Absolute(addrs.RootModuleInstance),
					&states.ResourceInstanceObjectSrc{
						Status:    states.ObjectReady,
						AttrsJSON: []byte(fmt.Sprintf(`{"id":"object-%d"}`, i)),
						Dependencies: []addrs.ConfigResource{
							{Resource: addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_thing", Name: "b"}},
						},
					},
					addrs.AbsProviderConfig{Provider: addrs.NewDefaultProvider("test"), Module: addrs.RootModule},
				)
			}
		})
	}
	state := newState()

	filename := filepath.Join(t.TempDir(), "tfplan")
	err := Create(filename, CreateArgs{
		ConfigSnapshot: &configload.Snapshot{
			Modules: map[string]*configload.SnapshotModule{
				"": {Dir: ".", Files: map[string][]byte{"main.tf": nil}},
			},
		},
		PreviousRunStateFile: &statefile.File{State: state.DeepCopy()},
		StateFile:            statefile.New(state, "lineage", 4),
		Plan: &plans.Plan{
			Changes:      plans.NewChangesSrc(),
			PriorState:   state,
			PrevRunState: state,
			VariableValues: map[string]plans.DynamicValue{
				"unused": mustDynamicValue(t, cty.StringVal("unused")),
			},
			Backend: &plans.Backend{
				Type:      "local",
				Config:    mustDynamicValue(t, cty.EmptyObjectVal),
				Workspace: "default",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for range 8 {
		pr, err := Open(filename)
		if err != nil {
			t.Fatal(err)
		}
		// This is the latest state snapshot, as loaded separately by
		// whatever is going to apply the plan.
		pr.ShareStateDataFrom(newState())
		plan, err := pr.ReadPlan()
		pr.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !statefile.StatesMarshalEqual(plan.PriorState, state) {
			t.Fatal("wrong prior state")
		}
		if !statefile.StatesMarshalEqual(plan.PrevRunState, state) {
			t.Fatal("wrong previous run state")
		}
	}
}

// testPlanFileWithSnapshots creates a plan file whose two state snapshots
// are the given members, which are added to the archive exactly as they are.
func testPlanFileWithSnapshots(t *testing.T, prior, prevRun testRawMember) string {
	t.Helper()

	// We start with a real plan file, to get everything else it contains.
	state := states.NewState()
	original := filepath.Join(t.TempDir(), "tfplan")
	err := Create(original, CreateArgs{
		ConfigSnapshot: &configload.Snapshot{
			Modules: map[string]*configload.SnapshotModule{
				"": {Dir: ".", Files: map[string][]byte{"main.tf": nil}},
			},
		},
		PreviousRunStateFile: &statefile.File{State: state},
		StateFile:            statefile.New(state, "lineage", 4),
		Plan: &plans.Plan{
			Changes:      plans.NewChangesSrc(),
			PriorState:   state,
			PrevRunState: state,
			VariableValues: map[string]plans.DynamicValue{
				"unused": mustDynamicValue(t, cty.StringVal("unused")),
			},
			Backend: &plans.Backend{
				Type:      "local",
				Config:    mustDynamicValue(t, cty.EmptyObjectVal),
				Workspace: "default",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(original)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	filename := filepath.Join(t.TempDir(), "tfplan")
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, zf := range zr.File {
		member, replace := map[string]testRawMember{tfstateFilename: prior, tfstatePreviousFilename: prevRun}[zf.Name]
		if !replace {
			if err := zw.Copy(zf); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var compressed bytes.Buffer
		fw, err := flate.NewWriter(&compressed, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(member.content)
		// Ending the last block that has any content before ending the
		// stream means that a reader has been given all of the content
		// before it finds out that there is no more.
		fw.Flush()
		if err := fw.Close(); err != nil {
			t.Fatal(err)
		}
		size := uint64(len(member.content))
		if member.claimedSize != 0 {
			size = member.claimedSize
		}
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name:               zf.Name,
			Method:             zip.Deflate,
			CRC32:              crc32.ChecksumIEEE(member.content),
			CompressedSize64:   uint64(compressed.Len()),
			UncompressedSize64: size,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(compressed.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

// testRawMember is the content of a member of a plan file archive, along
// with the uncompressed size that the archive should claim it has if that's
// to be something other than its real size.
type testRawMember struct {
	content     []byte
	claimedSize uint64
}

// testStateSnapshotSource returns what a snapshot of a state with a single
// output value looks like in a plan file.
func testStateSnapshotSource(t *testing.T, lineage string, serial uint64) []byte {
	t.Helper()
	state := states.NewState()
	state.SetOutputValue(
		addrs.OutputValue{Name: "a"}.Absolute(addrs.RootModuleInstance),
		cty.StringVal("a"), false,
	)
	var buf bytes.Buffer
	if err := statefile.Write(statefile.New(state, lineage, serial), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A snapshot that ends sooner than the archive says it should is damaged,
// and must be reported as such even if what there is of it matches the
// other snapshot.
func TestReadStateSnapshotShorterThanClaimed(t *testing.T) {
	prior := testStateSnapshotSource(t, "lineage", 4)
	prevRun := testStateSnapshotSource(t, "", 0)
	filename := testPlanFileWithSnapshots(t,
		testRawMember{content: prior},
		testRawMember{content: prevRun, claimedSize: uint64(len(prevRun)) + 1},
	)

	for _, order := range []string{"prior first", "previous first"} {
		pr, err := Open(filename)
		if err != nil {
			t.Fatal(err)
		}
		if order == "prior first" {
			if _, err := pr.ReadStateFile(); err != nil {
				t.Fatalf("%s: unexpected error reading the prior state: %s", order, err)
			}
		}
		if _, err := pr.ReadPrevStateFile(); err == nil {
			t.Errorf("%s: read the damaged previous run state without an error", order)
		}
		pr.Close()
	}
}

// The second snapshot is only a copy of the first if nothing that precedes
// the part they have in common could affect the state, which is true of
// what we write ourselves but not of everything that's valid.
func TestReadStateSnapshotWithStateInMetadata(t *testing.T) {
	prior := testStateSnapshotSource(t, "lineage", 4)
	prevRun := testStateSnapshotSource(t, "", 0)
	// This is still a valid snapshot, and has another output value that
	// is declared before everything else.
	if prior[0] != '{' {
		t.Fatalf("unexpected start of snapshot %q", prior[:1])
	}
	prior = append([]byte(`{"outputs":{"extra":{"value":"extra","type":"string"}},`), prior[1:]...)
	filename := testPlanFileWithSnapshots(t, testRawMember{content: prior}, testRawMember{content: prevRun})

	want := map[string]*statefile.File{}
	for name, src := range map[string][]byte{tfstateFilename: prior, tfstatePreviousFilename: prevRun} {
		file, err := statefile.Read(bytes.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		want[name] = file
	}
	if got := len(want[tfstateFilename].State.RootOutputValues); got != 2 {
		t.Fatalf("the prior state should have 2 output values; has %d", got)
	}

	for _, order := range []string{"prior first", "previous first"} {
		pr, err := Open(filename)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]*statefile.File{}
		if order == "prior first" {
			if got[tfstateFilename], err = pr.ReadStateFile(); err != nil {
				t.Fatal(err)
			}
		}
		if got[tfstatePreviousFilename], err = pr.ReadPrevStateFile(); err != nil {
			t.Fatal(err)
		}
		if got[tfstateFilename] == nil {
			if got[tfstateFilename], err = pr.ReadStateFile(); err != nil {
				t.Fatal(err)
			}
		}
		pr.Close()
		for name, file := range got {
			if !statefile.StatesMarshalEqual(file.State, want[name].State) {
				t.Errorf("%s: wrong state in %s: has %d output values; want %d", order, name, len(file.State.RootOutputValues), len(want[name].State.RootOutputValues))
			}
		}
	}
}

// The size that the archive records for a snapshot is only a hint about how
// much space reading it will need, so one that's wrong must lead to the same
// error as it would if we hadn't used it.
func TestReadStateSnapshotClaimingImpossibleSize(t *testing.T) {
	prior := testStateSnapshotSource(t, "lineage", 4)
	prevRun := testStateSnapshotSource(t, "", 0)
	filename := testPlanFileWithSnapshots(t,
		testRawMember{content: prior, claimedSize: 1 << 62},
		testRawMember{content: prevRun},
	)

	pr, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	if _, err := pr.ReadStateFile(); err == nil {
		t.Fatal("read the damaged prior state without an error")
	}
}
