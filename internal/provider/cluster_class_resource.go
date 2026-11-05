// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

var (
	_ resource.Resource                = &ClusterClassResource{}
	_ resource.ResourceWithConfigure   = &ClusterClassResource{}
	_ resource.ResourceWithImportState = &ClusterClassResource{}
)

// NewClusterClassResource returns the capi_cluster_class resource.
func NewClusterClassResource() resource.Resource {
	return &ClusterClassResource{}
}

// ClusterClassResource is a ClusterClass assembled from template refs: the
// cluster template, the control plane's template and machine template, and
// one worker class per template pair. A ref change in a template flows
// through the class to the cluster in one plan.
type ClusterClassResource struct {
	pd *providerData
}

// ClusterClassResourceModel is the capi_cluster_class model.
type ClusterClassResourceModel struct {
	ID             types.String  `tfsdk:"id"`
	Name           types.String  `tfsdk:"name"`
	Namespace      types.String  `tfsdk:"namespace"`
	Infrastructure types.Object  `tfsdk:"infrastructure"`
	ControlPlane   types.Object  `tfsdk:"control_plane"`
	Workers        types.Map     `tfsdk:"workers"`
	Variables      types.Dynamic `tfsdk:"variables"`
	Patches        types.Dynamic `tfsdk:"patches"`
	Wait           types.Object  `tfsdk:"wait"`
	Manifest       types.String  `tfsdk:"manifest"`
	Classes        types.List    `tfsdk:"classes"`
	Ref            types.Object  `tfsdk:"ref"`
	Conditions     types.List    `tfsdk:"conditions"`
}

// ControlPlaneClassModel is the control_plane block.
type ControlPlaneClassModel struct {
	Template       types.Object `tfsdk:"template"`
	Infrastructure types.Object `tfsdk:"infrastructure"`
	Labels         types.Map    `tfsdk:"labels"`
	Annotations    types.Map    `tfsdk:"annotations"`
}

// WorkerClassModel is one entry of workers.
type WorkerClassModel struct {
	Infrastructure types.Object `tfsdk:"infrastructure"`
	Bootstrap      types.Object `tfsdk:"bootstrap"`
	FailureDomain  types.String `tfsdk:"failure_domain"`
	Labels         types.Map    `tfsdk:"labels"`
	Annotations    types.Map    `tfsdk:"annotations"`
}

// ClassRefModel is the ref other resources take.
type ClassRefModel struct {
	Name      types.String `tfsdk:"name"`
	Namespace types.String `tfsdk:"namespace"`
}

func controlPlaneClassAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"template": types.ObjectType{AttrTypes: templateRefAttrTypes()}, "infrastructure": types.ObjectType{AttrTypes: templateRefAttrTypes()},
		"labels": types.MapType{ElemType: types.StringType}, "annotations": types.MapType{ElemType: types.StringType},
	}
}

func workerClassAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"infrastructure": types.ObjectType{AttrTypes: templateRefAttrTypes()}, "bootstrap": types.ObjectType{AttrTypes: templateRefAttrTypes()},
		"failure_domain": types.StringType, "labels": types.MapType{ElemType: types.StringType}, "annotations": types.MapType{ElemType: types.StringType},
	}
}

func classRefAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"name": types.StringType, "namespace": types.StringType}
}

// clusterClassWaitDefaults are the conditions waited for by default, per
// contract shape.
var (
	clusterClassWaitDefaults       = []string{"VariablesReady", "RefVersionsUpToDate"}
	clusterClassLegacyWaitDefaults = []string{"VariablesReconciled", "RefVersionsUpToDate"}
)

func (r *ClusterClassResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster_class"
}

func templateRefOptional(desc string) schema.SingleNestedAttribute {
	a := templateRefSchema(desc)
	a.Required, a.Optional = false, true
	return a
}

