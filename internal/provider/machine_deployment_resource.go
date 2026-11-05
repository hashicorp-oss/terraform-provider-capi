// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
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
	_ resource.Resource                = &MachineDeploymentResource{}
	_ resource.ResourceWithConfigure   = &MachineDeploymentResource{}
	_ resource.ResourceWithImportState = &MachineDeploymentResource{}
)

// NewMachineDeploymentResource returns the capi_machine_deployment resource.
func NewMachineDeploymentResource() resource.Resource {
	return &MachineDeploymentResource{}
}

// MachineDeploymentResource is a cluster's node group: a MachineDeployment
// in direct mode, or one entry of the Cluster's topology workers when the
// cluster uses a ClusterClass. It takes the cluster's own values (API
// version, Kubernetes version, failure domains, worker classes) from the
// live Cluster, so the configuration states only what the group is.
type MachineDeploymentResource struct {
	pd *providerData
}

// MachineDeploymentResourceModel is the capi_machine_deployment model.
type MachineDeploymentResourceModel struct {
	ID            types.String `tfsdk:"id"`
	Cluster       types.String `tfsdk:"cluster"`
	Namespace     types.String `tfsdk:"namespace"`
	Name          types.String `tfsdk:"name"`
	Replicas      types.Int64  `tfsdk:"replicas"`
	Autoscaling   types.Object `tfsdk:"autoscaling"`
	Version       types.String `tfsdk:"version"`
	Template      types.Object `tfsdk:"template"`
	Class         types.Object `tfsdk:"class"`
	FailureDomain types.String `tfsdk:"failure_domain"`
	Labels        types.Map    `tfsdk:"labels"`
	Annotations   types.Map    `tfsdk:"annotations"`
	Rollout       types.Object `tfsdk:"rollout"`
	Timeouts      types.Object `tfsdk:"timeouts"`
	Wait          types.Object `tfsdk:"wait"`
	ScaleToZero   types.Bool   `tfsdk:"scale_to_zero_on_delete"`
	ObjectName    types.String `tfsdk:"object_name"`
	Manifest      types.String `tfsdk:"manifest"`
	Status        types.Object `tfsdk:"status"`
	Conditions    types.List   `tfsdk:"conditions"`
}

// AutoscalingModel is the autoscaling block.
type AutoscalingModel struct {
	Min types.Int64 `tfsdk:"min"`
	Max types.Int64 `tfsdk:"max"`
}

// NodeGroupTemplateModel is the template block of direct mode.
type NodeGroupTemplateModel struct {
	Infrastructure types.Object `tfsdk:"infrastructure"`
	Bootstrap      types.Object `tfsdk:"bootstrap"`
}

// TemplateRefModel is a reference exported by the template resources.
type TemplateRefModel struct {
	APIGroup   types.String `tfsdk:"api_group"`
	APIVersion types.String `tfsdk:"api_version"`
	Kind       types.String `tfsdk:"kind"`
	Name       types.String `tfsdk:"name"`
	Namespace  types.String `tfsdk:"namespace"`
}

// NodeGroupClassModel is the class block of topology mode.
type NodeGroupClassModel struct {
	Name      types.String  `tfsdk:"name"`
	Variables types.Dynamic `tfsdk:"variables"`
}

// RolloutModel is the rollout block.
type RolloutModel struct {
	Strategy       types.String `tfsdk:"strategy"`
	MaxSurge       types.Int64  `tfsdk:"max_surge"`
	MaxUnavailable types.Int64  `tfsdk:"max_unavailable"`
}

// NodeTimeoutsModel is the timeouts block.
type NodeTimeoutsModel struct {
	NodeDrain        types.String `tfsdk:"node_drain"`
	NodeVolumeDetach types.String `tfsdk:"node_volume_detach"`
	NodeDeletion     types.String `tfsdk:"node_deletion"`
}

// NodeGroupStatusModel is the status block.
type NodeGroupStatusModel struct {
	Phase             types.String `tfsdk:"phase"`
	Replicas          types.Int64  `tfsdk:"replicas"`
	ReadyReplicas     types.Int64  `tfsdk:"ready_replicas"`
	AvailableReplicas types.Int64  `tfsdk:"available_replicas"`
	UpToDateReplicas  types.Int64  `tfsdk:"up_to_date_replicas"`
}

func autoscalingAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"min": types.Int64Type, "max": types.Int64Type}
}

func nodeGroupTemplateAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"infrastructure": types.ObjectType{AttrTypes: templateRefAttrTypes()},
		"bootstrap":      types.ObjectType{AttrTypes: templateRefAttrTypes()},
	}
}

func nodeGroupClassAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"name": types.StringType, "variables": types.DynamicType}
}

func rolloutAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"strategy": types.StringType, "max_surge": types.Int64Type, "max_unavailable": types.Int64Type}
}

func nodeTimeoutsAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"node_drain": types.StringType, "node_volume_detach": types.StringType, "node_deletion": types.StringType}
}

func nodeGroupStatusAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"phase": types.StringType, "replicas": types.Int64Type, "ready_replicas": types.Int64Type,
		"available_replicas": types.Int64Type, "up_to_date_replicas": types.Int64Type,
	}
}

// templateRefSchema is a template reference attribute.
func templateRefSchema(desc string) schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		MarkdownDescription: desc,
		Required:            true,
		Attributes: map[string]schema.Attribute{
			"api_group":   schema.StringAttribute{MarkdownDescription: "API group.", Required: true},
			"api_version": schema.StringAttribute{MarkdownDescription: "`group/version`.", Required: true},
			"kind":        schema.StringAttribute{MarkdownDescription: "Kind.", Required: true},
			"name":        schema.StringAttribute{MarkdownDescription: "Object name.", Required: true},
			"namespace":   schema.StringAttribute{MarkdownDescription: "Namespace.", Optional: true},
		},
	}
}

// nodeGroupWaitDefaults are the conditions a group is waited for by default.
var nodeGroupWaitDefaults = []string{"Available", "MachinesUpToDate"}

func (r *MachineDeploymentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machine_deployment"
}

