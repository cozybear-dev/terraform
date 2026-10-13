// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/parallel"
)

// parallelEncodeMinInstances is the number of resource instance objects
// below which encodeStateV4 just uses the standard library JSON encoder
// directly, because there is too little work to benefit from distributing it.
const parallelEncodeMinInstances = 64

var (
	emptyResourcesV4 = []byte("\n  \"resources\": [],\n")
	emptyInstancesV4 = []byte("\n      \"instances\": []\n    }")
)

// stateWriteError is the type of error returned by encodeStateV4 when the
// problem was with writing the result, rather than with producing it.
type stateWriteError struct {
	error
}

// encodeStateV4 writes to w exactly what json.MarshalIndent(sV4, "", "  ")
// would produce, followed by a newline.
//
// A state snapshot is dominated by its resource instance objects, which are
// all independent of one another, so when there are many of them this
// encodes them concurrently and then stitches the results together in the
// original order.
func encodeStateV4(sV4 *stateV4, w io.Writer) error {
	instanceCount := 0
	for i := range sV4.Resources {
		instanceCount += len(sV4.Resources[i].Instances)
	}
	if instanceCount < parallelEncodeMinInstances {
		return encodeStateV4Simple(sV4, w)
	}

	// The document without any resources, which we'll split at the point
	// where the resources belong.
	outer := *sV4
	outer.Resources = []resourceStateV4{}
	outerSrc, err := json.MarshalIndent(&outer, "", "  ")
	if err != nil {
		return err
	}
	if bytes.Count(outerSrc, emptyResourcesV4) != 1 {
		// Should not be possible, because all other content is nested at
		// least one level deeper, but we'll be robust.
		return encodeStateV4Simple(sV4, w)
	}
	resourcesAt := bytes.Index(outerSrc, emptyResourcesV4)

	// Each resource is similarly encoded without its instances, and then
	// each of its instances is encoded separately.
	type job struct {
		resource, instance int
	}
	jobs := make([]job, 0, len(sV4.Resources)+instanceCount)
	for i := range sV4.Resources {
		jobs = append(jobs, job{i, -1})
		for j := range sV4.Resources[i].Instances {
			jobs = append(jobs, job{i, j})
		}
	}

	bw := bufio.NewWriterSize(w, 1<<20)
	bw.Write(outerSrc[:resourcesAt])
	bw.WriteString("\n  \"resources\": [\n")

	// We encode a limited number of objects at a time and write the results
	// before continuing, so that we don't need to hold the encoded form of
	// the entire state in memory at once.
	const window = 4096
	type encodedWindow struct {
		jobs []job
		srcs [][]byte
	}
	// The results are written by a separate goroutine, so that encoding the
	// next window can proceed in the meantime.
	windows := make(chan encodedWindow, 1)
	written := make(chan struct{})
	go func() {
		defer close(written)
		for encoded := range windows {
			for i, job := range encoded.jobs {
				src := encoded.srcs[i]
				rs := &sV4.Resources[job.resource]
				if job.instance < 0 {
					// The start of a new resource.
					if job.resource > 0 {
						bw.WriteString(",\n")
					}
					bw.WriteString("    ")
					if len(rs.Instances) == 0 {
						bw.Write(src)
						continue
					}
					bw.Write(src[:len(src)-len(emptyInstancesV4)])
					bw.WriteString("\n      \"instances\": [\n")
					continue
				}

				if job.instance > 0 {
					bw.WriteString(",\n")
				}
				bw.WriteString("        ")
				bw.Write(src)
				recycleInstanceSrc(src)
				if job.instance == len(rs.Instances)-1 {
					bw.WriteString("\n      ]\n    }")
				}
			}
		}
	}()

	var dependencySrcs sync.Map
	var failed atomic.Bool
	for start := 0; start < len(jobs) && !failed.Load(); start += window {
		pending := jobs[start:min(start+window, len(jobs))]
		srcs := make([][]byte, len(pending))
		parallel.For(len(pending), 16, func(i int) {
			if failed.Load() {
				return
			}
			job := pending[i]
			rs := &sV4.Resources[job.resource]
			var src []byte
			var err error
			switch {
			case job.instance >= 0:
				src, err = marshalInstanceObjectV4(&rs.Instances[job.instance], &dependencySrcs)
			case len(rs.Instances) == 0:
				src, err = json.MarshalIndent(rs, "    ", "  ")
			default:
				header := *rs
				header.Instances = []instanceObjectStateV4{}
				src, err = json.MarshalIndent(&header, "    ", "  ")
				if err == nil && !bytes.HasSuffix(src, emptyInstancesV4) {
					failed.Store(true)
				}
			}
			if err != nil {
				failed.Store(true)
			}
			srcs[i] = src
		})
		if !failed.Load() {
			windows <- encodedWindow{pending, srcs}
		}
	}
	close(windows)
	<-written
	if failed.Load() {
		// This should happen only if the state contains something that
		// can't be encoded at all, in which case the simple implementation
		// will produce the appropriate error.
		_, err := json.MarshalIndent(sV4, "", "  ")
		if err == nil {
			err = errors.New("inconsistent result when encoding state")
		}
		return err
	}

	bw.WriteString("\n  ],\n")
	bw.Write(outerSrc[resourcesAt+len(emptyResourcesV4):])
	bw.WriteByte('\n')
	if err := bw.Flush(); err != nil {
		return stateWriteError{err}
	}
	return nil
}

