// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/terraform/internal/addrs"
)

// parallelDecodeMinSize is the size of state file below which we don't
// bother trying to decode the resources concurrently.
const parallelDecodeMinSize = 256 * 1024

// readStateV4Parallel is an alternative to the combination of
// sniffJSONStateVersion and readStateV4 for large state files, which decodes
// the individual resources concurrently.
//
// It only handles valid version 4 state files that produce no diagnostics
// at all. In any other case it returns false, and the caller must then use
// the normal sequential path, which will produce the appropriate result.
func readStateV4Parallel(src []byte) (*File, bool) {
	if len(src) < parallelDecodeMinSize {
		return nil, false
	}
	outerSrc, resourceSrcs, ok := splitStateV4Resources(src)
	if !ok {
		return nil, false
	}

	// outerSrc is the whole document except for the content of the
	// "resources" array, so decoding it validates everything that
	// splitStateV4Resources skipped over without fully parsing.
	version, diags := sniffJSONStateVersion(outerSrc)
	if len(diags) != 0 || version != 4 {
		return nil, false
	}
	sV4 := &stateV4{}
	if err := json.Unmarshal(outerSrc, sV4); err != nil {
		return nil, false
	}

	sV4.Resources = make([]resourceStateV4, len(resourceSrcs))
	// depAddrs remembers the address that each dependency string we've
	// seen so far represents, as a map from string to addrs.ConfigResource.
	var depAddrs sync.Map
	var failed atomic.Bool
	var next atomic.Int64
	var wg sync.WaitGroup
	const batch = 4
	for range min(runtime.GOMAXPROCS(0), len(resourceSrcs)/batch+1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !failed.Load() {
				end := int(next.Add(batch))
				start := end - batch
				if start >= len(resourceSrcs) {
					return
				}
				for i := start; i < min(end, len(resourceSrcs)); i++ {
					if !unmarshalResourceV4(resourceSrcs[i], &sV4.Resources[i]) {
						// It's either invalid or something unusual, so the
						// standard library decides what it means.
						sV4.Resources[i] = resourceStateV4{}
						if err := json.Unmarshal(resourceSrcs[i], &sV4.Resources[i]); err != nil {
							failed.Store(true)
							return
						}
					}
					shareDependenciesV4(sV4.Resources[i].Instances, &depAddrs)
				}
			}
		}()
	}
	wg.Wait()
	if failed.Load() {
		return nil, false
	}

	file, diags := prepareStateV4(sV4)
	if len(diags) != 0 {
		return nil, false
	}
	return file, true
}

// shareDependenciesV4 prepares the dependencies of the given objects of a
// resource for prepareStateV4, which deals with one object at a time, by
// doing the parts of that work which can be done separately for each
// resource and so concurrently.
//
// Consecutive objects of a resource usually have exactly the same
// dependencies, written identically, in which case we make them all refer
// to the same dependenciesV4 so that this is apparent without comparing
// them again. Any others are decoded, and their addresses are parsed, which
// is otherwise a significant amount of work for a state whose objects all
// have different dependencies. depAddrs is a map from string to
// addrs.ConfigResource which remembers the addresses already parsed.
func shareDependenciesV4(instances []instanceObjectStateV4, depAddrs *sync.Map) {
	var last *dependenciesV4
	for i := range instances {
		deps := instances[i].Dependencies
		if deps == nil || deps.src == nil {
			continue
		}
		if last != nil && bytes.Equal(deps.src, last.src) {
			instances[i].Dependencies = last
			continue
		}
		last = deps

		// If any of this fails, or produces any diagnostics, then we leave
		// it for prepareStateV4 to find that out for itself, so that
		// whatever it reports is reported in the usual order.
		list, err := deps.strings()
		if err != nil || len(list) == 0 {
			continue
		}
		parsed := make([]addrs.ConfigResource, len(list))
		ok := true
		for j, str := range list {
			if known, isKnown := depAddrs.Load(str); isKnown {
				parsed[j] = known.(addrs.ConfigResource)
				continue
			}
			addr, diags := addrs.ParseAbsResourceStr(str)
			if len(diags) != 0 {
				ok = false
				break
			}
			parsed[j] = addr.Config()
			depAddrs.Store(str, parsed[j])
		}
		if ok {
			deps.parsed = parsed
		}
	}
}

