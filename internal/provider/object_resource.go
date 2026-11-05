// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

var (
	_ resource.Resource                = &ObjectResource{}
	_ resource.ResourceWithConfigure   = &ObjectResource{}
	_ resource.ResourceWithImportState = &ObjectResource{}
)

// NewObjectResource returns the capi_object resource.
func NewObjectResource() resource.Resource {
	return &ObjectResource{}
}

// ObjectResource is one custom resource of an installed provider on the
// management cluster, server-side applied: the generic form of the typed
// object resources, for the kinds they do not cover (an IPAM pool, a
// MachineHealthCheck of an unusual shape).
type ObjectResource struct {
	pd *providerData
}

// ObjectResourceModel is the capi_object model.
type ObjectResourceModel struct {
	ID             types.String  `tfsdk:"id"`
	APIVersion     types.String  `tfsdk:"api_version"`
	Kind           types.String  `tfsdk:"kind"`
	ProviderType   types.String  `tfsdk:"provider_type"`
	ProviderName   types.String  `tfsdk:"provider_name"`
	Name           types.String  `tfsdk:"name"`
	Namespace      types.String  `tfsdk:"namespace"`
	Labels         types.Map     `tfsdk:"labels"`
	Annotations    types.Map     `tfsdk:"annotations"`
	Spec           types.Dynamic `tfsdk:"spec"`
	ForceConflicts types.Bool    `tfsdk:"force_conflicts"`
	Wait           types.Object  `tfsdk:"wait"`
	Manifest       types.String  `tfsdk:"manifest"`
	UID            types.String  `tfsdk:"uid"`
	Status         types.Dynamic `tfsdk:"status"`
}

func (r *ObjectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (r *ObjectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One object of an installed provider on the management cluster, server-side applied under this provider's field manager: the generic form of the typed object resources, for kinds they do not cover. `api_version` and `kind` name the object (take them from `data.capi_provider_components`); or give `kind` with `provider_type` and `provider_name` and the version is resolved from the installed CRDs at apply. Read compares every field `spec` sets against the live object; fields a controller owns never show as drift. Import with `kind:namespace/name`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"api_version": schema.StringAttribute{
				MarkdownDescription: "`group/version`. Resolved from the management cluster's CRDs when unset and `provider_type`/`provider_name` are given.",
				Optional:            true,
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"kind": schema.StringAttribute{
				MarkdownDescription: "Kind.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"provider_type": schema.StringAttribute{
				MarkdownDescription: "With `provider_name`, the provider whose CRDs serve `kind`: `core`, `infrastructure`, `bootstrap`, `control_plane`, `ipam` or `addon`. Only needed when `api_version` is unset.",
				Optional:            true,
			},
			"provider_name": schema.StringAttribute{
				MarkdownDescription: "Provider name as clusterctl knows it.",
				Optional:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Object name.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"namespace": schema.StringAttribute{
				MarkdownDescription: "Namespace. Default `default`; ignored for cluster-scoped kinds.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("default"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"labels":      metadataMapAttribute("Labels."),
			"annotations": metadataMapAttribute("Annotations."),
			"spec": schema.DynamicAttribute{
				MarkdownDescription: "The object's `spec`, as an object with the CRD's camelCase keys.",
				Optional:            true,
			},
			"force_conflicts": schema.BoolAttribute{
				MarkdownDescription: "Take ownership of fields another field manager holds instead of failing on the conflict.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"wait": objectWaitAttribute(nil),
			"manifest": schema.StringAttribute{
				MarkdownDescription: "The applied object as YAML.",
				Computed:            true,
			},
			"uid": schema.StringAttribute{
				MarkdownDescription: "The object's UID.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"status": schema.DynamicAttribute{
				MarkdownDescription: "The object's `status`, as read.",
				Computed:            true,
			},
		},
	}
}

func (r *ObjectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type", fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData))
		return
	}
	r.pd = pd
}

// resolveGVK returns the object's GVK from api_version, or from the
// installed CRDs of the named provider.
func (r *ObjectResource) resolveGVK(ctx context.Context, client *capi.ObjectClient, data *ObjectResourceModel) (k8sschema.GroupVersionKind, error) {
	if v := data.APIVersion.ValueString(); v != "" && !data.APIVersion.IsUnknown() {
		return gvkFor(v, data.Kind.ValueString()), nil
	}
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return k8sschema.GroupVersionKind{}, err
	}
	if data.ProviderName.ValueString() == "" {
		return catalog.ByKind(data.Kind.ValueString())
	}
	typ, ok := providerTypeFromSchema(data.ProviderType.ValueString())
	if !ok {
		return k8sschema.GroupVersionKind{}, fmt.Errorf("provider_type %q is not one of core, infrastructure, bootstrap, control_plane, ipam, addon", data.ProviderType.ValueString())
	}
	kinds, err := catalog.KindsOf(capi.ProviderKey{Type: typ, Name: data.ProviderName.ValueString()})
	if err != nil {
		return k8sschema.GroupVersionKind{}, err
	}
	gvk, ok := kinds[data.Kind.ValueString()]
	if !ok {
		return k8sschema.GroupVersionKind{}, fmt.Errorf("%w: provider %s/%s serves %v, not %s", capi.ErrKindNotFound, typ, data.ProviderName.ValueString(), capi.SortedKinds(kinds), data.Kind.ValueString())
	}
	return gvk, nil
}

