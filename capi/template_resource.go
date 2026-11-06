// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
)

// templateKind describes one of the template resources: which provider type
// its objects belong to, the attribute naming that provider, and the kind
// suffix a role maps to.
type templateKind struct {
	typeSuffix   string
	providerAttr string
	providerType capi.ProviderType
	providerDesc string
	roles        map[string]string // role -> kind suffix; nil when the resource has no role
	defaultRole  string
	description  string
}

var (
	machineTemplateKind = templateKind{
		typeSuffix:   "_machine_template",
		providerAttr: "infrastructure",
		providerType: capi.ProviderTypeInfrastructure,
		providerDesc: "Infrastructure provider name as clusterctl knows it (`tinkerbell`, `docker`).",
		roles:        map[string]string{"machine": "MachineTemplate", "cluster": "ClusterTemplate"},
		defaultRole:  "machine",
		description:  "An infrastructure provider's machine template (`role = \"machine\"`, the `<Infra>MachineTemplate` a MachineDeployment, a control plane or a ClusterClass refers to) or cluster template (`role = \"cluster\"`, the `<Infra>ClusterTemplate` a ClusterClass refers to).",
	}
	controlPlaneTemplateKind = templateKind{
		typeSuffix:   "_control_plane_template",
		providerAttr: "control_plane",
		providerType: capi.ProviderTypeControlPlane,
		providerDesc: "Control-plane provider name as clusterctl knows it (`talos`, `kubeadm`).",
		roles:        map[string]string{"control_plane": "ControlPlaneTemplate"},
		defaultRole:  "control_plane",
		description:  "A control-plane provider's `<CP>ControlPlaneTemplate`, the control plane half of a ClusterClass.",
	}
	bootstrapTemplateKind = templateKind{
		typeSuffix:   "_bootstrap_template",
		providerAttr: "bootstrap",
		providerType: capi.ProviderTypeBootstrap,
		providerDesc: "Bootstrap provider name as clusterctl knows it (`talos`, `kubeadm`).",
		roles:        map[string]string{"config": "ConfigTemplate"},
		defaultRole:  "config",
		description:  "A bootstrap provider's `<Bootstrap>ConfigTemplate`, the bootstrap half of a worker class or MachineDeployment.",
	}
)

var (
	_ resource.Resource                   = &templateResource{}
	_ resource.ResourceWithConfigure      = &templateResource{}
	_ resource.ResourceWithIdentity       = &templateResource{}
	_ resource.ResourceWithImportState    = &templateResource{}
	_ resource.ResourceWithModifyPlan     = &templateResource{}
	_ resource.ResourceWithValidateConfig = &templateResource{}
)

// NewMachineTemplateResource returns capi_machine_template.
func NewMachineTemplateResource() resource.Resource {
	return &templateResource{kind: machineTemplateKind}
}

// NewControlPlaneTemplateResource returns capi_control_plane_template.
func NewControlPlaneTemplateResource() resource.Resource {
	return &templateResource{kind: controlPlaneTemplateKind}
}

// NewBootstrapTemplateResource returns capi_bootstrap_template.
func NewBootstrapTemplateResource() resource.Resource {
	return &templateResource{kind: bootstrapTemplateKind}
}

// templateResource is an immutable CAPI template with the launch-template
// pattern: a stable logical name, one hash-named object per revision of the
// spec, a `ref` that always points at the current revision, and garbage
// collection of revisions nothing references any more.
type templateResource struct {
	kind templateKind
	pd   *providerData
}

// templateInputs is what one revision is rendered from.
type templateInputs struct {
	provider, role, name, namespace, apiVersion, kind string
	labels, annotations                               map[string]string
	spec                                              map[string]any
	revisionHistory                                   int64
}

func (r *templateResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + r.kind.typeSuffix
}

// IdentitySchema implements [resource.ResourceWithIdentity]. The identity is
// the logical name, which every revision shares; an import by identity takes
// the newest revision.
func (r *templateResource) IdentitySchema(
	_ context.Context,
	_ resource.IdentitySchemaRequest,
	resp *resource.IdentitySchemaResponse,
) {
	resp.IdentitySchema = namespacedIdentitySchema()
}

// templateIdentity is the identity of the template a plan or state names.
func templateIdentity(
	ctx context.Context,
	from attrGetter,
) (namespacedIdentityModel, diag.Diagnostics) {
	var namespace, name types.String
	diags := from.GetAttribute(ctx, path.Root("namespace"), &namespace)
	diags.Append(from.GetAttribute(ctx, path.Root("name"), &name)...)
	return namespacedIdentity(namespace, name), diags
}

