// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"bytes"
	"fmt"
	"reflect"
	"unsafe"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/hcl2shim"
	"github.com/hashicorp/terraform/internal/lang/fastjson"
	"github.com/hashicorp/terraform/internal/lang/marks"
	"github.com/hashicorp/terraform/internal/providers"
)

// ResourceInstanceObjectSrc is a not-fully-decoded version of
// ResourceInstanceObject. Decoding of it can be completed by first handling
// any schema migration steps to get to the latest schema version and then
// calling method Decode with the implied type of the latest schema.
type ResourceInstanceObjectSrc struct {
	// SchemaVersion is the resource-type-specific schema version number that
	// was current when either AttrsJSON or AttrsFlat was encoded. Migration
	// steps are required if this is less than the current version number
	// reported by the corresponding provider.
	SchemaVersion uint64

	// AttrsJSON is a JSON-encoded representation of the object attributes,
	// encoding the value (of the object type implied by the associated resource
	// type schema) that represents this remote object in Terraform Language
	// expressions, and is compared with configuration when producing a diff.
	//
	// This is retained in JSON format here because it may require preprocessing
	// before decoding if, for example, the stored attributes are for an older
	// schema version which the provider must upgrade before use. If the
	// version is current, it is valid to simply decode this using the
	// type implied by the current schema, without the need for the provider
	// to perform an upgrade first.
	//
	// When writing a ResourceInstanceObject into the state, AttrsJSON should
	// always be conformant to the current schema version and the current
	// schema version should be recorded in the SchemaVersion field.
	AttrsJSON []byte

	IdentitySchemaVersion uint64

	IdentityJSON []byte

	// AttrsFlat is a legacy form of attributes used in older state file
	// formats, and in the new state format for objects that haven't yet been
	// upgraded. This attribute is mutually exclusive with Attrs: for any
	// ResourceInstanceObject, only one of these attributes may be populated
	// and the other must be nil.
	//
	// An instance object with this field populated should be upgraded to use
	// Attrs at the earliest opportunity, since this legacy flatmap-based
	// format will be phased out over time. AttrsFlat should not be used when
	// writing new or updated objects to state; instead, callers must follow
	// the recommendations in the AttrsJSON documentation above.
	AttrsFlat map[string]string

	// AttrSensitivePaths is an array of paths to mark as sensitive coming out of
	// state, or to save as sensitive paths when saving state
	AttrSensitivePaths []cty.Path

	// These fields all correspond to the fields of the same name on
	// ResourceInstanceObject.
	Private             []byte
	Status              ObjectStatus
	Dependencies        []addrs.ConfigResource
	CreateBeforeDestroy bool

	// decodeValueCache stored the decoded value for repeated decodings.
	decodeValueCache cty.Value
	// decodeIdentityCache stored the decoded identity for repeated decodings.
	decodeIdentityCache cty.Value
	// attrsVerified is true if AttrsJSON is known to be what serializing
	// decodeValueCache produces, aside from any insignificant whitespace,
	// and there are no sensitive paths. If so then an object decoded from
	// this one can be encoded again without serializing its value.
	attrsVerified bool
}

// Decode unmarshals the raw representation of the object attributes. Pass the
// schema of the corresponding resource type for correct operation.
//
// Before calling Decode, the caller must check that the SchemaVersion field
// exactly equals the version number of the schema whose implied type is being
// passed, or else the result is undefined.
//
// If the object has an identity, the schema must also contain a resource
// identity schema for the identity to be decoded.
//
// The returned object may share internal references with the receiver and
// so the caller must not mutate the receiver any further once once this
// method is called.
func (os *ResourceInstanceObjectSrc) Decode(schema providers.Schema) (*ResourceInstanceObject, error) {
	var val cty.Value
	var err error
	attrsTy := schema.Body.ImpliedType()

	switch {
	case os.decodeValueCache != cty.NilVal:
		val = os.decodeValueCache

	case os.AttrsFlat != nil:
		// Legacy mode. We'll do our best to unpick this from the flatmap.
		val, err = hcl2shim.HCL2ValueFromFlatmap(os.AttrsFlat, attrsTy)
		if err != nil {
			return nil, err
		}

	default:
		val, err = fastjson.Unmarshal(os.AttrsJSON, attrsTy)
		val = marks.MarkPaths(val, marks.Sensitive, os.AttrSensitivePaths)
		if err != nil {
			return nil, err
		}
	}

	var identity cty.Value
	if os.decodeIdentityCache != cty.NilVal {
		identity = os.decodeIdentityCache
	} else if os.IdentityJSON != nil && schema.Identity != nil {
		identity, err = fastjson.Unmarshal(os.IdentityJSON, schema.Identity.ImpliedType())
		if err != nil {
			return nil, fmt.Errorf("failed to decode identity: %s. This is most likely a bug in the Provider, providers must not change the identity schema without updating the identity schema version", err.Error())
		}
	}

	obj := &ResourceInstanceObject{
		Value:               val,
		Identity:            identity,
		Status:              os.Status,
		Dependencies:        os.Dependencies,
		Private:             os.Private,
		CreateBeforeDestroy: os.CreateBeforeDestroy,
	}
	if len(os.AttrsJSON) != 0 {
		// The object is very likely to be stored again without its value
		// having changed, in which case it can keep the serialization it
		// already has as long as that turns out to be equivalent to what
		// encoding it would produce.
		obj.encoded = unsafe.Pointer(&encodedObject{
			value:         val,
			body:          schema.Body,
			schemaVersion: os.SchemaVersion,
			attrsJSON:     os.AttrsJSON,
			verified:      os.attrsVerified && os.decodeValueCache != cty.NilVal && os.AttrsFlat == nil,
		})
	}
	return obj, nil
}

