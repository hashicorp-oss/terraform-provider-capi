// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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
	_ resource.Resource                = &MachineHealthCheckResource{}
	_ resource.ResourceWithConfigure   = &MachineHealthCheckResource{}
	_ resource.ResourceWithImportState = &MachineHealthCheckResource{}
)

// NewMachineHealthCheckResource returns the capi_machine_health_check resource.
func NewMachineHealthCheckResource() resource.Resource {
	return &MachineHealthCheckResource{}
}

// MachineHealthCheckResource is a MachineHealthCheck: which machines of a
// cluster are watched, what makes one unhealthy, and when remediation is
// allowed. The v1beta2 `checks`/`remediation` shape and the legacy flat one
// render from the same configuration.
type MachineHealthCheckResource struct {
	pd *providerData
}

// MachineHealthCheckResourceModel is the capi_machine_health_check model.
type MachineHealthCheckResourceModel struct {
	ID                         types.String `tfsdk:"id"`
	Cluster                    types.String `tfsdk:"cluster"`
	Namespace                  types.String `tfsdk:"namespace"`
	Name                       types.String `tfsdk:"name"`
	MachineDeployment          types.String `tfsdk:"machine_deployment"`
	ControlPlane               types.Bool   `tfsdk:"control_plane"`
	Selector                   types.Map    `tfsdk:"selector"`
	NodeStartupTimeout         types.String `tfsdk:"node_startup_timeout"`
	UnhealthyNodeConditions    types.List   `tfsdk:"unhealthy_node_conditions"`
	UnhealthyMachineConditions types.List   `tfsdk:"unhealthy_machine_conditions"`
	Remediation                types.Object `tfsdk:"remediation"`
	Wait                       types.Object `tfsdk:"wait"`
	Manifest                   types.String `tfsdk:"manifest"`
	Status                     types.Object `tfsdk:"status"`
	Conditions                 types.List   `tfsdk:"conditions"`
}

// UnhealthyConditionModel is one unhealthy node or machine condition.
type UnhealthyConditionModel struct {
	Type    types.String `tfsdk:"type"`
	Status  types.String `tfsdk:"status"`
	Timeout types.String `tfsdk:"timeout"`
}

// RemediationModel is the remediation block.
type RemediationModel struct {
	MaxUnhealthy   types.String `tfsdk:"max_unhealthy"`
	UnhealthyRange types.String `tfsdk:"unhealthy_range"`
	Template       types.Object `tfsdk:"template"`
}

// RemediationTemplateModel is a reference to an external remediation template.
type RemediationTemplateModel struct {
	APIVersion types.String `tfsdk:"api_version"`
	Kind       types.String `tfsdk:"kind"`
	Name       types.String `tfsdk:"name"`
}

// HealthCheckStatusModel is the status block.
type HealthCheckStatusModel struct {
	ExpectedMachines    types.Int64 `tfsdk:"expected_machines"`
	CurrentHealthy      types.Int64 `tfsdk:"current_healthy"`
	RemediationsAllowed types.Int64 `tfsdk:"remediations_allowed"`
	Targets             types.List  `tfsdk:"targets"`
}

func unhealthyConditionAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"type": types.StringType, "status": types.StringType, "timeout": types.StringType}
}

func remediationTemplateAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"api_version": types.StringType, "kind": types.StringType, "name": types.StringType}
}

func remediationAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"max_unhealthy": types.StringType, "unhealthy_range": types.StringType,
		"template": types.ObjectType{AttrTypes: remediationTemplateAttrTypes()},
	}
}

func healthCheckStatusAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"expected_machines": types.Int64Type, "current_healthy": types.Int64Type, "remediations_allowed": types.Int64Type,
		"targets": types.ListType{ElemType: types.StringType},
	}
}

// healthCheckWaitDefaults is the condition waited for by default.
var healthCheckWaitDefaults = []string{"RemediationAllowed"}

// defaultUnhealthyNodeConditions are CAPI's conventional checks, used when
// the configuration names none.
var defaultUnhealthyNodeConditions = []unhealthyCondition{
	{Type: "Ready", Status: "Unknown", Timeout: 5 * time.Minute},
	{Type: "Ready", Status: "False", Timeout: 5 * time.Minute},
}

