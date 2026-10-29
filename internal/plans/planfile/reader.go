// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package planfile

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/terraform/internal/configs/configload"
	"github.com/hashicorp/terraform/internal/depsfile"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/plans/planproto"
	"github.com/hashicorp/terraform/internal/states"
	"github.com/hashicorp/terraform/internal/states/statefile"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

const tfstateFilename = "tfstate"
const tfstatePreviousFilename = "tfstate-prev"
const dependencyLocksFilename = ".terraform.lock.hcl" // matches the conventional name in an input configuration

// ErrUnusableLocalPlan is an error wrapper to indicate that we *think* the
// input represents plan file data, but can't use it for some reason (as
// explained in the error text). Callers can check against this type with
// errors.As() if they need to distinguish between corrupt plan files and more
// fundamental problems like an empty file.
type ErrUnusableLocalPlan struct {
	inner error
}

func errUnusable(err error) *ErrUnusableLocalPlan {
	return &ErrUnusableLocalPlan{inner: err}
}
func (e *ErrUnusableLocalPlan) Error() string {
	return e.inner.Error()
}
func (e *ErrUnusableLocalPlan) Unwrap() error {
	return e.inner
}

// Reader is the main type used to read plan files. Create a Reader by calling
// Open.
//
// A plan file is a random-access file format, so methods of Reader must
// be used to access the individual portions of the file for further
// processing.
type Reader struct {
	zip *zip.ReadCloser

	// The state snapshots are expensive to read when the state is large,
	// and a single operation typically asks for each of them more than
	// once, so we read each one only once and then return copies of it.
	priorState   embeddedStateFile
	prevRunState embeddedStateFile

	// rawPlan is the parsed but uninterpreted content of the "tfplan" file,
	// once ReadPlan has read it.
	rawPlanOnce sync.Once
	rawPlan     *planproto.Plan
	rawPlanErr  error

	// mu protects the following fields, and the content of the two state
	// snapshots above once they've been read.
	mu sync.Mutex

	// firstSnapshot is the first of the two state snapshots to be read.
	firstSnapshot *parsedStateSnapshot

	// shareFrom is the state given to ShareStateDataFrom, if any.
	shareFrom *states.State

	// prevRunSharesPrior is set once the previous run state has been made
	// to share data with the prior state.
	prevRunSharesPrior bool
}

// readStateSnapshot reads one of the two state snapshots embedded in the
// plan file.
//
// The two snapshots describe exactly the same objects whenever refreshing
// found nothing to update, which is common, and a large snapshot is expensive
// both to hold in memory and to parse. In that case they differ only in the
// metadata at the start, because the snapshot of the previous run state
// doesn't record a serial or a lineage. Whichever snapshot is requested
// first is read in full. For the other we then compare its content with
// that of the first as we decompress it, and if they're the same apart from
// the metadata then we return a copy of the first one's state without ever
// having the whole of the second in memory.
func (r *Reader) readStateSnapshot(zf *zip.File, description string) (*statefile.File, error) {
	r.mu.Lock()
	first := r.firstSnapshot
	if first == nil {
		first = &parsedStateSnapshot{rawReady: make(chan struct{}), done: make(chan struct{})}
		r.firstSnapshot = first
		r.mu.Unlock()

		raw, err := readStateSnapshotSource(zf, description)
		if err != nil {
			close(first.rawReady)
			first.err = err
			close(first.done)
			return nil, err
		}
		first.raw = raw
		close(first.rawReady)
		first.file, first.err = statefile.ReadBytes(raw)
		close(first.done)
		return first.file, first.err
	}
	r.mu.Unlock()

	// There are only two snapshots, so once we've compared this one with
	// the source of the first then nothing else needs that.
	<-first.rawReady
	file, ok := r.sameAsFirstStateSnapshot(zf, first)
	<-first.done
	statefile.RecycleReadBuffer(first.raw)
	first.raw = nil
	if ok {
		return file, nil
	}

	raw, err := readStateSnapshotSource(zf, description)
	if err != nil {
		return nil, err
	}
	return statefile.ReadBytes(raw)
}

// readStateSnapshotSource returns the whole content of a state snapshot
// embedded in the plan file.
func readStateSnapshotSource(zf *zip.File, description string) ([]byte, error) {
	src, err := zf.Open()
	if err != nil {
		return nil, errUnusable(fmt.Errorf("failed to extract %s from plan file: %s", description, err))
	}
	defer src.Close()
	var raw bytes.Buffer
	// The size recorded in the archive saves repeatedly growing the buffer
	// for a large snapshot, but if the archive is damaged then it can be
	// anything at all, so we use it only if it's possible for that much to
	// have been compressed into the data there is. Otherwise the buffer
	// just grows as needed until reading reports the problem.
	const maxDeflateRatio = 1032
	if size := zf.UncompressedSize64; size/maxDeflateRatio <= zf.CompressedSize64 {
		raw.Grow(int(size) + bytes.MinRead)
	}
	if _, err := raw.ReadFrom(src); err != nil {
		// The state file reader produces the appropriate error.
		_, err := statefile.Read(io.MultiReader(&raw, errReader{err}))
		return nil, err
	}
	return raw.Bytes(), nil
}

