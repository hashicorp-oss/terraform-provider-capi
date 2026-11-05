// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
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
	_ resource.Resource                   = &ClusterResourceSetResource{}
	_ resource.ResourceWithConfigure      = &ClusterResourceSetResource{}
	_ resource.ResourceWithImportState    = &ClusterResourceSetResource{}
	_ resource.ResourceWithValidateConfig = &ClusterResourceSetResource{}
)

const (
	// resourceSetLabel marks the ConfigMaps and Secrets a capi_cluster_resource_set owns.
	resourceSetLabel = "capi.terraform.io/cluster-resource-set"
	// resourceSetSecretType is the Secret type a ClusterResourceSet accepts.
	resourceSetSecretType = "addons.cluster.x-k8s.io/resource-set"
	// resourceSetManifestKey is the data key the manifests are stored under.
	resourceSetManifestKey = "manifest.yaml"
	// configMapLimit is the size a ConfigMap's data may reach.
	configMapLimit = 1024 * 1024
	// resourcesAppliedCondition is the ClusterResourceSet's condition.
	resourcesAppliedCondition = "ResourcesApplied"
)

// NewClusterResourceSetResource returns the capi_cluster_resource_set resource.
func NewClusterResourceSetResource() resource.Resource {
	return &ClusterResourceSetResource{}
}

// ClusterResourceSetResource is a ClusterResourceSet with the ConfigMaps
// and Secrets it references, plus what the controller does not do: pruning
// objects that leave the set from the matched clusters, and waiting for the
// applied objects to be ready.
type ClusterResourceSetResource struct {
	pd *providerData
}

// ClusterResourceSetResourceModel is the capi_cluster_resource_set model.
type ClusterResourceSetResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	Namespace       types.String `tfsdk:"namespace"`
	Cluster         types.String `tfsdk:"cluster"`
	ClusterSelector types.Map    `tfsdk:"cluster_selector"`
	Strategy        types.String `tfsdk:"strategy"`
	Manifests       types.List   `tfsdk:"manifests"`
	Secrets         types.List   `tfsdk:"secrets"`
	Prune           types.Bool   `tfsdk:"prune"`
	Wait            types.Object `tfsdk:"wait"`
	Manifest        types.String `tfsdk:"manifest"`
	Objects         types.List   `tfsdk:"objects"`
	Status          types.Object `tfsdk:"status"`
	Conditions      types.List   `tfsdk:"conditions"`
}

// ResourceSetWaitModel is the wait block.
type ResourceSetWaitModel struct {
	Applied types.Bool   `tfsdk:"applied"`
	Ready   types.Bool   `tfsdk:"ready"`
	Timeout types.String `tfsdk:"timeout"`
}

// AppliedObjectModel is one entry of objects.
type AppliedObjectModel struct {
	APIVersion types.String `tfsdk:"api_version"`
	Kind       types.String `tfsdk:"kind"`
	Namespace  types.String `tfsdk:"namespace"`
	Name       types.String `tfsdk:"name"`
}

// ResourceSetClusterStatusModel is one entry of status.clusters.
type ResourceSetClusterStatusModel struct {
	Name      types.String `tfsdk:"name"`
	Applied   types.Bool   `tfsdk:"applied"`
	Resources types.List   `tfsdk:"resources"`
}

// ResourceBindingModel is one resource of a cluster's binding.
type ResourceBindingModel struct {
	Kind    types.String `tfsdk:"kind"`
	Name    types.String `tfsdk:"name"`
	Applied types.Bool   `tfsdk:"applied"`
	Hash    types.String `tfsdk:"hash"`
}

func resourceSetWaitAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"applied": types.BoolType, "ready": types.BoolType, "timeout": types.StringType}
}

func appliedObjectAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"api_version": types.StringType, "kind": types.StringType, "namespace": types.StringType, "name": types.StringType}
}

func resourceBindingAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"kind": types.StringType, "name": types.StringType, "applied": types.BoolType, "hash": types.StringType}
}

func resourceSetClusterStatusAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name": types.StringType, "applied": types.BoolType,
		"resources": types.ListType{ElemType: types.ObjectType{AttrTypes: resourceBindingAttrTypes()}},
	}
}

func resourceSetStatusAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"clusters": types.ListType{ElemType: types.ObjectType{AttrTypes: resourceSetClusterStatusAttrTypes()}}}
}

func (r *ClusterResourceSetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster_resource_set"
}