func unhealthyConditionsAttribute(desc string) schema.ListNestedAttribute {
	return schema.ListNestedAttribute{
		MarkdownDescription: desc,
		Optional:            true,
		Computed:            true,
		NestedObject: schema.NestedAttributeObject{
			Attributes: map[string]schema.Attribute{
				"type":    schema.StringAttribute{MarkdownDescription: "Condition type (`Ready`).", Required: true},
				"status":  schema.StringAttribute{MarkdownDescription: "Condition status that is unhealthy (`False`, `Unknown`).", Required: true},
				"timeout": schema.StringAttribute{MarkdownDescription: "How long the condition may hold before the machine is unhealthy, as a Go duration.", Required: true},
			},
		},
	}
}

func (r *MachineHealthCheckResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machine_health_check"
}

func (r *MachineHealthCheckResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A MachineHealthCheck of a cluster on the management cluster the provider block names: which machines are watched (one of `machine_deployment`, `control_plane` or `selector`), what makes one unhealthy, and when remediation is allowed. The object's API version follows the installed core provider; the v1beta2 and legacy spec shapes render from the same configuration. Import with `namespace/name`.",
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
				MarkdownDescription: "Object name.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"machine_deployment": schema.StringAttribute{
				MarkdownDescription: "Watch the machines of this `capi_machine_deployment` (by its `name`), in either mode.",
				Optional:            true,
			},
			"control_plane": schema.BoolAttribute{
				MarkdownDescription: "Watch the control-plane machines.",
				Optional:            true,
			},
			"selector": schema.MapAttribute{
				MarkdownDescription: "Watch the machines carrying these labels.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"node_startup_timeout": schema.StringAttribute{
				MarkdownDescription: "How long a machine may go without a node before it is unhealthy, as a Go duration. CAPI's default is `10m`.",
				Optional:            true,
			},
			"unhealthy_node_conditions":    unhealthyConditionsAttribute("Node conditions that make a machine unhealthy. Default: `Ready` `Unknown` or `False` for `5m`."),
			"unhealthy_machine_conditions": unhealthyConditionsAttribute("Machine conditions that make a machine unhealthy."),
			"remediation": schema.SingleNestedAttribute{
				MarkdownDescription: "When and how unhealthy machines are remediated. Without it, the owner replaces each unhealthy machine while at most 100% are unhealthy.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"max_unhealthy": schema.StringAttribute{
						MarkdownDescription: "Remediate only while at most this many machines are unhealthy: a count (`2`) or a percentage (`40%`). Exclusive with `unhealthy_range`.",
						Optional:            true,
					},
					"unhealthy_range": schema.StringAttribute{
						MarkdownDescription: "Remediate only while the unhealthy count is in this range (`[1-3]`). Exclusive with `max_unhealthy`.",
						Optional:            true,
					},
					"template": schema.SingleNestedAttribute{
						MarkdownDescription: "An external remediation template (a Metal3 or custom remediation CRD) used instead of deleting the machine.",
						Optional:            true,
						Attributes: map[string]schema.Attribute{
							"api_version": schema.StringAttribute{Required: true},
							"kind":        schema.StringAttribute{Required: true},
							"name":        schema.StringAttribute{Required: true},
						},
					},
				},
			},
			"wait":     objectWaitAttribute(healthCheckWaitDefaults),
			"manifest": schema.StringAttribute{MarkdownDescription: "The applied object as YAML.", Computed: true},
			"status": schema.SingleNestedAttribute{
				MarkdownDescription: "The object's status.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"expected_machines":    schema.Int64Attribute{Computed: true},
					"current_healthy":      schema.Int64Attribute{Computed: true},
					"remediations_allowed": schema.Int64Attribute{Computed: true},
					"targets":              schema.ListAttribute{ElementType: types.StringType, Computed: true},
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

func (r *MachineHealthCheckResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// unhealthyCondition is one node or machine condition check.
type unhealthyCondition struct {
	Type, Status string
	Timeout      time.Duration
}

// healthCheckInputs is everything a MachineHealthCheck renders from.
type healthCheckInputs struct {
	Cluster, Namespace, Name string
	CoreVersion              string
	MatchLabels              map[string]string
	NodeStartupTimeout       *time.Duration
	NodeConditions           []unhealthyCondition
	MachineConditions        []unhealthyCondition
	MaxUnhealthy             string
	UnhealthyRange           string
	RemediationTemplate      *objectRef
}

// contractShape reports whether the v1beta2 shape applies.
func (in healthCheckInputs) contractShape() bool {
	return nodeGroupInputs{CoreVersion: in.CoreVersion}.contractRefs()
}

// validate checks the inputs.
func (in healthCheckInputs) validate() error {
	var errs []error
	if len(in.MatchLabels) == 0 {
		errs = append(errs, errors.New("one of machine_deployment, control_plane or selector is required"))
	}
	if in.MaxUnhealthy != "" && in.UnhealthyRange != "" {
		errs = append(errs, errors.New("remediation.max_unhealthy and remediation.unhealthy_range are exclusive"))
	}
	if in.MaxUnhealthy != "" {
		v := strings.TrimSuffix(in.MaxUnhealthy, "%")
		if v == "" || strings.Trim(v, "0123456789") != "" {
			errs = append(errs, fmt.Errorf("remediation.max_unhealthy %q is not a count or a percentage", in.MaxUnhealthy))
		}
	}
	if in.UnhealthyRange != "" && (!strings.HasPrefix(in.UnhealthyRange, "[") || !strings.HasSuffix(in.UnhealthyRange, "]")) {
		errs = append(errs, fmt.Errorf("remediation.unhealthy_range %q is not of the form [min-max]", in.UnhealthyRange))
	}
	for _, c := range append(append([]unhealthyCondition{}, in.NodeConditions...), in.MachineConditions...) {
		if c.Type == "" || c.Status == "" || c.Timeout <= 0 {
			errs = append(errs, fmt.Errorf("unhealthy condition %s=%s needs a type, a status and a positive timeout", c.Type, c.Status))
		}
	}
	return errors.Join(errs...)
}

// healthCheckSelector returns the labels the check matches machines by.
func healthCheckSelector(machineDeployment string, topology bool, controlPlane bool, selector map[string]string) map[string]string {
	switch {
	case machineDeployment != "" && topology:
		return map[string]string{topologyDeploymentLabel: machineDeployment}
	case machineDeployment != "":
		return map[string]string{deploymentNameLabel: machineDeployment}
	case controlPlane:
		return map[string]string{controlPlaneLabel: ""}
	default:
		return selector
	}
}

// controlPlaneLabel marks control-plane machines.
const controlPlaneLabel = "cluster.x-k8s.io/control-plane"

func (in healthCheckInputs) conditionList(conds []unhealthyCondition) []any {
	out := make([]any, 0, len(conds))
	for _, c := range conds {
		m := map[string]any{"type": c.Type, "status": c.Status}
		if in.contractShape() {
			m["timeoutSeconds"] = int64(c.Timeout.Seconds())
		} else {
			m["timeout"] = c.Timeout.String()
		}
		out = append(out, m)
	}
	return out
}

// maxUnhealthyValue renders max_unhealthy as the IntOrString CAPI expects.
func (in healthCheckInputs) maxUnhealthyValue() any {
	if strings.HasSuffix(in.MaxUnhealthy, "%") {
		return in.MaxUnhealthy
	}
	var n int64
	for _, ch := range in.MaxUnhealthy {
		n = n*10 + int64(ch-'0')
	}
	return n
}

// renderMachineHealthCheck builds the object in the contract's shape.
func renderMachineHealthCheck(in healthCheckInputs) *unstructured.Unstructured {
	gvk := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: in.CoreVersion, Kind: "MachineHealthCheck"}
	matchLabels := map[string]any{}
	for k, v := range in.MatchLabels {
		matchLabels[k] = v
	}
	spec := map[string]any{
		"clusterName": in.Cluster,
		"selector":    map[string]any{"matchLabels": matchLabels},
	}
	nodeConds := in.NodeConditions
	if len(nodeConds) == 0 {
		nodeConds = defaultUnhealthyNodeConditions
	}
	if in.contractShape() {
		checks := map[string]any{"unhealthyNodeConditions": in.conditionList(nodeConds)}
		if len(in.MachineConditions) > 0 {
			checks["unhealthyMachineConditions"] = in.conditionList(in.MachineConditions)
		}
		if in.NodeStartupTimeout != nil {
			checks["nodeStartupTimeoutSeconds"] = int64(in.NodeStartupTimeout.Seconds())
		}
		spec["checks"] = checks
		remediation := map[string]any{}
		if in.MaxUnhealthy != "" {
			remediation["triggerIf"] = map[string]any{"unhealthyLessThanOrEqualTo": in.maxUnhealthyValue()}
		} else if in.UnhealthyRange != "" {
			remediation["triggerIf"] = map[string]any{"unhealthyInRange": in.UnhealthyRange}
		}
		if t := in.RemediationTemplate; t != nil {
			remediation["templateRef"] = map[string]any{"apiVersion": t.APIVersion, "kind": t.Kind, "name": t.Name}
		}
		if len(remediation) > 0 {
			spec["remediation"] = remediation
		}
	} else {
		spec["unhealthyConditions"] = in.conditionList(nodeConds)
		if len(in.MachineConditions) > 0 {
			spec["unhealthyMachineConditions"] = in.conditionList(in.MachineConditions)
		}
		if in.NodeStartupTimeout != nil {
			spec["nodeStartupTimeout"] = in.NodeStartupTimeout.String()
		}
		if in.MaxUnhealthy != "" {
			spec["maxUnhealthy"] = in.maxUnhealthyValue()
		}
		if in.UnhealthyRange != "" {
			spec["unhealthyRange"] = in.UnhealthyRange
		}
		if t := in.RemediationTemplate; t != nil {
			spec["remediationTemplate"] = map[string]any{"apiVersion": t.APIVersion, "kind": t.Kind, "name": t.Name, "namespace": in.Namespace}
		}
	}
	obj := newObject(gvk, in.Namespace, in.Name, map[string]string{clusterNameLabel: in.Cluster}, nil)
	obj.Object["spec"] = spec
	return obj
}

// readLiveHealthCheck reads the configurable values back from either shape.
func readLiveHealthCheck(obj *unstructured.Unstructured) healthCheckInputs {
	in := healthCheckInputs{CoreVersion: obj.GroupVersionKind().Version, Namespace: obj.GetNamespace(), Name: obj.GetName()}
	in.Cluster, _, _ = unstructured.NestedString(obj.Object, "spec", "clusterName")
	in.MatchLabels, _, _ = unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
	conds := func(path ...string) []unhealthyCondition {
		raw, _, _ := unstructured.NestedSlice(obj.Object, path...)
		var out []unhealthyCondition
		for _, c := range raw {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			uc := unhealthyCondition{}
			uc.Type, _, _ = unstructured.NestedString(m, "type")
			uc.Status, _, _ = unstructured.NestedString(m, "status")
			if secs, ok, _ := unstructured.NestedInt64(m, "timeoutSeconds"); ok {
				uc.Timeout = time.Duration(secs) * time.Second
			} else if s, _, _ := unstructured.NestedString(m, "timeout"); s != "" {
				uc.Timeout, _ = time.ParseDuration(s)
			}
			out = append(out, uc)
		}
		return out
	}
	intOrString := func(v any) string {
		switch t := v.(type) {
		case string:
			return t
		case int64:
			return fmt.Sprint(t)
		case float64:
			return fmt.Sprint(int64(t))
		}
		return ""
	}
	if _, ok, _ := unstructured.NestedMap(obj.Object, "spec", "checks"); ok || in.contractShape() {
		in.NodeConditions = conds("spec", "checks", "unhealthyNodeConditions")
		in.MachineConditions = conds("spec", "checks", "unhealthyMachineConditions")
		if secs, ok, _ := unstructured.NestedInt64(obj.Object, "spec", "checks", "nodeStartupTimeoutSeconds"); ok {
			d := time.Duration(secs) * time.Second
			in.NodeStartupTimeout = &d
		}
		if v, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "remediation", "triggerIf", "unhealthyLessThanOrEqualTo"); ok {
			in.MaxUnhealthy = intOrString(v)
		}
		in.UnhealthyRange, _, _ = unstructured.NestedString(obj.Object, "spec", "remediation", "triggerIf", "unhealthyInRange")
		if ref, ok, _ := unstructured.NestedStringMap(obj.Object, "spec", "remediation", "templateRef"); ok && ref["name"] != "" {
			in.RemediationTemplate = &objectRef{APIVersion: ref["apiVersion"], Kind: ref["kind"], Name: ref["name"]}
		}
		return in
	}
	in.NodeConditions = conds("spec", "unhealthyConditions")
	in.MachineConditions = conds("spec", "unhealthyMachineConditions")
	if s, _, _ := unstructured.NestedString(obj.Object, "spec", "nodeStartupTimeout"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			in.NodeStartupTimeout = &d
		}
	}
	if v, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "maxUnhealthy"); ok {
		in.MaxUnhealthy = intOrString(v)
	}
	in.UnhealthyRange, _, _ = unstructured.NestedString(obj.Object, "spec", "unhealthyRange")
	if ref, ok, _ := unstructured.NestedStringMap(obj.Object, "spec", "remediationTemplate"); ok && ref["name"] != "" {
		in.RemediationTemplate = &objectRef{APIVersion: ref["apiVersion"], Kind: ref["kind"], Name: ref["name"], Namespace: ref["namespace"]}
	}
	return in
}

