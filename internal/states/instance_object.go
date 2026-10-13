// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package states

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"sync/atomic"
	"unsafe"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/lang/format"
	"github.com/hashicorp/terraform/internal/lang/marks"
	"github.com/hashicorp/terraform/internal/providers"
)

// ResourceInstanceObject is the local representation of a specific remote
// object associated with a resource instance. In practice not all remote
// objects are actually remote in the sense of being accessed over the network,
// but this is the most common case.
//
// It is not valid to mutate a ResourceInstanceObject once it has been created.
// Instead, create a new object and replace the existing one.
type ResourceInstanceObject struct {
	// Value is the object-typed value representing the remote object within
	// Terraform.
	Value cty.Value

	// Identity is the object-typed value representing the identity of the remote
	// object within Terraform.
	Identity cty.Value

	// Private is an opaque value set by the provider when this object was
	// last created or updated. Terraform Core does not use this value in
	// any way and it is not exposed anywhere in the user interface, so
	// a provider can use it for retaining any necessary private state.
	Private []byte

	// Status represents the "readiness" of the object as of the last time
	// it was updated.
	Status ObjectStatus

	// Dependencies is a set of absolute address to other resources this
	// instance dependeded on when it was applied. This is used to construct
	// the dependency relationships for an object whose configuration is no
	// longer available, such as if it has been removed from configuration
	// altogether, or is now deposed.
	Dependencies []addrs.ConfigResource

	// CreateBeforeDestroy reflects the status of the lifecycle
	// create_before_destroy option when this instance was last updated.
	// Because create_before_destroy also effects the overall ordering of the
	// destroy operations, we need to record the status to ensure a resource
	// removed from the config will still be destroyed in the same manner.
	CreateBeforeDestroy bool

	// encoded is a *encodedObject describing the serialized form of Value,
	// if that's already known, accessed atomically. It's valid only for as
	// long as Value remains the very same value.
	encoded unsafe.Pointer
}

// encodedObject records the serialized form of a particular value of a
// resource instance object, so that an object which is stored several times
// without changing, which is by far the most common case during planning,
// only needs to be serialized once.
type encodedObject struct {
	value         cty.Value
	body          *configschema.Block
	schemaVersion uint64

	attrsJSON      []byte
	sensitivePaths []cty.Path

	// verified is true if attrsJSON is known to be what serializing the
	// value produces, aside from any insignificant whitespace. That's not
	// certain for an object that was decoded from attrsJSON, because there
	// are several ways to write the same value.
	verified bool

	// sortedDependencies is a dependency list that's known to be sorted
	// already, identified by its first element and its length, if any.
	sortedDependencies    *addrs.ConfigResource
	sortedDependenciesLen int
}

// encodedFor returns what's already known about the serialized form of the
// object's current value under the given schema, if anything.
func (o *ResourceInstanceObject) encodedFor(schema providers.Schema) *encodedObject {
	enc := (*encodedObject)(atomic.LoadPointer(&o.encoded))
	if enc == nil || enc.body != schema.Body || enc.schemaVersion != uint64(schema.Version) || !sameValue(enc.value, o.Value) {
		return nil
	}
	return enc
}

// ShareEncodingFrom tells the receiver about the serialized form of the
// given other object, which avoids serializing the receiver's value again
// if it turns out to be the very same value. It's always safe to call this,
// because the information is ignored if the values are different.
func (o *ResourceInstanceObject) ShareEncodingFrom(other *ResourceInstanceObject) {
	if o == nil || other == nil {
		return
	}
	if enc := atomic.LoadPointer(&other.encoded); enc != nil {
		atomic.StorePointer(&o.encoded, enc)
	}
}

// sameValue returns true only if the two given values are certain to be
// equal, including their marks, because one is a copy of the other. Values
// are immutable, so two that refer to the same underlying data must be equal.
//
// This is far cheaper than comparing the values, but it can return false
// for values that are equal.
func sameValue(a, b cty.Value) bool {
	if !valueIsFourWords {
		return false
	}
	return *(*[4]uintptr)(unsafe.Pointer(&a)) == *(*[4]uintptr)(unsafe.Pointer(&b))
}

// valueIsFourWords is true if cty.Value has the layout that sameValue
// relies on, which is a type and a value that are each an interface value.
var valueIsFourWords = func() bool {
	ty := reflect.TypeOf(cty.Value{})
	if ty.Size() != 4*unsafe.Sizeof(uintptr(0)) || ty.NumField() != 2 {
		return false
	}
	tyField, vField := ty.Field(0).Type, ty.Field(1).Type
	return tyField.Kind() == reflect.Struct && tyField.NumField() == 1 &&
		tyField.Field(0).Type.Kind() == reflect.Interface &&
		vField.Kind() == reflect.Interface
}()

