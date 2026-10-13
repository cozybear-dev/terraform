// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	version "github.com/hashicorp/go-version"

	"github.com/hashicorp/terraform/internal/tfdiags"
	tfversion "github.com/hashicorp/terraform/version"
)

// ErrNoState is returned by ReadState when the state file is empty.
var ErrNoState = errors.New("no state")

// ErrUnusableState is an error wrapper to indicate that we *think* the input
// represents state data, but can't use it for some reason (as explained in the
// error text). Callers can check against this type with errors.As() if they
// need to distinguish between corrupt state and more fundamental problems like
// an empty file.
type ErrUnusableState struct {
	inner error
}

func errUnusable(err error) *ErrUnusableState {
	return &ErrUnusableState{inner: err}
}

func (e *ErrUnusableState) Error() string {
	return e.inner.Error()
}

func (e *ErrUnusableState) Unwrap() error {
	return e.inner
}

// Read reads a state from the given reader.
//
// Legacy state format versions 1 through 3 are supported, but the result will
// contain object attributes in the deprecated "flatmap" format and so must
// be upgraded by the caller before use.
//
// If the state file is empty, the special error value ErrNoState is returned.
// Otherwise, the returned error might be a wrapper around tfdiags.Diagnostics
// potentially describing multiple errors.
func Read(r io.Reader) (*File, error) {
	// Some callers provide us a "typed nil" *os.File here, which would
	// cause us to panic below if we tried to use it.
	if f, ok := r.(*os.File); ok && f == nil {
		return nil, ErrNoState
	}

	var diags tfdiags.Diagnostics

	// We actually just buffer the whole thing in memory, because states are
	// generally not huge and we need to do be able to sniff for a version
	// number before full parsing.
	src, err := readAll(r)
	if err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to read state file",
			fmt.Sprintf("The state file could not be read: %s", err),
		))
		return nil, diags.Err()
	}

	return ReadBytes(src)
}

// ReadBytes is like Read but for a caller that already has the entire content
// of the state file in memory, which avoids making another copy of it.
//
// The result doesn't retain any reference to the given buffer.
func ReadBytes(src []byte) (*File, error) {
	if len(src) == 0 {
		return nil, ErrNoState
	}

	state, err := readState(src)
	if err != nil {
		return nil, err
	}

	if state == nil {
		// Should never happen
		panic("readState returned nil state with no errors")
	}

	return state, nil
}

// readAll is like io.ReadAll except that, where it can determine how much
// data there is to read, it allocates a buffer of the right size up front
// rather than repeatedly growing and copying a smaller one, which is
// significant for a large state file.
func readAll(r io.Reader) ([]byte, error) {
	size := 0
	switch r := r.(type) {
	case *os.File:
		if info, err := r.Stat(); err == nil && info.Mode().IsRegular() {
			if offset, err := r.Seek(0, io.SeekCurrent); err == nil && offset >= 0 && offset < info.Size() {
				size = int(info.Size() - offset)
			}
		}
	case interface{ Len() int }:
		// As implemented by bytes.Reader, bytes.Buffer, and strings.Reader.
		size = r.Len()
	}
	if size <= 0 {
		return io.ReadAll(r)
	}

	// One extra byte of capacity allows the final read to report EOF
	// without us needing to grow the buffer.
	buf := readBuffer(size + 1)
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return buf, err
		}
	}
}

func readState(src []byte) (*File, error) {
	var diags tfdiags.Diagnostics

	if looksLikeVersion0(src) {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			unsupportedFormat,
			"The state is stored in a legacy binary format that is not supported since Terraform v0.7. To continue, first upgrade the state using Terraform 0.6.16 or earlier.",
		))
		return nil, errUnusable(diags.Err())
	}

	if file, ok := readStateV4Parallel(src); ok {
		return file, nil
	}

	version, versionDiags := sniffJSONStateVersion(src)
	diags = diags.Append(versionDiags)
	if versionDiags.HasErrors() {
		// This is the last point where there's a really good chance it's not a
		// state file at all. Past here, we'll assume errors mean it's state but
		// we can't use it.
		return nil, diags.Err()
	}

	var result *File
	var err error
	switch version {
	case 0:
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			unsupportedFormat,
			"The state file uses JSON syntax but has a version number of zero. There was never a JSON-based state format zero, so this state file is invalid and cannot be processed.",
		))
	case 1:
		result, diags = readStateV1(src)
	case 2:
		result, diags = readStateV2(src)
	case 3:
		result, diags = readStateV3(src)
	case 4:
		result, diags = readStateV4(src)
	default:
		thisVersion := tfversion.SemVer.String()
		creatingVersion := sniffJSONStateTerraformVersion(src)
		switch {
		case creatingVersion != "":
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				unsupportedFormat,
				fmt.Sprintf("The state file uses format version %d, which is not supported by Terraform %s. This state file was created by Terraform %s.", version, thisVersion, creatingVersion),
			))
		default:
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				unsupportedFormat,
				fmt.Sprintf("The state file uses format version %d, which is not supported by Terraform %s. This state file may have been created by a newer version of Terraform.", version, thisVersion),
			))
		}
	}

	if diags.HasErrors() {
		err = errUnusable(diags.Err())
	}

	return result, err
}

