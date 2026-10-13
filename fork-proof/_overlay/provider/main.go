// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

// Command provider is the deterministic provider that the fork's integrity
// proof plans and applies against. It is copied into a checkout of the
// pinned UPSTREAM commit and built there, so that nothing from the fork is
// part of it, and both builds of Terraform talk to the very same binary.
//
// It has no remote API and no source of nondeterminism: every computed value
// is a pure function of the configuration. It records every request it
// receives in the file named by FP_OPLOG, which is how the proof establishes
// that both builds asked the provider to do the same things, and in an order
// that honours the dependencies between resources.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/grpcwrap"
	"github.com/hashicorp/terraform/internal/plugin"
	simple "github.com/hashicorp/terraform/internal/provider-simple"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/tfplugin5"
)

func main() {
	plugin.Serve(&plugin.ServeOpts{
		GRPCProviderFunc: func() tfplugin5.ProviderServer {
			return server{grpcwrap.Provider(newProvider())}
		},
	})
}

// server adds to the wrapper that upstream uses for its own test providers
// the one thing this provider needs that it lacks: a way to report that a
// request failed. The wrapper discards the diagnostics that a provider
// returns, so the provider instead asks for an error by returning private
// data that begins with errorMarker, which is replaced here by an error
// diagnostic with the rest of that data as its message.
type server struct {
	tfplugin5.ProviderServer
}

const errorMarker = "fp-error:"

func failure(message string) []byte {
	return []byte(errorMarker + message)
}

func reportFailure(private []byte, diags []*tfplugin5.Diagnostic) ([]byte, []*tfplugin5.Diagnostic) {
	message, failed := strings.CutPrefix(string(private), errorMarker)
	if !failed {
		return private, diags
	}
	return nil, append(diags, &tfplugin5.Diagnostic{
		Severity: tfplugin5.Diagnostic_ERROR,
		Summary:  message,
		Detail:   "The configuration asked the provider to fail here.",
	})
}

func (s server) PlanResourceChange(ctx context.Context, req *tfplugin5.PlanResourceChange_Request) (*tfplugin5.PlanResourceChange_Response, error) {
	resp, err := s.ProviderServer.PlanResourceChange(ctx, req)
	if err == nil {
		resp.PlannedPrivate, resp.Diagnostics = reportFailure(resp.PlannedPrivate, resp.Diagnostics)
	}
	return resp, err
}

func (s server) ApplyResourceChange(ctx context.Context, req *tfplugin5.ApplyResourceChange_Request) (*tfplugin5.ApplyResourceChange_Response, error) {
	resp, err := s.ProviderServer.ApplyResourceChange(ctx, req)
	if err == nil {
		resp.Private, resp.Diagnostics = reportFailure(resp.Private, resp.Diagnostics)
	}
	return resp, err
}

type fp struct {
	providers.Interface
	schema providers.GetProviderSchemaResponse
	log    *opLog
}

func newProvider() providers.Interface {
	thing := providers.Schema{
		Version: 1,
		Body: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				// Computed, and a pure function of name.
				"id": {Computed: true, Type: cty.String},
				// Changing the name replaces the object.
				"name":   {Required: true, Type: cty.String},
				"value":  {Optional: true, Type: cty.String},
				"num":    {Optional: true, Type: cty.Number},
				"flag":   {Optional: true, Type: cty.Bool},
				"tags":   {Optional: true, Type: cty.Map(cty.String)},
				"items":  {Optional: true, Type: cty.List(cty.String)},
				"labels": {Optional: true, Type: cty.Set(cty.String)},
				"obj": {Optional: true, Type: cty.Object(map[string]cty.Type{
					"a": cty.String,
					"b": cty.Number,
				})},
				"dyn":    {Optional: true, Type: cty.DynamicPseudoType},
				"secret": {Optional: true, Type: cty.String, Sensitive: true},
				// Known during planning whenever value is.
				"derived": {Computed: true, Type: cty.String},
				// Unknown during planning whenever the object is created or
				// its value changes, and decided when it's applied.
				"late": {Computed: true, Type: cty.String},
				// Makes the provider misbehave. See behaviour.
				"behave": {Optional: true, Type: cty.String},
			},
			BlockTypes: map[string]*configschema.NestedBlock{
				"rule": {
					Nesting: configschema.NestingList,
					Block: configschema.Block{
						Attributes: map[string]*configschema.Attribute{
							"port":  {Required: true, Type: cty.Number},
							"cidr":  {Required: true, Type: cty.String},
							"proto": {Optional: true, Type: cty.String},
						},
					},
				},
			},
		},
	}
	data := providers.Schema{
		Body: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"id":     {Computed: true, Type: cty.String},
				"input":  {Required: true, Type: cty.String},
				"result": {Computed: true, Type: cty.String},
				"length": {Computed: true, Type: cty.Number},
				"behave": {Optional: true, Type: cty.String},
			},
		},
	}
	return fp{
		Interface: simple.Provider(),
		schema: providers.GetProviderSchemaResponse{
			Provider:      providers.Schema{Body: &configschema.Block{}},
			ResourceTypes: map[string]providers.Schema{"fp_thing": thing},
			DataSources:   map[string]providers.Schema{"fp_data": data},
			ServerCapabilities: providers.ServerCapabilities{
				PlanDestroy: true,
			},
		},
		log: &opLog{path: os.Getenv("FP_OPLOG")},
	}
}