func (r *templateResource) Schema(
	ctx context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	attrs := map[string]schema.Attribute{
		"timeouts": timeouts.Attributes(
			ctx,
			timeouts.Opts{Create: true, Update: true, Delete: true},
		),
		"id": schema.StringAttribute{
			Computed:      true,
			PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		},
		r.kind.providerAttr: schema.StringAttribute{
			MarkdownDescription: r.kind.providerDesc + " Selects the kind from the installed CRDs when `kind` is unset.",
			Required:            true,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
		},
		"name": schema.StringAttribute{
			MarkdownDescription: "Logical name. Each revision is the object `<name>-<hash of the spec>`.",
			Required:            true,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
		},
		"namespace": schema.StringAttribute{
			MarkdownDescription: "Namespace. Default `default`.",
			Optional:            true,
			Computed:            true,
			Default:             stringdefault.StaticString("default"),
			PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
		},
		"api_version": schema.StringAttribute{
			MarkdownDescription: "`group/version` of the template kind. Resolved from the installed CRDs at apply when unset; name it (from `data.capi_provider_components`) to render completely at plan.",
			Optional:            true,
			Computed:            true,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		},
		"kind": schema.StringAttribute{
			MarkdownDescription: "Template kind. Resolved from the provider's CRDs at apply when unset.",
			Optional:            true,
			Computed:            true,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		},
		"labels":      metadataMapAttribute("Labels on every revision."),
		"annotations": metadataMapAttribute("Annotations on every revision."),
		"spec": schema.DynamicAttribute{
			MarkdownDescription: "The template's inner spec (`spec.template.spec`), as an object with the CRD's camelCase keys. Any change is a new revision; consumers move their `ref` and CAPI rolls the change out.",
			Required:            true,
		},
		"revision_history": schema.Int64Attribute{
			MarkdownDescription: "How many unreferenced older revisions to keep. Referenced revisions are never deleted. Default `1`.",
			Optional:            true,
			Computed:            true,
			Default:             int64default.StaticInt64(1),
		},
		"object_name": schema.StringAttribute{
			MarkdownDescription: "Name of the current revision's object.",
			Computed:            true,
		},
		"revision": schema.StringAttribute{
			MarkdownDescription: "Content hash of the current revision.",
			Computed:            true,
		},
		"ref": schema.SingleNestedAttribute{
			MarkdownDescription: "Reference to the current revision, for `capi_machine_deployment`, `capi_cluster_class` and `capi_cluster`.",
			Computed:            true,
			Attributes: map[string]schema.Attribute{
				"api_group":   schema.StringAttribute{Computed: true},
				"api_version": schema.StringAttribute{Computed: true},
				"kind":        schema.StringAttribute{Computed: true},
				"name":        schema.StringAttribute{Computed: true},
				"namespace":   schema.StringAttribute{Computed: true},
			},
		},
		"revisions": schema.ListNestedAttribute{
			MarkdownDescription: "Every revision object still present, newest first by name, and what refers to it.",
			Computed:            true,
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{Computed: true},
					"referenced_by": schema.ListAttribute{
						ElementType: types.StringType,
						Computed:    true,
					},
				},
			},
		},
	}
	if len(r.kind.roles) > 1 {
		roles := make([]string, 0, len(r.kind.roles))
		for role := range r.kind.roles {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		attrs["role"] = schema.StringAttribute{
			MarkdownDescription: fmt.Sprintf(
				"Which template kind: %s. Default `%s`.",
				strings.Join(roles, ", "),
				r.kind.defaultRole,
			),
			Optional:      true,
			Computed:      true,
			Default:       stringdefault.StaticString(r.kind.defaultRole),
			Validators:    []validator.String{stringvalidator.OneOf(roles...)},
			PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
		}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: r.kind.description + " CAPI templates are immutable, so this resource works like a launch template: a spec change creates a new object named `<name>-<hash>`, `ref` moves to it, consumers roll out, and older revisions are deleted once nothing references them (a MachineSet keeps the old template during a rollout) beyond `revision_history`. Import with `namespace/object_name`.",
		Attributes:          attrs,
	}
}

func (r *templateResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData),
		)
		return
	}
	r.pd = pd
}

