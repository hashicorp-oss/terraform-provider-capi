// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

var (
	_ resource.Resource                = &ProviderResource{}
	_ resource.ResourceWithConfigure   = &ProviderResource{}
	_ resource.ResourceWithImportState = &ProviderResource{}
	_ resource.ResourceWithModifyPlan  = &ProviderResource{}
)

// privateKeyAdopted marks a provider that was already installed when the
// resource took it over (at create or import). Destroying the resource
// leaves such a provider installed: whatever installed it (clusterctl by
// hand, the cluster-api-operator, another configuration) still relies on it.
const privateKeyAdopted = "adopted"

func providerAdopted(ctx context.Context, r privateReader) (bool, diag.Diagnostics) {
	var adopted bool
	diags := loadPrivateJSON(ctx, r, privateKeyAdopted, &adopted)
	return adopted, diags
}

func markProviderAdopted(ctx context.Context, w privateWriter) diag.Diagnostics {
	return w.SetKey(ctx, privateKeyAdopted, []byte("true"))
}

// providerInstaller is the clusterctl lifecycle the resource drives.
type providerInstaller interface {
	Init(ctx context.Context, cluster *capi.Cluster, opts capi.InitOptions) error
	Upgrade(ctx context.Context, cluster *capi.Cluster, opts capi.UpgradeOptions) error
	Uninstall(ctx context.Context, cluster *capi.Cluster, opts capi.UninstallOptions) error
	Installed(ctx context.Context, cluster *capi.Cluster) ([]capi.InstalledProvider, error)
}

// NewProviderResource returns the capi_provider resource.
func NewProviderResource() resource.Resource {
	return &ProviderResource{}
}

// ProviderResource is one clusterctl provider on the management cluster the
// provider block names: Create is `clusterctl init`, Update is `clusterctl
// upgrade apply` (or a re-render at the installed version when only the
// customizations changed), Delete is `clusterctl delete` keeping CRDs. It is
// what aws_eks_addon is for an addon the cluster runs.
type ProviderResource struct {
	pd        *providerData
	installer providerInstaller
}

// ProviderResourceModel is the capi_provider model: the shared provider
// object (minus resource_config) plus identity, variables and outputs.
type ProviderResourceModel struct {
	ID                    types.String `tfsdk:"id"`
	Type                  types.String `tfsdk:"type"`
	Name                  types.String `tfsdk:"name"`
	Version               types.String `tfsdk:"version"`
	FetchConfig           types.Object `tfsdk:"fetch_config"`
	ConfigVariables       types.Map    `tfsdk:"config_variables"`
	SecretConfigVariables types.Map    `tfsdk:"secret_config_variables"`
	Deployment            types.Object `tfsdk:"deployment"`
	Manager               types.Object `tfsdk:"manager"`
	AdditionalManifests   types.String `tfsdk:"additional_manifests"`
	ManifestPatches       types.List   `tfsdk:"manifest_patches"`
	Patches               types.List   `tfsdk:"patches"`
	Variables             types.Map    `tfsdk:"variables"`
	InstalledVersion      types.String `tfsdk:"installed_version"`
	Namespace             types.String `tfsdk:"namespace"`
	Kinds                 types.Map    `tfsdk:"kinds"`
	Ref                   types.Object `tfsdk:"ref"`
}

// ProviderRefModel is what other resources take to name the provider.
type ProviderRefModel struct {
	Type    types.String `tfsdk:"type"`
	Name    types.String `tfsdk:"name"`
	Version types.String `tfsdk:"version"`
}

func providerRefAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"type": types.StringType, "name": types.StringType, "version": types.StringType}
}

var providerTypeNames = []string{"core", "infrastructure", "bootstrap", "control-plane", "ipam", "addon"}

func (r *ProviderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_provider"
}