func (r *ClusterResourceSetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manifests the ClusterResourceSet controller applies to every cluster `cluster_selector` matches, the way `aws_eks_addon` with a configuration does: a `ClusterResourceSet` on the management cluster the provider block names, with the ConfigMaps (`manifests`) and Secrets (`secrets`) it references created and kept in step by this resource. Two things the controller never does are done here: `prune` deletes objects that leave the set from each matched cluster, and `wait.ready` waits for the applied workloads, CRDs and anything with a `Ready` condition, since the `ClusterResourceSetBinding` only says applied. A manifest may not exceed a ConfigMap's 1 MiB; split it. Import with `namespace/name`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name": schema.StringAttribute{
				MarkdownDescription: "Object name; the ConfigMaps are `<name>-<index>` and the Secrets `<name>-secret-<index>`.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"namespace": schema.StringAttribute{
				MarkdownDescription: "Namespace, which the matched Clusters share. Default `default`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("default"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"cluster": schema.StringAttribute{
				MarkdownDescription: "Apply to this one cluster (selects by its `cluster.x-k8s.io/cluster-name` label). Exclusive with `cluster_selector`. The selector is immutable.",
				Optional:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"cluster_selector": schema.MapAttribute{
				MarkdownDescription: "Apply to every Cluster in the namespace carrying these labels. Exclusive with `cluster`. Immutable.",
				ElementType:         types.StringType,
				Optional:            true,
				PlanModifiers:       []planmodifier.Map{mapplanmodifier.RequiresReplace()},
			},
			"strategy": schema.StringAttribute{
				MarkdownDescription: "`ApplyOnce` (default: each resource is applied to a cluster once) or `Reconcile` (re-applied whenever its content changes). Immutable.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("ApplyOnce"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"manifests": schema.ListAttribute{
				MarkdownDescription: "Multi-document YAML manifests, one ConfigMap each.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"secrets": schema.ListAttribute{
				MarkdownDescription: "Multi-document YAML manifests holding sensitive content, one Secret each.",
				ElementType:         types.StringType,
				Optional:            true,
				Sensitive:           true,
			},
			"prune": schema.BoolAttribute{
				MarkdownDescription: "Delete objects that leave the set, and every object on destroy, from each matched cluster through its kubeconfig Secret. A cluster that is gone is skipped.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"wait": schema.SingleNestedAttribute{
				MarkdownDescription: "What to wait for after an apply.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"applied": schema.BoolAttribute{MarkdownDescription: "Wait until every matched cluster's binding reports every resource applied. Default true.", Optional: true},
					"ready":   schema.BoolAttribute{MarkdownDescription: "Then wait on each matched cluster for Deployments, StatefulSets, DaemonSets and Jobs to be available, CRDs to be established, and anything else with a `Ready` condition to be Ready. Default false.", Optional: true},
					"timeout": schema.StringAttribute{MarkdownDescription: "Go duration. Default `10m`.", Optional: true},
				},
			},
			"manifest": schema.StringAttribute{MarkdownDescription: "The applied ClusterResourceSet as YAML.", Computed: true},
			"objects": schema.ListNestedAttribute{
				MarkdownDescription: "The objects the manifests define, in order: what `prune` tracks.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"api_version": schema.StringAttribute{Computed: true},
						"kind":        schema.StringAttribute{Computed: true},
						"namespace":   schema.StringAttribute{Computed: true},
						"name":        schema.StringAttribute{Computed: true},
					},
				},
			},
			"status": schema.SingleNestedAttribute{
				MarkdownDescription: "What each matched cluster's binding reports.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"clusters": schema.ListNestedAttribute{
						Computed: true,
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"name":    schema.StringAttribute{Computed: true},
								"applied": schema.BoolAttribute{MarkdownDescription: "Every resource applied.", Computed: true},
								"resources": schema.ListNestedAttribute{
									Computed: true,
									NestedObject: schema.NestedAttributeObject{
										Attributes: map[string]schema.Attribute{
											"kind":    schema.StringAttribute{Computed: true},
											"name":    schema.StringAttribute{Computed: true},
											"applied": schema.BoolAttribute{Computed: true},
											"hash":    schema.StringAttribute{Computed: true},
										},
									},
								},
							},
						},
					},
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