func (r *templateResource) ValidateConfig(
	ctx context.Context,
	req resource.ValidateConfigRequest,
	resp *resource.ValidateConfigResponse,
) {
	var apiVersion, kind types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("api_version"), &apiVersion)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("kind"), &kind)...)
	if (!apiVersion.IsNull() && !apiVersion.IsUnknown()) != (!kind.IsNull() && !kind.IsUnknown()) {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_version"),
			"Incomplete kind",
			"api_version and kind are set together, or neither is set and both are resolved from the installed CRDs.",
		)
	}
}

// ModifyPlan computes the revision from the planned spec, so a change shows
// as the new object name and ref in the plan, before anything is applied.
func (r *templateResource) ModifyPlan(
	ctx context.Context,
	req resource.ModifyPlanRequest,
	resp *resource.ModifyPlanResponse,
) {
	if req.Plan.Raw.IsNull() {
		return
	}
	in, d := r.inputs(ctx, req.Plan)
	if d.HasError() || in.spec == nil {
		// An unknown spec: everything derived from it stays unknown.
		return
	}
	hash, err := capi.SpecHash(templateSpec(in.spec))
	if err != nil {
		return
	}
	objectName := in.name + "-" + hash
	resp.Diagnostics.Append(
		resp.Plan.SetAttribute(ctx, path.Root("object_name"), types.StringValue(objectName))...)
	resp.Diagnostics.Append(
		resp.Plan.SetAttribute(ctx, path.Root("revision"), types.StringValue(hash))...)
	if in.apiVersion != "" && in.kind != "" {
		resp.Diagnostics.Append(
			resp.Plan.SetAttribute(
				ctx,
				path.Root("ref"),
				templateRefValue(gvkFor(in.apiVersion, in.kind), in.namespace, objectName),
			)...)
	}
}

// inputs reads the attributes a revision is rendered from. spec is nil when
// it is not known yet.
func (r *templateResource) inputs(
	ctx context.Context,
	from attrGetter,
) (templateInputs, diag.Diagnostics) {
	var diags diag.Diagnostics
	var in templateInputs
	get := func(name string, target any) {
		diags.Append(from.GetAttribute(ctx, path.Root(name), target)...)
	}
	var provider, role, name, namespace, apiVersion, kind types.String
	var labels, annotations types.Map
	var spec types.Dynamic
	var history types.Int64
	get(r.kind.providerAttr, &provider)
	if len(r.kind.roles) > 1 {
		get("role", &role)
	}
	get("name", &name)
	get("namespace", &namespace)
	get("api_version", &apiVersion)
	get("kind", &kind)
	get("labels", &labels)
	get("annotations", &annotations)
	get("spec", &spec)
	get("revision_history", &history)
	if diags.HasError() {
		return in, diags
	}
	str := func(s types.String) string {
		if s.IsNull() || s.IsUnknown() {
			return ""
		}
		return s.ValueString()
	}
	in.provider, in.role, in.name, in.namespace = str(
		provider,
	), str(
		role,
	), str(
		name,
	), str(
		namespace,
	)
	in.apiVersion, in.kind = str(apiVersion), str(kind)
	if in.role == "" {
		in.role = r.kind.defaultRole
	}
	if in.namespace == "" {
		in.namespace = "default"
	}
	in.revisionHistory = 1
	if !history.IsNull() && !history.IsUnknown() {
		in.revisionHistory = history.ValueInt64()
	}
	var d diag.Diagnostics
	in.labels, d = stringMap(ctx, labels)
	diags.Append(d...)
	in.annotations, d = stringMap(ctx, annotations)
	diags.Append(d...)
	if !spec.IsUnknown() && !spec.IsUnderlyingValueUnknown() {
		in.spec, d = dynamicToMap(spec)
		diags.Append(d...)
		if in.spec == nil {
			in.spec = map[string]any{}
		}
	}
	return in, diags
}

// attrGetter is what Plan, State and Config have in common.
type attrGetter interface {
	GetAttribute(ctx context.Context, p path.Path, target any) diag.Diagnostics
}

// templateSpec wraps the inner spec the way the object carries it.
func templateSpec(inner map[string]any) map[string]any {
	return map[string]any{"template": map[string]any{"spec": inner}}
}

