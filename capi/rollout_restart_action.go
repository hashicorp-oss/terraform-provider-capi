// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
)

// Ensure provider defined types fully satisfy framework interfaces.
var (
	_ action.Action              = &rolloutRestartAction{}
	_ action.ActionWithConfigure = &rolloutRestartAction{}
)

func rolloutRestartPatch(contract bool, at time.Time) []byte {
	stamp := at.UTC().Format(time.RFC3339)
	if contract {
		return []byte(fmt.Sprintf(`{"spec":{"rollout":{"after":%q}}}`, stamp))
	}
	return []byte(fmt.Sprintf(`{"spec":{"rolloutAfter":%q}}`, stamp))
}

func isContract(coreVersion string) bool {
	return version.CompareKubeAwareVersionStrings(coreVersion, "v1beta2") >= 0
}

type rolloutRestartAction struct {
	actionBase
	now func() time.Time
}

func NewRolloutRestartAction() action.Action { return &rolloutRestartAction{now: time.Now} }

type rolloutRestartActionModel struct {
	Kind       types.String `tfsdk:"kind"`
	APIVersion types.String `tfsdk:"api_version"`
	Name       types.String `tfsdk:"name"`
	Namespace  types.String `tfsdk:"namespace"`
}

func (a *rolloutRestartAction) Metadata(
	_ context.Context,
	req action.MetadataRequest,
	resp *action.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_rollout_restart"
}

func (a *rolloutRestartAction) Schema(
	_ context.Context,
	_ action.SchemaRequest,
	resp *action.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Roll a MachineDeployment or a control plane out again without a spec change, the way `clusterctl alpha rollout restart` does: `spec.rollout.after` (v1beta2 contract) or `spec.rolloutAfter` is set to now, and CAPI replaces every machine.",
		Attributes: map[string]schema.Attribute{
			"kind": schema.StringAttribute{
				MarkdownDescription: "`MachineDeployment` (default) or a control-plane kind that supports a rollout time (`KubeadmControlPlane`).",
				Optional:            true,
			},
			"api_version": schema.StringAttribute{
				MarkdownDescription: "`group/version` of `kind`. Resolved from the installed CRDs when unset.",
				Optional:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Object name (a `capi_machine_deployment`'s `object_name`).",
				Required:            true,
			},
			"namespace": namespaceAttribute(),
		},
	}
}

func (a *rolloutRestartAction) Invoke(
	ctx context.Context,
	req action.InvokeRequest,
	resp *action.InvokeResponse,
) {
	var data rolloutRestartActionModel
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
	progress(
		resp,
		"Restarting rollout of %s %s/%s",
		kind,
		namespaceOf(data.Namespace),
		data.Name.ValueString(),
	)
	if _, err := client.Patch(
		ctx,
		gvk,
		namespaceOf(data.Namespace),
		data.Name.ValueString(),
		k8stypes.MergePatchType,
		rolloutRestartPatch(isContract(core), a.now()),
	); err != nil {
		resp.Diagnostics.AddError("Restarting rollout", err.Error())
	}
}