func (r *MachineDeploymentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A node group of a cluster on the management cluster the provider block names, the way `aws_eks_node_group` is one of an EKS cluster: a `MachineDeployment` (`template`), or one entry of the Cluster's topology workers when the cluster uses a ClusterClass (`class`), owned by this resource's own field manager so `capi_cluster` and other groups never touch it. The API version, the default Kubernetes version, the failure domains and the worker classes come from the live Cluster; the configuration states only what the group is. `replicas` and `autoscaling` are exclusive: with autoscaling the cluster autoscaler owns the count and it never shows as drift. Import with `namespace/cluster/name`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"cluster": schema.StringAttribute{
				MarkdownDescription: "Name of the Cluster.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"namespace": schema.StringAttribute{
				MarkdownDescription: "Namespace of the Cluster. Default `default`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("default"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Group name: the MachineDeployment's name, or the topology entry's.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"replicas": schema.Int64Attribute{
				MarkdownDescription: "Desired machines. Exclusive with `autoscaling`.",
				Optional:            true,
			},
			"autoscaling": schema.SingleNestedAttribute{
				MarkdownDescription: "Let the cluster autoscaler own the count between `min` and `max` (the `cluster.x-k8s.io/cluster-api-autoscaler-node-group-*` annotations).",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"min": schema.Int64Attribute{Required: true},
					"max": schema.Int64Attribute{Required: true},
				},
			},
			"version": schema.StringAttribute{
				MarkdownDescription: "Kubernetes version of the machines (`v1.34.0`). Defaults to the cluster's topology version; required without a ClusterClass.",
				Optional:            true,
				Computed:            true,
			},
			"template": schema.SingleNestedAttribute{
				MarkdownDescription: "Direct mode: the machine template and bootstrap config template of the group, from `capi_machine_template` and `capi_bootstrap_template` `ref`s. Changing a ref rolls the group out. Exclusive with `class`.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"infrastructure": templateRefSchema("The `<Infra>MachineTemplate` ref."),
					"bootstrap":      templateRefSchema("The `<Bootstrap>ConfigTemplate` ref."),
				},
			},
			"class": schema.SingleNestedAttribute{
				MarkdownDescription: "Topology mode: the ClusterClass worker class the group instantiates, with variable overrides. Exclusive with `template`.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{MarkdownDescription: "Worker class name, validated against the ClusterClass.", Required: true},
					"variables": schema.DynamicAttribute{
						MarkdownDescription: "Variable overrides for this group, as an object of variable name to value.",
						Optional:            true,
					},
				},
			},
			"failure_domain": schema.StringAttribute{
				MarkdownDescription: "Failure domain, validated against the Cluster's `status.failureDomains`.",
				Optional:            true,
			},
			"labels":      metadataMapAttribute("Labels on the machines (and their nodes)."),
			"annotations": metadataMapAttribute("Annotations on the machines."),
			"rollout": schema.SingleNestedAttribute{
				MarkdownDescription: "Rollout strategy.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"strategy":        schema.StringAttribute{MarkdownDescription: "`RollingUpdate` (default) or `OnDelete`.", Optional: true},
					"max_surge":       schema.Int64Attribute{MarkdownDescription: "Machines above the desired count during a rollout.", Optional: true},
					"max_unavailable": schema.Int64Attribute{MarkdownDescription: "Machines below the desired count during a rollout.", Optional: true},
				},
			},
			"timeouts": schema.SingleNestedAttribute{
				MarkdownDescription: "Node lifecycle timeouts, as Go durations.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"node_drain":         schema.StringAttribute{MarkdownDescription: "How long to drain a node before deleting its machine.", Optional: true},
					"node_volume_detach": schema.StringAttribute{MarkdownDescription: "How long to wait for volumes to detach.", Optional: true},
					"node_deletion":      schema.StringAttribute{MarkdownDescription: "How long to wait for the node object to be deleted.", Optional: true},
				},
			},
			"wait": objectWaitAttribute(nodeGroupWaitDefaults),
			"scale_to_zero_on_delete": schema.BoolAttribute{
				MarkdownDescription: "Scale the group to zero and wait for its machines to drain before deleting the object, for providers that release hardware slowly.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"object_name": schema.StringAttribute{
				MarkdownDescription: "Name of the realised MachineDeployment (generated by the topology controller in topology mode).",
				Computed:            true,
			},
			"manifest": schema.StringAttribute{
				MarkdownDescription: "What was applied: the MachineDeployment, or the Cluster patch carrying the topology entry.",
				Computed:            true,
			},
			"status": schema.SingleNestedAttribute{
				MarkdownDescription: "The MachineDeployment's status.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"phase":               schema.StringAttribute{Computed: true},
					"replicas":            schema.Int64Attribute{Computed: true},
					"ready_replicas":      schema.Int64Attribute{Computed: true},
					"available_replicas":  schema.Int64Attribute{Computed: true},
					"up_to_date_replicas": schema.Int64Attribute{Computed: true},
				},
			},
			"conditions": schema.ListNestedAttribute{
				MarkdownDescription: "The MachineDeployment's conditions.",
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

func (r *MachineDeploymentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// inputs converts the model to render inputs; cluster facts fill defaults.
func (r *MachineDeploymentResource) inputs(ctx context.Context, data *MachineDeploymentResourceModel, facts clusterFacts) (nodeGroupInputs, diag.Diagnostics) {
	var diags diag.Diagnostics
	in := nodeGroupInputs{
		Cluster: data.Cluster.ValueString(), Namespace: data.Namespace.ValueString(), Name: data.Name.ValueString(),
		CoreVersion: facts.CoreVersion, FailureDomain: data.FailureDomain.ValueString(),
	}
	if !data.Replicas.IsNull() && !data.Replicas.IsUnknown() {
		v := data.Replicas.ValueInt64()
		in.Replicas = &v
	}
	if !data.Autoscaling.IsNull() && !data.Autoscaling.IsUnknown() {
		var a AutoscalingModel
		diags.Append(data.Autoscaling.As(ctx, &a, basetypes.ObjectAsOptions{})...)
		mn, mx := a.Min.ValueInt64(), a.Max.ValueInt64()
		in.AutoscalingMin, in.AutoscalingMax = &mn, &mx
	}
	in.Version = data.Version.ValueString()
	if data.Version.IsUnknown() || in.Version == "" {
		in.Version = facts.Version
	}
	if !data.Template.IsNull() && !data.Template.IsUnknown() {
		var t NodeGroupTemplateModel
		diags.Append(data.Template.As(ctx, &t, basetypes.ObjectAsOptions{})...)
		in.Infrastructure = refFromModel(ctx, t.Infrastructure, &diags)
		in.Bootstrap = refFromModel(ctx, t.Bootstrap, &diags)
	}
	if !data.Class.IsNull() && !data.Class.IsUnknown() {
		var c NodeGroupClassModel
		diags.Append(data.Class.As(ctx, &c, basetypes.ObjectAsOptions{})...)
		in.Class = c.Name.ValueString()
		vars, d := dynamicToMap(c.Variables)
		diags.Append(d...)
		in.Variables = vars
	}
	var d diag.Diagnostics
	in.Labels, d = stringMap(ctx, data.Labels)
	diags.Append(d...)
	in.Annotations, d = stringMap(ctx, data.Annotations)
	diags.Append(d...)
	if !data.Rollout.IsNull() && !data.Rollout.IsUnknown() {
		var ro RolloutModel
		diags.Append(data.Rollout.As(ctx, &ro, basetypes.ObjectAsOptions{})...)
		in.Strategy = ro.Strategy.ValueString()
		if !ro.MaxSurge.IsNull() {
			v := ro.MaxSurge.ValueInt64()
			in.MaxSurge = &v
		}
		if !ro.MaxUnavailable.IsNull() {
			v := ro.MaxUnavailable.ValueInt64()
			in.MaxUnavailable = &v
		}
	}
	if !data.Timeouts.IsNull() && !data.Timeouts.IsUnknown() {
		var to NodeTimeoutsModel
		diags.Append(data.Timeouts.As(ctx, &to, basetypes.ObjectAsOptions{})...)
		in.NodeDrain = durationOrNil(to.NodeDrain, "timeouts.node_drain", &diags)
		in.NodeVolumeDetach = durationOrNil(to.NodeVolumeDetach, "timeouts.node_volume_detach", &diags)
		in.NodeDeletion = durationOrNil(to.NodeDeletion, "timeouts.node_deletion", &diags)
	}
	return in, diags
}

func refFromModel(ctx context.Context, obj types.Object, diags *diag.Diagnostics) *objectRef {
	if obj.IsNull() || obj.IsUnknown() {
		return nil
	}
	var m TemplateRefModel
	diags.Append(obj.As(ctx, &m, basetypes.ObjectAsOptions{})...)
	return &objectRef{APIGroup: m.APIGroup.ValueString(), APIVersion: m.APIVersion.ValueString(), Kind: m.Kind.ValueString(), Name: m.Name.ValueString(), Namespace: m.Namespace.ValueString()}
}

func durationOrNil(s types.String, where string, diags *diag.Diagnostics) *time.Duration {
	if s.IsNull() || s.IsUnknown() || s.ValueString() == "" {
		return nil
	}
	d, err := time.ParseDuration(s.ValueString())
	if err != nil {
		diags.AddError("Invalid "+where, fmt.Sprintf("%q is not a Go duration: %s.", s.ValueString(), err))
		return nil
	}
	return &d
}

// facts reads the Cluster (and its ClusterClass) the group belongs to.
func (r *MachineDeploymentResource) facts(ctx context.Context, client *capi.ObjectClient, namespace, name string) (clusterFacts, *unstructured.Unstructured, error) {
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return clusterFacts{}, nil, err
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		return clusterFacts{}, nil, err
	}
	clusterGVK := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: "Cluster"}
	cluster, err := client.Get(ctx, clusterGVK, namespace, name)
	if err != nil {
		return clusterFacts{}, nil, fmt.Errorf("reading Cluster %s/%s: %w", namespace, name, err)
	}
	var class *unstructured.Unstructured
	className, classNamespace := clusterClassRef(cluster)
	if className != "" {
		if classNamespace == "" {
			classNamespace = namespace
		}
		class, err = client.Get(ctx, k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: "ClusterClass"}, classNamespace, className)
		if err != nil && !errors.Is(err, capi.ErrObjectNotFound) {
			return clusterFacts{}, nil, fmt.Errorf("reading ClusterClass %s/%s: %w", classNamespace, className, err)
		}
	}
	return readClusterFacts(cluster, class), cluster, nil
}