// sameAsFirstStateSnapshot returns the content of the given state snapshot
// if it turns out to be the same as the given first snapshot, whose source
// must be available, apart from its metadata. It returns false if they
// differ, or if anything at all is unusual, in which case the caller must
// read the snapshot in the normal way.
func (r *Reader) sameAsFirstStateSnapshot(zf *zip.File, first *parsedStateSnapshot) (*statefile.File, bool) {
	firstHeader, firstBody, ok := splitStateSnapshot(first.raw)
	if !ok {
		return nil, false
	}
	src, err := zf.Open()
	if err != nil {
		return nil, false
	}
	defer src.Close()

	// The metadata is within the first part of the content, and everything
	// after it must match the first snapshot.
	buf := make([]byte, 256*1024)
	n, err := io.ReadFull(src, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, false
	}
	header, body, ok := splitStateSnapshot(buf[:n])
	if !ok {
		return nil, false
	}
	// The header is needed after the buffer is reused below.
	header = bytes.Clone(header)
	rest := firstBody
	for {
		if len(body) > len(rest) || !bytes.Equal(body, rest[:len(body)]) {
			return nil, false
		}
		rest = rest[len(body):]
		if err != nil {
			// That was the end of the content.
			break
		}
		n, err = io.ReadFull(src, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return nil, false
		}
		body = buf[:n]
	}
	if len(rest) != 0 {
		return nil, false
	}
	// Reaching the end of the content part way through filling the buffer
	// is reported in the same way as the archive member being damaged so
	// that it ends sooner than it should, but only in the first case does
	// reading again report a normal end.
	if _, err := src.Read(buf[:1]); err != io.EOF {
		return nil, false
	}

	// The metadata is whatever this snapshot says it is, which we find by
	// parsing it along with an empty state.
	meta, err := statefile.ReadBytes(slices.Concat(header, emptyStateSnapshotBody))
	if err != nil {
		return nil, false
	}
	// The two have the same state only if nothing but the metadata precedes
	// the body in either of them, which we establish by each being exactly
	// what we'd write for its metadata. Anything else that's valid there,
	// such as more output values, would also be part of the state.
	if !isStateSnapshotHeader(header, meta) {
		return nil, false
	}
	<-first.done
	if first.err != nil || first.file == nil {
		// Reading this one too will produce the appropriate error.
		return nil, false
	}
	if !isStateSnapshotHeader(firstHeader, first.file) {
		return nil, false
	}
	// Whatever asked for the first snapshot can already be making it share
	// data with another state, which modifies it.
	r.mu.Lock()
	state := first.file.State.DeepCopy()
	r.mu.Unlock()
	return &statefile.File{
		TerraformVersion: meta.TerraformVersion,
		Serial:           meta.Serial,
		Lineage:          meta.Lineage,
		State:            state,
	}, true
}

// stateSnapshotBodyStart is what begins the part of a state snapshot that
// describes the state itself, following the metadata, in the form that
// [statefile.Write] produces. A JSON string can't contain a newline, so this
// can't appear inside the metadata.
var stateSnapshotBodyStart = []byte("\n  \"outputs\": ")

// emptyStateSnapshotBody is the body of a snapshot of an empty state.
var emptyStateSnapshotBody = []byte("\n  \"outputs\": {},\n  \"resources\": [],\n  \"check_results\": null\n}\n")

// splitStateSnapshot splits the source of a state snapshot into its metadata
// and the rest, if it has the form that [statefile.Write] produces.
func splitStateSnapshot(raw []byte) (header, body []byte, ok bool) {
	// The metadata is always small.
	at := bytes.Index(raw[:min(len(raw), 4096)], stateSnapshotBodyStart)
	if at < 0 {
		return nil, nil, false
	}
	return raw[:at], raw[at:], true
}

// isStateSnapshotHeader returns true if the given header, as returned by
// splitStateSnapshot, is exactly what [statefile.Write] produces before the
// body of a snapshot that has the metadata of the given file.
func isStateSnapshotHeader(header []byte, file *statefile.File) bool {
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
	want, _, ok := splitStateSnapshot(buf.Bytes())
	return ok && bytes.Equal(header, want)
}

// parsedStateSnapshot is the result of parsing a state snapshot, which
// becomes available once done is closed.
type parsedStateSnapshot struct {
	// raw is the source of the snapshot, which is available once rawReady
	// is closed, unless reading it failed, and until the other snapshot
	// has been compared with it.
	rawReady chan struct{}
	raw      []byte

	// file and err are the result, available once done is closed.
	done chan struct{}
	file *statefile.File
	err  error
}