// splitStateV4Resources finds the "resources" property of the top-level
// object in the given JSON document, which must be an array of objects,
// and returns the source of each of its elements along with a copy of
// the rest of the document where that array is replaced with an empty one.
//
// This only does as much parsing as is needed to find the boundaries of
// the array elements, so the caller must validate each of the results
// by parsing it properly. It returns false if the document doesn't have the
// expected overall shape.
func splitStateV4Resources(src []byte) (outerSrc []byte, resourceSrcs [][]byte, ok bool) {
	const resourcesKey = `"resources"`

	i := skipJSONSpace(src, 0)
	if i >= len(src) || src[i] != '{' {
		return nil, nil, false
	}
	i = skipJSONSpace(src, i+1)

	arrayStart, arrayEnd := -1, -1
	for {
		if i >= len(src) || src[i] != '"' {
			// This also rejects an empty object, which is fine because
			// that isn't a state we could do anything useful with here.
			return nil, nil, false
		}
		keyEnd, ok := skipJSONString(src, i)
		if !ok {
			return nil, nil, false
		}
		key := src[i:keyEnd]
		i = skipJSONSpace(src, keyEnd)
		if i >= len(src) || src[i] != ':' {
			return nil, nil, false
		}
		i = skipJSONSpace(src, i+1)
		var valueEnd int
		switch {
		case string(key) == resourcesKey:
			if arrayStart != -1 || i >= len(src) || src[i] != '[' {
				return nil, nil, false
			}
			// The resources are nearly all of the document, so we find
			// where each of them ends as we pass over the array rather
			// than passing over it again afterwards.
			arrayStart = i
			var ok bool
			resourceSrcs, arrayEnd, ok = splitJSONObjectArray(src, i)
			if !ok {
				return nil, nil, false
			}
			valueEnd = arrayEnd
		case bytes.IndexByte(key, '\\') != -1 || strings.EqualFold(string(key), resourcesKey):
			// encoding/json matches property names case-insensitively and
			// after interpreting escape sequences, so this might be
			// another way to write "resources". We don't expect to find
			// that in practice, so we'll leave it to the normal decoder.
			return nil, nil, false
		default:
			var ok bool
			valueEnd, ok = skipJSONValue(src, i)
			if !ok {
				return nil, nil, false
			}
		}

		i = skipJSONSpace(src, valueEnd)
		if i >= len(src) {
			return nil, nil, false
		}
		if src[i] == '}' {
			break
		}
		if src[i] != ',' {
			return nil, nil, false
		}
		i = skipJSONSpace(src, i+1)
	}
	if arrayStart == -1 {
		return nil, nil, false
	}

	outerSrc = make([]byte, 0, arrayStart+2+len(src)-arrayEnd)
	outerSrc = append(outerSrc, src[:arrayStart]...)
	outerSrc = append(outerSrc, '[', ']')
	outerSrc = append(outerSrc, src[arrayEnd:]...)
	return outerSrc, resourceSrcs, true
}

// parallelSplitMinSize is the size of array below which splitJSONObjectArray
// doesn't bother trying to find its elements concurrently. It's a variable
// only so that tests can change it.
var parallelSplitMinSize = 4 << 20