// shareUnchangedData arranges for the receiver to use the same memory as
// the given object for its serialized attributes and for its dependencies,
// wherever the two have exactly the same content.
//
// When an object is stored again after being refreshed or planned it has
// very often not changed at all, but encoding it still produces a new copy
// of its data. During planning we keep several snapshots of the state at
// once, so without this each of them would end up retaining its own copy
// of the data for every object, rather than sharing the one they all
// started with. The shared data is immutable by convention.
//
// Serialized attributes that differ only in insignificant whitespace are
// considered to have the same content, in which case the receiver ends up
// with the whitespace of the given object.
func (os *ResourceInstanceObjectSrc) shareUnchangedData(previous *ResourceInstanceObjectSrc) {
	if os == nil || previous == nil {
		return
	}
	if len(os.AttrsJSON) != 0 && equivalentJSON(os.AttrsJSON, previous.AttrsJSON) {
		os.AttrsJSON = previous.AttrsJSON
	}
	if len(os.Dependencies) != 0 && len(os.Dependencies) == len(previous.Dependencies) && &os.Dependencies[0] != &previous.Dependencies[0] {
		same := true
		for i := range os.Dependencies {
			if !os.Dependencies[i].Equal(previous.Dependencies[i]) {
				same = false
				break
			}
		}
		if same {
			os.Dependencies = previous.Dependencies
		}
	}
}

// equivalentJSON returns true if the two given JSON values, which must both
// be valid, are the same aside from differences in insignificant whitespace.
//
// The serialized attributes of an object read from a state file retain the
// indentation they had in the file, whereas encoding an object produces
// the most compact form, so the two typically differ only in whitespace
// when they represent the same object.
func equivalentJSON(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}

	skipSpace := func(src []byte, i int) int {
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
	// stringEnd returns the index just after the end of the string that
	// starts at src[i], or -1 if the string is unterminated.
	stringEnd := func(src []byte, i int) int {
		for i++; i < len(src); i++ {
			switch src[i] {
			case '\\':
				i++
			case '"':
				return i + 1
			}
		}
		return -1
	}

	i, j := 0, 0
	for {
		i, j = skipSpace(a, i), skipSpace(b, j)
		if i >= len(a) || j >= len(b) {
			return i >= len(a) && j >= len(b)
		}
		if a[i] != b[j] {
			return false
		}
		if a[i] != '"' {
			i++
			j++
			continue
		}
		// Whitespace inside a string is significant, so strings must match
		// exactly.
		aEnd, bEnd := stringEnd(a, i), stringEnd(b, j)
		if aEnd < 0 || bEnd < 0 || !bytes.Equal(a[i:aEnd], b[j:bEnd]) {
			return false
		}
		i, j = aEnd, bEnd
	}
}

// HasDecodedValue returns true if the receiver already knows the result of
// decoding its attributes, in which case [ResourceInstanceObjectSrc.Decode]
// is cheap.
func (os *ResourceInstanceObjectSrc) HasDecodedValue() bool {
	return os.decodeValueCache != cty.NilVal
}