func (r *ClusterResourceSetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ValidateConfig checks the selector form and the manifests when known.
func (r *ClusterResourceSetResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data ClusterResourceSetResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !data.Cluster.IsNull() && !data.ClusterSelector.IsNull() {
		resp.Diagnostics.AddError("Invalid cluster resource set", "cluster and cluster_selector are exclusive.")
	}
	if data.Cluster.IsNull() && data.ClusterSelector.IsNull() {
		resp.Diagnostics.AddError("Invalid cluster resource set", "One of cluster or cluster_selector is required.")
	}
	for _, list := range []struct {
		name string
		list types.List
	}{{"manifests", data.Manifests}, {"secrets", data.Secrets}} {
		if list.list.IsNull() || list.list.IsUnknown() {
			continue
		}
		var docs []types.String
		resp.Diagnostics.Append(list.list.ElementsAs(ctx, &docs, false)...)
		for i, doc := range docs {
			if doc.IsUnknown() {
				continue
			}
			if err := validateManifest(doc.ValueString()); err != nil {
				resp.Diagnostics.AddError("Invalid cluster resource set", fmt.Sprintf("%s[%d]: %s", list.name, i, err))
			}
		}
	}
}

// validateManifest checks a manifest's size and syntax.
func validateManifest(doc string) error {
	if len(doc) > configMapLimit {
		return fmt.Errorf("is %d bytes, above the 1 MiB a ConfigMap holds; split it", len(doc))
	}
	if _, err := capi.DecodeDocuments([]byte(doc)); err != nil {
		return err
	}
	return nil
}

// --- rendering ---

// appliedObject identifies one object a manifest defines.
type appliedObject struct {
	APIVersion, Kind, Namespace, Name string
}

func (o appliedObject) String() string {
	if o.Namespace != "" {
		return fmt.Sprintf("%s %s/%s", o.Kind, o.Namespace, o.Name)
	}
	return fmt.Sprintf("%s %s", o.Kind, o.Name)
}

// resourceSetInputs is everything the set renders from.
type resourceSetInputs struct {
	Name, Namespace string
	APIVersion      string
	ClusterSelector map[string]string
	Strategy        string
	Manifests       []string
	Secrets         []string
}

func (in resourceSetInputs) configMapName(i int) string { return fmt.Sprintf("%s-%d", in.Name, i) }
func (in resourceSetInputs) secretName(i int) string    { return fmt.Sprintf("%s-secret-%d", in.Name, i) }

// objects lists every object the manifests and secrets define, in order.
func (in resourceSetInputs) objects() ([]appliedObject, error) {
	var out []appliedObject
	for _, doc := range append(append([]string{}, in.Manifests...), in.Secrets...) {
		objs, err := capi.DecodeDocuments([]byte(doc))
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			out = append(out, appliedObject{APIVersion: o.GetAPIVersion(), Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName()})
		}
	}
	return out, nil
}

// render builds the ClusterResourceSet and the ConfigMaps and Secrets it
// references.
func (in resourceSetInputs) render() (crs *unstructured.Unstructured, holders []*unstructured.Unstructured) {
	labels := map[string]string{resourceSetLabel: in.Name}
	resources := make([]any, 0, len(in.Manifests)+len(in.Secrets))
	for i, doc := range in.Manifests {
		cm := newObject(k8sschema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, in.Namespace, in.configMapName(i), labels, nil)
		cm.Object["data"] = map[string]any{resourceSetManifestKey: doc}
		holders = append(holders, cm)
		resources = append(resources, map[string]any{"kind": "ConfigMap", "name": cm.GetName()})
	}
	for i, doc := range in.Secrets {
		secret := newObject(capi.SecretGVK, in.Namespace, in.secretName(i), labels, nil)
		secret.Object["type"] = resourceSetSecretType
		secret.Object["stringData"] = map[string]any{resourceSetManifestKey: doc}
		holders = append(holders, secret)
		resources = append(resources, map[string]any{"kind": "Secret", "name": secret.GetName()})
	}
	matchLabels := map[string]any{}
	for k, v := range in.ClusterSelector {
		matchLabels[k] = v
	}
	strategy := in.Strategy
	if strategy == "" {
		strategy = "ApplyOnce"
	}
	crs = newObject(gvkFor(in.APIVersion, "ClusterResourceSet"), in.Namespace, in.Name, nil, nil)
	crs.Object["spec"] = map[string]any{
		"clusterSelector": map[string]any{"matchLabels": matchLabels},
		"strategy":        strategy,
		"resources":       resources,
	}
	return crs, holders
}

// resourceRefs reads a ClusterResourceSet's resource references.
func resourceRefs(crs *unstructured.Unstructured) []appliedObject {
	raw, _, _ := unstructured.NestedSlice(crs.Object, "spec", "resources")
	var out []appliedObject
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			kind, _ := m["kind"].(string)
			name, _ := m["name"].(string)
			out = append(out, appliedObject{Kind: kind, Name: name, Namespace: crs.GetNamespace()})
		}
	}
	return out
}

// resourceBinding is one resource of a cluster's binding to the set.
type resourceBinding struct {
	Kind, Name, Hash string
	Applied          bool
}

// readBinding returns the set's resources from a ClusterResourceSetBinding,
// and whether the binding names the set at all.
func readBinding(binding *unstructured.Unstructured, setName string) ([]resourceBinding, bool) {
	raw, _, _ := unstructured.NestedSlice(binding.Object, "spec", "bindings")
	for _, b := range raw {
		m, ok := b.(map[string]any)
		if !ok || m["clusterResourceSetName"] != setName {
			continue
		}
		resources, _, _ := unstructured.NestedSlice(m, "resources")
		var out []resourceBinding
		for _, r := range resources {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			rb := resourceBinding{}
			rb.Kind, _ = rm["kind"].(string)
			rb.Name, _ = rm["name"].(string)
			rb.Hash, _ = rm["hash"].(string)
			rb.Applied, _ = rm["applied"].(bool)
			out = append(out, rb)
		}
		return out, true
	}
	return nil, false
}

