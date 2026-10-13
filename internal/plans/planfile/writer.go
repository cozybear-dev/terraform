// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package planfile

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/terraform/internal/configs/configload"
	"github.com/hashicorp/terraform/internal/depsfile"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statefile"
)

type CreateArgs struct {
	// ConfigSnapshot is a snapshot of the configuration that the plan
	// was created from.
	ConfigSnapshot *configload.Snapshot

	// PreviousRunStateFile is a representation of the state snapshot we used
	// as the original input when creating this plan, containing the same
	// information as recorded at the end of the previous apply except for
	// upgrading managed resource instance data to the provider's latest
	// schema versions.
	PreviousRunStateFile *statefile.File

	// BaseStateFile is a representation of the state snapshot we used to
	// create the plan, which is the result of asking the providers to refresh
	// all previously-stored objects to match the current situation in the
	// remote system. (If this plan was created with refreshing disabled,
	// this should be the same as PreviousRunStateFile.)
	StateFile *statefile.File

	// Plan records the plan itself, which is the main artifact inside a
	// saved plan file.
	Plan *plans.Plan

	// DependencyLocks records the dependency lock information that we
	// checked prior to creating the plan, so we can make sure that all of the
	// same dependencies are still available when applying the plan.
	DependencyLocks *depsfile.Locks
}

// Create creates a new plan file with the given filename, overwriting any
// file that might already exist there.
//
// A plan file contains both a snapshot of the configuration and of the latest
// state file in addition to the plan itself, so that Terraform can detect
// if the world has changed since the plan was created and thus refuse to
// apply it.
func Create(filename string, args CreateArgs) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()
	zw.RegisterCompressor(zip.Deflate, newParallelDeflateWriter)

	// The plan and the two state snapshots account for almost all of the
	// work of writing a plan file for a large configuration, and are
	// independent of one another, so we prepare their compressed forms in
	// memory, partly concurrently, and then add the results to the archive
	// in order.
	members := []*compressedMember{
		{name: tfplanFilename, write: func(w io.Writer) error {
			if err := writeTfplan(args.Plan, w); err != nil {
				return fmt.Errorf("failed to write plan: %s", err)
			}
			return nil
		}},
		{name: tfstateFilename, write: func(w io.Writer) error {
			if err := statefile.Write(args.StateFile, w); err != nil {
				return fmt.Errorf("failed to write state snapshot: %s", err)
			}
			return nil
		}},
		{name: tfstatePreviousFilename, write: func(w io.Writer) error {
			if err := statefile.Write(args.PreviousRunStateFile, w); err != nil {
				return fmt.Errorf("failed to write previous state snapshot: %s", err)
			}
			return nil
		}},
	}
	// The plan needs a lot of memory while it's being serialized, so we
	// deal with it on its own first and then with the two state snapshots
	// together, to limit the peak memory usage.
	members[0].compress()
	prior, prevRun := members[1], members[2]
	if args.StateFile != nil && args.PreviousRunStateFile != nil && statefile.StatesDefinitelyMarshalEqual(args.StateFile.State, args.PreviousRunStateFile.State) {
		// Refreshing usually finds that nothing has changed, and then the
		// two snapshots describe exactly the same objects and differ only
		// in the metadata at the start, because the snapshot of the previous
		// run state doesn't record a serial or a lineage. In that case we
		// only produce the rest once and store it for both.
		prior.split = stateSnapshotBodyStart
		prior.compress()
		if prior.err == nil && !prevRun.sameBodyAs(prior, args.PreviousRunStateFile) {
			prevRun.compress()
		}
	} else {
		var wg sync.WaitGroup
		for _, member := range members[1:] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				member.compress()
			}()
		}
		wg.Wait()
	}
	for _, member := range members {
		if member.err != nil {
			return member.err
		}
		// Adding a member that's already compressed means providing the
		// modification time in the form the archive stores it, too.
		now := time.Now().UTC()
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name:               member.name,
			Method:             zip.Deflate,
			Modified:           now,
			ModifiedDate:       uint16(now.Day() + int(now.Month())<<5 + (now.Year()-1980)<<9),
			ModifiedTime:       uint16(now.Second()/2 + now.Minute()<<5 + now.Hour()<<11),
			CRC32:              member.crc,
			CompressedSize64:   uint64(len(member.header) + member.compressed.Len()),
			UncompressedSize64: uint64(member.size),
		})
		if err != nil {
			return fmt.Errorf("failed to create embedded %s file: %s", member.name, err)
		}
		if _, err := w.Write(member.header); err != nil {
			return fmt.Errorf("failed to write embedded %s file: %s", member.name, err)
		}
		if _, err := w.Write(member.compressed.Bytes()); err != nil {
			return fmt.Errorf("failed to write embedded %s file: %s", member.name, err)
		}
		member.compressed = nil // no longer needed
	}

	// tfconfig directory
	{
		err := WriteConfigSnapshot(args.ConfigSnapshot, zw)
		if err != nil {
			return fmt.Errorf("failed to write config snapshot: %s", err)
		}
	}

	// .terraform.lock.hcl file, containing dependency lock information
	if args.DependencyLocks != nil { // (this was a later addition, so not all callers set it, but main callers should)
		src, diags := depsfile.SaveLocksToBytes(args.DependencyLocks)
		if diags.HasErrors() {
			return fmt.Errorf("failed to write embedded dependency lock file: %s", diags.Err().Error())
		}

		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     dependencyLocksFilename,
			Method:   zip.Deflate,
			Modified: time.Now(),
		})
		if err != nil {
			return fmt.Errorf("failed to create embedded dependency lock file: %s", err)
		}
		_, err = w.Write(src)
		if err != nil {
			return fmt.Errorf("failed to write embedded dependency lock file: %s", err)
		}
	}

	// Compressed data is written in the background, so closing the archive
	// is what reports any failure to write the last of it.
	if err := zw.Close(); err != nil {
		return fmt.Errorf("failed to write plan file: %s", err)
	}

	return nil
}

