// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// The clusterctl verbs as one-shot, stateless actions: what the module used
// to script around kubectl and clusterctl. Each works on the management
// cluster the provider block names.

// remediateMachineAnnotation asks the MachineHealthCheck controller to
// remediate a Machine first.
const remediateMachineAnnotation = "cluster.x-k8s.io/remediate-machine"

// actionBase carries the provider connection.
type actionBase struct {
	pd *providerData
}

func (a *actionBase) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Action Configure Type", fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData))
		return
	}
	a.pd = pd
}

// progress sends a progress message when Terraform listens.
func progress(resp *action.InvokeResponse, format string, args ...any) {
	if resp.SendProgress != nil {
		resp.SendProgress(action.InvokeProgressEvent{Message: fmt.Sprintf(format, args...)})
	}
}

// coreGVK resolves a core kind at the management cluster's core version.
func (a *actionBase) coreGVK(ctx context.Context, client *capi.ObjectClient, kind string) (k8sschema.GroupVersionKind, error) {
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return k8sschema.GroupVersionKind{}, err
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		return k8sschema.GroupVersionKind{}, err
	}
	return k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: kind}, nil
}

func namespaceAttribute() schema.StringAttribute {
	return schema.StringAttribute{MarkdownDescription: "Namespace. Default `default`.", Optional: true}
}

func namespaceOf(s types.String) string {
	if s.IsNull() || s.IsUnknown() || s.ValueString() == "" {
		return "default"
	}
	return s.ValueString()
}

// --- patches ---

// pausePatch sets or clears Cluster.spec.paused.
func pausePatch(paused bool) []byte {
	if paused {
		return []byte(`{"spec":{"paused":true}}`)
	}
	return []byte(`{"spec":{"paused":null}}`)
}

// rolloutRestartPatch asks for a rollout after now: `spec.rollout.after` on
// the v1beta2 contract, `spec.rolloutAfter` before it.
func rolloutRestartPatch(contract bool, at time.Time) []byte {
	stamp := at.UTC().Format(time.RFC3339)
	if contract {
		return []byte(fmt.Sprintf(`{"spec":{"rollout":{"after":%q}}}`, stamp))
	}
	return []byte(fmt.Sprintf(`{"spec":{"rolloutAfter":%q}}`, stamp))
}

// remediatePatch marks a Machine for remediation.
func remediatePatch() []byte {
	b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{remediateMachineAnnotation: ""}}})
	return b
}

// isContract reports whether a core API version is on the v1beta2 contract.
func isContract(coreVersion string) bool {
	return version.CompareKubeAwareVersionStrings(coreVersion, "v1beta2") >= 0
}

// --- capi_pause / capi_resume ---

// PauseAction pauses or resumes a Cluster's reconciliation.
type PauseAction struct {
	actionBase
	resume bool
}

// NewPauseAction returns the capi_pause action.
func NewPauseAction() action.Action { return &PauseAction{} }

// NewResumeAction returns the capi_resume action.
func NewResumeAction() action.Action { return &PauseAction{resume: true} }

// PauseActionModel is the capi_pause and capi_resume configuration.
type PauseActionModel struct {
	Cluster   types.String `tfsdk:"cluster"`
	Namespace types.String `tfsdk:"namespace"`
}

func (a *PauseAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	if a.resume {
		resp.TypeName = req.ProviderTypeName + "_resume"
		return
	}
	resp.TypeName = req.ProviderTypeName + "_pause"
}

func (a *PauseAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	desc := "Pause a Cluster's reconciliation (`Cluster.spec.paused`), the way `clusterctl` pauses before a move: no controller touches the cluster's objects until `capi_resume`."
	if a.resume {
		desc = "Resume a Cluster's reconciliation by clearing `Cluster.spec.paused`."
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: desc,
		Attributes: map[string]schema.Attribute{
			"cluster":   schema.StringAttribute{MarkdownDescription: "Cluster name.", Required: true},
			"namespace": namespaceAttribute(),
		},
	}
}