// bindingSatisfied reports whether a binding has applied every expected
// resource, and names the first that is not.
func bindingSatisfied(bound []resourceBinding, expected []appliedObject) (bool, string) {
	applied := map[string]bool{}
	for _, b := range bound {
		applied[b.Kind+"/"+b.Name] = b.Applied
	}
	for _, e := range expected {
		ok, present := applied[e.Kind+"/"+e.Name]
		if !present {
			return false, e.Kind + " " + e.Name + " not bound yet"
		}
		if !ok {
			return false, e.Kind + " " + e.Name + " not applied"
		}
	}
	return true, ""
}

// objectReady decides whether an applied object is ready, by kind.
func objectReady(obj *unstructured.Unstructured) (bool, string) {
	conds := readNodeGroupStatus(obj).Conditions
	condition := func(typ string) (string, string) {
		for _, c := range conds {
			if c.Type == typ {
				return c.Status, orDefault(c.Message, c.Reason)
			}
		}
		return "", ""
	}
	switch obj.GroupVersionKind().GroupKind() {
	case k8sschema.GroupKind{Group: "apps", Kind: "Deployment"}, k8sschema.GroupKind{Group: "apps", Kind: "StatefulSet"}:
		if observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration"); observed < obj.GetGeneration() {
			return false, "not observed yet"
		}
		want, ok, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		if !ok {
			want = 1
		}
		available, _, _ := unstructured.NestedInt64(obj.Object, "status", "availableReplicas")
		if available < want {
			return false, fmt.Sprintf("%d of %d replicas available", available, want)
		}
		return true, ""
	case k8sschema.GroupKind{Group: "apps", Kind: "DaemonSet"}:
		desired, _, _ := unstructured.NestedInt64(obj.Object, "status", "desiredNumberScheduled")
		ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "numberReady")
		if ready < desired {
			return false, fmt.Sprintf("%d of %d pods ready", ready, desired)
		}
		return true, ""
	case k8sschema.GroupKind{Group: "batch", Kind: "Job"}:
		if status, _ := condition("Complete"); status == "True" {
			return true, ""
		}
		if status, msg := condition("Failed"); status == "True" {
			return false, "failed: " + msg
		}
		return false, "not complete"
	case k8sschema.GroupKind{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}:
		if status, msg := condition("Established"); status != "True" {
			return false, "not established: " + msg
		}
		return true, ""
	}
	if status, msg := condition("Ready"); status != "" && status != "True" {
		return false, "Ready=" + status + ": " + msg
	}
	return true, ""
}

// --- model conversion ---

func (r *ClusterResourceSetResource) inputs(ctx context.Context, data *ClusterResourceSetResourceModel, apiVersion string) (resourceSetInputs, diag.Diagnostics) {
	var diags diag.Diagnostics
	in := resourceSetInputs{Name: data.Name.ValueString(), Namespace: data.Namespace.ValueString(), APIVersion: apiVersion, Strategy: data.Strategy.ValueString()}
	selector, d := stringMap(ctx, data.ClusterSelector)
	diags.Append(d...)
	in.ClusterSelector = addonClusterSelector(data.Cluster.ValueString(), selector)
	in.Manifests, d = stringList(ctx, data.Manifests)
	diags.Append(d...)
	in.Secrets, d = stringList(ctx, data.Secrets)
	diags.Append(d...)
	return in, diags
}

// waitSettings reads the wait block.
func (r *ClusterResourceSetResource) waitSettings(ctx context.Context, data *ClusterResourceSetResourceModel, diags *diag.Diagnostics) (applied, ready bool, timeout time.Duration) {
	applied, ready, timeout = true, false, 10*time.Minute
	if data.Wait.IsNull() || data.Wait.IsUnknown() {
		return applied, ready, timeout
	}
	var w ResourceSetWaitModel
	diags.Append(data.Wait.As(ctx, &w, basetypes.ObjectAsOptions{})...)
	if !w.Applied.IsNull() {
		applied = w.Applied.ValueBool()
	}
	if !w.Ready.IsNull() {
		ready = w.Ready.ValueBool()
	}
	if d := durationOrNil(w.Timeout, "wait.timeout", diags); d != nil {
		timeout = *d
	}
	return applied, ready, timeout
}

func objectsValue(ctx context.Context, objs []appliedObject, diags *diag.Diagnostics) types.List {
	models := make([]AppliedObjectModel, 0, len(objs))
	for _, o := range objs {
		models = append(models, AppliedObjectModel{APIVersion: types.StringValue(o.APIVersion), Kind: types.StringValue(o.Kind), Namespace: stringOrNull(o.Namespace), Name: types.StringValue(o.Name)})
	}
	v, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: appliedObjectAttrTypes()}, models)
	diags.Append(d...)
	return v
}

func objectsFromState(ctx context.Context, list types.List, diags *diag.Diagnostics) []appliedObject {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var models []AppliedObjectModel
	diags.Append(list.ElementsAs(ctx, &models, false)...)
	out := make([]appliedObject, 0, len(models))
	for _, m := range models {
		out = append(out, appliedObject{APIVersion: m.APIVersion.ValueString(), Kind: m.Kind.ValueString(), Namespace: m.Namespace.ValueString(), Name: m.Name.ValueString()})
	}
	return out
}

