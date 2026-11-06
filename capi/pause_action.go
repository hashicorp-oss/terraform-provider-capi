// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

// Ensure provider defined types fully satisfy framework interfaces.
var (
	_ action.Action              = &pauseAction{}
	_ action.ActionWithConfigure = &pauseAction{}
)

func pausePatch(paused bool) []byte {
	if paused {
		return []byte(`{"spec":{"paused":true}}`)
	}
	return []byte(`{"spec":{"paused":null}}`)
}

type pauseAction struct {
	actionBase
	resume bool
}

func NewPauseAction() action.Action { return &pauseAction{} }

func NewResumeAction() action.Action { return &pauseAction{resume: true} }

type pauseActionModel struct {
	Cluster   types.String `tfsdk:"cluster"`
	Namespace types.String `tfsdk:"namespace"`
}

func (a *pauseAction) Metadata(
	_ context.Context,
	req action.MetadataRequest,
	resp *action.MetadataResponse,
) {
	if a.resume {
		resp.TypeName = req.ProviderTypeName + "_resume"
		return
	}
	resp.TypeName = req.ProviderTypeName + "_pause"
}

func (a *pauseAction) Schema(
	_ context.Context,
	_ action.SchemaRequest,
	resp *action.SchemaResponse,
) {
	desc := "Pause a Cluster's reconciliation (`Cluster.spec.paused`), the way `clusterctl` pauses before a move: no controller touches the cluster's objects until `capi_resume`."
	if a.resume {
		desc = "Resume a Cluster's reconciliation by clearing `Cluster.spec.paused`."
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: desc,
		Attributes: map[string]schema.Attribute{
			"cluster": schema.StringAttribute{
				MarkdownDescription: "Cluster name.",
				Required:            true,
			},
			"namespace": namespaceAttribute(),
		},
	}
}

func (a *pauseAction) Invoke(
	ctx context.Context,
	req action.InvokeRequest,
	resp *action.InvokeResponse,
) {
	var data pauseActionModel
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
	progress(
		resp,
		"%s cluster %s/%s",
		verb,
		namespaceOf(data.Namespace),
		data.Cluster.ValueString(),
	)
	if _, err := client.Patch(
		ctx,
		gvk,
		namespaceOf(data.Namespace),
		data.Cluster.ValueString(),
		k8stypes.MergePatchType,
		pausePatch(!a.resume),
	); err != nil {
		resp.Diagnostics.AddError(verb+" cluster", err.Error())
	}
}
