// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package gcs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	"github.com/hashicorp/terraform/internal/states/remote"
	"github.com/hashicorp/terraform/internal/states/statemgr"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

// singleRequestUploadAttempts is the number of times that Put tries to
// upload a large state in a single request before falling back on a chunked
// upload, and singleRequestUploadBackoff is how much longer it waits before
// each successive attempt. They're variables only so that tests can change
// them.
var (
	singleRequestUploadAttempts = 3
	singleRequestUploadBackoff  = time.Second
)

// compressStateEnvVar is the environment variable that, when set to a true
// value, makes the client store state snapshots compressed.
//
// A state is JSON with a great deal of repetition, so it typically
// compresses to a few percent of its size, which matters when it's hundreds
// of megabytes and has to be transferred at the start and end of every
// operation. It's off by default because it changes how the object is
// stored, which affects anything else that reads the bucket directly.
// Compressed snapshots are always readable, whether or not this is set.
const compressStateEnvVar = "TF_GCS_STATE_GZIP"

func compressStateEnabled() bool {
	enabled, _ := strconv.ParseBool(os.Getenv(compressStateEnvVar))
	return enabled
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(data)/16 + 1024)
	// The fastest level already removes nearly all of the repetition.
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var buf bytes.Buffer
	// A guess at the size, to limit how often the buffer has to grow.
	buf.Grow(len(data) * 16)
	if _, err := buf.ReadFrom(zr); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// A state object of at least parallelDownloadMinSize bytes is downloaded in
// up to parallelDownloadParts parts at once, each of at least
// parallelDownloadMinPart bytes. They're variables only so that tests can
// change them.
var (
	parallelDownloadMinSize int64 = 64 << 20
	parallelDownloadMinPart int64 = 16 << 20
	parallelDownloadParts         = 8
)

// readStateFile returns the content of the object that the given reader
// has just been opened on, as it's stored.
func readStateFile(r *storage.Reader) ([]byte, error) {
	// The size is only a hint, because an object that's stored compressed
	// is larger once it has been read, but it's nearly always right and
	// saves repeatedly growing and copying the buffer for a large state.
	contents := make([]byte, 0, max(r.Attrs.Size, 0)+bytes.MinRead)
	for {
		if len(contents) == cap(contents) {
			contents = append(contents, 0)[:len(contents)]
		}
		n, err := r.Read(contents[len(contents):cap(contents)])
		contents = contents[:len(contents)+n]
		if err != nil {
			if err == io.EOF {
				return contents, nil
			}
			return nil, err
		}
	}
}

// readInParts returns true if an object with the given attributes, which
// are those of a reader that has just been opened on it, is one to read
// with readStateFileInParts.
func readInParts(attrs storage.ReaderObjectAttrs) bool {
	// Without a generation we can't be sure of reading parts of the same
	// object, and without a checksum we can't verify the result.
	return attrs.Size >= parallelDownloadMinSize && attrs.Size/parallelDownloadMinPart >= 2 &&
		attrs.Generation > 0 && attrs.CRC32C != 0
}

// readStateFileInParts reads a large object, described by the given
// attributes, as several ranges at once. It returns false if that didn't
// produce the object, in which case the caller must read it as one stream
// instead.
//
// A single download from Cloud Storage is typically limited to some tens of
// megabytes per second however fast the network is, so a state of hundreds
// of megabytes takes many seconds to arrive at the start of every operation,
// and the ranges of one object can be read at the same time.
//
// The ranges all ask for the generation that the attributes are of, so they
// can only be parts of the same snapshot, and the result is only
// used if its checksum is the one that the service reported for the whole
// object. Anything else, including any failure to read a range, leaves the
// object to be read in the ordinary way.
func (c *remoteClient) readStateFileInParts(ctx context.Context, attrs storage.ReaderObjectAttrs) ([]byte, bool) {
	size, generation, wantCRC := attrs.Size, attrs.Generation, attrs.CRC32C
	parts := int(min(int64(parallelDownloadParts), size/parallelDownloadMinPart))
	if parts < 2 {
		return nil, false
	}
	partSize := (size + int64(parts) - 1) / int64(parts)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	obj := c.stateFile().Generation(generation).ReadCompressed(true)
	contents := make([]byte, size)
	errs := make(chan error, parts)
	for part := range parts {
		start := int64(part) * partSize
		end := min(start+partSize, size)
		go func() {
			errs <- func() error {
				pr, err := obj.NewRangeReader(ctx, start, end-start)
				if err != nil {
					return err
				}
				defer pr.Close()
				if _, err := io.ReadFull(pr, contents[start:end]); err != nil {
					return err
				}
				// There must be nothing more in the range than we asked for.
				var extra [1]byte
				if n, _ := pr.Read(extra[:]); n != 0 {
					return errors.New("range was longer than requested")
				}
				return nil
			}()
		}()
	}
	var failed error
	for range parts {
		// We wait for all of them even after a failure, so that nothing
		// is still writing to the buffer when we return.
		if err := <-errs; err != nil && failed == nil {
			failed = err
			cancel()
		}
	}
	if failed != nil {
		log.Printf("[DEBUG] gcs: reading the state in parts failed, so reading it whole: %s", failed)
		return nil, false
	}
	if gotCRC := crc32.Checksum(contents, crc32cTable); gotCRC != wantCRC {
		log.Printf("[WARN] gcs: the state read in parts has checksum %08x but the object has %08x, so reading it whole", gotCRC, wantCRC)
		return nil, false
	}
	return contents, true
}

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// remoteClient is used by "state/remote".State to read and write
// blobs representing state.
// Implements "state/remote".ClientLocker
type remoteClient struct {
	storageClient *storage.Client
	bucketName    string
	stateFilePath string
	lockFilePath  string
	encryptionKey []byte
	kmsKeyName    string
}