// --- cluster access ---

type resourceSetKinds struct {
	Set, Binding, Cluster k8sschema.GroupVersionKind
}

func (r *ClusterResourceSetResource) kinds(ctx context.Context, client *capi.ObjectClient) (resourceSetKinds, error) {
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return resourceSetKinds{}, err
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		return resourceSetKinds{}, err
	}
	set, err := catalog.ByKind("ClusterResourceSet")
	if err != nil {
		return resourceSetKinds{}, fmt.Errorf("ClusterResourceSet is not served on the management cluster (the ClusterResourceSet feature gate is off, or the core provider is missing): %w", err)
	}
	binding, err := catalog.ByKind("ClusterResourceSetBinding")
	if err != nil {
		return resourceSetKinds{}, err
	}
	return resourceSetKinds{Set: set, Binding: binding, Cluster: k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: "Cluster"}}, nil
}

// matchedClusters lists the clusters the selector matches, sorted.
func (r *ClusterResourceSetResource) matchedClusters(ctx context.Context, client *capi.ObjectClient, kinds resourceSetKinds, namespace string, selector map[string]string) ([]string, error) {
	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+selector[k])
	}
	list, err := client.List(ctx, kinds.Cluster, namespace, strings.Join(parts, ","))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for i := range list {
		if list[i].GetDeletionTimestamp() == nil {
			names = append(names, list[i].GetName())
		}
	}
	sort.Strings(names)
	return names, nil
}

// clusterStatus is what a cluster's binding reports for the set.
type clusterStatus struct {
	Name      string
	Bound     bool
	Resources []resourceBinding
}

func (r *ClusterResourceSetResource) bindings(ctx context.Context, client *capi.ObjectClient, kinds resourceSetKinds, namespace, name string, clusters []string) ([]clusterStatus, error) {
	out := make([]clusterStatus, 0, len(clusters))
	for _, cluster := range clusters {
		cs := clusterStatus{Name: cluster}
		binding, err := client.Get(ctx, kinds.Binding, namespace, cluster)
		if err != nil && !errors.Is(err, capi.ErrObjectNotFound) {
			return nil, err
		}
		if binding != nil {
			cs.Resources, cs.Bound = readBinding(binding, name)
		}
		out = append(out, cs)
	}
	return out, nil
}

