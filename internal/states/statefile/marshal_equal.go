// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package statefile

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sync"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/hashicorp/terraform/internal/states"
)

// StatesMarshalEqual returns true if and only if the two given states have
// an identical (byte-for-byte) statefile representation.
//
// This function compares only the portions of the state that are persisted
// in state files, so for example it will not return false if the only
// differences between the two states are local values or descendant module
// outputs.
func StatesMarshalEqual(a, b *states.State) bool {
	// nil states are not valid states, and so they can never martial equal.
	if a == nil || b == nil {
		return false
	}
	if StatesDefinitelyDiffer(a, b) {
		return false
	}

	var aDigest, bDigest MarshalDigest
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		aDigest = StateMarshalDigest(a)
	}()
	bDigest = StateMarshalDigest(b)
	wg.Wait()

	return aDigest == bDigest
}

// StatesDefinitelyDiffer returns true only if it can establish cheaply that
// StatesMarshalEqual would return false for the two given states. If it
// returns false then the states might or might not be equal, and the caller
// must compare them some other way.
//
// Comparing the serializations of two states means serializing both of them
// in full, which is expensive for a large state. When a state has been
// modified, comparing it with the state it was derived from usually reveals
// the difference long before that, because the objects that weren't
// modified share their data with the original and so can be recognized
// as identical without examining their content.
func StatesDefinitelyDiffer(a, b *states.State) bool {
	if a == nil || b == nil {
		// nil states never marshal equal.
		return true
	}

	if len(a.RootOutputValues) != len(b.RootOutputValues) {
		return true
	}
	for name := range a.RootOutputValues {
		if _, ok := b.RootOutputValues[name]; !ok {
			return true
		}
	}

	// Every resource appears in the serialization, even if it has no
	// instances, but modules don't appear in their own right.
	resourceCount := func(s *states.State) int {
		count := 0
		for _, ms := range s.Modules {
			count += len(ms.Resources)
		}
		return count
	}
	if resourceCount(a) != resourceCount(b) {
		return true
	}

	for moduleKey, aMS := range a.Modules {
		bMS := b.Modules[moduleKey]
		for resourceKey, aRS := range aMS.Resources {
			if bMS == nil {
				return true
			}
			bRS := bMS.Resources[resourceKey]
			if bRS == nil {
				return true
			}
			for instanceKey, aIS := range aRS.Instances {
				bIS := bRS.Instances[instanceKey]
				if aIS == nil || aIS.Current == nil {
					continue
				}
				if bIS == nil || bIS.Current == nil {
					// The current object of this instance appears in only
					// one of the serializations.
					return true
				}
				if instanceObjectsDefinitelyDiffer(aIS.Current, bIS.Current) {
					return true
				}
			}
		}
	}
	return false
}

// instanceObjectsDefinitelyDiffer is the part of StatesDefinitelyDiffer that
// compares two objects for the same resource instance.
func instanceObjectsDefinitelyDiffer(a, b *states.ResourceInstanceObjectSrc) bool {
	if a.Status != b.Status {
		return true
	}
	if a.AttrsFlat != nil || b.AttrsFlat != nil {
		return false
	}
	aSrc, bSrc := a.AttrsJSON, b.AttrsJSON
	if len(aSrc) == 0 || len(bSrc) == 0 {
		return false
	}
	if len(aSrc) == len(bSrc) && &aSrc[0] == &bSrc[0] {
		// Exactly the same data.
		return false
	}
	// The attributes appear in the serialization after being normalized
	// in the same way that marshaling a json.RawMessage normalizes them,
	// so if the two differ after that then the serializations differ.
	aNorm, err := json.Marshal(json.RawMessage(aSrc))
	if err != nil {
		return false
	}
	bNorm, err := json.Marshal(json.RawMessage(bSrc))
	if err != nil {
		return false
	}
	return !bytes.Equal(aNorm, bNorm)
}

// MarshalDigest is a digest of the statefile representation of a state,
// as returned by StateMarshalDigest.
type MarshalDigest [sha256.Size]byte

// StateMarshalDigest returns a digest of the statefile representation of the
// given state, which must not be nil.
//
// Two states have the same digest if and only if StatesMarshalEqual would
// return true for them, so a caller that needs to compare many states against
// the same state can retain the digest of that state instead of serializing
// it again for each comparison.
func StateMarshalDigest(s *states.State) MarshalDigest {
	// We write here a temporary file that has no header information
	// populated, thus ensuring that we're only comparing the state itself
	// and not any metadata.
	h := sha256.New()
	err := Write(&File{State: s}, h)
	if err != nil {
		// Should never happen, because we're writing to an in-memory hash
		panic(err)
	}
	var ret MarshalDigest
	h.Sum(ret[:0])
	return ret
}