// resolveGVK returns the template kind from api_version/kind, else from the
// provider's installed CRDs by role suffix.
func (r *templateResource) resolveGVK(
	ctx context.Context,
	client *capi.ObjectClient,
	in templateInputs,
) (k8sschema.GroupVersionKind, error) {
	if in.apiVersion != "" && in.kind != "" {
		return gvkFor(in.apiVersion, in.kind), nil
	}
	suffix, ok := r.kind.roles[in.role]
	if !ok {
		return k8sschema.GroupVersionKind{}, fmt.Errorf("role %q is not supported", in.role)
	}
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return k8sschema.GroupVersionKind{}, err
	}
	return catalog.Resolve(capi.ProviderKey{Type: r.kind.providerType, Name: in.provider}, suffix)
}

// render builds the revision object.
func (r *templateResource) render(
	in templateInputs,
	gvk k8sschema.GroupVersionKind,
) (*unstructured.Unstructured, string, error) {
	full := templateSpec(in.spec)
	hash, err := capi.SpecHash(full)
	if err != nil {
		return nil, "", err
	}
	labels := map[string]string{templateLabel: in.name}
	for k, v := range in.labels {
		labels[k] = v
	}
	annotations := map[string]string{revisionAnnotation: hash}
	for k, v := range in.annotations {
		annotations[k] = v
	}
	obj := newObject(gvk, in.namespace, in.name+"-"+hash, labels, annotations)
	obj.Object["spec"] = full
	return obj, hash, nil
}

func (r *templateResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var t timeouts.Value
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("timeouts"), &t)...)
	createTimeout, timeoutDiags := t.Create(ctx, defaultTimeout)
	resp.Diagnostics.Append(timeoutDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	r.converge(ctx, req.Plan, &resp.State, &resp.Diagnostics)
	id, d := templateIdentity(ctx, req.Plan)
	resp.Diagnostics.Append(d...)
	resp.Diagnostics.Append(setIdentity(ctx, resp.Identity, id)...)
}

func (r *templateResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var t timeouts.Value
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("timeouts"), &t)...)
	updateTimeout, timeoutDiags := t.Update(ctx, defaultTimeout)
	resp.Diagnostics.Append(timeoutDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	r.converge(ctx, req.Plan, &resp.State, &resp.Diagnostics)
	id, d := templateIdentity(ctx, req.Plan)
	resp.Diagnostics.Append(d...)
	resp.Diagnostics.Append(setIdentity(ctx, resp.Identity, id)...)
}

// converge applies the planned revision and collects old ones.
func (r *templateResource) converge(
	ctx context.Context,
	plan attrGetter,
	state attrSetter,
	diags *diag.Diagnostics,
) {
	in, d := r.inputs(ctx, plan)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	gvk, err := r.resolveGVK(ctx, client, in)
	if err != nil {
		diags.AddError("Resolving template kind", err.Error())
		return
	}
	obj, hash, err := r.render(in, gvk)
	if err != nil {
		diags.AddError("Rendering template", err.Error())
		return
	}
	if _, err := client.Apply(ctx, obj, false); err != nil {
		diags.AddError("Applying "+gvk.Kind, err.Error())
		return
	}
	revisions, refs, d := r.collect(ctx, client, gvk, in, obj.GetName())
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	r.setState(ctx, state, plan, in, gvk, obj.GetName(), hash, revisions, refs, diags)
}

// collect lists the template's revisions, deletes the unreferenced ones
// beyond revision_history (never the current one), and returns what is left
// with its referrers.
func (r *templateResource) collect(
	ctx context.Context,
	client *capi.ObjectClient,
	gvk k8sschema.GroupVersionKind,
	in templateInputs,
	current string,
) ([]unstructured.Unstructured, capi.ReferenceIndex, diag.Diagnostics) {
	var diags diag.Diagnostics
	catalog, err := client.Catalog(ctx)
	if err != nil {
		diags.AddError("Reading CRDs", err.Error())
		return nil, nil, diags
	}
	refs, err := capi.TemplateReferences(ctx, client, catalog, in.namespace)
	if err != nil {
		diags.AddError("Indexing template references", err.Error())
		return nil, nil, diags
	}
	revisions, err := client.List(ctx, gvk, in.namespace, templateLabel+"="+in.name)
	if err != nil {
		diags.AddError("Listing revisions", err.Error())
		return nil, nil, diags
	}
	// Newest first: creation time, then name, so history keeps the latest.
	sort.SliceStable(revisions, func(i, j int) bool {
		ti, tj := revisions[i].GetCreationTimestamp(), revisions[j].GetCreationTimestamp()
		if !ti.Equal(&tj) {
			return ti.After(tj.Time)
		}
		return revisions[i].GetName() > revisions[j].GetName()
	})
	var kept []unstructured.Unstructured
	unreferenced := int64(0)
	for _, rev := range revisions {
		name := rev.GetName()
		if name == current || len(refs.Referrers(gvk.Kind, name)) > 0 {
			kept = append(kept, rev)
			continue
		}
		if unreferenced < in.revisionHistory {
			unreferenced++
			kept = append(kept, rev)
			continue
		}
		if err := client.Delete(ctx, gvk, in.namespace, name); err != nil {
			diags.AddError("Deleting old revision", err.Error())
			return nil, nil, diags
		}
	}
	return kept, refs, diags
}