// splitJSONObjectArray returns the source of each element of the JSON array
// that starts at src[i], all of which must be objects, along with the index
// just after the end of the array. Like skipJSONValue it assumes that the
// content is valid, and it returns false if it finds otherwise.
func splitJSONObjectArray(src []byte, i int) (elems [][]byte, end int, ok bool) {
	if i >= len(src) || src[i] != '[' {
		return nil, 0, false
	}
	first := skipJSONSpace(src, i+1)
	if first >= len(src) {
		return nil, 0, false
	}
	if src[first] == ']' {
		return nil, first + 1, true
	}

	// Finding the end of each element means examining everything in the
	// array, which for a large state takes a significant time if we do it
	// sequentially. Instead we guess at where some of the elements start,
	// by looking for what separates the elements when we write an array
	// like this one ourselves, and then scan from each of those places
	// concurrently. A guess can be wrong, since the same thing could appear
	// inside an element in a file with different indentation, but the scan
	// before each one tells us whether an element really starts there, in
	// which case the result is exactly what scanning sequentially finds.
	// If any guess is wrong then we do just scan sequentially.
	if workers := runtime.GOMAXPROCS(0); workers > 1 && len(src)-first >= parallelSplitMinSize {
		starts := []int{first}
		for k := 1; k < workers; k++ {
			from := first + (len(src)-first)/workers*k
			if from <= starts[len(starts)-1] {
				continue
			}
			at := bytes.Index(src[from:], jsonObjectArraySeparator)
			if at < 0 {
				break
			}
			// The element starts at the last byte of the separator.
			starts = append(starts, from+at+len(jsonObjectArraySeparator)-1)
		}
		if len(starts) > 1 {
			type result struct {
				elems [][]byte
				end   int // only for the scan that reaches the end of the array
				ok    bool
			}
			results := make([]result, len(starts))
			var wg sync.WaitGroup
			for k := range starts {
				wg.Add(1)
				go func() {
					defer wg.Done()
					limit := -1
					if k+1 < len(starts) {
						limit = starts[k+1]
					}
					r := &results[k]
					r.elems, r.end, r.ok = scanJSONObjectArray(src, starts[k], limit)
				}()
			}
			wg.Wait()
			allOK, count := true, 0
			for _, r := range results {
				allOK = allOK && r.ok
				count += len(r.elems)
			}
			if allOK {
				elems = make([][]byte, 0, count)
				for _, r := range results {
					elems = append(elems, r.elems...)
				}
				return elems, results[len(results)-1].end, true
			}
		}
	}

	return scanJSONObjectArray(src, first, -1)
}

// jsonObjectArraySeparator is what appears between two elements of an array
// of non-empty objects that's a property of the top-level object, when the
// document is indented in the way that we indent a state file. The second
// of those elements starts at its last byte.
var jsonObjectArraySeparator = []byte("\n    },\n    {")

// scanJSONObjectArray finds the elements of an array of objects, starting
// with the one that's expected to start at src[i].
//
// If limit is negative then it continues to the end of the array, and
// returns the index just after that. Otherwise it stops when it reaches
// src[limit], and succeeds only if that's exactly where another element
// starts.
func scanJSONObjectArray(src []byte, i, limit int) (elems [][]byte, end int, ok bool) {
	for {
		if i >= len(src) || src[i] != '{' {
			// This includes a trailing comma.
			return nil, 0, false
		}
		elemEnd, ok := skipJSONValue(src, i)
		if !ok {
			return nil, 0, false
		}
		elems = append(elems, src[i:elemEnd])
		i = skipJSONSpace(src, elemEnd)
		if i >= len(src) {
			return nil, 0, false
		}
		if src[i] == ']' {
			// The array ends here, which is only right if we weren't
			// expecting to find another element first.
			return elems, i + 1, limit < 0
		}
		if src[i] != ',' {
			return nil, 0, false
		}
		i = skipJSONSpace(src, i+1)
		if limit >= 0 && i >= limit {
			return elems, 0, i == limit
		}
	}
}

func skipJSONSpace(src []byte, i int) int {
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// skipJSONString returns the index just after the end of the JSON string
// that starts at src[i], which must be a quote character.
func skipJSONString(src []byte, i int) (int, bool) {
	i++
	for {
		n := bytes.IndexByte(src[i:], '"')
		if n < 0 {
			return 0, false
		}
		i += n + 1

		// This quote ends the string unless it's escaped, which is true
		// if it's preceded by an odd number of backslashes.
		backslashes := 0
		for j := i - 2; src[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return i, true
		}
	}
}

// skipJSONValue returns the index just after the end of the JSON value
// that starts at src[i], assuming that the value is valid.
func skipJSONValue(src []byte, i int) (int, bool) {
	if i >= len(src) {
		return 0, false
	}
	switch src[i] {
	case '"':
		return skipJSONString(src, i)
	case '{', '[':
		depth := 0
		for i < len(src) {
			// Most of what's between strings is the whitespace used for
			// indentation, so we pass over everything that isn't
			// significant here as quickly as we can.
			for !jsonStructural[src[i]] {
				i++
				if i >= len(src) {
					return 0, false
				}
			}
			switch src[i] {
			case '"':
				end, ok := skipJSONString(src, i)
				if !ok {
					return 0, false
				}
				i = end
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return 0, false
	default:
		start := i
		for i < len(src) {
			switch src[i] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return i, i > start
			}
			i++
		}
		return i, i > start
	}
}

// jsonStructural is true for each byte that skipJSONValue needs to notice
// while it's passing over the content of an array or object.
var jsonStructural = [256]bool{'"': true, '{': true, '[': true, '}': true, ']': true}