func encodeStateV4Simple(sV4 *stateV4, w io.Writer) error {
	src, err := json.MarshalIndent(sV4, "", "  ")
	if err != nil {
		return err
	}
	src = append(src, '\n')
	if _, err := w.Write(src); err != nil {
		return stateWriteError{err}
	}
	return nil
}

var (
	instanceEndV4          = []byte("\n        }")
	instanceCBDV4          = []byte(",\n          \"create_before_destroy\": true")
	instanceDependenciesV4 = []byte(",\n          \"dependencies\": ")
)

// marshalInstanceObjectV4 returns exactly what
// json.MarshalIndent(is, "        ", "  ") would return.
//
// The dependencies of an object can account for much of its size, and many
// objects typically have exactly the same dependencies, sharing a single
// slice of strings. We therefore encode each distinct slice of dependencies
// only once, remembering the result in dependencySrcs, and then insert it
// into the encoding of the rest of each object that has those dependencies.
func marshalInstanceObjectV4(is *instanceObjectStateV4, dependencySrcs *sync.Map) ([]byte, error) {
	// The serialized attributes are nearly all of the content, and they
	// are already JSON, so we format them ourselves and encode the rest
	// around a placeholder that we then replace with the result.
	if len(is.AttributesRaw) < minDirectAttributesV4 {
		return marshalInstanceObjectV4Standard(is, dependencySrcs)
	}
	rest := *is
	rest.AttributesRaw = instanceAttributesPlaceholderV4
	src, err := marshalInstanceObjectV4Standard(&rest, dependencySrcs)
	if err != nil {
		return marshalInstanceObjectV4Standard(is, dependencySrcs)
	}
	// A string can't contain a newline, so this can only match the property
	// itself, and only at this level of nesting because of the indentation.
	at := bytes.Index(src, instanceAttributesV4)
	if at < 0 {
		return marshalInstanceObjectV4Standard(is, dependencySrcs)
	}
	valueAt := at + len(instanceAttributesV4) - len(instanceAttributesPlaceholderV4)

	ret := instanceSrcBuffer(len(src) + len(is.AttributesRaw) + len(is.AttributesRaw)/2)
	ret = append(ret, src[:valueAt]...)
	ret, ok := appendIndentedJSON(ret, is.AttributesRaw, "          ", "  ")
	if !ok {
		// The attributes aren't valid, or are something unusual, so the
		// standard library decides what happens.
		return marshalInstanceObjectV4Standard(is, dependencySrcs)
	}
	ret = append(ret, src[at+len(instanceAttributesV4):]...)
	return ret, nil
}