func (r *ClusterClassResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A ClusterClass on the management cluster the provider block names, assembled from template refs: `capi_machine_template` with `role = \"cluster\"` for `infrastructure`, `capi_control_plane_template` and a machine template for `control_plane`, and a machine template and `capi_bootstrap_template` per worker class. `variables` and `patches` are passed through as CAPI defines them. A new template revision flows through the class to every cluster using it in one plan. The class exports `ref` for `capi_cluster` and `classes`, the worker class names `capi_machine_deployment.class` validates against. Import with `namespace/name`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name": schema.StringAttribute{
				MarkdownDescription: "Object name.",
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
			"infrastructure": templateRefSchema("The `<Infra>ClusterTemplate` ref."),
			"control_plane": schema.SingleNestedAttribute{
				MarkdownDescription: "The control plane class.",
				Required:            true,
				Attributes: map[string]schema.Attribute{
					"template":       templateRefSchema("The `<CP>ControlPlaneTemplate` ref."),
					"infrastructure": templateRefOptional("The control-plane machines' `<Infra>MachineTemplate` ref, for control planes that manage machines."),
					"labels":         metadataMapAttribute("Labels on the control-plane machines."),
					"annotations":    metadataMapAttribute("Annotations on the control-plane machines."),
				},
			},
			"workers": schema.MapNestedAttribute{
				MarkdownDescription: "Worker classes, keyed by class name.",
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"infrastructure": templateRefSchema("The `<Infra>MachineTemplate` ref."),
						"bootstrap":      templateRefSchema("The `<Bootstrap>ConfigTemplate` ref."),
						"failure_domain": schema.StringAttribute{MarkdownDescription: "Default failure domain of the class.", Optional: true},
						"labels":         metadataMapAttribute("Labels on the class's machines."),
						"annotations":    metadataMapAttribute("Annotations on the class's machines."),
					},
				},
			},
			"variables": schema.DynamicAttribute{
				MarkdownDescription: "`spec.variables`: a list of variable definitions (`name`, `required`, `schema.openAPIV3Schema`), as CAPI defines them.",
				Optional:            true,
			},
			"patches": schema.DynamicAttribute{
				MarkdownDescription: "`spec.patches`: a list of patches (`name`, `definitions`, `external`, `enabledIf`), as CAPI defines them.",
				Optional:            true,
			},
			"wait":     objectWaitAttribute(clusterClassWaitDefaults),
			"manifest": schema.StringAttribute{MarkdownDescription: "The applied object as YAML.", Computed: true},
			"classes": schema.ListAttribute{
				MarkdownDescription: "The worker class names, sorted.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"ref": schema.SingleNestedAttribute{
				MarkdownDescription: "Reference for `capi_cluster.class`.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"name":      schema.StringAttribute{Computed: true},
					"namespace": schema.StringAttribute{Computed: true},
				},
			},
			"conditions": schema.ListNestedAttribute{
				MarkdownDescription: "The object's conditions.",
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
		},
	}
}