func (p fp) GetProviderSchema() providers.GetProviderSchemaResponse {
	return p.schema
}

func (p fp) GetResourceIdentitySchemas() providers.GetResourceIdentitySchemasResponse {
	return providers.GetResourceIdentitySchemasResponse{}
}

func (p fp) ValidateProviderConfig(providers.ValidateProviderConfigRequest) (resp providers.ValidateProviderConfigResponse) {
	return resp
}

func (p fp) ValidateResourceConfig(req providers.ValidateResourceConfigRequest) (resp providers.ValidateResourceConfigResponse) {
	p.log.record("validate", req.TypeName, req.Config)
	return resp
}

func (p fp) ValidateDataResourceConfig(req providers.ValidateDataResourceConfigRequest) (resp providers.ValidateDataResourceConfigResponse) {
	p.log.record("validate_data", req.TypeName, req.Config)
	return resp
}

func (p fp) ConfigureProvider(providers.ConfigureProviderRequest) (resp providers.ConfigureProviderResponse) {
	return resp
}

func (p fp) UpgradeResourceState(req providers.UpgradeResourceStateRequest) (resp providers.UpgradeResourceStateResponse) {
	ty := p.schema.ResourceTypes[req.TypeName].Body.ImpliedType()
	val, err := ctyjson.Unmarshal(req.RawStateJSON, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	p.log.record("upgrade:"+strconv.FormatInt(req.Version, 10), req.TypeName, val)
	resp.UpgradedState = val
	return resp
}

// behaviour returns what the given object, if it has one, asks the provider
// to do other than behave normally, and the argument to that.
//
//	fail_plan       planning it is an error
//	fail_apply      creating or updating it is an error and changes nothing
//	fail_partial    creating or updating it is an error, but it is saved
//	fail_destroy    destroying it is an error
//	sleep:<ms>      applying any change to it takes that long
//	drift           refreshing it finds that its value has changed
func behaviour(obj cty.Value) (kind string, arg int) {
	if obj.IsNull() || !obj.IsKnown() {
		return "", 0
	}
	v := obj.GetAttr("behave")
	if v.IsNull() || !v.IsKnown() {
		return "", 0
	}
	kind, rest, _ := strings.Cut(v.AsString(), ":")
	arg, _ = strconv.Atoi(rest)
	return kind, arg
}

func (p fp) ReadResource(req providers.ReadResourceRequest) (resp providers.ReadResourceResponse) {
	p.log.record("read", req.TypeName, req.PriorState)
	resp.NewState = req.PriorState
	resp.Private = req.Private
	if kind, _ := behaviour(req.PriorState); kind == "drift" {
		m := req.PriorState.AsValueMap()
		if v := m["value"]; !v.IsNull() && !strings.HasSuffix(v.AsString(), "-drifted") {
			m["value"] = cty.StringVal(v.AsString() + "-drifted")
			m["derived"] = cty.StringVal("derived:" + m["value"].AsString())
			resp.NewState = cty.ObjectVal(m)
		}
	}
	return resp
}

func knownString(v cty.Value) (string, bool) {
	if !v.IsKnown() {
		return "", false
	}
	if v.IsNull() {
		return "", true
	}
	return v.AsString(), true
}

func (p fp) PlanResourceChange(req providers.PlanResourceChangeRequest) (resp providers.PlanResourceChangeResponse) {
	p.log.record("plan", req.TypeName, cty.TupleVal([]cty.Value{req.PriorState, req.ProposedNewState, req.Config}))
	resp.PlannedPrivate = req.PriorPrivate
	if req.ProposedNewState.IsNull() {
		resp.PlannedState = req.ProposedNewState
		return resp
	}
	if kind, _ := behaviour(req.ProposedNewState); kind == "fail_plan" {
		resp.PlannedPrivate = failure("fp_thing was asked to fail when planned")
	}

	m := req.ProposedNewState.AsValueMap()
	m["id"] = cty.UnknownVal(cty.String)
	if name, ok := knownString(m["name"]); ok {
		m["id"] = cty.StringVal("id-" + name)
	}
	m["derived"] = cty.UnknownVal(cty.String)
	if value, ok := knownString(m["value"]); ok {
		m["derived"] = cty.StringVal("derived:" + value)
	}
	prior := req.PriorState
	if prior.IsNull() || !prior.GetAttr("value").RawEquals(m["value"]) || !prior.GetAttr("name").RawEquals(m["name"]) {
		m["late"] = cty.UnknownVal(cty.String)
	} else {
		m["late"] = prior.GetAttr("late")
	}
	if !prior.IsNull() && !prior.GetAttr("name").RawEquals(m["name"]) {
		resp.RequiresReplace = []cty.Path{cty.GetAttrPath("name")}
	}
	resp.PlannedState = cty.ObjectVal(m)
	return resp
}

func (p fp) ApplyResourceChange(req providers.ApplyResourceChangeRequest) (resp providers.ApplyResourceChangeResponse) {
	op, subject := "apply", req.PlannedState
	if req.PlannedState.IsNull() {
		op, subject = "delete", req.PriorState
	} else if req.PriorState.IsNull() {
		op = "create"
	}
	done := p.log.begin(op, req.TypeName, cty.TupleVal([]cty.Value{req.PriorState, req.PlannedState}), subject)
	defer done()

	resp.Private = req.PlannedPrivate
	kind, arg := behaviour(subject)
	if kind == "sleep" {
		time.Sleep(time.Duration(arg) * time.Millisecond)
	}

	if req.PlannedState.IsNull() {
		if kind == "fail_destroy" {
			// The object is still there.
			resp.Private = failure("fp_thing was asked to fail when destroyed")
			resp.NewState = req.PriorState
			return resp
		}
		resp.NewState = req.PlannedState
		return resp
	}
	if kind == "fail_apply" {
		// Nothing was changed.
		resp.Private = failure("fp_thing was asked to fail when applied")
		resp.NewState = req.PriorState
		return resp
	}

	m := req.PlannedState.AsValueMap()
	name, _ := knownString(m["name"])
	value, _ := knownString(m["value"])
	m["id"] = cty.StringVal("id-" + name)
	m["derived"] = cty.StringVal("derived:" + value)
	if !m["late"].IsKnown() {
		m["late"] = cty.StringVal("late:" + name + ":" + value)
	}
	resp.NewState = cty.ObjectVal(m)
	if kind == "fail_partial" {
		// The object was changed, and the request still failed.
		resp.Private = failure("fp_thing was asked to fail after being saved")
	}
	return resp
}

func (p fp) ImportResourceState(req providers.ImportResourceStateRequest) (resp providers.ImportResourceStateResponse) {
	ty := p.schema.ResourceTypes[req.TypeName].Body.ImpliedType()
	m := make(map[string]cty.Value)
	for name, aty := range ty.AttributeTypes() {
		m[name] = cty.NullVal(aty)
	}
	m["name"] = cty.StringVal(req.ID)
	m["id"] = cty.StringVal("id-" + req.ID)
	m["value"] = cty.StringVal("imported")
	m["derived"] = cty.StringVal("derived:imported")
	m["late"] = cty.StringVal("late:" + req.ID + ":imported")
	m["rule"] = cty.ListValEmpty(ty.AttributeType("rule").ElementType())
	state := cty.ObjectVal(m)
	p.log.record("import:"+req.ID, req.TypeName, state)
	resp.ImportedResources = []providers.ImportedResource{{TypeName: req.TypeName, State: state}}
	return resp
}

func (p fp) ReadDataSource(req providers.ReadDataSourceRequest) (resp providers.ReadDataSourceResponse) {
	p.log.record("read_data", req.TypeName, req.Config)
	m := req.Config.AsValueMap()
	input := m["input"].AsString()
	m["id"] = cty.StringVal("data-" + input)
	m["result"] = cty.StringVal("data:" + input)
	m["length"] = cty.NumberIntVal(int64(len(input)))
	resp.State = cty.ObjectVal(m)
	return resp
}

// opLog appends a line to a file for each request the provider receives.
//
// Each line is "<sequence> <phase> <operation> <type> <name> <digest>". The
// sequence number orders the lines within one run of the provider, the phase
// is "begin" or "end" for the operations that change something and "call"
// for the rest, and the digest identifies the values in the request.
type opLog struct {
	path string
	mu   sync.Mutex
	seq  int
}

func (l *opLog) write(phase, op, typeName, name, digest string) {
	if l.path == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	// One write of one short line, appended, so that the lines of several
	// instances of the provider don't interleave, and so that the line is
	// there for whatever reads the file next even if this process, or
	// Terraform, is killed straight afterwards. It isn't forced to disk,
	// which only a power failure would call for.
	fmt.Fprintf(f, "%06d %s %s %s %s %s\n", l.seq, phase, op, typeName, name, digest)
}

func nameOf(v cty.Value) string {
	if v.IsNull() || !v.IsKnown() || !v.Type().IsObjectType() || !v.Type().HasAttribute("name") {
		return "-"
	}
	if name, ok := knownString(v.GetAttr("name")); ok && name != "" {
		return strconv.Quote(name)
	}
	return "-"
}

func (l *opLog) record(op, typeName string, v cty.Value) {
	subject := v
	if v.Type().IsTupleType() {
		// The planned or proposed object, or failing that the prior one.
		for _, ev := range v.AsValueSlice() {
			if !ev.IsNull() {
				subject = ev
			}
		}
		if es := v.AsValueSlice(); len(es) > 1 && !es[1].IsNull() {
			subject = es[1]
		}
	}
	l.write("call", op, typeName, nameOf(subject), digest(v))
}

func (l *opLog) begin(op, typeName string, v, subject cty.Value) (end func()) {
	name, d := nameOf(subject), digest(v)
	l.write("begin", op, typeName, name, d)
	return func() {
		l.write("end", op, typeName, name, d)
	}
}

// digest identifies a value by a hash of a canonical rendering of it.
func digest(v cty.Value) string {
	var b strings.Builder
	render(&b, v)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// render writes a rendering of the given value that depends only on what
// the value is, so that two requests carrying the same values have the
// same digest.
//
// An integer is written exactly, whatever its size. Any other number is
// written as the nearest 64-bit float, because the fork documents that it
// can send a provider the same number in a different encoding (as a decimal
// string rather than as a float, difference 9 in README-FORK.md), and those
// two encodings of a fraction decode to values that differ beyond the
// precision of a float.
func render(b *strings.Builder, v cty.Value) {
	v, _ = v.Unmark()
	ty := v.Type()
	switch {
	case !v.IsKnown():
		b.WriteString("unknown(" + ty.FriendlyName() + ")")
	case v.IsNull():
		b.WriteString("null(" + ty.FriendlyName() + ")")
	case ty == cty.String:
		b.WriteString(strconv.Quote(v.AsString()))
	case ty == cty.Bool:
		b.WriteString(strconv.FormatBool(v.True()))
	case ty == cty.Number:
		f := v.AsBigFloat()
		if f.IsInt() {
			i, _ := f.Int(new(big.Int))
			b.WriteString(i.String())
		} else {
			f64, _ := f.Float64()
			b.WriteString(strconv.FormatFloat(f64, 'g', -1, 64))
		}
	case ty.IsListType(), ty.IsSetType(), ty.IsTupleType():
		b.WriteString("[")
		for it := v.ElementIterator(); it.Next(); {
			_, ev := it.Element()
			render(b, ev)
			b.WriteString(",")
		}
		b.WriteString("]")
	case ty.IsMapType():
		b.WriteString("{")
		for it := v.ElementIterator(); it.Next(); {
			ek, ev := it.Element()
			b.WriteString(strconv.Quote(ek.AsString()) + ":")
			render(b, ev)
			b.WriteString(",")
		}
		b.WriteString("}")
	case ty.IsObjectType():
		names := make([]string, 0, len(ty.AttributeTypes()))
		for name := range ty.AttributeTypes() {
			names = append(names, name)
		}
		sort.Strings(names)
		b.WriteString("{")
		for _, name := range names {
			b.WriteString(name + "=")
			render(b, v.GetAttr(name))
			b.WriteString(",")
		}
		b.WriteString("}")
	default:
		b.WriteString("other(" + ty.FriendlyName() + ")")
	}
}