func sniffJSONStateVersion(src []byte) (uint64, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	type VersionSniff struct {
		Version *uint64 `json:"version"`
	}
	var sniff VersionSniff
	err := json.Unmarshal(src, &sniff)
	if err != nil {
		switch tErr := err.(type) {
		case *json.SyntaxError:
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				unsupportedFormat,
				fmt.Sprintf("The state file could not be parsed as JSON: syntax error at byte offset %d.", tErr.Offset),
			))
		case *json.UnmarshalTypeError:
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				unsupportedFormat,
				fmt.Sprintf("The version in the state file is %s. A positive whole number is required.", tErr.Value),
			))
		default:
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				unsupportedFormat,
				"The state file could not be parsed as JSON.",
			))
		}
	}

	if sniff.Version == nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			unsupportedFormat,
			"The state file does not have a \"version\" attribute, which is required to identify the format version.",
		))
		return 0, diags
	}

	return *sniff.Version, diags
}

// sniffJSONStateTerraformVersion attempts to sniff the Terraform version
// specification from the given state file source code. The result is either
// a version string or an empty string if no version number could be extracted.
//
// This is a best-effort function intended to produce nicer error messages. It
// should not be used for any real processing.
func sniffJSONStateTerraformVersion(src []byte) string {
	type VersionSniff struct {
		Version string `json:"terraform_version"`
	}
	var sniff VersionSniff

	err := json.Unmarshal(src, &sniff)
	if err != nil {
		return ""
	}

	// Attempt to parse the string as a version so we won't report garbage
	// as a version number.
	_, err = version.NewVersion(sniff.Version)
	if err != nil {
		return ""
	}

	return sniff.Version
}

// unsupportedFormat is a diagnostic summary message for when the state file
// seems to not be a state file at all, or is not a supported version.
//
// Use invalidFormat instead for the subtly-different case of "this looks like
// it's intended to be a state file but it's not structured correctly".
const unsupportedFormat = "Unsupported state file format"

const upgradeFailed = "State format upgrade failed"

// readBuffers holds buffers given to RecycleReadBuffer, as pointers to
// byte slices. It never holds many, and whatever isn't reused is released
// when the garbage collector next runs.
var readBuffers sync.Pool

// minRecycledReadBuffer is the size of buffer below which recycling isn't
// worthwhile.
const minRecycledReadBuffer = 1 << 20

// RecycleReadBuffer offers a buffer that the caller has finished with, for
// reuse when Read next needs one of about the same size. The caller must not
// use the buffer again afterwards.
//
// This is for a caller that reads the source of a large state itself and
// passes it to ReadBytes, when another state of similar size is likely to be
// read soon afterwards, as when applying a saved plan. Each source is as
// large as the state it describes and is needed only briefly, but without
// this they would all exist at once until the garbage collector next runs.
func RecycleReadBuffer(buf []byte) {
	if cap(buf) < minRecycledReadBuffer {
		return
	}
	buf = buf[:0]
	readBuffers.Put(&buf)
}

// readBuffer returns an empty buffer with at least the given capacity.
func readBuffer(size int) []byte {
	if size >= minRecycledReadBuffer {
		if recycled, ok := readBuffers.Get().(*[]byte); ok {
			// One that's much too large would waste more than it saves.
			if buf := *recycled; cap(buf) >= size && cap(buf)-size <= size/4 {
				return buf[:0]
			}
		}
	}
	return make([]byte, 0, size)
}