// errReader is a reader that always fails with the given error.
type errReader struct {
	err error
}

func (r errReader) Read([]byte) (int, error) {
	return 0, r.err
}

// ShareStateDataFrom arranges for the state snapshots read from the plan
// file to use the same memory as the given state for the data of any
// resource instance object that is exactly the same in both, rather than
// each retaining its own copy.
//
// When applying a saved plan the caller has typically also loaded the latest
// state snapshot, which the snapshots in the plan file were derived from and
// so mostly match. For a large state this avoids keeping several copies of
// nearly the same data in memory. It has no effect on the content of the
// snapshots returned by the other methods.
//
// The given state must not be modified afterwards.
func (r *Reader) ShareStateDataFrom(state *states.State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shareFrom = state
	r.shareStateData()
}

// shareStateData makes the state snapshots that have been read so far share
// as much data as possible, with the state given to ShareStateDataFrom and
// with each other. The caller must hold r.mu.
func (r *Reader) shareStateData() {
	for _, f := range []*embeddedStateFile{&r.priorState, &r.prevRunState} {
		if f.loaded.Load() && f.file != nil && r.shareFrom != nil && f.sharedFrom != r.shareFrom {
			f.file.State.ShareUnchangedDataFrom(r.shareFrom)
			f.sharedFrom = r.shareFrom
		}
	}
	if !r.prevRunSharesPrior && r.priorState.loaded.Load() && r.prevRunState.loaded.Load() {
		// These two are identical for any object that didn't change when
		// it was refreshed, which is typically almost all of them.
		if r.priorState.file != nil && r.prevRunState.file != nil {
			r.prevRunState.file.State.ShareUnchangedDataFrom(r.priorState.file.State)
		}
		r.prevRunSharesPrior = true
	}
}

// embeddedStateFile is the result of reading one of the state snapshots
// embedded in a plan file.
type embeddedStateFile struct {
	once sync.Once
	file *statefile.File
	err  error

	// loaded is set once file and err have their final values.
	loaded atomic.Bool

	// sharedFrom is the state that the snapshot was most recently made to
	// share data with by Reader.shareStateData, if any.
	sharedFrom *states.State
}

// read returns a copy of the state snapshot with the given filename, reading
// it from the plan file first if this is the first request for it.
func (f *embeddedStateFile) read(r *Reader, filename, description string) (*statefile.File, error) {
	f.once.Do(func() {
		for _, file := range r.zip.File {
			if file.Name == filename {
				f.file, f.err = r.readStateSnapshot(file, description)
				return
			}
		}
		f.err = errUnusable(statefile.ErrNoState)
	})
	f.loaded.Store(true)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.shareStateData()
	return f.file.DeepCopy(), f.err
}

// Open creates a Reader for the file at the given filename, or returns an error
// if the file doesn't seem to be a planfile. NOTE: Most commands that accept a
// plan file should use OpenWrapped instead, so they can support both local and
// cloud plan files.
func Open(filename string) (*Reader, error) {
	r, err := zip.OpenReader(filename)
	if err != nil {
		// To give a better error message, we'll sniff to see if this looks
		// like our old plan format from versions prior to 0.12.
		if b, sErr := os.ReadFile(filename); sErr == nil {
			if bytes.HasPrefix(b, []byte("tfplan")) {
				return nil, errUnusable(fmt.Errorf("the given plan file was created by an earlier version of Terraform; plan files cannot be shared between different Terraform versions"))
			}
		}
		return nil, err
	}

	// Sniff to make sure this looks like a plan file, as opposed to any other
	// random zip file the user might have around.
	var planFile *zip.File
	for _, file := range r.File {
		if file.Name == tfplanFilename {
			planFile = file
			break
		}
	}
	if planFile == nil {
		return nil, fmt.Errorf("the given file is not a valid plan file")
	}

	// For now, we'll just accept the presence of the tfplan file as enough,
	// and wait to validate the version when the caller requests the plan
	// itself.

	return &Reader{
		zip: r,
	}, nil
}

