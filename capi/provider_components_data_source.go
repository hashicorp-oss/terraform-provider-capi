// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

var _ datasource.DataSource = &providerComponentsDataSource{}

// NewProviderComponentsDataSource returns the capi_provider_components data source.
func NewProviderComponentsDataSource() datasource.DataSource {
	return &providerComponentsDataSource{}
}

// providerComponentsDataSource reads a provider release the way clusterctl
// init would fetch it and reports the kinds it ships, so templates and other
// objects of that provider can be rendered before any cluster exists.
type providerComponentsDataSource struct{}

// providerComponentsModel is the capi_provider_components model.
type providerComponentsModel struct {
	Type            types.String `tfsdk:"type"`
	Name            types.String `tfsdk:"name"`
	Version         types.String `tfsdk:"version"`
	FetchConfig     types.Object `tfsdk:"fetch_config"`
	CoreContract    types.String `tfsdk:"core_contract"`
	URL             types.String `tfsdk:"url"`
	ResolvedVersion types.String `tfsdk:"resolved_version"`
	Contract        types.String `tfsdk:"contract"`
	Kinds           types.Map    `tfsdk:"kinds"`
}

// kindRefModel is one entry of `kinds`: a kind at the API version the
// provider's generator would emit for it.
type kindRefModel struct {
	APIVersion types.String `tfsdk:"api_version"`
	Group      types.String `tfsdk:"group"`
	Version    types.String `tfsdk:"version"`
	Kind       types.String `tfsdk:"kind"`
}

func kindRefAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"api_version": types.StringType,
		"group":       types.StringType,
		"version":     types.StringType,
		"kind":        types.StringType,
	}
}

func (d *providerComponentsDataSource) Metadata(
	_ context.Context,
	req datasource.MetadataRequest,
	resp *datasource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_provider_components"
}

func (d *providerComponentsDataSource) Schema(
	_ context.Context,
	_ datasource.SchemaRequest,
	resp *datasource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The components of a CAPI provider release, fetched exactly as `clusterctl init` would fetch them (pinned `version`, `fetch_config`, the clusterctl config file and environment all apply), and the kinds its CRDs serve. With literal inputs it is read at plan time, so objects of the provider (`capi_machine_template`, `capi_object`) can name their `api_version` and `kind` from `kinds` and render completely on the first plan, before any cluster exists.",
		Attributes: map[string]schema.Attribute{
			"type": schema.StringAttribute{
				MarkdownDescription: "Provider type: `core`, `infrastructure`, `bootstrap`, `control_plane`, `ipam` or `addon`.",
				Required:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Provider name as clusterctl knows it (`tinkerbell`, `talos`, `kubeadm`, ...).",
				Required:            true,
			},
			"version": schema.StringAttribute{
				MarkdownDescription: "Release tag (`v0.7.9`). Omitted means the repository's latest, which needs a GitHub API call.",
				Optional:            true,
			},
			"fetch_config": schema.SingleNestedAttribute{
				MarkdownDescription: "Where the components come from, as on a `capi_cluster` provider entry: `owner`/`repository` build a GitHub releases URL, `url` or `oci` are used verbatim.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"owner": schema.StringAttribute{
						MarkdownDescription: "GitHub owner.",
						Optional:            true,
					},
					"repository": schema.StringAttribute{
						MarkdownDescription: "GitHub repository.",
						Optional:            true,
					},
					"url": schema.StringAttribute{
						MarkdownDescription: "Verbatim clusterctl components URL.",
						Optional:            true,
					},
					"oci": schema.StringAttribute{
						MarkdownDescription: "Verbatim OCI reference.",
						Optional:            true,
					},
				},
			},
			"core_contract": schema.StringAttribute{
				MarkdownDescription: "Core API version the provider's kinds are resolved against, the way the installed core provider would (`v1beta2`). Defaults to the contract the release declares.",
				Optional:            true,
			},
			"url": schema.StringAttribute{
				MarkdownDescription: "Resolved components URL.",
				Computed:            true,
			},
			"resolved_version": schema.StringAttribute{
				MarkdownDescription: "The release that was fetched.",
				Computed:            true,
			},
			"contract": schema.StringAttribute{
				MarkdownDescription: "CAPI contract the release implements, from its metadata.",
				Computed:            true,
			},
			"kinds": schema.MapNestedAttribute{
				MarkdownDescription: "Every CRD kind the release ships, keyed by kind name (`TinkerbellMachineTemplate`), at the API version objects of it should use.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"api_version": schema.StringAttribute{
							MarkdownDescription: "`group/version`.",
							Computed:            true,
						},
						"group": schema.StringAttribute{
							MarkdownDescription: "API group.",
							Computed:            true,
						},
						"version": schema.StringAttribute{
							MarkdownDescription: "API version.",
							Computed:            true,
						},
						"kind": schema.StringAttribute{
							MarkdownDescription: "Kind.",
							Computed:            true,
						},
					},
				},
			},
		},
	}
}