// --- model conversion ---

func (r *MachineHealthCheckResource) inputs(ctx context.Context, data *MachineHealthCheckResourceModel, facts clusterFacts) (healthCheckInputs, diag.Diagnostics) {
	var diags diag.Diagnostics
	in := healthCheckInputs{
		Cluster: data.Cluster.ValueString(), Namespace: data.Namespace.ValueString(), Name: data.Name.ValueString(),
		CoreVersion: facts.CoreVersion,
	}
	selector, d := stringMap(ctx, data.Selector)
	diags.Append(d...)
	chosen := 0
	for _, set := range []bool{data.MachineDeployment.ValueString() != "", data.ControlPlane.ValueBool(), len(selector) > 0} {
		if set {
			chosen++
		}
	}
	if chosen > 1 {
		diags.AddError("Invalid machine health check", "machine_deployment, control_plane and selector are exclusive.")
	}
	in.MatchLabels = healthCheckSelector(data.MachineDeployment.ValueString(), facts.TopologyMode, data.ControlPlane.ValueBool(), selector)
	in.NodeStartupTimeout = durationOrNil(data.NodeStartupTimeout, "node_startup_timeout", &diags)
	in.NodeConditions = unhealthyConditionsFromList(ctx, data.UnhealthyNodeConditions, "unhealthy_node_conditions", &diags)
	in.MachineConditions = unhealthyConditionsFromList(ctx, data.UnhealthyMachineConditions, "unhealthy_machine_conditions", &diags)
	if !data.Remediation.IsNull() && !data.Remediation.IsUnknown() {
		var rm RemediationModel
		diags.Append(data.Remediation.As(ctx, &rm, basetypes.ObjectAsOptions{})...)
		in.MaxUnhealthy = rm.MaxUnhealthy.ValueString()
		in.UnhealthyRange = rm.UnhealthyRange.ValueString()
		if !rm.Template.IsNull() && !rm.Template.IsUnknown() {
			var t RemediationTemplateModel
			diags.Append(rm.Template.As(ctx, &t, basetypes.ObjectAsOptions{})...)
			in.RemediationTemplate = &objectRef{APIVersion: t.APIVersion.ValueString(), Kind: t.Kind.ValueString(), Name: t.Name.ValueString()}
		}
	}
	return in, diags
}