func (r *ClusterClassResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// --- rendering ---

// workerClassInputs is one worker class.
type workerClassInputs struct {
	Name           string
	Infrastructure *objectRef
	Bootstrap      *objectRef
	FailureDomain  string
	Labels         map[string]string
	Annotations    map[string]string
}

// clusterClassInputs is everything a ClusterClass renders from.
type clusterClassInputs struct {
	Name, Namespace    string
	CoreVersion        string
	Infrastructure     *objectRef
	ControlPlane       *objectRef
	ControlPlaneInfra  *objectRef
	ControlPlaneLabels map[string]string
	ControlPlaneAnnots map[string]string
	Workers            []workerClassInputs
	Variables          []any
	Patches            []any
}

// contractShape reports whether the v1beta2 shape applies.
func (in clusterClassInputs) contractShape() bool {
	return nodeGroupInputs{CoreVersion: in.CoreVersion}.contractRefs()
}

// validate checks the inputs.
func (in clusterClassInputs) validate() error {
	var errs []error
	if in.Infrastructure == nil || in.Infrastructure.Name == "" {
		errs = append(errs, errors.New("infrastructure is required"))
	}
	if in.ControlPlane == nil || in.ControlPlane.Name == "" {
		errs = append(errs, errors.New("control_plane.template is required"))
	}
	for _, w := range in.Workers {
		if w.Infrastructure == nil || w.Bootstrap == nil {
			errs = append(errs, fmt.Errorf("worker class %q needs infrastructure and bootstrap", w.Name))
		}
	}
	return errors.Join(errs...)
}

// templateRef renders a template reference in the contract's shape: v1beta2
// `templateRef {apiVersion, kind, name}`, legacy `ref {..., namespace}`.
func (in clusterClassInputs) templateRef(r *objectRef) map[string]any {
	ref := map[string]any{"apiVersion": r.APIVersion, "kind": r.Kind, "name": r.Name}
	if in.contractShape() {
		return map[string]any{"templateRef": ref}
	}
	ns := r.Namespace
	if ns == "" {
		ns = in.Namespace
	}
	ref["namespace"] = ns
	return map[string]any{"ref": ref}
}

func metadataBlock(labels, annotations map[string]string) map[string]any {
	meta := map[string]any{}
	if len(labels) > 0 {
		meta["labels"] = toAnyMap(labels)
	}
	if len(annotations) > 0 {
		meta["annotations"] = toAnyMap(annotations)
	}
	return meta
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// renderClusterClass builds the object.
func renderClusterClass(in clusterClassInputs) *unstructured.Unstructured {
	spec := map[string]any{"infrastructure": in.templateRef(in.Infrastructure)}
	cp := in.templateRef(in.ControlPlane)
	if in.ControlPlaneInfra != nil {
		cp["machineInfrastructure"] = in.templateRef(in.ControlPlaneInfra)
	}
	if meta := metadataBlock(in.ControlPlaneLabels, in.ControlPlaneAnnots); len(meta) > 0 {
		cp["metadata"] = meta
	}
	spec["controlPlane"] = cp
	workers := append([]workerClassInputs(nil), in.Workers...)
	sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })
	if len(workers) > 0 {
		classes := make([]any, 0, len(workers))
		for _, w := range workers {
			entry := map[string]any{"class": w.Name}
			meta := metadataBlock(w.Labels, w.Annotations)
			if in.contractShape() {
				entry["infrastructure"] = in.templateRef(w.Infrastructure)
				entry["bootstrap"] = in.templateRef(w.Bootstrap)
				if len(meta) > 0 {
					entry["metadata"] = meta
				}
			} else {
				tmpl := map[string]any{"infrastructure": in.templateRef(w.Infrastructure), "bootstrap": in.templateRef(w.Bootstrap)}
				if len(meta) > 0 {
					tmpl["metadata"] = meta
				}
				entry["template"] = tmpl
			}
			if w.FailureDomain != "" {
				entry["failureDomain"] = w.FailureDomain
			}
			classes = append(classes, entry)
		}
		spec["workers"] = map[string]any{"machineDeployments": classes}
	}
	if len(in.Variables) > 0 {
		spec["variables"] = in.Variables
	}
	if len(in.Patches) > 0 {
		spec["patches"] = in.Patches
	}
	obj := newObject(k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: in.CoreVersion, Kind: "ClusterClass"}, in.Namespace, in.Name, nil, nil)
	obj.Object["spec"] = spec
	return obj
}

// liveClusterClass is what Read compares the configuration against.
type liveClusterClass struct {
	Infrastructure    string
	ControlPlane      string
	ControlPlaneInfra string
	Workers           map[string]liveWorkerClass
	Variables         []any
	Patches           []any
}

type liveWorkerClass struct {
	Infrastructure, Bootstrap, FailureDomain string
}

func refName(m map[string]any, shape bool) string {
	key := "ref"
	if shape {
		key = "templateRef"
	}
	name, _, _ := unstructured.NestedString(m, key, "name")
	return name
}