func (r *ProviderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := map[string]schema.Attribute{
		"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		"type": schema.StringAttribute{
			MarkdownDescription: "Provider type: " + strings.Join(providerTypeNames, ", ") + ".",
			Required:            true,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
		},
		"name": schema.StringAttribute{
			MarkdownDescription: "Provider name as clusterctl knows it (`cluster-api`, `tinkerbell`, `talos`, `helm`).",
			Required:            true,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
		},
		"variables": schema.MapAttribute{
			MarkdownDescription: "clusterctl variables for the install (`CLUSTER_TOPOLOGY = \"true\"`), ahead of the clusterctl config file and the environment.",
			ElementType:         types.StringType,
			Optional:            true,
		},
		"installed_version": schema.StringAttribute{MarkdownDescription: "The version clusterctl's inventory reports.", Computed: true},
		"namespace":         schema.StringAttribute{MarkdownDescription: "Namespace the provider runs in.", Computed: true},
		"kinds": schema.MapNestedAttribute{
			MarkdownDescription: "The CRD kinds the provider serves, keyed by kind name, at the API version objects of it should use.",
			Computed:            true,
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"api_version": schema.StringAttribute{Computed: true},
					"group":       schema.StringAttribute{Computed: true},
					"version":     schema.StringAttribute{Computed: true},
					"kind":        schema.StringAttribute{Computed: true},
				},
			},
		},
		"ref": schema.SingleNestedAttribute{
			MarkdownDescription: "Reference for other resources: type, name and installed version.",
			Computed:            true,
			Attributes: map[string]schema.Attribute{
				"type":    schema.StringAttribute{Computed: true},
				"name":    schema.StringAttribute{Computed: true},
				"version": schema.StringAttribute{Computed: true},
			},
		},
	}
	for name, a := range providerNestedObject().Attributes {
		if name == "resource_config" {
			continue
		}
		attrs[name] = a
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "One CAPI provider on the management cluster the provider block names, driven through the clusterctl lifecycle the way `aws_eks_addon` drives an addon: create is `clusterctl init` (converging: a provider installed at another version is upgraded first), a changed entry is `clusterctl upgrade apply` (a changed customization at the same version re-renders it), destroy is `clusterctl delete` keeping its CRDs. A provider that was already installed when the resource was created, or that is imported, is adopted: the resource manages its version, but destroy leaves it installed for whatever installed it (a warning says so at plan time); the adoption is recorded in private state. The customizations (`deployment`, `manager`, `patches`, ...) follow the cluster-api-operator provider CRDs without needing the operator. Declare the `core` provider explicitly and have the others depend on it; on a cluster's first init clusterctl adds the core itself when missing, and this resource asks it not to add the kubeadm providers. A provider also declared in a `capi_cluster`'s maps is managed twice; keep it in one place. Import with `type/name`.",
		Attributes:          attrs,
	}
}

func (r *ProviderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type", fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData))
		return
	}
	r.pd = pd
	if r.installer == nil {
		r.installer = capi.NewClusterctlInstaller("")
	}
}

// config converts the model to the provider config and the clusterctl variables.
func (r *ProviderResource) config(ctx context.Context, data *ProviderResourceModel) (capi.ProviderConfig, map[string]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	typ := capi.ProviderType(data.Type.ValueString())
	if !contains(providerTypeNames, string(typ)) {
		diags.AddError("Invalid provider type", fmt.Sprintf("type %q is not one of %s.", typ, strings.Join(providerTypeNames, ", ")))
		return capi.ProviderConfig{}, nil, diags
	}
	pm := ProviderModel{
		Version: data.Version, FetchConfig: data.FetchConfig, ConfigVariables: data.ConfigVariables, SecretConfigVariables: data.SecretConfigVariables,
		Deployment: data.Deployment, Manager: data.Manager, AdditionalManifests: data.AdditionalManifests, ManifestPatches: data.ManifestPatches, Patches: data.Patches,
		ResourceConfig: types.StringNull(),
	}
	cfg, d := buildProviderConfig(ctx, data.Name.ValueString(), typ, pm)
	diags.Append(d...)
	vars, d := stringMap(ctx, data.Variables)
	diags.Append(d...)
	return cfg, vars, diags
}