func (r *MachineDeploymentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data MachineDeploymentResourceModel
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

func (r *MachineDeploymentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data MachineDeploymentResourceModel
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

// apply renders and applies the group, waits for its rollout and fills the
// computed attributes.
func (r *MachineDeploymentResource) apply(ctx context.Context, data *MachineDeploymentResourceModel, diags *diag.Diagnostics) {
	client, d := r.pd.objectClient(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	facts, _, err := r.facts(ctx, client, data.Namespace.ValueString(), data.Cluster.ValueString())
	if err != nil {
		diags.AddError("Reading cluster", err.Error())
		return
	}
	in, d := r.inputs(ctx, data, facts)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	if err := errors.Join(in.validate(), in.checkAgainstCluster(facts)); err != nil {
		diags.AddError("Invalid machine deployment", err.Error())
		return
	}
	conditions, timeout, d := objectWaitFromModel(ctx, data.Wait, nodeGroupWaitDefaults)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	data.Version = stringOrNull(in.Version)

	var md *unstructured.Unstructured
	if in.topologyMode() {
		_, patch := renderTopologyEntry(in)
		groupClient := client.WithFieldManager(nodeGroupFieldManager(in.Name))
		if _, err := groupClient.Apply(ctx, patch, true); err != nil {
			diags.AddError("Applying topology entry", err.Error())
			return
		}
		data.Manifest = types.StringValue(manifestYAML(patch))
		md, err = r.waitRealised(ctx, client, in, conditions, timeout)
		if err != nil {
			diags.AddError("Waiting for machine deployment", r.withMachineFailures(ctx, client, in, md, err))
			return
		}
	} else {
		obj := renderMachineDeployment(in)
		data.Manifest = types.StringValue(manifestYAML(obj))
		if _, err := client.Apply(ctx, obj, false); err != nil {
			diags.AddError("Applying MachineDeployment", err.Error())
			return
		}
		gvk := obj.GroupVersionKind()
		if len(conditions) > 0 {
			if err := client.WaitForConditions(ctx, gvk, in.Namespace, in.Name, conditions, capi.WaitOptions{Timeout: timeout}); err != nil {
				diags.AddError("Waiting for machine deployment", r.withMachineFailures(ctx, client, in, obj, err))
				return
			}
		}
		md, err = client.Get(ctx, gvk, in.Namespace, in.Name)
		if err != nil {
			diags.AddError("Reading machine deployment", err.Error())
			return
		}
	}
	data.ID = types.StringValue(fmt.Sprintf("%s/%s/%s", in.Namespace, in.Cluster, in.Name))
	diags.Append(r.fill(ctx, data, md)...)
}

// waitRealised waits for the topology controller to create the group's
// MachineDeployment, then for its conditions.
func (r *MachineDeploymentResource) waitRealised(ctx context.Context, client *capi.ObjectClient, in nodeGroupInputs, conditions []string, timeout time.Duration) (*unstructured.Unstructured, error) {
	gvk := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: in.CoreVersion, Kind: "MachineDeployment"}
	selector := fmt.Sprintf("%s=%s,%s=%s", clusterNameLabel, in.Cluster, topologyDeploymentLabel, in.Name)
	deadline := time.Now().Add(timeout)
	var md *unstructured.Unstructured
	for md == nil {
		list, err := client.List(ctx, gvk, in.Namespace, selector)
		if err != nil {
			return nil, err
		}
		if len(list) > 0 {
			md = &list[0]
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the topology controller created no MachineDeployment for %s within %v", in.Name, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if len(conditions) == 0 {
		return md, nil
	}
	if err := client.WaitForConditions(ctx, gvk, in.Namespace, md.GetName(), conditions, capi.WaitOptions{Timeout: time.Until(deadline)}); err != nil {
		return md, err
	}
	return client.Get(ctx, gvk, in.Namespace, md.GetName())
}

// withMachineFailures appends to a wait error what each machine of the
// group reports, so a stuck rollout names the machine, its failing
// condition and the timeout that bounds its deletion.
func (r *MachineDeploymentResource) withMachineFailures(ctx context.Context, client *capi.ObjectClient, in nodeGroupInputs, md *unstructured.Unstructured, err error) string {
	if md == nil {
		return err.Error()
	}
	gvk := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: in.CoreVersion, Kind: "Machine"}
	machines, listErr := client.List(ctx, gvk, in.Namespace, fmt.Sprintf("%s=%s", deploymentNameLabel, md.GetName()))
	if listErr != nil {
		return err.Error()
	}
	if desc := describeMachineFailures(readMachineFailures(machines)); desc != "" {
		return err.Error() + "\n" + desc
	}
	return err.Error()
}

// fill writes the computed attributes from the realised MachineDeployment.
func (r *MachineDeploymentResource) fill(ctx context.Context, data *MachineDeploymentResourceModel, md *unstructured.Unstructured) diag.Diagnostics {
	var diags diag.Diagnostics
	if md == nil {
		data.ObjectName = types.StringNull()
		data.Status = types.ObjectNull(nodeGroupStatusAttrTypes())
		data.Conditions = types.ListNull(types.ObjectType{AttrTypes: conditionAttrTypes()})
		return diags
	}
	st := readNodeGroupStatus(md)
	data.ObjectName = types.StringValue(st.ObjectName)
	statusVal, d := types.ObjectValueFrom(ctx, nodeGroupStatusAttrTypes(), NodeGroupStatusModel{
		Phase: stringOrNull(st.Phase), Replicas: types.Int64Value(st.Replicas), ReadyReplicas: types.Int64Value(st.ReadyReplicas),
		AvailableReplicas: types.Int64Value(st.AvailableReplicas), UpToDateReplicas: types.Int64Value(st.UpToDateReplicas),
	})
	diags.Append(d...)
	data.Status = statusVal
	conds := make([]ConditionModel, 0, len(st.Conditions))
	for _, c := range st.Conditions {
		conds = append(conds, ConditionModel{Type: types.StringValue(c.Type), Status: types.StringValue(c.Status), Reason: stringOrNull(c.Reason), Message: stringOrNull(c.Message)})
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: conditionAttrTypes()}, conds)
	diags.Append(d...)
	data.Conditions = list
	return diags
}

// drift copies the live values of the configured fields into the model.
func (r *MachineDeploymentResource) drift(ctx context.Context, data *MachineDeploymentResourceModel, live liveNodeGroup) diag.Diagnostics {
	var diags diag.Diagnostics
	if data.Autoscaling.IsNull() && live.Replicas != nil {
		data.Replicas = types.Int64Value(*live.Replicas)
	}
	if live.Version != "" {
		data.Version = types.StringValue(live.Version)
	}
	data.FailureDomain = stringOrNull(live.FailureDomain)
	if !data.Template.IsNull() && (live.Infrastructure != "" || live.Bootstrap != "") {
		var t NodeGroupTemplateModel
		diags.Append(data.Template.As(ctx, &t, basetypes.ObjectAsOptions{})...)
		t.Infrastructure = refWithName(ctx, t.Infrastructure, live.Infrastructure, &diags)
		t.Bootstrap = refWithName(ctx, t.Bootstrap, live.Bootstrap, &diags)
		tv, d := types.ObjectValueFrom(ctx, nodeGroupTemplateAttrTypes(), t)
		diags.Append(d...)
		data.Template = tv
	}
	if !data.Class.IsNull() && live.Class != "" {
		var c NodeGroupClassModel
		diags.Append(data.Class.As(ctx, &c, basetypes.ObjectAsOptions{})...)
		c.Name = types.StringValue(live.Class)
		cv, d := types.ObjectValueFrom(ctx, nodeGroupClassAttrTypes(), c)
		diags.Append(d...)
		data.Class = cv
	}
	return diags
}

func refWithName(ctx context.Context, obj types.Object, name string, diags *diag.Diagnostics) types.Object {
	if obj.IsNull() || name == "" {
		return obj
	}
	var m TemplateRefModel
	diags.Append(obj.As(ctx, &m, basetypes.ObjectAsOptions{})...)
	m.Name = types.StringValue(name)
	v, d := types.ObjectValueFrom(ctx, templateRefAttrTypes(), m)
	diags.Append(d...)
	return v
}

func (r *MachineDeploymentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data MachineDeploymentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	facts, cluster, err := r.facts(ctx, client, data.Namespace.ValueString(), data.Cluster.ValueString())
	if errors.Is(err, capi.ErrObjectNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading cluster", err.Error())
		return
	}
	mdGVK := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: facts.CoreVersion, Kind: "MachineDeployment"}
	if !data.Class.IsNull() {
		entry := findTopologyEntry(cluster, data.Name.ValueString())
		if entry == nil {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.Append(r.drift(ctx, &data, readLiveTopologyEntry(entry))...)
		list, err := client.List(ctx, mdGVK, data.Namespace.ValueString(), fmt.Sprintf("%s=%s,%s=%s", clusterNameLabel, data.Cluster.ValueString(), topologyDeploymentLabel, data.Name.ValueString()))
		if err != nil {
			resp.Diagnostics.AddError("Reading machine deployment", err.Error())
			return
		}
		var md *unstructured.Unstructured
		if len(list) > 0 {
			md = &list[0]
			live := readLiveMachineDeployment(md)
			if live.Version != "" {
				data.Version = types.StringValue(live.Version)
			}
		}
		resp.Diagnostics.Append(r.fill(ctx, &data, md)...)
	} else {
		md, found, d := readLive(ctx, client, mdGVK, data.Namespace.ValueString(), data.Name.ValueString())
		resp.Diagnostics.Append(d...)
		if d.HasError() {
			return
		}
		if !found {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.Append(r.drift(ctx, &data, readLiveMachineDeployment(md))...)
		resp.Diagnostics.Append(r.fill(ctx, &data, md)...)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *MachineDeploymentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data MachineDeploymentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	_, timeout, d := objectWaitFromModel(ctx, data.Wait, nodeGroupWaitDefaults)
	resp.Diagnostics.Append(d...)
	facts, _, err := r.facts(ctx, client, data.Namespace.ValueString(), data.Cluster.ValueString())
	if errors.Is(err, capi.ErrObjectNotFound) {
		return // the cluster is gone, and the group with it
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading cluster", err.Error())
		return
	}
	in, d := r.inputs(ctx, &data, facts)
	resp.Diagnostics.Append(d...)
	mdGVK := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: facts.CoreVersion, Kind: "MachineDeployment"}
	name := data.Name.ValueString()
	if in.topologyMode() {
		name = data.ObjectName.ValueString()
	}
	if data.ScaleToZero.ValueBool() && name != "" {
		zero := int64(0)
		scaled := in
		scaled.Replicas = &zero
		scaled.AutoscalingMin, scaled.AutoscalingMax = nil, nil
		if in.topologyMode() {
			_, patch := renderTopologyEntry(scaled)
			if _, err := client.WithFieldManager(nodeGroupFieldManager(in.Name)).Apply(ctx, patch, true); err != nil {
				resp.Diagnostics.AddError("Scaling to zero", err.Error())
				return
			}
		} else if _, err := client.Apply(ctx, renderMachineDeployment(scaled), false); err != nil {
			resp.Diagnostics.AddError("Scaling to zero", err.Error())
			return
		}
		if err := r.waitZero(ctx, client, mdGVK, in.Namespace, name, timeout); err != nil {
			resp.Diagnostics.AddError("Scaling to zero", err.Error())
			return
		}
	}
	if in.topologyMode() {
		// The topology controller owns the realised MachineDeployment and
		// deletes it once the entry is gone; deleting it here would race a
		// re-creation.
		if _, err := client.WithFieldManager(nodeGroupFieldManager(in.Name)).Apply(ctx, renderTopologyRemoval(in), true); err != nil {
			resp.Diagnostics.AddError("Removing topology entry", err.Error())
			return
		}
		if name == "" {
			return
		}
		if err := client.WaitForDeletion(ctx, mdGVK, in.Namespace, name, capi.WaitOptions{Timeout: timeout}); err != nil {
			resp.Diagnostics.AddError("Waiting for machine deployment deletion", err.Error())
		}
		return
	}
	resp.Diagnostics.Append(deleteAndWait(ctx, client, mdGVK, in.Namespace, name, timeout)...)
}

// waitZero waits for a MachineDeployment to report no replicas.
func (r *MachineDeploymentResource) waitZero(ctx context.Context, client *capi.ObjectClient, gvk k8sschema.GroupVersionKind, namespace, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		md, err := client.Get(ctx, gvk, namespace, name)
		if errors.Is(err, capi.ErrObjectNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if replicas, _, _ := unstructured.NestedInt64(md.Object, "status", "replicas"); replicas == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s/%s still has machines after %v", namespace, name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// ImportState takes `namespace/cluster/name`.
func (r *MachineDeploymentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected namespace/cluster/name, got %q.", req.ID))
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	facts, cluster, err := r.facts(ctx, client, parts[0], parts[1])
	if err != nil {
		resp.Diagnostics.AddError("Reading cluster", err.Error())
		return
	}
	data := MachineDeploymentResourceModel{
		ID: types.StringValue(req.ID), Cluster: types.StringValue(parts[1]), Namespace: types.StringValue(parts[0]), Name: types.StringValue(parts[2]),
		Replicas: types.Int64Null(), Autoscaling: types.ObjectNull(autoscalingAttrTypes()), Version: types.StringNull(),
		Template: types.ObjectNull(nodeGroupTemplateAttrTypes()), Class: types.ObjectNull(nodeGroupClassAttrTypes()),
		FailureDomain: types.StringNull(), Labels: types.MapNull(types.StringType), Annotations: types.MapNull(types.StringType),
		Rollout: types.ObjectNull(rolloutAttrTypes()), Timeouts: types.ObjectNull(nodeTimeoutsAttrTypes()), Wait: types.ObjectNull(objectWaitAttrTypes()),
		ScaleToZero: types.BoolValue(false), Manifest: types.StringNull(),
	}
	mdGVK := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: facts.CoreVersion, Kind: "MachineDeployment"}
	if facts.TopologyMode {
		entry := findTopologyEntry(cluster, parts[2])
		if entry == nil {
			resp.Diagnostics.AddError("Machine deployment not found", fmt.Sprintf("Cluster %s/%s has no topology entry %q.", parts[0], parts[1], parts[2]))
			return
		}
		live := readLiveTopologyEntry(entry)
		cv, d := types.ObjectValueFrom(ctx, nodeGroupClassAttrTypes(), NodeGroupClassModel{Name: types.StringValue(live.Class), Variables: types.DynamicNull()})
		resp.Diagnostics.Append(d...)
		data.Class = cv
		if live.Replicas != nil {
			data.Replicas = types.Int64Value(*live.Replicas)
		}
		data.FailureDomain = stringOrNull(live.FailureDomain)
		list, _ := client.List(ctx, mdGVK, parts[0], fmt.Sprintf("%s=%s,%s=%s", clusterNameLabel, parts[1], topologyDeploymentLabel, parts[2]))
		var md *unstructured.Unstructured
		if len(list) > 0 {
			md = &list[0]
			data.Version = stringOrNull(readLiveMachineDeployment(md).Version)
		}
		resp.Diagnostics.Append(r.fill(ctx, &data, md)...)
	} else {
		md, err := client.Get(ctx, mdGVK, parts[0], parts[2])
		if err != nil {
			resp.Diagnostics.AddError("Machine deployment not found", err.Error())
			return
		}
		live := readLiveMachineDeployment(md)
		if live.Replicas != nil {
			data.Replicas = types.Int64Value(*live.Replicas)
		}
		data.Version = stringOrNull(live.Version)
		data.FailureDomain = stringOrNull(live.FailureDomain)
		refOf := func(path ...string) types.Object {
			ref, _, _ := unstructured.NestedMap(md.Object, path...)
			apiVersion, _ := ref["apiVersion"].(string)
			group, _ := ref["apiGroup"].(string)
			if group == "" {
				group = gvkFor(apiVersion, "").Group
			}
			kind, _ := ref["kind"].(string)
			name, _ := ref["name"].(string)
			v, d := types.ObjectValueFrom(ctx, templateRefAttrTypes(), TemplateRefModel{
				APIGroup: types.StringValue(group), APIVersion: stringOrNull(apiVersion), Kind: types.StringValue(kind), Name: types.StringValue(name), Namespace: types.StringValue(parts[0]),
			})
			resp.Diagnostics.Append(d...)
			return v
		}
		tv, d := types.ObjectValueFrom(ctx, nodeGroupTemplateAttrTypes(), NodeGroupTemplateModel{
			Infrastructure: refOf("spec", "template", "spec", "infrastructureRef"), Bootstrap: refOf("spec", "template", "spec", "bootstrap", "configRef"),
		})
		resp.Diagnostics.Append(d...)
		data.Template = tv
		resp.Diagnostics.Append(r.fill(ctx, &data, md)...)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