// attrSetter is what State offers for setting attributes.
type attrSetter interface {
	SetAttribute(ctx context.Context, p path.Path, val any) diag.Diagnostics
}

// setState writes every attribute of the resource.
func (r *templateResource) setState(
	ctx context.Context,
	state attrSetter,
	plan attrGetter,
	in templateInputs,
	gvk k8sschema.GroupVersionKind,
	objectName, hash string,
	revisions []unstructured.Unstructured,
	refs capi.ReferenceIndex,
	diags *diag.Diagnostics,
) {
	set := func(name string, v any) { diags.Append(state.SetAttribute(ctx, path.Root(name), v)...) }
	// Copy the planned inputs through, then the computed attributes.
	for _, name := range []string{r.kind.providerAttr, "name", "namespace", "labels", "annotations", "spec", "revision_history", "timeouts"} {
		var v attr.Value
		switch name {
		case "timeouts":
			var t timeouts.Value
			diags.Append(plan.GetAttribute(ctx, path.Root(name), &t)...)
			v = t
		case "labels", "annotations":
			var m types.Map
			diags.Append(plan.GetAttribute(ctx, path.Root(name), &m)...)
			v = m
		case "spec":
			var s types.Dynamic
			diags.Append(plan.GetAttribute(ctx, path.Root(name), &s)...)
			v = s
		case "revision_history":
			var i types.Int64
			diags.Append(plan.GetAttribute(ctx, path.Root(name), &i)...)
			v = i
		default:
			var s types.String
			diags.Append(plan.GetAttribute(ctx, path.Root(name), &s)...)
			v = s
		}
		set(name, v)
	}
	if len(r.kind.roles) > 1 {
		set("role", types.StringValue(in.role))
	}
	set("id", types.StringValue(fmt.Sprintf("%s:%s/%s", gvk.Kind, in.namespace, in.name)))
	set("api_version", types.StringValue(gvk.GroupVersion().String()))
	set("kind", types.StringValue(gvk.Kind))
	set("object_name", types.StringValue(objectName))
	set("revision", types.StringValue(hash))
	set("ref", templateRefValue(gvk, in.namespace, objectName))
	set("revisions", revisionsValue(ctx, gvk.Kind, revisions, refs))
}

// templateRefValue renders a ref object.
func templateRefValue(gvk k8sschema.GroupVersionKind, namespace, name string) types.Object {
	v, _ := types.ObjectValue(templateRefAttrTypes(), map[string]attr.Value{
		"api_group": types.StringValue(
			gvk.Group,
		),
		"api_version": types.StringValue(gvk.GroupVersion().String()),
		"kind": types.StringValue(
			gvk.Kind,
		),
		"name":      types.StringValue(name),
		"namespace": types.StringValue(namespace),
	})
	return v
}

func templateRefAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"api_group": types.StringType, "api_version": types.StringType, "kind": types.StringType,
		"name": types.StringType, "namespace": types.StringType,
	}
}

func revisionAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":          types.StringType,
		"referenced_by": types.ListType{ElemType: types.StringType},
	}
}

func revisionsValue(
	ctx context.Context,
	kind string,
	revisions []unstructured.Unstructured,
	refs capi.ReferenceIndex,
) types.List {
	elems := make([]attr.Value, 0, len(revisions))
	for _, rev := range revisions {
		referrers := refs.Referrers(kind, rev.GetName())
		list, _ := types.ListValueFrom(ctx, types.StringType, referrers)
		if referrers == nil {
			list, _ = types.ListValueFrom(ctx, types.StringType, []string{})
		}
		obj, _ := types.ObjectValue(
			revisionAttrTypes(),
			map[string]attr.Value{"name": types.StringValue(rev.GetName()), "referenced_by": list},
		)
		elems = append(elems, obj)
	}
	v, _ := types.ListValue(types.ObjectType{AttrTypes: revisionAttrTypes()}, elems)
	return v
}