// NewResourceInstanceObjectFromIR converts the receiving
// ImportedResource into a ResourceInstanceObject that has status ObjectReady.
//
// The returned object does not know its own resource type, so the caller must
// retain the ResourceType value from the source object if this information is
// needed.
//
// The returned object also has no dependency addresses, but the caller may
// freely modify the direct fields of the returned object without affecting
// the receiver.
func NewResourceInstanceObjectFromIR(ir providers.ImportedResource) *ResourceInstanceObject {
	return &ResourceInstanceObject{
		Status:   ObjectReady,
		Value:    ir.State,
		Private:  ir.Private,
		Identity: ir.Identity,
	}
}

// ObjectStatus represents the status of a RemoteObject.
type ObjectStatus rune

//go:generate go tool golang.org/x/tools/cmd/stringer -type ObjectStatus

const (
	// ObjectReady is an object status for an object that is ready to use.
	ObjectReady ObjectStatus = 'R'

	// ObjectTainted is an object status representing an object that is in
	// an unrecoverable bad state due to a partial failure during a create,
	// update, or delete operation. Since it cannot be moved into the
	// ObjectRead state, a tainted object must be replaced.
	ObjectTainted ObjectStatus = 'T'

	// ObjectPlanned is a special object status used only for the transient
	// placeholder objects we place into state during the refresh and plan
	// walks to stand in for objects that will be created during apply.
	//
	// Any object of this status must have a corresponding change recorded
	// in the current plan, whose value must then be used in preference to
	// the value stored in state when evaluating expressions. A planned
	// object stored in state will be incomplete if any of its attributes are
	// not yet known, and the plan must be consulted in order to "see" those
	// unknown values, because the state is not able to represent them.
	ObjectPlanned ObjectStatus = 'P'
)

// Encode marshals values within the receiver to produce a
// ResourceInstanceObjectSrc ready to be written to a state file.
//
// The schema must contain the resource type body, and the given value must
// conform its implied type. The schema must also contain the version number
// of the schema, which will be recorded in the source object so it can be
// used to detect when schema migration is required on read.
// The schema may also contain an resource identity schema and version number,
// which will be used to encode the resource identity.
//
// The returned object may share internal references with the receiver and
// so the caller must not mutate the receiver any further once once this
// method is called.
func (o *ResourceInstanceObject) Encode(schema providers.Schema) (*ResourceInstanceObjectSrc, error) {
	known := o.encodedFor(schema)
	if known != nil && known.verified {
		return o.encodeWith(schema, known)
	}

	// If it contains marks, remove these marks before traversing the
	// structure with UnknownAsNull, and save the PathValueMarks
	// so we can save them in state.
	val, sensitivePaths, err := unmarkValueForStorage(o.Value)
	if err != nil {
		return nil, err
	}

	// Our state serialization can't represent unknown values, so we convert
	// them to nulls here. This is lossy, but nobody should be writing unknown
	// values here and expecting to get them out again later.
	//
	// We get unknown values here while we're building out a "planned state"
	// during the plan phase, but the value stored in the plan takes precedence
	// for expression evaluation. The apply step should never produce unknown
	// values, but if it does it's the responsibility of the caller to detect
	// and raise an error about that.
	//
	// Replacing unknown values means building a whole new value, so we
	// skip it in the common case where there aren't any.
	if !val.IsWhollyKnown() {
		val = cty.UnknownAsNull(val)
	}

	src, err := marshalJSON(val, schema.Body.ImpliedType())
	if err != nil {
		return nil, err
	}

	if known != nil && equivalentJSON(src, known.attrsJSON) {
		// The object was decoded from a serialization which we now know
		// is equivalent to the one we've just produced, so we continue
		// with that and let the new one be freed, so that everything
		// derived from the original shares its memory.
		src = known.attrsJSON
	}
	enc := &encodedObject{
		value:          o.Value,
		body:           schema.Body,
		schemaVersion:  uint64(schema.Version),
		attrsJSON:      src,
		sensitivePaths: sensitivePaths,
		verified:       true,
	}
	atomic.StorePointer(&o.encoded, unsafe.Pointer(enc))

	return o.encodeWith(schema, enc)
}