// installed finds the provider in the inventory.
func (r *ProviderResource) installed(ctx context.Context, cluster *capi.Cluster, typ capi.ProviderType, name string) (*capi.InstalledProvider, error) {
	list, err := r.installer.Installed(ctx, cluster)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Type == typ && list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

// fill writes the computed attributes from the inventory and the CRDs.
func (r *ProviderResource) fill(ctx context.Context, data *ProviderResourceModel, inst *capi.InstalledProvider) diag.Diagnostics {
	var diags diag.Diagnostics
	data.ID = types.StringValue(data.Type.ValueString() + "/" + data.Name.ValueString())
	data.InstalledVersion, data.Namespace = stringOrNull(inst.Version), stringOrNull(inst.Namespace)
	ref, d := types.ObjectValueFrom(ctx, providerRefAttrTypes(), ProviderRefModel{Type: data.Type, Name: data.Name, Version: stringOrNull(inst.Version)})
	diags.Append(d...)
	data.Ref = ref
	data.Kinds = types.MapNull(types.ObjectType{AttrTypes: kindRefAttrTypes()})
	client, d := r.pd.objectClient(ctx)
	diags.Append(d...)
	if d.HasError() {
		return diags
	}
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return diags
	}
	kinds, err := catalog.KindsOf(capi.ProviderKey{Type: capi.ProviderType(data.Type.ValueString()), Name: data.Name.ValueString()})
	if err != nil {
		return diags // a provider without CRDs (an addon that ships none) has no kinds
	}
	models := map[string]KindRefModel{}
	for _, name := range capi.SortedKinds(kinds) {
		gvk := kinds[name]
		models[name] = KindRefModel{APIVersion: types.StringValue(gvk.GroupVersion().String()), Group: types.StringValue(gvk.Group), Version: types.StringValue(gvk.Version), Kind: types.StringValue(gvk.Kind)}
	}
	kv, d := types.MapValueFrom(ctx, types.ObjectType{AttrTypes: kindRefAttrTypes()}, models)
	diags.Append(d...)
	data.Kinds = kv
	return diags
}