// StatesDefinitelyMarshalEqual returns true only if it can establish cheaply
// that StatesMarshalEqual would return true for the two given states. If it
// returns false then the two might or might not be equal.
//
// This is the counterpart of StatesDefinitelyDiffer. Two snapshots of a
// state between which nothing has changed share the data of their objects,
// so they can be recognized as the same without serializing either of them.
func StatesDefinitelyMarshalEqual(a, b *states.State) bool {
	if a == nil || b == nil {
		return false
	}
	if a == b {
		return true
	}
	if !a.CheckResults.Equal(b.CheckResults) {
		return false
	}

	if len(a.RootOutputValues) != len(b.RootOutputValues) {
		return false
	}
	for name, aOV := range a.RootOutputValues {
		bOV := b.RootOutputValues[name]
		if aOV == nil || bOV == nil {
			return false
		}
		if aOV != bOV && (aOV.Sensitive != bOV.Sensitive || !aOV.Addr.Equal(bOV.Addr) || !outputValuesMarshalEqual(aOV.Value, bOV.Value)) {
			return false
		}
	}

	if len(a.Modules) != len(b.Modules) {
		return false
	}
	for moduleKey, aMS := range a.Modules {
		bMS := b.Modules[moduleKey]
		if aMS == nil || bMS == nil || len(aMS.Resources) != len(bMS.Resources) {
			return false
		}
		for resourceKey, aRS := range aMS.Resources {
			bRS := bMS.Resources[resourceKey]
			if aRS == nil || bRS == nil || len(aRS.Instances) != len(bRS.Instances) {
				return false
			}
			if !aRS.Addr.Equal(bRS.Addr) || !aRS.ProviderConfig.Equal(bRS.ProviderConfig) {
				return false
			}
			for instanceKey, aIS := range aRS.Instances {
				bIS := bRS.Instances[instanceKey]
				if aIS == nil || bIS == nil || len(aIS.Deposed) != len(bIS.Deposed) {
					return false
				}
				if !instanceObjectsDefinitelyMarshalEqual(aIS.Current, bIS.Current) {
					return false
				}
				for deposedKey, aObj := range aIS.Deposed {
					bObj := bIS.Deposed[deposedKey]
					if aObj == nil || bObj == nil || !instanceObjectsDefinitelyMarshalEqual(aObj, bObj) {
						return false
					}
				}
			}
		}
	}
	return true
}

// outputValuesMarshalEqual returns true only if the two given output values
// are serialized identically.
//
// Being equal as values isn't enough for that. A number is written as the
// shortest decimal that identifies it at the precision it's held with, so
// the result of pow(2, 60), which is held as a 64-bit float, is written as
// 1152921504606847000, while the same number read back from a state file
// is written as 1152921504606846976. Types that are equal can be written
// differently too. So the two are compared as they'd be written. There are
// only as many output values as the root module declares.
func outputValuesMarshalEqual(a, b cty.Value) bool {
	if !a.RawEquals(b) {
		return false
	}
	aSrc, err := ctyjson.Marshal(a, a.Type())
	if err != nil {
		return false
	}
	bSrc, err := ctyjson.Marshal(b, b.Type())
	if err != nil || !bytes.Equal(aSrc, bSrc) {
		return false
	}
	aTypeSrc, err := ctyjson.MarshalType(a.Type())
	if err != nil {
		return false
	}
	bTypeSrc, err := ctyjson.MarshalType(b.Type())
	return err == nil && bytes.Equal(aTypeSrc, bTypeSrc)
}

// instanceObjectsDefinitelyMarshalEqual returns true only if the two given
// objects, either of which can be nil, have exactly the same content.
func instanceObjectsDefinitelyMarshalEqual(a, b *states.ResourceInstanceObjectSrc) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Status != b.Status || a.CreateBeforeDestroy != b.CreateBeforeDestroy ||
		a.SchemaVersion != b.SchemaVersion || a.IdentitySchemaVersion != b.IdentitySchemaVersion {
		return false
	}
	if a.AttrsFlat != nil || b.AttrsFlat != nil {
		return false
	}
	// Unchanged objects usually share their data, in which case these
	// comparisons don't need to examine it.
	if !bytes.Equal(a.AttrsJSON, b.AttrsJSON) || !bytes.Equal(a.IdentityJSON, b.IdentityJSON) || !bytes.Equal(a.Private, b.Private) {
		return false
	}
	// A nil value doesn't always serialize in the same way as an empty one.
	if (a.AttrsJSON == nil) != (b.AttrsJSON == nil) || (a.IdentityJSON == nil) != (b.IdentityJSON == nil) || (a.Private == nil) != (b.Private == nil) {
		return false
	}

	if len(a.AttrSensitivePaths) != len(b.AttrSensitivePaths) {
		return false
	}
	if len(a.AttrSensitivePaths) != 0 && &a.AttrSensitivePaths[0] != &b.AttrSensitivePaths[0] {
		for i, path := range a.AttrSensitivePaths {
			if !path.Equals(b.AttrSensitivePaths[i]) {
				return false
			}
		}
	}
	if len(a.Dependencies) != len(b.Dependencies) {
		return false
	}
	if len(a.Dependencies) != 0 && &a.Dependencies[0] != &b.Dependencies[0] {
		for i, dep := range a.Dependencies {
			if !dep.Equal(b.Dependencies[i]) {
				return false
			}
		}
	}
	return true
}