// compressedMember is a member of a plan file archive whose compressed form
// is prepared in memory before it's added to the archive.
type compressedMember struct {
	name  string
	write func(w io.Writer) error

	// split, if set, is something that's expected to appear near the start
	// of what's written. Everything before it is then compressed separately
	// from the rest, so that another member with different content before
	// that point can use the same compressed form for the rest.
	split []byte

	// header is the compressed form of what precedes the split, if there
	// is one, and compressed is that of everything else. One directly
	// following the other is the compressed form of the whole member.
	header     []byte
	compressed *bytes.Buffer

	// crc and size are the checksum and size of the uncompressed content,
	// and bodyCRC and bodySize are those of the part that follows the split.
	crc      uint32
	size     int64
	bodyCRC  uint32
	bodySize int64
	err      error
}

// compress calls the write function and retains the compressed form of
// what it writes, along with the checksum and size of the original.
func (m *compressedMember) compress() {
	m.header, m.size, m.bodySize = nil, 0, 0
	m.compressed = &bytes.Buffer{}
	crc := crc32.NewIEEE()
	cw, err := newParallelDeflateWriter(m.compressed)
	if err != nil {
		m.err = err
		return
	}
	// Writing in large pieces lets the checksum and the compressor each
	// work on a substantial amount of data at a time.
	body := io.MultiWriter(crc, cw, (*sizeWriter)(&m.bodySize))
	sw := &splitWriter{body: body, split: m.split}
	bw := bufio.NewWriterSize(sw, 1<<20)
	err = m.write(bw)
	if err == nil {
		err = bw.Flush()
	}
	if closeErr := cw.Close(); err == nil {
		err = closeErr
	}
	m.err = err
	m.bodyCRC = crc.Sum32()
	m.setHeader(sw.header)
	if !sw.found {
		// There's nothing that another member could share.
		m.split = nil
	}
}