// sameSource returns true if the given object is certain to decode to the
// same result as the receiver because it was derived from the same data,
// which is true of an unmodified copy of the receiver.
func (os *ResourceInstanceObjectSrc) sameSource(other *ResourceInstanceObjectSrc) bool {
	sameBytes := func(a, b []byte) bool {
		return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
	}
	samePaths := func(a, b []cty.Path) bool {
		return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
	}
	return os.AttrsFlat == nil && other.AttrsFlat == nil &&
		os.Status == other.Status &&
		os.SchemaVersion == other.SchemaVersion &&
		os.IdentitySchemaVersion == other.IdentitySchemaVersion &&
		sameBytes(os.AttrsJSON, other.AttrsJSON) &&
		sameBytes(os.IdentityJSON, other.IdentityJSON) &&
		samePaths(os.AttrSensitivePaths, other.AttrSensitivePaths)
}

// CompleteUpgrade creates a new ResourceInstanceObjectSrc by copying the
// metadata from the receiver and writing in the given new schema version
// and attribute value that are presumed to have resulted from upgrading
// from an older schema version.
func (os *ResourceInstanceObjectSrc) CompleteUpgrade(newAttrs cty.Value, newType cty.Type, newSchemaVersion uint64) (*ResourceInstanceObjectSrc, error) {
	new := os.DeepCopy()
	new.AttrsFlat = nil // We always use JSON after an upgrade, even if the source used flatmap

	// This is the same principle as ResourceInstanceObject.Encode, but
	// avoiding a decode/re-encode cycle because we don't have type info
	// available for the "old" attributes.
	newAttrs = cty.UnknownAsNull(newAttrs)
	src, exact, err := marshalJSONExact(newAttrs, newType)
	if err != nil {
		return nil, err
	}

	// The result is very often the same as what we started with, because
	// providers are asked to upgrade every object even when its schema
	// version hasn't changed. In that case we keep the original
	// serialization and let the new one be freed, so that this object
	// continues to share its memory with the others derived from it.
	if len(os.AttrsJSON) != 0 && equivalentJSON(src, os.AttrsJSON) {
		src = os.AttrsJSON
	}
	new.AttrsJSON = src
	new.SchemaVersion = newSchemaVersion

	// The caller will typically decode the result immediately. If we know
	// that decoding would just give back the value we were given then we
	// keep that value, saving the considerable cost of parsing what we've
	// just serialized. As for an object produced by
	// [ResourceInstanceObject.Encode], the value we keep must have its marks.
	new.decodeValueCache = cty.NilVal
	new.attrsVerified = false
	if exact {
		new.decodeValueCache = marks.MarkPaths(newAttrs, marks.Sensitive, new.AttrSensitivePaths)
		new.attrsVerified = len(new.AttrSensitivePaths) == 0
	}
	return new, nil
}

func (os *ResourceInstanceObjectSrc) CompleteIdentityUpgrade(newAttrs cty.Value, schema providers.Schema) (*ResourceInstanceObjectSrc, error) {
	new := os.DeepCopy()

	src, err := ctyjson.Marshal(newAttrs, schema.Identity.ImpliedType())
	if err != nil {
		return nil, err
	}

	new.IdentityJSON = src
	new.IdentitySchemaVersion = uint64(schema.IdentityVersion)
	return new, nil
}

// Equal compares two ResourceInstanceObjectSrc objects for equality, skipping
// any internal fields which are not stored to the final serialized state.
func (os *ResourceInstanceObjectSrc) Equal(other *ResourceInstanceObjectSrc) bool {
	if os == other {
		return true
	}
	if os == nil || other == nil {
		return false
	}

	if os.SchemaVersion != other.SchemaVersion {
		return false
	}
	if os.IdentitySchemaVersion != other.IdentitySchemaVersion {
		return false
	}
	if os.Status != other.Status {
		return false
	}
	if os.CreateBeforeDestroy != other.CreateBeforeDestroy {
		return false
	}

	if !bytes.Equal(os.AttrsJSON, other.AttrsJSON) {
		return false
	}
	if !bytes.Equal(os.IdentityJSON, other.IdentityJSON) {
		return false
	}
	if !bytes.Equal(os.Private, other.Private) {
		return false
	}

	// Compare legacy AttrsFlat maps. We shouldn't see this ever being used, but
	// deal with in just in case until we remove it entirely. These are all
	// simple maps of strings, so DeepEqual is perfectly fine here.
	if !reflect.DeepEqual(os.AttrsFlat, other.AttrsFlat) {
		return false
	}

	// We skip fields that have no functional impact on resource state.

	return true
}