// readLiveClusterClass reads the template names back from either shape.
func readLiveClusterClass(obj *unstructured.Unstructured) liveClusterClass {
	shape := nodeGroupInputs{CoreVersion: obj.GroupVersionKind().Version}.contractRefs()
	out := liveClusterClass{Workers: map[string]liveWorkerClass{}}
	if infra, ok, _ := unstructured.NestedMap(obj.Object, "spec", "infrastructure"); ok {
		out.Infrastructure = refName(infra, shape)
	}
	if cp, ok, _ := unstructured.NestedMap(obj.Object, "spec", "controlPlane"); ok {
		out.ControlPlane = refName(cp, shape)
		if mi, ok, _ := unstructured.NestedMap(cp, "machineInfrastructure"); ok {
			out.ControlPlaneInfra = refName(mi, shape)
		}
	}
	mds, _, _ := unstructured.NestedSlice(obj.Object, "spec", "workers", "machineDeployments")
	for _, md := range mds {
		m, ok := md.(map[string]any)
		if !ok {
			continue
		}
		class, _ := m["class"].(string)
		w := liveWorkerClass{}
		w.FailureDomain, _ = m["failureDomain"].(string)
		holder := m
		if !shape {
			holder, _, _ = unstructured.NestedMap(m, "template")
		}
		if infra, ok, _ := unstructured.NestedMap(holder, "infrastructure"); ok {
			w.Infrastructure = refName(infra, shape)
		}
		if bs, ok, _ := unstructured.NestedMap(holder, "bootstrap"); ok {
			w.Bootstrap = refName(bs, shape)
		}
		out.Workers[class] = w
	}
	out.Variables, _, _ = unstructured.NestedSlice(obj.Object, "spec", "variables")
	out.Patches, _, _ = unstructured.NestedSlice(obj.Object, "spec", "patches")
	return out
}

// --- model conversion ---

func dynamicToList(d types.Dynamic, where string) ([]any, diag.Diagnostics) {
	var diags diag.Diagnostics
	if d.IsNull() || d.IsUnknown() || d.IsUnderlyingValueNull() || d.IsUnderlyingValueUnknown() {
		return nil, diags
	}
	v, err := goFromAttr(d.UnderlyingValue())
	if err != nil {
		diags.AddError("Invalid "+where, err.Error())
		return nil, diags
	}
	list, ok := v.([]any)
	if !ok {
		diags.AddError("Invalid "+where, fmt.Sprintf("%s must be a list, got %T.", where, v))
		return nil, diags
	}
	return list, diags
}

func dynamicFromList(list []any) (types.Dynamic, diag.Diagnostics) {
	var diags diag.Diagnostics
	if list == nil {
		return types.DynamicNull(), diags
	}
	v, err := attrFromGo(list)
	if err != nil {
		diags.AddError("Converting list", err.Error())
		return types.DynamicNull(), diags
	}
	return types.DynamicValue(v), diags
}

func (r *ClusterClassResource) inputs(ctx context.Context, data *ClusterClassResourceModel, core string) (clusterClassInputs, diag.Diagnostics) {
	var diags diag.Diagnostics
	in := clusterClassInputs{Name: data.Name.ValueString(), Namespace: data.Namespace.ValueString(), CoreVersion: core}
	in.Infrastructure = refFromModel(ctx, data.Infrastructure, &diags)
	if !data.ControlPlane.IsNull() && !data.ControlPlane.IsUnknown() {
		var cp ControlPlaneClassModel
		diags.Append(data.ControlPlane.As(ctx, &cp, basetypes.ObjectAsOptions{})...)
		in.ControlPlane = refFromModel(ctx, cp.Template, &diags)
		in.ControlPlaneInfra = refFromModel(ctx, cp.Infrastructure, &diags)
		var d diag.Diagnostics
		in.ControlPlaneLabels, d = stringMap(ctx, cp.Labels)
		diags.Append(d...)
		in.ControlPlaneAnnots, d = stringMap(ctx, cp.Annotations)
		diags.Append(d...)
	}
	if !data.Workers.IsNull() && !data.Workers.IsUnknown() {
		workers := map[string]WorkerClassModel{}
		diags.Append(data.Workers.ElementsAs(ctx, &workers, false)...)
		for name, w := range workers {
			wc := workerClassInputs{Name: name, FailureDomain: w.FailureDomain.ValueString()}
			wc.Infrastructure = refFromModel(ctx, w.Infrastructure, &diags)
			wc.Bootstrap = refFromModel(ctx, w.Bootstrap, &diags)
			var d diag.Diagnostics
			wc.Labels, d = stringMap(ctx, w.Labels)
			diags.Append(d...)
			wc.Annotations, d = stringMap(ctx, w.Annotations)
			diags.Append(d...)
			in.Workers = append(in.Workers, wc)
		}
	}
	var d diag.Diagnostics
	in.Variables, d = dynamicToList(data.Variables, "variables")
	diags.Append(d...)
	in.Patches, d = dynamicToList(data.Patches, "patches")
	diags.Append(d...)
	return in, diags
}