// setHeader records the given content as what precedes the body, which
// must already have been compressed.
func (m *compressedMember) setHeader(header []byte) {
	m.header = nil
	m.crc, m.size = m.bodyCRC, m.bodySize
	if len(header) == 0 {
		return
	}
	var buf bytes.Buffer
	// The error is always nil for a valid compression level, and writing
	// to a bytes.Buffer cannot fail.
	fw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	fw.Write(header)
	// This ends the current block and aligns the output to a byte boundary
	// without ending the stream, so that the body can directly follow it.
	fw.Flush()
	m.header = buf.Bytes()
	m.crc = crc32Combine(crc32.ChecksumIEEE(header), m.bodyCRC, m.bodySize)
	m.size = int64(len(header)) + m.bodySize
}

// sameBodyAs makes the receiver, which must be a state snapshot of the given
// file, use the body of the given other member, which must be a snapshot of
// a state that is known to be serialized identically and must already have
// been compressed. It returns false if that isn't possible, in which case
// the receiver is unchanged.
func (m *compressedMember) sameBodyAs(other *compressedMember, file *statefile.File) bool {
	if other.split == nil || other.err != nil {
		return false
	}
	// The part before the body depends only on the metadata of the file,
	// so we can find what it is by writing an empty state.
	var buf bytes.Buffer
	err := statefile.Write(&statefile.File{
		TerraformVersion: file.TerraformVersion,
		Serial:           file.Serial,
		Lineage:          file.Lineage,
		State:            states.NewState(),
	}, &buf)
	if err != nil {
		return false
	}
	header, _, ok := splitStateSnapshot(buf.Bytes())
	if !ok {
		return false
	}
	m.compressed, m.bodyCRC, m.bodySize, m.err = other.compressed, other.bodyCRC, other.bodySize, nil
	m.setHeader(header)
	return true
}

// splitWriter passes what's written to it on to body, except for whatever
// precedes the first occurrence of split, which it keeps as header instead.
// The split is only recognized if it's within the first write.
type splitWriter struct {
	body  io.Writer
	split []byte

	started bool
	found   bool
	header  []byte
}

func (w *splitWriter) Write(p []byte) (int, error) {
	if w.started || w.split == nil {
		return w.body.Write(p)
	}
	w.started = true
	// What we're looking for is always near the start.
	at := bytes.Index(p[:min(len(p), 4096)], w.split)
	if at < 0 {
		return w.body.Write(p)
	}
	w.found = true
	w.header = bytes.Clone(p[:at])
	n, err := w.body.Write(p[at:])
	return at + n, err
}

// crc32Combine returns the IEEE CRC-32 checksum of the concatenation of two
// sequences of bytes, given the checksum of each and the length of the second.
func crc32Combine(crc1, crc2 uint32, len2 int64) uint32 {
	if len2 <= 0 {
		return crc1
	}

	// A checksum is a remainder of polynomial division over GF(2), so
	// appending len2 zero bytes to the first sequence transforms its
	// checksum by a linear operator, which we apply by repeated squaring.
	// This is the same method that zlib uses.
	times := func(mat *[32]uint32, vec uint32) uint32 {
		var sum uint32
		for i := 0; vec != 0; i, vec = i+1, vec>>1 {
			if vec&1 != 0 {
				sum ^= mat[i]
			}
		}
		return sum
	}
	square := func(dst, src *[32]uint32) {
		for i := range src {
			dst[i] = times(src, src[i])
		}
	}

	var even, odd [32]uint32
	// The operator for one zero bit.
	odd[0] = crc32.IEEE
	for i, row := 1, uint32(1); i < 32; i, row = i+1, row<<1 {
		odd[i] = row
	}
	square(&even, &odd) // two zero bits
	square(&odd, &even) // four zero bits

	// Apply len2 zero bytes to crc1, the first squaring giving the operator
	// for one zero byte.
	for {
		square(&even, &odd)
		if len2&1 != 0 {
			crc1 = times(&even, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
		square(&odd, &even)
		if len2&1 != 0 {
			crc1 = times(&odd, crc1)
		}
		len2 >>= 1
		if len2 == 0 {
			break
		}
	}
	return crc1 ^ crc2
}

// sizeWriter counts the bytes written to it.
type sizeWriter int64

func (w *sizeWriter) Write(p []byte) (int, error) {
	*w += sizeWriter(len(p))
	return len(p), nil
}