func (a *PauseAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var data PauseActionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := a.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	gvk, err := a.coreGVK(ctx, client, "Cluster")
	if err != nil {
		resp.Diagnostics.AddError("Resolving Cluster", err.Error())
		return
	}
	verb := "Pausing"
	if a.resume {
		verb = "Resuming"
	}
	progress(resp, "%s cluster %s/%s", verb, namespaceOf(data.Namespace), data.Cluster.ValueString())
	if _, err := client.Patch(ctx, gvk, namespaceOf(data.Namespace), data.Cluster.ValueString(), k8stypes.MergePatchType, pausePatch(!a.resume)); err != nil {
		resp.Diagnostics.AddError(verb+" cluster", err.Error())
	}
}

// --- capi_rollout_restart ---

// RolloutRestartAction triggers a rollout of a MachineDeployment or a
// control plane without a spec change.
type RolloutRestartAction struct {
	actionBase
	now func() time.Time
}

// NewRolloutRestartAction returns the capi_rollout_restart action.
func NewRolloutRestartAction() action.Action { return &RolloutRestartAction{now: time.Now} }

// RolloutRestartActionModel is the capi_rollout_restart configuration.
type RolloutRestartActionModel struct {
	Kind       types.String `tfsdk:"kind"`
	APIVersion types.String `tfsdk:"api_version"`
	Name       types.String `tfsdk:"name"`
	Namespace  types.String `tfsdk:"namespace"`
}

func (a *RolloutRestartAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_rollout_restart"
}

func (a *RolloutRestartAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Roll a MachineDeployment or a control plane out again without a spec change, the way `clusterctl alpha rollout restart` does: `spec.rollout.after` (v1beta2 contract) or `spec.rolloutAfter` is set to now, and CAPI replaces every machine.",
		Attributes: map[string]schema.Attribute{
			"kind":        schema.StringAttribute{MarkdownDescription: "`MachineDeployment` (default) or a control-plane kind that supports a rollout time (`KubeadmControlPlane`).", Optional: true},
			"api_version": schema.StringAttribute{MarkdownDescription: "`group/version` of `kind`. Resolved from the installed CRDs when unset.", Optional: true},
			"name":        schema.StringAttribute{MarkdownDescription: "Object name (a `capi_machine_deployment`'s `object_name`).", Required: true},
			"namespace":   namespaceAttribute(),
		},
	}
}

func (a *RolloutRestartAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var data RolloutRestartActionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := a.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	kind := data.Kind.ValueString()
	if kind == "" {
		kind = "MachineDeployment"
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
	var gvk k8sschema.GroupVersionKind
	switch {
	case data.APIVersion.ValueString() != "":
		gvk = gvkFor(data.APIVersion.ValueString(), kind)
	case kind == "MachineDeployment":
		gvk = k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: core, Kind: kind}
	default:
		gvk, err = catalog.ByKind(kind)
		if err != nil {
			resp.Diagnostics.AddError("Resolving "+kind, err.Error())
			return
		}
	}
	if a.now == nil {
		a.now = time.Now
	}
	progress(resp, "Restarting rollout of %s %s/%s", kind, namespaceOf(data.Namespace), data.Name.ValueString())
	if _, err := client.Patch(ctx, gvk, namespaceOf(data.Namespace), data.Name.ValueString(), k8stypes.MergePatchType, rolloutRestartPatch(isContract(core), a.now())); err != nil {
		resp.Diagnostics.AddError("Restarting rollout", err.Error())
	}
}

// --- capi_remediate_machine ---

// RemediateMachineAction marks a Machine for remediation.
type RemediateMachineAction struct{ actionBase }

// NewRemediateMachineAction returns the capi_remediate_machine action.
func NewRemediateMachineAction() action.Action { return &RemediateMachineAction{} }

// RemediateMachineActionModel is the capi_remediate_machine configuration.
type RemediateMachineActionModel struct {
	Machine   types.String `tfsdk:"machine"`
	Namespace types.String `tfsdk:"namespace"`
}