// instanceSrcBuffers holds buffers that marshalInstanceObjectV4 returned
// and that its caller has finished with, as pointers to byte slices.
//
// Writing a large state encodes a great many objects, each into a buffer
// that's needed only until it's been written, and those add up to more than
// the size of the state file for each state we write. Reusing them means
// that far less of that builds up between runs of the garbage collector.
var instanceSrcBuffers sync.Pool

// instanceSrcBuffer returns an empty buffer with at least the given
// capacity.
func instanceSrcBuffer(size int) []byte {
	if recycled, ok := instanceSrcBuffers.Get().(*[]byte); ok {
		if buf := *recycled; cap(buf) >= size {
			return buf[:0]
		}
		// It's too small, so we let it go. Objects of the same resource
		// type tend to be of similar sizes, so the pool soon settles on
		// buffers that fit.
	}
	return make([]byte, 0, size+size/8)
}

// recycleInstanceSrc offers a result of marshalInstanceObjectV4 that the
// caller has finished with for reuse. The caller must not use it again.
func recycleInstanceSrc(src []byte) {
	// Small ones aren't worth keeping, and very large ones would be kept
	// for the sake of objects that mostly don't need them.
	if cap(src) < 1024 || cap(src) > 1<<20 {
		return
	}
	instanceSrcBuffers.Put(&src)
}

// appendIndentedSimpleStrings appends to dst the same thing that
//
//	json.MarshalIndent(strs, prefix, indent)
//
// returns for a non-empty slice of strings that consist only of printable
// ASCII characters, none of which need to be escaped, which is what the
// addresses of resources nearly always are. For anything else it returns
// false, and the caller must use the standard library instead.
//
// In a state whose objects each have different dependencies, encoding those
// is otherwise most of the work of writing the state.
func appendIndentedSimpleStrings(dst []byte, strs []string, prefix, indent string) ([]byte, bool) {
	if len(strs) == 0 {
		return dst, false
	}
	size := 2 + len(prefix)
	for _, str := range strs {
		size += len(str) + len(prefix) + len(indent) + 4
	}
	dst = slices.Grow(dst, size)
	dst = append(dst, '[')
	for i, str := range strs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '\n')
		dst = append(dst, prefix...)
		dst = append(dst, indent...)
		dst = append(dst, '"')
		start := len(dst)
		dst = append(dst, str...)
		if !simpleJSONStringContent(dst[start:]) {
			return dst, false
		}
		dst = append(dst, '"')
	}
	dst = append(dst, '\n')
	dst = append(dst, prefix...)
	dst = append(dst, ']')
	return dst, true
}

// appendIndentedSimpleAddrs is like appendIndentedSimpleStrings for the
// string representations of the given addresses, without building each of
// those strings separately.
func appendIndentedSimpleAddrs(dst []byte, deps []addrs.ConfigResource, prefix, indent string) ([]byte, bool) {
	if len(deps) == 0 {
		return dst, false
	}
	// A rough guess at the length of an address, to avoid growing the
	// buffer repeatedly.
	dst = slices.Grow(dst, 2+len(prefix)+len(deps)*(len(prefix)+len(indent)+48))
	dst = append(dst, '[')
	for i, dep := range deps {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '\n')
		dst = append(dst, prefix...)
		dst = append(dst, indent...)
		dst = append(dst, '"')
		start := len(dst)
		dst = dep.AppendString(dst)
		if !simpleJSONStringContent(dst[start:]) {
			return dst, false
		}
		dst = append(dst, '"')
	}
	dst = append(dst, '\n')
	dst = append(dst, prefix...)
	dst = append(dst, ']')
	return dst, true
}