func (d *providerComponentsDataSource) Read(
	ctx context.Context,
	req datasource.ReadRequest,
	resp *datasource.ReadResponse,
) {
	var data providerComponentsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	typ, ok := providerTypeFromSchema(data.Type.ValueString())
	if !ok {
		resp.Diagnostics.AddError(
			"Invalid provider type",
			fmt.Sprintf(
				"type %q is not one of core, infrastructure, bootstrap, control_plane, ipam, addon.",
				data.Type.ValueString(),
			),
		)
		return
	}
	cfg := capi.ProviderConfig{
		Name:    data.Name.ValueString(),
		Type:    typ,
		Version: data.Version.ValueString(),
	}
	if !data.FetchConfig.IsNull() && !data.FetchConfig.IsUnknown() {
		var fc fetchConfigModel
		resp.Diagnostics.Append(data.FetchConfig.As(ctx, &fc, basetypes.ObjectAsOptions{})...)
		if resp.Diagnostics.HasError() {
			return
		}
		cfg.FetchConfig = &capi.FetchConfig{
			Owner:      fc.Owner.ValueString(),
			Repository: fc.Repository.ValueString(),
			URL:        fc.URL.ValueString(),
			OCI:        fc.OCI.ValueString(),
		}
	}

	comps, err := capi.FetchProviderComponents(ctx, "", cfg, data.CoreContract.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Fetching provider components", err.Error())
		return
	}
	data.URL = types.StringValue(comps.URL)
	data.ResolvedVersion = types.StringValue(comps.Version)
	data.Contract = stringOrNull(comps.Contract)
	kinds := map[string]kindRefModel{}
	for _, name := range capi.SortedKinds(comps.Kinds) {
		gvk := comps.Kinds[name]
		kinds[name] = kindRefModel{
			APIVersion: types.StringValue(
				gvk.GroupVersion().String(),
			),
			Group:   types.StringValue(gvk.Group),
			Version: types.StringValue(gvk.Version),
			Kind:    types.StringValue(gvk.Kind),
		}
	}
	kindsVal, diags := types.MapValueFrom(
		ctx,
		types.ObjectType{AttrTypes: kindRefAttrTypes()},
		kinds,
	)
	resp.Diagnostics.Append(diags...)
	data.Kinds = kindsVal
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// providerTypeFromSchema maps the schema spelling of a provider type to the
// backend type.
func providerTypeFromSchema(s string) (capi.ProviderType, bool) {
	switch s {
	case "core":
		return capi.ProviderTypeCore, true
	case "infrastructure":
		return capi.ProviderTypeInfrastructure, true
	case "bootstrap":
		return capi.ProviderTypeBootstrap, true
	case "control_plane", "control-plane":
		return capi.ProviderTypeControlPlane, true
	case "ipam":
		return capi.ProviderTypeIPAM, true
	case "addon":
		return capi.ProviderTypeAddon, true
	}
	return "", false
}