func (a *RemediateMachineAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_remediate_machine"
}

func (a *RemediateMachineAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Mark a Machine for remediation (the `cluster.x-k8s.io/remediate-machine` annotation): the MachineHealthCheck that covers it treats it as unhealthy and its owner replaces it, subject to the check's remediation limits.",
		Attributes: map[string]schema.Attribute{
			"machine":   schema.StringAttribute{MarkdownDescription: "Machine name.", Required: true},
			"namespace": namespaceAttribute(),
		},
	}
}

func (a *RemediateMachineAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var data RemediateMachineActionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := a.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	gvk, err := a.coreGVK(ctx, client, "Machine")
	if err != nil {
		resp.Diagnostics.AddError("Resolving Machine", err.Error())
		return
	}
	progress(resp, "Marking machine %s/%s for remediation", namespaceOf(data.Namespace), data.Machine.ValueString())
	if _, err := client.Patch(ctx, gvk, namespaceOf(data.Namespace), data.Machine.ValueString(), k8stypes.MergePatchType, remediatePatch()); err != nil {
		resp.Diagnostics.AddError("Marking machine for remediation", err.Error())
	}
}

// --- capi_move ---

// MoveAction moves CAPI objects between management clusters.
type MoveAction struct {
	actionBase
	mover capi.Mover
}

// NewMoveAction returns the capi_move action.
func NewMoveAction() action.Action { return &MoveAction{} }

// MoveActionModel is the capi_move configuration.
type MoveActionModel struct {
	Namespace      types.String `tfsdk:"namespace"`
	FromKubeconfig types.String `tfsdk:"from_kubeconfig"`
	ToKubeconfig   types.String `tfsdk:"to_kubeconfig"`
}

func (a *MoveAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_move"
}

func (a *MoveAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "`clusterctl move`: carry every CAPI object of a namespace, with what they own (Hardware, rufio Machines, Workflows, HelmChartProxies and the rest), from one management cluster to another whose providers are already installed. Clusters are paused on the source and resumed on the target by clusterctl. The source defaults to the management cluster the provider block names.",
		Attributes: map[string]schema.Attribute{
			"namespace":       namespaceAttribute(),
			"from_kubeconfig": schema.StringAttribute{MarkdownDescription: "Kubeconfig content of the source management cluster. Default: the provider block's.", Optional: true},
			"to_kubeconfig":   schema.StringAttribute{MarkdownDescription: "Kubeconfig content of the target management cluster.", Required: true},
		},
	}
}

func (a *MoveAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var data MoveActionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	dir, err := os.MkdirTemp("", "capi-tf-move-")
	if err != nil {
		resp.Diagnostics.AddError("Preparing move", err.Error())
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	write := func(name, content string) (string, error) {
		p := filepath.Join(dir, name)
		return p, os.WriteFile(p, []byte(content), 0o600)
	}
	from := &capi.Cluster{Name: "source"}
	if data.FromKubeconfig.ValueString() != "" {
		p, err := write("from.kubeconfig", data.FromKubeconfig.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Preparing move", err.Error())
			return
		}
		from.KubeconfigPath = p
	} else {
		conn, d := a.pd.managementOrDiag(ctx)
		resp.Diagnostics.Append(d...)
		if d.HasError() {
			return
		}
		from.KubeconfigPath = conn.KubeconfigPath
	}
	toPath, err := write("to.kubeconfig", data.ToKubeconfig.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Preparing move", err.Error())
		return
	}
	to := &capi.Cluster{Name: "target", KubeconfigPath: toPath}
	if a.mover == nil {
		a.mover = capi.NewClusterctlMover("")
	}
	progress(resp, "Moving CAPI objects of namespace %s", namespaceOf(data.Namespace))
	if err := a.mover.Move(ctx, from, to, capi.MoveOptions{Namespace: namespaceOf(data.Namespace)}); err != nil {
		resp.Diagnostics.AddError("Moving CAPI objects", err.Error())
	}
}