// drift copies the live template names and the live variables and patches
// into the model.
func (r *ClusterClassResource) drift(ctx context.Context, data *ClusterClassResourceModel, live liveClusterClass) diag.Diagnostics {
	var diags diag.Diagnostics
	data.Infrastructure = refWithName(ctx, data.Infrastructure, live.Infrastructure, &diags)
	if !data.ControlPlane.IsNull() {
		var cp ControlPlaneClassModel
		diags.Append(data.ControlPlane.As(ctx, &cp, basetypes.ObjectAsOptions{})...)
		cp.Template = refWithName(ctx, cp.Template, live.ControlPlane, &diags)
		cp.Infrastructure = refWithName(ctx, cp.Infrastructure, live.ControlPlaneInfra, &diags)
		cv, d := types.ObjectValueFrom(ctx, controlPlaneClassAttrTypes(), cp)
		diags.Append(d...)
		data.ControlPlane = cv
	}
	if !data.Workers.IsNull() {
		workers := map[string]WorkerClassModel{}
		diags.Append(data.Workers.ElementsAs(ctx, &workers, false)...)
		for name, w := range workers {
			lw, ok := live.Workers[name]
			if !ok {
				delete(workers, name)
				continue
			}
			w.Infrastructure = refWithName(ctx, w.Infrastructure, lw.Infrastructure, &diags)
			w.Bootstrap = refWithName(ctx, w.Bootstrap, lw.Bootstrap, &diags)
			w.FailureDomain = keepString(w.FailureDomain, lw.FailureDomain)
			workers[name] = w
		}
		wv, d := types.MapValueFrom(ctx, types.ObjectType{AttrTypes: workerClassAttrTypes()}, workers)
		diags.Append(d...)
		data.Workers = wv
	}
	if !data.Variables.IsNull() || len(live.Variables) > 0 {
		v, d := dynamicFromList(live.Variables)
		diags.Append(d...)
		data.Variables = v
	}
	if !data.Patches.IsNull() || len(live.Patches) > 0 {
		v, d := dynamicFromList(live.Patches)
		diags.Append(d...)
		data.Patches = v
	}
	return diags
}

// fill writes the computed attributes.
func (r *ClusterClassResource) fill(ctx context.Context, data *ClusterClassResourceModel, obj *unstructured.Unstructured) diag.Diagnostics {
	var diags diag.Diagnostics
	data.ID = types.StringValue(obj.GetNamespace() + "/" + obj.GetName())
	ref, d := types.ObjectValueFrom(ctx, classRefAttrTypes(), ClassRefModel{Name: types.StringValue(obj.GetName()), Namespace: types.StringValue(obj.GetNamespace())})
	diags.Append(d...)
	data.Ref = ref
	live := readLiveClusterClass(obj)
	classes := make([]string, 0, len(live.Workers))
	for name := range live.Workers {
		classes = append(classes, name)
	}
	sort.Strings(classes)
	cl, d := types.ListValueFrom(ctx, types.StringType, classes)
	diags.Append(d...)
	data.Classes = cl
	conds := make([]ConditionModel, 0)
	for _, c := range readNodeGroupStatus(obj).Conditions {
		conds = append(conds, ConditionModel{Type: types.StringValue(c.Type), Status: types.StringValue(c.Status), Reason: stringOrNull(c.Reason), Message: stringOrNull(c.Message)})
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: conditionAttrTypes()}, conds)
	diags.Append(d...)
	data.Conditions = list
	return diags
}

// --- cluster access ---

func (r *ClusterClassResource) gvk(ctx context.Context, client *capi.ObjectClient) (k8sschema.GroupVersionKind, error) {
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return k8sschema.GroupVersionKind{}, err
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		return k8sschema.GroupVersionKind{}, err
	}
	return k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: "ClusterClass"}, nil
}