// simpleJSONStringContent returns true if the given bytes are the content
// of a string that the standard library would write between quotes exactly
// as it is: printable ASCII characters with none that need to be escaped.
func simpleJSONStringContent(content []byte) bool {
	for _, c := range content {
		if !simpleJSONStringByte[c] {
			return false
		}
	}
	return true
}

var simpleJSONStringByte = func() (simple [256]bool) {
	for c := 0x20; c < 0x7f; c++ {
		simple[c] = true
	}
	for _, c := range []byte{'"', '\\', '<', '>', '&'} {
		simple[c] = false
	}
	return simple
}()

// minDirectAttributesV4 is the size of serialized attributes below which
// marshalInstanceObjectV4 just leaves everything to the standard library.
const minDirectAttributesV4 = 256

var (
	// instanceAttributesPlaceholderV4 stands in for the attributes of an
	// object while the rest of it is being encoded.
	instanceAttributesPlaceholderV4 = json.RawMessage("0")
	// instanceAttributesV4 is how the placeholder then appears.
	instanceAttributesV4 = []byte("\n          \"attributes\": 0")
)

// marshalInstanceObjectV4Standard is marshalInstanceObjectV4 without the
// special treatment of the attributes.
func marshalInstanceObjectV4Standard(is *instanceObjectStateV4, dependencySrcs *sync.Map) ([]byte, error) {
	// Two slices with the same first element and the same length have
	// exactly the same elements, so that identifies what we've already
	// encoded.
	type dependenciesKey struct {
		first unsafe.Pointer
		count int
	}
	var key dependenciesKey
	switch deps := is.Dependencies; {
	case deps != nil && len(deps.addrs) != 0:
		key = dependenciesKey{unsafe.Pointer(&deps.addrs[0]), len(deps.addrs)}
	case deps != nil && len(deps.list) != 0:
		key = dependenciesKey{unsafe.Pointer(&deps.list[0]), len(deps.list)}
	default:
		return json.MarshalIndent(is, "        ", "  ")
	}
	var dependenciesSrc []byte
	if cached, ok := dependencySrcs.Load(key); ok {
		dependenciesSrc = cached.([]byte)
	} else {
		var ok bool
		if deps := is.Dependencies; len(deps.addrs) != 0 {
			dependenciesSrc, ok = appendIndentedSimpleAddrs(nil, deps.addrs, "          ", "  ")
		} else {
			dependenciesSrc, ok = appendIndentedSimpleStrings(nil, deps.list, "          ", "  ")
		}
		if !ok {
			dependencies, err := is.Dependencies.strings()
			if err != nil {
				return nil, err
			}
			dependenciesSrc, err = json.MarshalIndent(dependencies, "          ", "  ")
			if err != nil {
				return nil, err
			}
		}
		dependencySrcs.Store(key, dependenciesSrc)
	}

	rest := *is
	rest.Dependencies = nil
	src, err := json.MarshalIndent(&rest, "        ", "  ")
	if err != nil {
		return nil, err
	}

	// The dependencies belong after all of the other properties except for
	// create_before_destroy, if that's present.
	insertAt := len(src) - len(instanceEndV4)
	if !bytes.HasSuffix(src, instanceEndV4) {
		return json.MarshalIndent(is, "        ", "  ")
	}
	if rest.CreateBeforeDestroy {
		if !bytes.HasSuffix(src[:insertAt], instanceCBDV4) {
			return json.MarshalIndent(is, "        ", "  ")
		}
		insertAt -= len(instanceCBDV4)
	}

	ret := make([]byte, 0, len(src)+len(instanceDependenciesV4)+len(dependenciesSrc))
	ret = append(ret, src[:insertAt]...)
	ret = append(ret, instanceDependenciesV4...)
	ret = append(ret, dependenciesSrc...)
	ret = append(ret, src[insertAt:]...)
	return ret, nil
}