func (c *remoteClient) Get() (payload *remote.Payload, diags tfdiags.Diagnostics) {
	ctx := context.TODO()
	// We ask for the object as it's stored, so that if it's stored compressed
	// then that's how it's transferred, and we decompress it ourselves below.
	stateFileReader, err := c.stateFile().ReadCompressed(true).NewReader(ctx)
	if err != nil {
		if err == storage.ErrObjectNotExist {
			return nil, diags
		} else {
			return nil, diags.Append(fmt.Errorf("Failed to open state file at %v: %v", c.stateFileURL(), err))
		}
	}
	// These describe the version of the object that we go on to read,
	// which is the one this reader was opened on unless we have to open
	// another below.
	stateFileReaderAttrs := stateFileReader.Attrs

	var stateFileContents []byte
	read := false
	if readInParts(stateFileReaderAttrs) {
		// We read the parts with requests of their own, so we're not
		// going to read from this one, and we don't leave it open with
		// the object arriving on it unread.
		stateFileReader.Close()
		stateFileContents, read = c.readStateFileInParts(ctx, stateFileReaderAttrs)
		if !read {
			// Start again, reading it as one stream.
			stateFileReader, err = c.stateFile().ReadCompressed(true).NewReader(ctx)
			if err != nil {
				if err == storage.ErrObjectNotExist {
					return nil, diags
				}
				return nil, diags.Append(fmt.Errorf("Failed to open state file at %v: %v", c.stateFileURL(), err))
			}
			stateFileReaderAttrs = stateFileReader.Attrs
		}
	}
	if !read {
		stateFileContents, err = readStateFile(stateFileReader)
		stateFileReader.Close()
		if err != nil {
			return nil, diags.Append(fmt.Errorf("Failed to read state file from %v: %v", c.stateFileURL(), err))
		}
	}

	if stateFileReaderAttrs.ContentEncoding == "gzip" {
		stateFileContents, err = gunzip(stateFileContents)
		if err != nil {
			return nil, diags.Append(fmt.Errorf("Failed to decompress state file from %v: %v", c.stateFileURL(), err))
		}
	}

	stateFileAttrs, err := c.stateFile().Attrs(ctx)
	if err != nil {
		return nil, diags.Append(fmt.Errorf("Failed to read state file attrs from %v: %v", c.stateFileURL(), err))
	}

	result := &remote.Payload{
		Data: stateFileContents,
		MD5:  stateFileAttrs.MD5,
	}
	// Every version of an object has a different generation number, so
	// that of the version we actually read identifies this snapshot.
	if generation := stateFileReaderAttrs.Generation; generation > 0 {
		result.Revision = strconv.FormatInt(generation, 10)
	}

	return result, diags
}

// CurrentRevision implements remote.ClientRevisioner, using the generation
// number of the state object, which can be found without retrieving its
// content.
func (c *remoteClient) CurrentRevision() (string, error) {
	attrs, err := c.stateFile().Attrs(context.TODO())
	if err != nil {
		if err == storage.ErrObjectNotExist {
			return "", nil
		}
		return "", err
	}
	if attrs.Generation <= 0 {
		return "", nil
	}
	return strconv.FormatInt(attrs.Generation, 10), nil
}