// ReadPlan reads the plan embedded in the plan file.
//
// Errors can be returned for various reasons, including if the plan file
// is not of an appropriate format version, if it was created by a different
// version of Terraform, if it is invalid, etc.
func (r *Reader) ReadPlan() (*plans.Plan, error) {
	var planFile *zip.File
	for _, file := range r.zip.File {
		if file.Name == tfplanFilename {
			planFile = file
			break
		}
	}
	if planFile == nil {
		// This should never happen because we checked for this file during
		// Open, but we'll check anyway to be safe.
		return nil, errUnusable(fmt.Errorf("the plan file is invalid"))
	}

	pr, err := planFile.Open()
	if err != nil {
		return nil, errUnusable(fmt.Errorf("failed to retrieve plan from plan file: %s", err))
	}
	defer pr.Close()

	// There's a slight mismatch in how plans.Plan is modeled vs. how
	// the underlying plan file format works, because the "tfplan" embedded
	// file contains only some top-level metadata and the planned changes,
	// and not the previous run or prior states. Therefore we need to
	// build this up in multiple steps.
	// This is some technical debt because historically we considered the
	// planned changes and prior state as totally separate, but later realized
	// that it made sense for a plans.Plan to include the prior state directly
	// so we can see what state the plan applies to. Hopefully later we'll
	// clean this up some more so that we don't have two different ways to
	// access the prior state (this and the ReadStateFile method).
	//
	// The three parts are independent of one another and each can take a
	// significant amount of time to read for a large configuration, so we
	// read the two state snapshots in the background while reading the
	// planned changes.
	var prevRunStateFile, priorStateFile *statefile.File
	var prevRunStateErr, priorStateErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		prevRunStateFile, prevRunStateErr = r.ReadPrevStateFile()
	}()
	go func() {
		defer wg.Done()
		priorStateFile, priorStateErr = r.ReadStateFile()
	}()
	// A single operation typically reads the plan more than once, and
	// parsing the planned changes is expensive for a large plan, so we
	// parse them only the first time.
	r.rawPlanOnce.Do(func() {
		r.rawPlan, r.rawPlanErr = readRawTfplan(pr)
	})
	var ret *plans.Plan
	err = r.rawPlanErr
	if err == nil {
		ret, err = planFromRawTfplan(r.rawPlan)
	}
	wg.Wait()
	if err != nil {
		return nil, errUnusable(err)
	}
	if err := prevRunStateErr; err != nil {
		return nil, errUnusable(fmt.Errorf("failed to read previous run state from plan file: %s", err))
	}
	if err := priorStateErr; err != nil {
		return nil, errUnusable(fmt.Errorf("failed to read prior state from plan file: %s", err))
	}

	ret.PrevRunState = prevRunStateFile.State
	ret.PriorState = priorStateFile.State

	return ret, nil
}

// ReadStateFile reads the state file embedded in the plan file, which
// represents the "PriorState" as defined in plans.Plan.
//
// If the plan file contains no embedded state file, the returned error is
// statefile.ErrNoState.
func (r *Reader) ReadStateFile() (*statefile.File, error) {
	return r.priorState.read(r, tfstateFilename, "state")
}

// ReadPrevStateFile reads the previous state file embedded in the plan file, which
// represents the "PrevRunState" as defined in plans.Plan.
//
// If the plan file contains no embedded previous state file, the returned error is
// statefile.ErrNoState.
func (r *Reader) ReadPrevStateFile() (*statefile.File, error) {
	return r.prevRunState.read(r, tfstatePreviousFilename, "previous state")
}

// ReadConfigSnapshot reads the configuration snapshot embedded in the plan
// file.
//
// This is a lower-level alternative to ReadConfig that just extracts the
// source files, without attempting to parse them.
func (r *Reader) ReadConfigSnapshot() (*configload.Snapshot, error) {
	return ReadConfigSnapshot(&r.zip.Reader)
}

// ReadDependencyLocks reads the dependency lock information embedded in
// the plan file.
//
// Some test codepaths create plan files without dependency lock information,
// but all main codepaths should populate this. If reading a file without
// the dependency information, this will return error diagnostics.
func (r *Reader) ReadDependencyLocks() (*depsfile.Locks, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	for _, file := range r.zip.File {
		if file.Name == dependencyLocksFilename {
			r, err := file.Open()
			if err != nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Failed to read dependency locks from plan file",
					fmt.Sprintf("Couldn't read the dependency lock information embedded in the plan file: %s.", err),
				))
				return nil, diags
			}
			src, err := io.ReadAll(r)
			if err != nil {
				diags = diags.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Failed to read dependency locks from plan file",
					fmt.Sprintf("Couldn't read the dependency lock information embedded in the plan file: %s.", err),
				))
				return nil, diags
			}
			locks, moreDiags := depsfile.LoadLocksFromBytes(src, "<saved-plan>")
			diags = diags.Append(moreDiags)
			return locks, diags
		}
	}

	// If we fall out here then this is a file without dependency information.
	diags = diags.Append(tfdiags.Sourceless(
		tfdiags.Error,
		"Saved plan has no dependency lock information",
		"The specified saved plan file does not include any dependency lock information. This is a bug in the previous run of Terraform that created this file.",
	))
	return nil, diags
}

// Close closes the file, after which no other operations may be performed.
func (r *Reader) Close() error {
	return r.zip.Close()
}