// render builds the object to apply.
func (r *ObjectResource) render(ctx context.Context, data *ObjectResourceModel, gvk k8sschema.GroupVersionKind) (*unstructured.Unstructured, diag.Diagnostics) {
	var diags diag.Diagnostics
	labels, d := stringMap(ctx, data.Labels)
	diags.Append(d...)
	annotations, d := stringMap(ctx, data.Annotations)
	diags.Append(d...)
	spec, d := dynamicToMap(data.Spec)
	diags.Append(d...)
	if diags.HasError() {
		return nil, diags
	}
	obj := newObject(gvk, data.Namespace.ValueString(), data.Name.ValueString(), labels, annotations)
	if spec != nil {
		obj.Object["spec"] = spec
	}
	return obj, diags
}

func (r *ObjectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data ObjectResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ObjectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data ObjectResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// apply renders, applies and waits, then fills the computed attributes.
func (r *ObjectResource) apply(ctx context.Context, data *ObjectResourceModel, diags *diag.Diagnostics) {
	client, d := r.pd.objectClient(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	gvk, err := r.resolveGVK(ctx, client, data)
	if err != nil {
		diags.AddError("Resolving kind", err.Error())
		return
	}
	obj, d := r.render(ctx, data, gvk)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	conditions, timeout, d := objectWaitFromModel(ctx, data.Wait, nil)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	live, d := applyAndWait(ctx, client, obj, data.ForceConflicts.ValueBool(), conditions, timeout)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	data.APIVersion = types.StringValue(gvk.GroupVersion().String())
	data.ID = types.StringValue(fmt.Sprintf("%s:%s/%s", gvk.Kind, obj.GetNamespace(), obj.GetName()))
	data.Manifest = types.StringValue(manifestYAML(obj))
	diags.Append(r.readInto(data, live)...)
}

// readInto fills the computed attributes from a live object and, when the
// live object disagrees with the configured spec, replaces spec with the
// live values of the configured keys so the plan shows the drift.
func (r *ObjectResource) readInto(data *ObjectResourceModel, live *unstructured.Unstructured) diag.Diagnostics {
	var diags diag.Diagnostics
	data.UID = types.StringValue(string(live.GetUID()))
	status, _, _ := unstructured.NestedMap(live.Object, "status")
	st, d := dynamicFromMap(status)
	diags.Append(d...)
	data.Status = st

	desired, d := dynamicToMap(data.Spec)
	diags.Append(d...)
	if desired == nil {
		return diags
	}
	liveSpec, _, _ := unstructured.NestedMap(live.Object, "spec")
	if !subsetEqual(desired, liveSpec) {
		projected, d := dynamicFromMap(projectSubset(desired, liveSpec))
		diags.Append(d...)
		data.Spec = projected
	}
	return diags
}

func (r *ObjectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ObjectResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	gvk := gvkFor(data.APIVersion.ValueString(), data.Kind.ValueString())
	live, found, d := readLive(ctx, client, gvk, data.Namespace.ValueString(), data.Name.ValueString())
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(r.readInto(&data, live)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ObjectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data ObjectResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	_, timeout, d := objectWaitFromModel(ctx, data.Wait, nil)
	resp.Diagnostics.Append(d...)
	gvk := gvkFor(data.APIVersion.ValueString(), data.Kind.ValueString())
	resp.Diagnostics.Append(deleteAndWait(ctx, client, gvk, data.Namespace.ValueString(), data.Name.ValueString(), timeout)...)
}

func (r *ObjectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	kind, namespace, name, ok := parseObjectImportID(req.ID)
	if !ok {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected kind:namespace/name, got %q.", req.ID))
		return
	}
	if namespace == "" {
		namespace = "default"
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	catalog, err := client.Catalog(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Resolving kind", err.Error())
		return
	}
	gvk, err := catalog.ByKind(kind)
	if err != nil {
		resp.Diagnostics.AddError("Resolving kind", err.Error())
		return
	}
	live, err := client.Get(ctx, gvk, namespace, name)
	if errors.Is(err, capi.ErrObjectNotFound) {
		resp.Diagnostics.AddError("Object not found", fmt.Sprintf("%s %s/%s does not exist on the management cluster.", kind, namespace, name))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading object", err.Error())
		return
	}
	spec, _, _ := unstructured.NestedMap(live.Object, "spec")
	specVal, d := dynamicFromMap(spec)
	resp.Diagnostics.Append(d...)
	data := ObjectResourceModel{
		ID: types.StringValue(fmt.Sprintf("%s:%s/%s", kind, namespace, name)), APIVersion: types.StringValue(gvk.GroupVersion().String()),
		Kind: types.StringValue(kind), ProviderType: types.StringNull(), ProviderName: types.StringNull(),
		Name: types.StringValue(name), Namespace: types.StringValue(namespace),
		Labels: types.MapNull(types.StringType), Annotations: types.MapNull(types.StringType),
		Spec: specVal, ForceConflicts: types.BoolValue(false), Wait: types.ObjectNull(objectWaitAttrTypes()),
		Manifest: types.StringValue(manifestYAML(live)),
	}
	resp.Diagnostics.Append(r.readInto(&data, live)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