func (r *templateResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	in, d := r.inputs(ctx, req.State)
	resp.Diagnostics.Append(d...)
	var objectName types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("object_name"), &objectName)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The identity follows from the state, whatever the read finds.
	id, d := templateIdentity(ctx, req.State)
	resp.Diagnostics.Append(d...)
	resp.Diagnostics.Append(setIdentity(ctx, resp.Identity, id)...)
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	gvk := gvkFor(in.apiVersion, in.kind)
	live, found, d := readLive(ctx, client, gvk, in.namespace, objectName.ValueString())
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	// Drift on the inner spec the configuration owns.
	liveInner, _, _ := unstructured.NestedMap(live.Object, "spec", "template", "spec")
	if in.spec != nil && !subsetEqual(in.spec, liveInner) {
		projected, d := dynamicFromMap(projectSubset(in.spec, liveInner))
		resp.Diagnostics.Append(d...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("spec"), projected)...)
	}
	catalog, err := client.Catalog(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Reading CRDs", err.Error())
		return
	}
	refs, err := capi.TemplateReferences(ctx, client, catalog, in.namespace)
	if err != nil {
		resp.Diagnostics.AddError("Indexing template references", err.Error())
		return
	}
	revisions, err := client.List(ctx, gvk, in.namespace, templateLabel+"="+in.name)
	if err != nil {
		resp.Diagnostics.AddError("Listing revisions", err.Error())
		return
	}
	resp.Diagnostics.Append(
		resp.State.SetAttribute(
			ctx,
			path.Root("revisions"),
			revisionsValue(ctx, gvk.Kind, revisions, refs),
		)...)
}

func (r *templateResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var t timeouts.Value
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("timeouts"), &t)...)
	deleteTimeout, timeoutDiags := t.Delete(ctx, defaultTimeout)
	resp.Diagnostics.Append(timeoutDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	in, d := r.inputs(ctx, req.State)
	resp.Diagnostics.Append(d...)
	var objectName types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("object_name"), &objectName)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	gvk := gvkFor(in.apiVersion, in.kind)
	catalog, err := client.Catalog(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Reading CRDs", err.Error())
		return
	}
	refs, err := capi.TemplateReferences(ctx, client, catalog, in.namespace)
	if err != nil {
		resp.Diagnostics.AddError("Indexing template references", err.Error())
		return
	}
	if referrers := refs.Referrers(gvk.Kind, objectName.ValueString()); len(referrers) > 0 {
		resp.Diagnostics.AddError(
			"Template still referenced",
			fmt.Sprintf(
				"%s %s/%s is referenced by %s; move those references first.",
				gvk.Kind,
				in.namespace,
				objectName.ValueString(),
				strings.Join(referrers, ", "),
			),
		)
		return
	}
	revisions, err := client.List(ctx, gvk, in.namespace, templateLabel+"="+in.name)
	if err != nil {
		resp.Diagnostics.AddError("Listing revisions", err.Error())
		return
	}
	for _, rev := range revisions {
		if referrers := refs.Referrers(gvk.Kind, rev.GetName()); len(referrers) > 0 {
			resp.Diagnostics.AddWarning(
				"Revision left in place",
				fmt.Sprintf(
					"%s %s/%s is still referenced by %s and was not deleted.",
					gvk.Kind,
					in.namespace,
					rev.GetName(),
					strings.Join(referrers, ", "),
				),
			)
			continue
		}
		if err := client.Delete(ctx, gvk, in.namespace, rev.GetName()); err != nil {
			resp.Diagnostics.AddError("Deleting revision", err.Error())
			return
		}
	}
	if err := client.WaitForDeletion(
		ctx,
		gvk,
		in.namespace,
		objectName.ValueString(),
		capi.WaitOptions{Timeout: 5 * time.Minute},
	); err != nil {
		resp.Diagnostics.AddError("Deleting "+gvk.Kind, err.Error())
	}
}