// waitApplied polls until the set's condition is True at its generation
// and every matched cluster's binding has applied every resource.
func (r *ClusterResourceSetResource) waitApplied(ctx context.Context, client *capi.ObjectClient, kinds resourceSetKinds, in resourceSetInputs, expected []appliedObject, deadline time.Time) error {
	for {
		crs, err := client.Get(ctx, kinds.Set, in.Namespace, in.Name)
		if err != nil {
			return err
		}
		why := ""
		observed, _, _ := unstructured.NestedInt64(crs.Object, "status", "observedGeneration")
		if observed < crs.GetGeneration() {
			why = "the controller has not reconciled the current generation yet"
		} else if status, _ := conditionOf(crs, resourcesAppliedCondition); status != "True" {
			why = capi.ConditionSummary(crs, []string{resourcesAppliedCondition})
		} else {
			clusters, err := r.matchedClusters(ctx, client, kinds, in.Namespace, in.ClusterSelector)
			if err != nil {
				return err
			}
			statuses, err := r.bindings(ctx, client, kinds, in.Namespace, in.Name, clusters)
			if err != nil {
				return err
			}
			for _, cs := range statuses {
				if !cs.Bound {
					why = cs.Name + ": no binding yet"
					break
				}
				if ok, what := bindingSatisfied(cs.Resources, expected); !ok {
					why = cs.Name + ": " + what
					break
				}
			}
		}
		if why == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ClusterResourceSet %s/%s not applied in time: %s", in.Namespace, in.Name, why)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func conditionOf(obj *unstructured.Unstructured, typ string) (status, message string) {
	for _, c := range readNodeGroupStatus(obj).Conditions {
		if c.Type == typ {
			return c.Status, c.Message
		}
	}
	return "", ""
}

// waitReady polls each matched cluster until every applied object is ready.
func (r *ClusterResourceSetResource) waitReady(ctx context.Context, client *capi.ObjectClient, kinds resourceSetKinds, in resourceSetInputs, objects []appliedObject, deadline time.Time) error {
	clusters, err := r.matchedClusters(ctx, client, kinds, in.Namespace, in.ClusterSelector)
	if err != nil {
		return err
	}
	for _, cluster := range clusters {
		workload, cleanup, err := client.WorkloadClient(ctx, in.Namespace, cluster, capi.DefaultFieldManager)
		if err != nil {
			return err
		}
		err = r.waitReadyOn(ctx, workload, cluster, objects, deadline)
		cleanup()
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ClusterResourceSetResource) waitReadyOn(ctx context.Context, workload *capi.ObjectClient, cluster string, objects []appliedObject, deadline time.Time) error {
	for {
		why := ""
		for _, o := range objects {
			live, err := workload.Get(ctx, gvkFor(o.APIVersion, o.Kind), o.Namespace, o.Name)
			if errors.Is(err, capi.ErrObjectNotFound) {
				why = o.String() + " not found"
				break
			}
			if err != nil {
				return fmt.Errorf("on %s: %w", cluster, err)
			}
			if ok, what := objectReady(live); !ok {
				why = o.String() + ": " + what
				break
			}
		}
		if why == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("on %s, not ready in time: %s", cluster, why)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// prune deletes objects from every matched cluster, last defined first. A
// cluster without a kubeconfig (gone, or never initialized) is skipped with
// a warning.
func (r *ClusterResourceSetResource) prune(ctx context.Context, client *capi.ObjectClient, kinds resourceSetKinds, namespace string, selector map[string]string, objects []appliedObject, diags *diag.Diagnostics) {
	if len(objects) == 0 {
		return
	}
	clusters, err := r.matchedClusters(ctx, client, kinds, namespace, selector)
	if err != nil {
		diags.AddError("Listing matched clusters", err.Error())
		return
	}
	for _, cluster := range clusters {
		workload, cleanup, err := client.WorkloadClient(ctx, namespace, cluster, capi.DefaultFieldManager)
		if errors.Is(err, capi.ErrNoKubeconfig) {
			diags.AddWarning("Prune skipped", fmt.Sprintf("Cluster %s has no kubeconfig Secret; its objects were not deleted.", cluster))
			continue
		}
		if err != nil {
			diags.AddError("Connecting to "+cluster, err.Error())
			return
		}
		for i := len(objects) - 1; i >= 0; i-- {
			o := objects[i]
			if err := workload.Delete(ctx, gvkFor(o.APIVersion, o.Kind), o.Namespace, o.Name); err != nil {
				diags.AddError("Pruning "+o.String()+" on "+cluster, err.Error())
			}
		}
		cleanup()
		if diags.HasError() {
			return
		}
	}
}

// --- CRUD ---

func (r *ClusterResourceSetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data ClusterResourceSetResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &data, nil, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterResourceSetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data, prior ClusterResourceSetResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &data, &prior, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// apply renders and applies the holders and the set, removes holders and
// (with prune) objects that left the set, and waits.
func (r *ClusterResourceSetResource) apply(ctx context.Context, data, prior *ClusterResourceSetResourceModel, diags *diag.Diagnostics) {
	client, d := r.pd.objectClient(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	kinds, err := r.kinds(ctx, client)
	if err != nil {
		diags.AddError("Resolving ClusterResourceSet", err.Error())
		return
	}
	in, d := r.inputs(ctx, data, kinds.Set.GroupVersion().String())
	diags.Append(d...)
	waitApplied, waitReady, timeout := r.waitSettings(ctx, data, diags)
	if diags.HasError() {
		return
	}
	if len(in.ClusterSelector) == 0 {
		diags.AddError("Invalid cluster resource set", "One of cluster or cluster_selector is required.")
		return
	}
	objects, err := in.objects()
	if err != nil {
		diags.AddError("Invalid manifests", err.Error())
		return
	}
	crs, holders := in.render()
	for _, h := range holders {
		if _, err := client.Apply(ctx, h, false); err != nil {
			diags.AddError("Applying "+h.GetKind()+" "+h.GetName(), err.Error())
			return
		}
	}
	if _, err := client.Apply(ctx, crs, false); err != nil {
		diags.AddError("Applying ClusterResourceSet", err.Error())
		return
	}
	// Holders the set no longer references.
	keep := map[string]bool{}
	for _, h := range holders {
		keep[h.GetKind()+"/"+h.GetName()] = true
	}
	for _, gvk := range []k8sschema.GroupVersionKind{{Version: "v1", Kind: "ConfigMap"}, capi.SecretGVK} {
		stale, err := client.List(ctx, gvk, in.Namespace, resourceSetLabel+"="+in.Name)
		if err != nil {
			diags.AddError("Listing "+gvk.Kind+"s", err.Error())
			return
		}
		for i := range stale {
			if !keep[gvk.Kind+"/"+stale[i].GetName()] {
				if err := client.Delete(ctx, gvk, in.Namespace, stale[i].GetName()); err != nil {
					diags.AddError("Deleting "+gvk.Kind+" "+stale[i].GetName(), err.Error())
					return
				}
			}
		}
	}
	if prior != nil && prior.Prune.ValueBool() {
		now := map[string]bool{}
		for _, o := range objects {
			now[o.String()] = true
		}
		var removed []appliedObject
		for _, o := range objectsFromState(ctx, prior.Objects, diags) {
			if !now[o.String()] {
				removed = append(removed, o)
			}
		}
		r.prune(ctx, client, kinds, in.Namespace, in.ClusterSelector, removed, diags)
		if diags.HasError() {
			return
		}
	}
	deadline := time.Now().Add(timeout)
	if waitApplied {
		if err := r.waitApplied(ctx, client, kinds, in, resourceRefs(crs), deadline); err != nil {
			diags.AddError("Waiting for ClusterResourceSet", err.Error())
			return
		}
	}
	if waitReady {
		if err := r.waitReady(ctx, client, kinds, in, objects, deadline); err != nil {
			diags.AddError("Waiting for applied objects", err.Error())
			return
		}
	}
	data.ID = types.StringValue(fmt.Sprintf("%s/%s", in.Namespace, in.Name))
	data.Manifest = types.StringValue(manifestYAML(crs))
	data.Objects = objectsValue(ctx, objects, diags)
	diags.Append(r.fill(ctx, client, kinds, data, in.ClusterSelector)...)
}

// fill writes status and conditions from the set and the bindings.
func (r *ClusterResourceSetResource) fill(ctx context.Context, client *capi.ObjectClient, kinds resourceSetKinds, data *ClusterResourceSetResourceModel, selector map[string]string) diag.Diagnostics {
	var diags diag.Diagnostics
	namespace, name := data.Namespace.ValueString(), data.Name.ValueString()
	crs, err := client.Get(ctx, kinds.Set, namespace, name)
	if err != nil {
		diags.AddError("Reading ClusterResourceSet", err.Error())
		return diags
	}
	clusters, err := r.matchedClusters(ctx, client, kinds, namespace, selector)
	if err != nil {
		diags.AddError("Listing matched clusters", err.Error())
		return diags
	}
	statuses, err := r.bindings(ctx, client, kinds, namespace, name, clusters)
	if err != nil {
		diags.AddError("Reading bindings", err.Error())
		return diags
	}
	models := make([]ResourceSetClusterStatusModel, 0, len(statuses))
	for _, cs := range statuses {
		all := cs.Bound && len(cs.Resources) > 0
		res := make([]ResourceBindingModel, 0, len(cs.Resources))
		for _, rb := range cs.Resources {
			all = all && rb.Applied
			res = append(res, ResourceBindingModel{Kind: types.StringValue(rb.Kind), Name: types.StringValue(rb.Name), Applied: types.BoolValue(rb.Applied), Hash: stringOrNull(rb.Hash)})
		}
		resList, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: resourceBindingAttrTypes()}, res)
		diags.Append(d...)
		models = append(models, ResourceSetClusterStatusModel{Name: types.StringValue(cs.Name), Applied: types.BoolValue(all), Resources: resList})
	}
	clusterList, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: resourceSetClusterStatusAttrTypes()}, models)
	diags.Append(d...)
	st, d := types.ObjectValueFrom(ctx, resourceSetStatusAttrTypes(), map[string]attr.Value{"clusters": clusterList})
	diags.Append(d...)
	data.Status = st
	conds := make([]ConditionModel, 0)
	for _, c := range readNodeGroupStatus(crs).Conditions {
		conds = append(conds, ConditionModel{Type: types.StringValue(c.Type), Status: types.StringValue(c.Status), Reason: stringOrNull(c.Reason), Message: stringOrNull(c.Message)})
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: conditionAttrTypes()}, conds)
	diags.Append(d...)
	data.Conditions = list
	return diags
}

// readHolders reads the manifests back from the ConfigMaps and Secrets the
// set references, in the set's order.
func (r *ClusterResourceSetResource) readHolders(ctx context.Context, client *capi.ObjectClient, crs *unstructured.Unstructured) (manifests, secrets []string, err error) {
	for _, ref := range resourceRefs(crs) {
		switch ref.Kind {
		case "ConfigMap":
			cm, err := client.Get(ctx, k8sschema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, crs.GetNamespace(), ref.Name)
			if err != nil {
				return nil, nil, err
			}
			data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
			manifests = append(manifests, data[resourceSetManifestKey])
		case "Secret":
			secret, err := client.Get(ctx, capi.SecretGVK, crs.GetNamespace(), ref.Name)
			if err != nil {
				return nil, nil, err
			}
			data, _, _ := unstructured.NestedStringMap(secret.Object, "data")
			raw, _ := base64.StdEncoding.DecodeString(data[resourceSetManifestKey])
			secrets = append(secrets, string(raw))
		}
	}
	return manifests, secrets, nil
}

func (r *ClusterResourceSetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ClusterResourceSetResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	kinds, err := r.kinds(ctx, client)
	if errors.Is(err, capi.ErrKindNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Resolving ClusterResourceSet", err.Error())
		return
	}
	crs, found, d := readLive(ctx, client, kinds.Set, data.Namespace.ValueString(), data.Name.ValueString())
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	selector, _, _ := unstructured.NestedStringMap(crs.Object, "spec", "clusterSelector", "matchLabels")
	setSelectorFromLive(ctx, &data.Cluster, &data.ClusterSelector, selector, &resp.Diagnostics)
	data.Strategy = types.StringValue(orDefault(nestedString(crs, "spec", "strategy"), "ApplyOnce"))
	manifests, secrets, err := r.readHolders(ctx, client, crs)
	if err != nil && !errors.Is(err, capi.ErrObjectNotFound) {
		resp.Diagnostics.AddError("Reading manifests", err.Error())
		return
	}
	if err == nil {
		data.Manifests = types.ListNull(types.StringType)
		if len(manifests) > 0 {
			v, d := types.ListValueFrom(ctx, types.StringType, manifests)
			resp.Diagnostics.Append(d...)
			data.Manifests = v
		}
		data.Secrets = types.ListNull(types.StringType)
		if len(secrets) > 0 {
			v, d := types.ListValueFrom(ctx, types.StringType, secrets)
			resp.Diagnostics.Append(d...)
			data.Secrets = v
		}
	}
	resp.Diagnostics.Append(r.fill(ctx, client, kinds, &data, selector)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func nestedString(obj *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj.Object, fields...)
	return s
}

func (r *ClusterResourceSetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data ClusterResourceSetResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	_, _, timeout := r.waitSettings(ctx, &data, &resp.Diagnostics)
	kinds, err := r.kinds(ctx, client)
	if errors.Is(err, capi.ErrKindNotFound) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Resolving ClusterResourceSet", err.Error())
		return
	}
	namespace, name := data.Namespace.ValueString(), data.Name.ValueString()
	if data.Prune.ValueBool() {
		selector, d := stringMap(ctx, data.ClusterSelector)
		resp.Diagnostics.Append(d...)
		r.prune(ctx, client, kinds, namespace, addonClusterSelector(data.Cluster.ValueString(), selector), objectsFromState(ctx, data.Objects, &resp.Diagnostics), &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	resp.Diagnostics.Append(deleteAndWait(ctx, client, kinds.Set, namespace, name, timeout)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, gvk := range []k8sschema.GroupVersionKind{{Version: "v1", Kind: "ConfigMap"}, capi.SecretGVK} {
		holders, err := client.List(ctx, gvk, namespace, resourceSetLabel+"="+name)
		if err != nil {
			resp.Diagnostics.AddError("Listing "+gvk.Kind+"s", err.Error())
			return
		}
		for i := range holders {
			if err := client.Delete(ctx, gvk, namespace, holders[i].GetName()); err != nil {
				resp.Diagnostics.AddError("Deleting "+gvk.Kind+" "+holders[i].GetName(), err.Error())
			}
		}
	}
}

// ImportState takes `namespace/name`.
func (r *ClusterResourceSetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
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
	kinds, err := r.kinds(ctx, client)
	if err != nil {
		resp.Diagnostics.AddError("Resolving ClusterResourceSet", err.Error())
		return
	}
	crs, err := client.Get(ctx, kinds.Set, parts[0], parts[1])
	if err != nil {
		resp.Diagnostics.AddError("ClusterResourceSet not found", err.Error())
		return
	}
	selector, _, _ := unstructured.NestedStringMap(crs.Object, "spec", "clusterSelector", "matchLabels")
	data := ClusterResourceSetResourceModel{
		ID: types.StringValue(req.ID), Name: types.StringValue(parts[1]), Namespace: types.StringValue(parts[0]),
		Cluster: types.StringNull(), ClusterSelector: types.MapNull(types.StringType),
		Strategy:  types.StringValue(orDefault(nestedString(crs, "spec", "strategy"), "ApplyOnce")),
		Manifests: types.ListNull(types.StringType), Secrets: types.ListNull(types.StringType),
		Prune: types.BoolValue(false), Wait: types.ObjectNull(resourceSetWaitAttrTypes()), Manifest: types.StringNull(),
	}
	if len(selector) == 1 && selector[clusterNameLabel] != "" {
		data.Cluster = types.StringValue(selector[clusterNameLabel])
	}
	setSelectorFromLive(ctx, &data.Cluster, &data.ClusterSelector, selector, &resp.Diagnostics)
	manifests, secrets, err := r.readHolders(ctx, client, crs)
	if err != nil {
		resp.Diagnostics.AddError("Reading manifests", err.Error())
		return
	}
	in := resourceSetInputs{Manifests: manifests, Secrets: secrets}
	objects, err := in.objects()
	if err != nil {
		resp.Diagnostics.AddError("Reading manifests", err.Error())
		return
	}
	if len(manifests) > 0 {
		v, d := types.ListValueFrom(ctx, types.StringType, manifests)
		resp.Diagnostics.Append(d...)
		data.Manifests = v
	}
	if len(secrets) > 0 {
		v, d := types.ListValueFrom(ctx, types.StringType, secrets)
		resp.Diagnostics.Append(d...)
		data.Secrets = v
	}
	data.Objects = objectsValue(ctx, objects, &resp.Diagnostics)
	resp.Diagnostics.Append(r.fill(ctx, client, kinds, &data, selector)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
