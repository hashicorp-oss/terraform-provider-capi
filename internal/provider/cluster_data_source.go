// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

var (
	_ datasource.DataSource              = &ClusterDataSource{}
	_ datasource.DataSourceWithConfigure = &ClusterDataSource{}
)

// NewClusterDataSource returns the capi_cluster data source.
func NewClusterDataSource() datasource.DataSource {
	return &ClusterDataSource{}
}

// ClusterDataSource reads a Cluster on the management cluster the provider
// block names: its conditions and the describe tree, the way `clusterctl
// describe cluster` shows them.
type ClusterDataSource struct {
	pd *providerData
}

// ClusterDataSourceModel is the capi_cluster data source model.
type ClusterDataSourceModel struct {
	Name                    types.String `tfsdk:"name"`
	Namespace               types.String `tfsdk:"namespace"`
	Phase                   types.String `tfsdk:"phase"`
	Endpoint                types.String `tfsdk:"endpoint"`
	KubernetesVersion       types.String `tfsdk:"kubernetes_version"`
	ControlPlaneInitialized types.Bool   `tfsdk:"control_plane_initialized"`
	Available               types.Bool   `tfsdk:"available"`
	Paused                  types.Bool   `tfsdk:"paused"`
	Conditions              types.List   `tfsdk:"conditions"`
	Description             types.String `tfsdk:"description"`
}

// ConditionModel is one status condition.
type ConditionModel struct {
	Type    types.String `tfsdk:"type"`
	Status  types.String `tfsdk:"status"`
	Reason  types.String `tfsdk:"reason"`
	Message types.String `tfsdk:"message"`
}

func conditionAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"type":    types.StringType,
		"status":  types.StringType,
		"reason":  types.StringType,
		"message": types.StringType,
	}
}

func (d *ClusterDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func (d *ClusterDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Cluster on the management cluster the provider block names: its phase, endpoint, conditions and the `clusterctl describe cluster` tree.",
		Attributes: map[string]schema.Attribute{
			"name":      schema.StringAttribute{MarkdownDescription: "Cluster name.", Required: true},
			"namespace": schema.StringAttribute{MarkdownDescription: "Namespace. Default `default`.", Optional: true},
			"phase":     schema.StringAttribute{MarkdownDescription: "`status.phase`.", Computed: true},
			"endpoint":  schema.StringAttribute{MarkdownDescription: "API server URL from `spec.controlPlaneEndpoint`.", Computed: true},
			"kubernetes_version": schema.StringAttribute{
				MarkdownDescription: "`spec.topology.version` when the cluster uses a ClusterClass.",
				Computed:            true,
			},
			"control_plane_initialized": schema.BoolAttribute{MarkdownDescription: "The `ControlPlaneInitialized` condition.", Computed: true},
			"available":                 schema.BoolAttribute{MarkdownDescription: "The `Available` (v1beta2) or `Ready` (v1beta1) condition.", Computed: true},
			"paused":                    schema.BoolAttribute{MarkdownDescription: "`spec.paused`.", Computed: true},
			"conditions": schema.ListNestedAttribute{
				MarkdownDescription: "`status.conditions`.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"type":    schema.StringAttribute{Computed: true},
						"status":  schema.StringAttribute{Computed: true},
						"reason":  schema.StringAttribute{Computed: true},
						"message": schema.StringAttribute{Computed: true},
					},
				},
			},
			"description": schema.StringAttribute{MarkdownDescription: "The rendered object tree.", Computed: true},
		},
	}
}

func (d *ClusterDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Data Source Configure Type", fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData))
		return
	}
	d.pd = pd
}

func (d *ClusterDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data ClusterDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	conn, diags := d.pd.managementOrDiag(ctx)
	resp.Diagnostics.Append(diags...)
	if diags.HasError() {
		return
	}
	namespace := "default"
	if !data.Namespace.IsNull() && data.Namespace.ValueString() != "" {
		namespace = data.Namespace.ValueString()
	}
	applier := capi.NewDynamicApplier()
	obj, err := applier.Get(ctx, conn.Cluster, data.Name.ValueString(), namespace)
	if errors.Is(err, capi.ErrClusterNotFound) {
		resp.Diagnostics.AddError("Cluster not found", fmt.Sprintf("Cluster %s/%s does not exist on the management cluster.", namespace, data.Name.ValueString()))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading cluster", err.Error())
		return
	}
	resp.Diagnostics.Append(populateClusterStatus(ctx, &data, obj)...)

	description, err := capi.NewClusterctlInfoRetriever("").Describe(ctx, conn.Cluster, data.Name.ValueString(), namespace)
	if err != nil {
		resp.Diagnostics.AddWarning("Describe unavailable", err.Error())
	}
	data.Description = stringOrNull(description)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// populateClusterStatus fills the status attributes from a Cluster object.
func populateClusterStatus(ctx context.Context, data *ClusterDataSourceModel, obj *unstructured.Unstructured) (diags diag.Diagnostics) {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	data.Phase = stringOrNull(phase)
	data.Endpoint = stringOrNull(clusterEndpointURL(obj))
	version, _, _ := unstructured.NestedString(obj.Object, "spec", "topology", "version")
	data.KubernetesVersion = stringOrNull(version)
	paused, _, _ := unstructured.NestedBool(obj.Object, "spec", "paused")
	data.Paused = types.BoolValue(paused)

	var conds []ConditionModel
	initialized, available := false, false
	raw, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range raw {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(m, "type")
		status, _, _ := unstructured.NestedString(m, "status")
		reason, _, _ := unstructured.NestedString(m, "reason")
		message, _, _ := unstructured.NestedString(m, "message")
		conds = append(conds, ConditionModel{Type: types.StringValue(typ), Status: types.StringValue(status), Reason: stringOrNull(reason), Message: stringOrNull(message)})
		if status == "True" {
			switch typ {
			case "ControlPlaneInitialized":
				initialized = true
			case "Available", "Ready":
				available = true
			}
		}
	}
	data.ControlPlaneInitialized = types.BoolValue(initialized)
	data.Available = types.BoolValue(available)
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: conditionAttrTypes()}, conds)
	diags = append(diags, d...)
	if conds == nil {
		list = types.ListNull(types.ObjectType{AttrTypes: conditionAttrTypes()})
	}
	data.Conditions = list
	return diags
}

// clusterEndpointURL renders spec.controlPlaneEndpoint as a URL, or "".
func clusterEndpointURL(obj *unstructured.Unstructured) string {
	host, _, _ := unstructured.NestedString(obj.Object, "spec", "controlPlaneEndpoint", "host")
	if host == "" {
		return ""
	}
	port, _, _ := unstructured.NestedInt64(obj.Object, "spec", "controlPlaneEndpoint", "port")
	if port == 0 {
		port = 6443
	}
	return fmt.Sprintf("https://%s:%d", host, port)
}