// ImportState takes `namespace/object_name` and recovers the logical name
// and revision from the object's label and annotation.
func (r *templateResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	// An import ID names one revision object; an identity names the logical
	// template, whose newest revision is taken.
	var namespace, objectName, logicalName string
	if req.ID == "" && req.Identity != nil {
		var id namespacedIdentityModel
		resp.Diagnostics.Append(req.Identity.Get(ctx, &id)...)
		if resp.Diagnostics.HasError() {
			return
		}
		namespace, logicalName = defaultNamespace(id.Namespace).ValueString(), id.Name.ValueString()
	} else {
		var found bool
		namespace, objectName, found = strings.Cut(strings.TrimSpace(req.ID), "/")
		if !found || namespace == "" || objectName == "" {
			resp.Diagnostics.AddError(
				"Invalid import ID",
				fmt.Sprintf("Expected namespace/object_name, got %q.", req.ID),
			)
			return
		}
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	catalog, err := client.Catalog(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Reading CRDs", err.Error())
		return
	}
	// Find the object among the kinds this resource can hold.
	var live *unstructured.Unstructured
	var gvk k8sschema.GroupVersionKind
	var provider string
	var role string
	for _, p := range catalog.Providers(r.kind.providerType) {
		for roleName, suffix := range r.kind.roles {
			candidate, err := catalog.Resolve(p, suffix)
			if err != nil {
				continue
			}
			obj, err := r.importCandidate(
				ctx,
				client,
				candidate,
				namespace,
				objectName,
				logicalName,
			)
			if err != nil || obj == nil {
				continue
			}
			live, gvk, provider, role = obj, candidate, p.Name, roleName
			objectName = obj.GetName()
		}
	}
	if live == nil {
		name := objectName
		if logicalName != "" {
			name = logicalName + " (any revision)"
		}
		resp.Diagnostics.AddError(
			"Template not found",
			fmt.Sprintf(
				"no %s template named %s/%s on the management cluster.",
				r.kind.providerType,
				namespace,
				name,
			),
		)
		return
	}
	logical := live.GetLabels()[templateLabel]
	if logical == "" {
		logical = strings.TrimSuffix(objectName, "-"+live.GetAnnotations()[revisionAnnotation])
	}
	inner, _, _ := unstructured.NestedMap(live.Object, "spec", "template", "spec")
	spec, d := dynamicFromMap(inner)
	resp.Diagnostics.Append(d...)
	hash, _ := capi.SpecHash(templateSpec(inner))
	set := func(name string, v any) { resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(name), v)...) }
	set("id", types.StringValue(fmt.Sprintf("%s:%s/%s", gvk.Kind, namespace, logical)))
	set(r.kind.providerAttr, types.StringValue(provider))
	if len(r.kind.roles) > 1 {
		set("role", types.StringValue(role))
	}
	set("name", types.StringValue(logical))
	set("namespace", types.StringValue(namespace))
	set("api_version", types.StringValue(gvk.GroupVersion().String()))
	set("kind", types.StringValue(gvk.Kind))
	set("labels", types.MapNull(types.StringType))
	set("annotations", types.MapNull(types.StringType))
	set("spec", spec)
	set("revision_history", types.Int64Value(1))
	set("object_name", types.StringValue(objectName))
	set("revision", types.StringValue(hash))
	set("ref", templateRefValue(gvk, namespace, objectName))
	set(
		"revisions",
		revisionsValue(ctx, gvk.Kind, []unstructured.Unstructured{*live}, capi.ReferenceIndex{}),
	)
	resp.Diagnostics.Append(
		setIdentity(
			ctx,
			resp.Identity,
			namespacedIdentity(types.StringValue(namespace), types.StringValue(logical)),
		)...)
}

// importCandidate finds a template object of one candidate kind: the named
// revision, or with a logical name its newest revision.
func (r *templateResource) importCandidate(
	ctx context.Context,
	client *capi.ObjectClient,
	gvk k8sschema.GroupVersionKind,
	namespace, objectName, logicalName string,
) (*unstructured.Unstructured, error) {
	if logicalName == "" {
		return client.Get(ctx, gvk, namespace, objectName)
	}
	revisions, err := client.List(ctx, gvk, namespace, templateLabel+"="+logicalName)
	if err != nil || len(revisions) == 0 {
		return nil, err
	}
	sort.SliceStable(revisions, func(i, j int) bool {
		ti, tj := revisions[i].GetCreationTimestamp(), revisions[j].GetCreationTimestamp()
		if !ti.Equal(&tj) {
			return ti.After(tj.Time)
		}
		return revisions[i].GetName() > revisions[j].GetName()
	})
	return &revisions[0], nil
}