func unhealthyConditionsFromList(ctx context.Context, list types.List, where string, diags *diag.Diagnostics) []unhealthyCondition {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var models []UnhealthyConditionModel
	diags.Append(list.ElementsAs(ctx, &models, false)...)
	out := make([]unhealthyCondition, 0, len(models))
	for i, m := range models {
		c := unhealthyCondition{Type: m.Type.ValueString(), Status: m.Status.ValueString()}
		if d := durationOrNil(m.Timeout, fmt.Sprintf("%s[%d].timeout", where, i), diags); d != nil {
			c.Timeout = *d
		}
		out = append(out, c)
	}
	return out
}

func unhealthyConditionsValue(ctx context.Context, conds []unhealthyCondition, diags *diag.Diagnostics) types.List {
	models := make([]UnhealthyConditionModel, 0, len(conds))
	for _, c := range conds {
		models = append(models, UnhealthyConditionModel{Type: types.StringValue(c.Type), Status: types.StringValue(c.Status), Timeout: types.StringValue(c.Timeout.String())})
	}
	v, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: unhealthyConditionAttrTypes()}, models)
	diags.Append(d...)
	return v
}

// setFromLive writes the configurable attributes from the live object: the
// selector is folded back into machine_deployment, control_plane or
// selector, the conditions and remediation into their blocks.
func (r *MachineHealthCheckResource) setFromLive(ctx context.Context, data *MachineHealthCheckResourceModel, live healthCheckInputs) diag.Diagnostics {
	var diags diag.Diagnostics
	data.MachineDeployment, data.ControlPlane, data.Selector = types.StringNull(), types.BoolNull(), types.MapNull(types.StringType)
	switch {
	case len(live.MatchLabels) == 1 && live.MatchLabels[topologyDeploymentLabel] != "":
		data.MachineDeployment = types.StringValue(live.MatchLabels[topologyDeploymentLabel])
	case len(live.MatchLabels) == 1 && live.MatchLabels[deploymentNameLabel] != "":
		data.MachineDeployment = types.StringValue(live.MatchLabels[deploymentNameLabel])
	case len(live.MatchLabels) == 1 && hasKey(live.MatchLabels, controlPlaneLabel):
		data.ControlPlane = types.BoolValue(true)
	default:
		m, d := types.MapValueFrom(ctx, types.StringType, live.MatchLabels)
		diags.Append(d...)
		data.Selector = m
	}
	data.NodeStartupTimeout = types.StringNull()
	if live.NodeStartupTimeout != nil {
		data.NodeStartupTimeout = types.StringValue(live.NodeStartupTimeout.String())
	}
	data.UnhealthyNodeConditions = unhealthyConditionsValue(ctx, live.NodeConditions, &diags)
	data.UnhealthyMachineConditions = types.ListNull(types.ObjectType{AttrTypes: unhealthyConditionAttrTypes()})
	if len(live.MachineConditions) > 0 {
		data.UnhealthyMachineConditions = unhealthyConditionsValue(ctx, live.MachineConditions, &diags)
	}
	data.Remediation = types.ObjectNull(remediationAttrTypes())
	if live.MaxUnhealthy != "" || live.UnhealthyRange != "" || live.RemediationTemplate != nil {
		rm := RemediationModel{MaxUnhealthy: stringOrNull(live.MaxUnhealthy), UnhealthyRange: stringOrNull(live.UnhealthyRange), Template: types.ObjectNull(remediationTemplateAttrTypes())}
		if t := live.RemediationTemplate; t != nil {
			tv, d := types.ObjectValueFrom(ctx, remediationTemplateAttrTypes(), RemediationTemplateModel{
				APIVersion: types.StringValue(t.APIVersion), Kind: types.StringValue(t.Kind), Name: types.StringValue(t.Name),
			})
			diags.Append(d...)
			rm.Template = tv
		}
		rv, d := types.ObjectValueFrom(ctx, remediationAttrTypes(), rm)
		diags.Append(d...)
		data.Remediation = rv
	}
	return diags
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

// fill writes the computed attributes from the live object.
func (r *MachineHealthCheckResource) fill(ctx context.Context, data *MachineHealthCheckResourceModel, obj *unstructured.Unstructured) diag.Diagnostics {
	var diags diag.Diagnostics
	expected, _, _ := unstructured.NestedInt64(obj.Object, "status", "expectedMachines")
	healthy, _, _ := unstructured.NestedInt64(obj.Object, "status", "currentHealthy")
	allowed, _, _ := unstructured.NestedInt64(obj.Object, "status", "remediationsAllowed")
	targets, _, _ := unstructured.NestedStringSlice(obj.Object, "status", "targets")
	sort.Strings(targets)
	targetList, d := types.ListValueFrom(ctx, types.StringType, targets)
	diags.Append(d...)
	statusVal, d := types.ObjectValueFrom(ctx, healthCheckStatusAttrTypes(), HealthCheckStatusModel{
		ExpectedMachines: types.Int64Value(expected), CurrentHealthy: types.Int64Value(healthy), RemediationsAllowed: types.Int64Value(allowed), Targets: targetList,
	})
	diags.Append(d...)
	data.Status = statusVal
	st := readNodeGroupStatus(obj)
	conds := make([]ConditionModel, 0, len(st.Conditions))
	for _, c := range st.Conditions {
		conds = append(conds, ConditionModel{Type: types.StringValue(c.Type), Status: types.StringValue(c.Status), Reason: stringOrNull(c.Reason), Message: stringOrNull(c.Message)})
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: conditionAttrTypes()}, conds)
	diags.Append(d...)
	data.Conditions = list
	return diags
}