func (r *ProviderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data ProviderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	conn, d := r.pd.managementOrDiag(ctx)
	resp.Diagnostics.Append(d...)
	cfg, vars, d := r.config(ctx, &data)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	inst, d := r.install(ctx, conn.Cluster, cfg, vars, resp.Private)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.fill(ctx, &data, inst)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// install runs clusterctl init for the provider and reads it back. A
// provider that was already installed is adopted: init converges its
// version, and the resource records that it did not install it.
func (r *ProviderResource) install(ctx context.Context, cluster *capi.Cluster, cfg capi.ProviderConfig, vars map[string]string, w privateWriter) (*capi.InstalledProvider, diag.Diagnostics) {
	var diags diag.Diagnostics
	before, err := r.installed(ctx, cluster, cfg.Type, cfg.Name)
	if err != nil {
		diags.AddError("Reading provider inventory", err.Error())
		return nil, diags
	}
	set := capi.ProviderSet{}
	set.Add(cfg)
	if err := r.installer.Init(ctx, cluster, capi.InitOptions{Providers: set, Variables: vars, NoDefaults: true}); err != nil {
		diags.AddError("Installing provider", err.Error())
		return nil, diags
	}
	inst, err := r.installed(ctx, cluster, cfg.Type, cfg.Name)
	if err != nil {
		diags.AddError("Reading provider inventory", err.Error())
		return nil, diags
	}
	if inst == nil {
		diags.AddError("Provider not installed", fmt.Sprintf("clusterctl init returned but the inventory has no %s provider %q.", cfg.Type, cfg.Name))
		return nil, diags
	}
	if before != nil {
		diags.Append(markProviderAdopted(ctx, w)...)
		diags.AddWarning("Provider adopted", fmt.Sprintf("The %s provider %q was already installed (%s in %s) before this resource was created. The resource now manages its version, but destroying it leaves the provider installed, since something else installed it. Remove it with `clusterctl delete` once nothing else relies on it.", cfg.Type, cfg.Name, before.Version, before.Namespace))
	}
	return inst, diags
}

func (r *ProviderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data ProviderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	conn, d := r.pd.managementOrDiag(ctx)
	resp.Diagnostics.Append(d...)
	cfg, vars, d := r.config(ctx, &data)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	set := capi.ProviderSet{}
	set.Add(cfg)
	if err := r.installer.Upgrade(ctx, conn.Cluster, capi.UpgradeOptions{Providers: set, Targets: []capi.ProviderKey{{Type: cfg.Type, Name: cfg.Name}}, Variables: vars}); err != nil {
		resp.Diagnostics.AddError("Upgrading provider", err.Error())
		return
	}
	inst, err := r.installed(ctx, conn.Cluster, cfg.Type, cfg.Name)
	if err != nil {
		resp.Diagnostics.AddError("Reading provider inventory", err.Error())
		return
	}
	if inst == nil {
		resp.Diagnostics.AddError("Provider not installed", fmt.Sprintf("clusterctl upgrade returned but the inventory has no %s provider %q.", cfg.Type, cfg.Name))
		return
	}
	resp.Diagnostics.Append(r.fill(ctx, &data, inst)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ProviderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ProviderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	conn, d := r.pd.managementOrDiag(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	inst, err := r.installed(ctx, conn.Cluster, capi.ProviderType(data.Type.ValueString()), data.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddWarning("Provider inventory unreachable", err.Error())
		return
	}
	if inst == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	// A pinned version that drifted is drift; an unpinned one follows the inventory.
	if !data.Version.IsNull() && !sameVersion(data.Version.ValueString(), inst.Version) {
		data.Version = types.StringValue(inst.Version)
	}
	resp.Diagnostics.Append(r.fill(ctx, &data, inst)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// sameVersion compares release tags ignoring a leading v.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

func (r *ProviderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data ProviderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	adopted, d := providerAdopted(ctx, req.Private)
	resp.Diagnostics.Append(d...)
	if adopted {
		resp.Diagnostics.AddWarning("Provider left installed", fmt.Sprintf("The %s provider %q was installed before this resource adopted it, so it stays installed; only the resource is removed.", data.Type.ValueString(), data.Name.ValueString()))
		return
	}
	conn, d := r.pd.managementOrDiag(ctx)
	resp.Diagnostics.Append(d...)
	cfg, _, d := r.config(ctx, &data)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.uninstall(ctx, conn.Cluster, cfg)...)
}

// uninstall runs clusterctl delete for a provider the resource installed;
// one already gone is not an error.
func (r *ProviderResource) uninstall(ctx context.Context, cluster *capi.Cluster, cfg capi.ProviderConfig) diag.Diagnostics {
	var diags diag.Diagnostics
	inst, err := r.installed(ctx, cluster, cfg.Type, cfg.Name)
	if err != nil {
		diags.AddError("Reading provider inventory", err.Error())
		return diags
	}
	if inst == nil {
		return diags
	}
	set := capi.ProviderSet{}
	set.Add(cfg)
	if err := r.installer.Uninstall(ctx, cluster, capi.UninstallOptions{Providers: set}); err != nil {
		diags.AddError("Deleting provider", err.Error())
	}
	return diags
}

// ModifyPlan says at plan time when destroying the resource will leave an
// adopted provider installed.
func (r *ProviderResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if !req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}
	adopted, d := providerAdopted(ctx, req.Private)
	resp.Diagnostics.Append(d...)
	if !adopted {
		return
	}
	var data ProviderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	resp.Diagnostics.AddWarning("Provider will be left installed", fmt.Sprintf("The %s provider %q was installed before this resource adopted it: destroying the resource removes it from state and leaves the provider installed.", data.Type.ValueString(), data.Name.ValueString()))
}

// ImportState takes `type/name`.
func (r *ProviderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(strings.TrimSpace(req.ID), "/", 2)
	if len(parts) != 2 || !contains(providerTypeNames, parts[0]) || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected type/name with type one of %s, got %q.", strings.Join(providerTypeNames, ", "), req.ID))
		return
	}
	conn, d := r.pd.managementOrDiag(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	inst, err := r.installed(ctx, conn.Cluster, capi.ProviderType(parts[0]), parts[1])
	if err != nil {
		resp.Diagnostics.AddError("Reading provider inventory", err.Error())
		return
	}
	if inst == nil {
		resp.Diagnostics.AddError("Provider not installed", fmt.Sprintf("The inventory has no %s provider %q.", parts[0], parts[1]))
		return
	}
	data := ProviderResourceModel{
		Type: types.StringValue(parts[0]), Name: types.StringValue(parts[1]), Version: stringOrNull(inst.Version),
		FetchConfig: types.ObjectNull(fetchConfigAttrTypes()), ConfigVariables: types.MapNull(types.StringType), SecretConfigVariables: types.MapNull(types.StringType),
		Deployment: types.ObjectNull(deploymentAttrTypes()), Manager: types.ObjectNull(managerAttrTypes()), AdditionalManifests: types.StringNull(),
		ManifestPatches: types.ListNull(types.StringType), Patches: types.ListNull(types.ObjectType{AttrTypes: patchAttrTypes()}), Variables: types.MapNull(types.StringType),
	}
	resp.Diagnostics.Append(r.fill(ctx, &data, inst)...)
	// An imported provider was installed by something else: destroy leaves it.
	resp.Diagnostics.Append(markProviderAdopted(ctx, resp.Private)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