// --- CRUD ---

func (r *ClusterClassResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data ClusterClassResourceModel
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

func (r *ClusterClassResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data ClusterClassResourceModel
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

func (r *ClusterClassResource) apply(ctx context.Context, data *ClusterClassResourceModel, diags *diag.Diagnostics) {
	client, d := r.pd.objectClient(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	gvk, err := r.gvk(ctx, client)
	if err != nil {
		diags.AddError("Resolving ClusterClass", err.Error())
		return
	}
	in, d := r.inputs(ctx, data, gvk.Version)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	if err := in.validate(); err != nil {
		diags.AddError("Invalid cluster class", err.Error())
		return
	}
	defaults := clusterClassWaitDefaults
	if !in.contractShape() {
		defaults = clusterClassLegacyWaitDefaults
	}
	conditions, timeout, d := objectWaitFromModel(ctx, data.Wait, defaults)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	obj := renderClusterClass(in)
	data.Manifest = types.StringValue(manifestYAML(obj))
	live, d := applyAndWait(ctx, client, obj, false, conditions, timeout)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	diags.Append(r.fill(ctx, data, live)...)
}

func (r *ClusterClassResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ClusterClassResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	gvk, err := r.gvk(ctx, client)
	if errors.Is(err, capi.ErrKindNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Resolving ClusterClass", err.Error())
		return
	}
	obj, found, d := readLive(ctx, client, gvk, data.Namespace.ValueString(), data.Name.ValueString())
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(r.drift(ctx, &data, readLiveClusterClass(obj))...)
	resp.Diagnostics.Append(r.fill(ctx, &data, obj)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterClassResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data ClusterClassResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	_, timeout, d := objectWaitFromModel(ctx, data.Wait, clusterClassWaitDefaults)
	resp.Diagnostics.Append(d...)
	gvk, err := r.gvk(ctx, client)
	if errors.Is(err, capi.ErrKindNotFound) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Resolving ClusterClass", err.Error())
		return
	}
	// The webhook refuses to delete a class a Cluster uses; say which.
	clusterGVK := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: gvk.Version, Kind: "Cluster"}
	clusters, err := client.List(ctx, clusterGVK, data.Namespace.ValueString(), "")
	if err == nil {
		var users []string
		for i := range clusters {
			if name, _ := clusterClassRef(&clusters[i]); name == data.Name.ValueString() {
				users = append(users, clusters[i].GetName())
			}
		}
		if len(users) > 0 {
			resp.Diagnostics.AddError("ClusterClass in use", fmt.Sprintf("ClusterClass %s/%s is used by cluster %s. Delete or move those clusters first.", data.Namespace.ValueString(), data.Name.ValueString(), strings.Join(users, ", ")))
			return
		}
	}
	resp.Diagnostics.Append(deleteAndWait(ctx, client, gvk, data.Namespace.ValueString(), data.Name.ValueString(), timeout)...)
}

// ImportState takes `namespace/name`.
func (r *ClusterClassResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected namespace/name, got %q.", req.ID))
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	gvk, err := r.gvk(ctx, client)
	if err != nil {
		resp.Diagnostics.AddError("Resolving ClusterClass", err.Error())
		return
	}
	obj, err := client.Get(ctx, gvk, parts[0], parts[1])
	if err != nil {
		resp.Diagnostics.AddError("ClusterClass not found", err.Error())
		return
	}
	shape := nodeGroupInputs{CoreVersion: gvk.Version}.contractRefs()
	refOf := func(holder map[string]any) types.Object {
		key := "ref"
		if shape {
			key = "templateRef"
		}
		ref, ok, _ := unstructured.NestedStringMap(holder, key)
		if !ok || ref["name"] == "" {
			return types.ObjectNull(templateRefAttrTypes())
		}
		ns := ref["namespace"]
		if ns == "" {
			ns = parts[0]
		}
		v, d := types.ObjectValueFrom(ctx, templateRefAttrTypes(), TemplateRefModel{
			APIGroup: types.StringValue(gvkFor(ref["apiVersion"], "").Group), APIVersion: types.StringValue(ref["apiVersion"]), Kind: types.StringValue(ref["kind"]), Name: types.StringValue(ref["name"]), Namespace: types.StringValue(ns),
		})
		resp.Diagnostics.Append(d...)
		return v
	}
	data := ClusterClassResourceModel{
		Name: types.StringValue(parts[1]), Namespace: types.StringValue(parts[0]),
		Wait: types.ObjectNull(objectWaitAttrTypes()), Manifest: types.StringNull(), Variables: types.DynamicNull(), Patches: types.DynamicNull(),
	}
	infra, _, _ := unstructured.NestedMap(obj.Object, "spec", "infrastructure")
	data.Infrastructure = refOf(infra)
	cp, _, _ := unstructured.NestedMap(obj.Object, "spec", "controlPlane")
	cpInfra, _, _ := unstructured.NestedMap(cp, "machineInfrastructure")
	cpLabels, _, _ := unstructured.NestedStringMap(cp, "metadata", "labels")
	cpAnnots, _, _ := unstructured.NestedStringMap(cp, "metadata", "annotations")
	cpModel := ControlPlaneClassModel{Template: refOf(cp), Infrastructure: refOf(cpInfra), Labels: types.MapNull(types.StringType), Annotations: types.MapNull(types.StringType)}
	if len(cpLabels) > 0 {
		v, d := types.MapValueFrom(ctx, types.StringType, cpLabels)
		resp.Diagnostics.Append(d...)
		cpModel.Labels = v
	}
	if len(cpAnnots) > 0 {
		v, d := types.MapValueFrom(ctx, types.StringType, cpAnnots)
		resp.Diagnostics.Append(d...)
		cpModel.Annotations = v
	}
	cv, d := types.ObjectValueFrom(ctx, controlPlaneClassAttrTypes(), cpModel)
	resp.Diagnostics.Append(d...)
	data.ControlPlane = cv
	workers := map[string]WorkerClassModel{}
	mds, _, _ := unstructured.NestedSlice(obj.Object, "spec", "workers", "machineDeployments")
	for _, md := range mds {
		m, ok := md.(map[string]any)
		if !ok {
			continue
		}
		class, _ := m["class"].(string)
		holder := m
		if !shape {
			holder, _, _ = unstructured.NestedMap(m, "template")
		}
		wInfra, _, _ := unstructured.NestedMap(holder, "infrastructure")
		wBoot, _, _ := unstructured.NestedMap(holder, "bootstrap")
		fd, _ := m["failureDomain"].(string)
		w := WorkerClassModel{Infrastructure: refOf(wInfra), Bootstrap: refOf(wBoot), FailureDomain: stringOrNull(fd), Labels: types.MapNull(types.StringType), Annotations: types.MapNull(types.StringType)}
		if labels, _, _ := unstructured.NestedStringMap(holder, "metadata", "labels"); len(labels) > 0 {
			v, d := types.MapValueFrom(ctx, types.StringType, labels)
			resp.Diagnostics.Append(d...)
			w.Labels = v
		}
		if annots, _, _ := unstructured.NestedStringMap(holder, "metadata", "annotations"); len(annots) > 0 {
			v, d := types.MapValueFrom(ctx, types.StringType, annots)
			resp.Diagnostics.Append(d...)
			w.Annotations = v
		}
		workers[class] = w
	}
	data.Workers = types.MapNull(types.ObjectType{AttrTypes: workerClassAttrTypes()})
	if len(workers) > 0 {
		wv, d := types.MapValueFrom(ctx, types.ObjectType{AttrTypes: workerClassAttrTypes()}, workers)
		resp.Diagnostics.Append(d...)
		data.Workers = wv
	}
	live := readLiveClusterClass(obj)
	if len(live.Variables) > 0 {
		v, d := dynamicFromList(live.Variables)
		resp.Diagnostics.Append(d...)
		data.Variables = v
	}
	if len(live.Patches) > 0 {
		v, d := dynamicFromList(live.Patches)
		resp.Diagnostics.Append(d...)
		data.Patches = v
	}
	resp.Diagnostics.Append(r.fill(ctx, &data, obj)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
