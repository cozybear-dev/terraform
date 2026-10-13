// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

// Package bench is a provider with no remote API, used for benchmarking
// Terraform Core with large configurations and states.
package bench

import (
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/hashicorp/terraform/internal/configs/configschema"
	simple "github.com/hashicorp/terraform/internal/provider-simple"
	"github.com/hashicorp/terraform/internal/providers"
)

type bench struct {
	providers.Interface
	schema providers.GetProviderSchemaResponse
}

func Provider() providers.Interface {
	res := providers.Schema{
		Body: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"id":     {Computed: true, Type: cty.String},
				"arn":    {Computed: true, Type: cty.String},
				"name":   {Required: true, Type: cty.String},
				"value":  {Optional: true, Type: cty.String},
				"secret": {Optional: true, Type: cty.String, Sensitive: true},
				"tags":   {Optional: true, Type: cty.Map(cty.String)},
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
				"id":   {Computed: true, Type: cty.String},
				"name": {Required: true, Type: cty.String},
			},
		},
	}
	return bench{
		Interface: simple.Provider(),
		schema: providers.GetProviderSchemaResponse{
			Provider:      providers.Schema{Body: &configschema.Block{}},
			ResourceTypes: map[string]providers.Schema{"bench_resource": res},
			DataSources:   map[string]providers.Schema{"bench_resource": data},
			ServerCapabilities: providers.ServerCapabilities{
				PlanDestroy: true,
			},
		},
	}
}

func (b bench) GetProviderSchema() providers.GetProviderSchemaResponse {
	return b.schema
}

func (b bench) GetResourceIdentitySchemas() providers.GetResourceIdentitySchemasResponse {
	return providers.GetResourceIdentitySchemasResponse{}
}

func (b bench) UpgradeResourceState(req providers.UpgradeResourceStateRequest) (resp providers.UpgradeResourceStateResponse) {
	ty := b.schema.ResourceTypes[req.TypeName].Body.ImpliedType()
	val, err := ctyjson.Unmarshal(req.RawStateJSON, ty)
	resp.Diagnostics = resp.Diagnostics.Append(err)
	resp.UpgradedState = val
	return resp
}

func (b bench) ReadResource(req providers.ReadResourceRequest) (resp providers.ReadResourceResponse) {
	resp.NewState = req.PriorState
	return resp
}

func (b bench) PlanResourceChange(req providers.PlanResourceChangeRequest) (resp providers.PlanResourceChangeResponse) {
	if req.ProposedNewState.IsNull() {
		resp.PlannedState = req.ProposedNewState
		return resp
	}
	m := req.ProposedNewState.AsValueMap()
	for _, name := range []string{"id", "arn"} {
		if m[name].IsNull() {
			m[name] = cty.UnknownVal(cty.String)
		}
	}
	resp.PlannedState = cty.ObjectVal(m)
	return resp
}

func (b bench) ApplyResourceChange(req providers.ApplyResourceChangeRequest) (resp providers.ApplyResourceChangeResponse) {
	if req.PlannedState.IsNull() {
		resp.NewState = req.PlannedState
		return resp
	}
	m := req.PlannedState.AsValueMap()
	name := m["name"].AsString()
	if !m["id"].IsKnown() {
		m["id"] = cty.StringVal("id-" + name)
	}
	if !m["arn"].IsKnown() {
		m["arn"] = cty.StringVal("arn:bench:eu-west-1:123456789012:resource/" + name)
	}
	resp.NewState = cty.ObjectVal(m)
	return resp
}

func (b bench) ReadDataSource(req providers.ReadDataSourceRequest) (resp providers.ReadDataSourceResponse) {
	m := req.Config.AsValueMap()
	m["id"] = cty.StringVal("data-" + m["name"].AsString())
	resp.State = cty.ObjectVal(m)
	return resp
}