// --- CRUD ---

func (r *MachineHealthCheckResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data MachineHealthCheckResourceModel
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

func (r *MachineHealthCheckResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data MachineHealthCheckResourceModel
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

func (r *MachineHealthCheckResource) apply(ctx context.Context, data *MachineHealthCheckResourceModel, diags *diag.Diagnostics) {
	client, d := r.pd.objectClient(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	facts, err := r.facts(ctx, client, data.Namespace.ValueString(), data.Cluster.ValueString())
	if err != nil {
		diags.AddError("Reading cluster", err.Error())
		return
	}
	in, d := r.inputs(ctx, data, facts)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	if err := in.validate(); err != nil {
		diags.AddError("Invalid machine health check", err.Error())
		return
	}
	conditions, timeout, d := objectWaitFromModel(ctx, data.Wait, healthCheckWaitDefaults)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	obj := renderMachineHealthCheck(in)
	data.Manifest = types.StringValue(manifestYAML(obj))
	live, d := applyAndWait(ctx, client, obj, false, conditions, timeout)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	data.ID = types.StringValue(fmt.Sprintf("%s/%s", in.Namespace, in.Name))
	// The defaults the render filled in are state too.
	if len(in.NodeConditions) == 0 {
		data.UnhealthyNodeConditions = unhealthyConditionsValue(ctx, defaultUnhealthyNodeConditions, diags)
	}
	if data.UnhealthyMachineConditions.IsUnknown() {
		data.UnhealthyMachineConditions = types.ListNull(types.ObjectType{AttrTypes: unhealthyConditionAttrTypes()})
	}
	diags.Append(r.fill(ctx, data, live)...)
}

// facts reads the Cluster the check belongs to.
func (r *MachineHealthCheckResource) facts(ctx context.Context, client *capi.ObjectClient, namespace, cluster string) (clusterFacts, error) {
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return clusterFacts{}, err
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		return clusterFacts{}, err
	}
	obj, err := client.Get(ctx, k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: "Cluster"}, namespace, cluster)
	if err != nil {
		return clusterFacts{}, fmt.Errorf("reading Cluster %s/%s: %w", namespace, cluster, err)
	}
	return readClusterFacts(obj, nil), nil
}

func (r *MachineHealthCheckResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data MachineHealthCheckResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	catalog, err := client.Catalog(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Reading management cluster", err.Error())
		return
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		resp.Diagnostics.AddError("Reading management cluster", err.Error())
		return
	}
	gvk := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: "MachineHealthCheck"}
	obj, found, d := readLive(ctx, client, gvk, data.Namespace.ValueString(), data.Name.ValueString())
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	live := readLiveHealthCheck(obj)
	if live.Cluster != "" {
		data.Cluster = types.StringValue(live.Cluster)
	}
	resp.Diagnostics.Append(r.setFromLive(ctx, &data, live)...)
	resp.Diagnostics.Append(r.fill(ctx, &data, obj)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *MachineHealthCheckResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data MachineHealthCheckResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	_, timeout, d := objectWaitFromModel(ctx, data.Wait, healthCheckWaitDefaults)
	resp.Diagnostics.Append(d...)
	catalog, err := client.Catalog(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Reading management cluster", err.Error())
		return
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		resp.Diagnostics.AddError("Reading management cluster", err.Error())
		return
	}
	gvk := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: "MachineHealthCheck"}
	resp.Diagnostics.Append(deleteAndWait(ctx, client, gvk, data.Namespace.ValueString(), data.Name.ValueString(), timeout)...)
}

// ImportState takes `namespace/name`.
func (r *MachineHealthCheckResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
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
	catalog, err := client.Catalog(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Reading management cluster", err.Error())
		return
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		resp.Diagnostics.AddError("Reading management cluster", err.Error())
		return
	}
	gvk := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: "MachineHealthCheck"}
	obj, err := client.Get(ctx, gvk, parts[0], parts[1])
	if err != nil {
		resp.Diagnostics.AddError("Machine health check not found", err.Error())
		return
	}
	live := readLiveHealthCheck(obj)
	data := MachineHealthCheckResourceModel{
		ID: types.StringValue(req.ID), Cluster: types.StringValue(live.Cluster), Namespace: types.StringValue(parts[0]), Name: types.StringValue(parts[1]),
		Wait: types.ObjectNull(objectWaitAttrTypes()), Manifest: types.StringNull(),
	}
	resp.Diagnostics.Append(r.setFromLive(ctx, &data, live)...)
	resp.Diagnostics.Append(r.fill(ctx, &data, obj)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