// encodeWith is the remainder of Encode once the serialized form of the
// object's value has been determined.
func (o *ResourceInstanceObject) encodeWith(schema providers.Schema, enc *encodedObject) (*ResourceInstanceObjectSrc, error) {
	src, sensitivePaths := enc.attrsJSON, enc.sensitivePaths
	var err error
	var idJSON []byte
	// If the Identity is known and not null we can marshal it.
	if !o.Identity.IsNull() && o.Identity.IsWhollyKnown() && schema.Identity != nil {
		idJSON, err = marshalJSON(o.Identity, schema.Identity.ImpliedType())
		if err != nil {
			return nil, err
		}
	}

	// Dependencies are collected and merged in an unordered format (using map
	// keys as a set), then later changed to a slice (in random ordering) to be
	// stored in state as an array. To avoid pointless thrashing of state in
	// refresh-only runs, we can either override comparison of dependency lists
	// (more desirable, but tricky for Reasons) or just sort when encoding.
	// Encoding of instances can happen concurrently, so we must copy the
	// dependencies to avoid mutating what may be a shared array of values.
	//
	// The dependencies are usually already sorted, because they most often
	// come either from the graph builder or from a prior state, both of
	// which sort them. In that case we use the given slice directly, since
	// dependencies are immutable once assigned.
	dependencies := o.Dependencies
	dependencyLess := func(a, b addrs.ConfigResource) int {
		switch {
		case a.StringLess(b):
			return -1
		case b.StringLess(a):
			return 1
		default:
			return 0
		}
	}
	switch {
	case dependencies == nil:
		dependencies = []addrs.ConfigResource{}
	case len(dependencies) != 0 && enc.sortedDependencies == &dependencies[0] && enc.sortedDependenciesLen == len(dependencies):
		// We've already checked these.
	case !slices.IsSortedFunc(dependencies, dependencyLess):
		dependencies = slices.Clone(dependencies)
		slices.SortFunc(dependencies, dependencyLess)
	case len(dependencies) != 0:
		// An object is typically encoded several times with the same
		// dependencies, and there can be a great many of them, so we
		// remember that there's no need to check these again.
		checked := *enc
		checked.sortedDependencies = &dependencies[0]
		checked.sortedDependenciesLen = len(dependencies)
		atomic.CompareAndSwapPointer(&o.encoded, unsafe.Pointer(enc), unsafe.Pointer(&checked))
	}

	return &ResourceInstanceObjectSrc{
		SchemaVersion:         uint64(schema.Version),
		AttrsJSON:             src,
		AttrSensitivePaths:    sensitivePaths,
		Private:               o.Private,
		Status:                o.Status,
		Dependencies:          dependencies,
		CreateBeforeDestroy:   o.CreateBeforeDestroy,
		IdentityJSON:          idJSON,
		IdentitySchemaVersion: uint64(schema.IdentityVersion),
		// The cached value must have all its marks since it bypasses decoding.
		decodeValueCache:    o.Value,
		decodeIdentityCache: o.Identity,
		attrsVerified:       len(sensitivePaths) == 0,
	}, nil
}

// AsTainted returns a deep copy of the receiver with the status updated to
// ObjectTainted.
func (o *ResourceInstanceObject) AsTainted() *ResourceInstanceObject {
	if o == nil {
		// A nil object can't be tainted, but we'll allow this anyway to
		// avoid a crash, since we presumably intend to eventually record
		// the object has having been deleted anyway.
		return nil
	}
	if o.Status == ObjectTainted {
		return o
	}

	ret := o.DeepCopy()
	ret.Status = ObjectTainted
	return ret
}

// unmarkValueForStorage takes a value that possibly contains marked values
// and returns an equal value without markings along with the separated mark
// metadata that should be stored alongside the value in another field.
//
// This function only accepts the marks that are valid to store, and so will
// return an error if other marks are present. Marks that this package doesn't
// know how to store must be dealt with somehow by a caller -- presumably by
// replacing each marked value with some sort of storage placeholder -- before
// writing a value into the state.
func unmarkValueForStorage(v cty.Value) (unmarkedV cty.Value, sensitivePaths []cty.Path, err error) {
	if !marks.ContainsMarked(v) {
		// Removing marks means building a whole new value, so we skip it
		// in the common case where there aren't any.
		return v, nil, nil
	}
	val, pvms := v.UnmarkDeepWithPaths()
	sensitivePaths, withOtherMarks := marks.PathsWithMark(pvms, marks.Sensitive)
	_, withOtherMarks = marks.PathsWithMark(withOtherMarks, marks.Deprecation)
	if len(withOtherMarks) != 0 {
		return cty.NilVal, nil, fmt.Errorf(
			"%s: cannot serialize value marked as %#v for inclusion in a state snapshot (this is a bug in Terraform)",
			format.CtyPath(withOtherMarks[0].Path), withOtherMarks[0].Marks,
		)
	}

	// sort the sensitive paths for consistency in comparison and serialization
	sort.Slice(sensitivePaths, func(i, j int) bool {
		// use our human-readable format of paths for comparison
		return format.CtyPath(sensitivePaths[i]) < format.CtyPath(sensitivePaths[j])
	})

	return val, sensitivePaths, nil
}