func (c *remoteClient) Put(data []byte) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	ctx := context.TODO()
	compressed := false
	if compressStateEnabled() {
		// Stored like this, the object is still served decompressed to any
		// client that doesn't ask for it compressed, which includes other
		// versions of Terraform.
		gz, err := gzipBytes(data)
		if err != nil {
			return diags.Append(fmt.Errorf("Failed to compress state for %v: %v", c.stateFileURL(), err))
		}
		data, compressed = gz, true
	}
	upload := func(chunkSize int) error {
		stateFileWriter := c.stateFile().NewWriter(ctx)
		stateFileWriter.ChunkSize = chunkSize
		if compressed {
			stateFileWriter.ContentEncoding = "gzip"
			stateFileWriter.ContentType = "application/json"
		}
		if len(c.kmsKeyName) > 0 {
			stateFileWriter.KMSKeyName = c.kmsKeyName
		}
		if _, err := stateFileWriter.Write(data); err != nil {
			// Closing is what releases the resources of a failed upload.
			stateFileWriter.Close()
			return err
		}
		return stateFileWriter.Close()
	}

	var err error
	if len(data) <= googleapi.DefaultUploadChunkSize {
		err = upload(googleapi.DefaultUploadChunkSize)
	} else {
		// By default the client library uploads in chunks, each of which is
		// a separate request that must complete before the next begins.
		// For a large state that takes many times longer than sending the
		// same data in one request, which is what a chunk size of zero
		// asks for. The library can't retry such an upload itself, because
		// it doesn't keep what it has sent, but we still have all of the
		// data, so we retry the whole upload ourselves for the same kinds
		// of error that it would have retried. If that keeps failing then
		// we fall back on the chunked upload, which can make progress over
		// a connection that isn't reliable enough for a single request.
		for attempt := 0; attempt < singleRequestUploadAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(time.Duration(attempt) * singleRequestUploadBackoff)
			}
			if err = upload(0); err == nil || !storage.ShouldRetry(err) {
				break
			}
		}
		if err != nil && storage.ShouldRetry(err) {
			err = upload(googleapi.DefaultUploadChunkSize)
		}
	}
	if err != nil {
		return diags.Append(fmt.Errorf("Failed to upload state to %v: %v", c.stateFileURL(), err))
	}

	return diags
}

func (c *remoteClient) Delete() tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	ctx := context.TODO()
	if err := c.stateFile().Delete(ctx); err != nil {
		return diags.Append(fmt.Errorf("Failed to delete state file %v: %v", c.stateFileURL(), err))
	}

	return diags
}

// Lock writes to a lock file, ensuring file creation. Returns the generation
// number, which must be passed to Unlock().
func (c *remoteClient) Lock(info *statemgr.LockInfo) (string, error) {
	ctx := context.TODO()

	// update the path we're using
	// we can't set the ID until the info is written
	info.Path = c.lockFileURL()

	infoJson, err := json.Marshal(info)
	if err != nil {
		return "", err
	}

	lockFile := c.lockFile()
	w := lockFile.If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	err = func() error {
		if _, err := w.Write(infoJson); err != nil {
			return err
		}
		return w.Close()
	}()

	if err != nil {
		return "", c.lockError(fmt.Errorf("writing %q failed: %v", c.lockFileURL(), err))
	}

	info.ID = strconv.FormatInt(w.Attrs().Generation, 10)

	return info.ID, nil
}

func (c *remoteClient) Unlock(id string) error {
	ctx := context.TODO()

	gen, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("Lock ID should be numerical value, got '%s'", id)
	}

	if err := c.lockFile().If(storage.Conditions{GenerationMatch: gen}).Delete(ctx); err != nil {
		return c.lockError(err)
	}

	return nil
}

func (c *remoteClient) lockError(err error) *statemgr.LockError {
	lockErr := &statemgr.LockError{
		Err: err,
	}

	info, infoErr := c.lockInfo()
	if infoErr != nil {
		lockErr.Err = errors.Join(lockErr.Err, infoErr)
	} else {
		lockErr.Info = info
	}
	return lockErr
}

// lockInfo reads the lock file, parses its contents and returns the parsed
// LockInfo struct.
func (c *remoteClient) lockInfo() (*statemgr.LockInfo, error) {
	ctx := context.TODO()

	r, err := c.lockFile().NewReader(ctx)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	rawData, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	info := &statemgr.LockInfo{}
	if err := json.Unmarshal(rawData, info); err != nil {
		return nil, err
	}

	// We use the Generation as the ID, so overwrite the ID in the json.
	// This can't be written into the Info, since the generation isn't known
	// until it's written.
	attrs, err := c.lockFile().Attrs(ctx)
	if err != nil {
		return nil, err
	}
	info.ID = strconv.FormatInt(attrs.Generation, 10)

	return info, nil
}

func (c *remoteClient) stateFile() *storage.ObjectHandle {
	h := c.storageClient.Bucket(c.bucketName).Object(c.stateFilePath)
	if len(c.encryptionKey) > 0 {
		return h.Key(c.encryptionKey)
	}
	return h
}

func (c *remoteClient) stateFileURL() string {
	return fmt.Sprintf("gs://%v/%v", c.bucketName, c.stateFilePath)
}

func (c *remoteClient) lockFile() *storage.ObjectHandle {
	return c.storageClient.Bucket(c.bucketName).Object(c.lockFilePath)
}

func (c *remoteClient) lockFileURL() string {
	return fmt.Sprintf("gs://%v/%v", c.bucketName, c.lockFilePath)
}
