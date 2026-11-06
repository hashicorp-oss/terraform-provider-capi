// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

// Ensure provider defined types fully satisfy framework interfaces.
var (
	_ action.Action              = &remediateMachineAction{}
	_ action.ActionWithConfigure = &remediateMachineAction{}
)

const remediateMachineAnnotation = "cluster.x-k8s.io/remediate-machine"

func remediatePatch() []byte {
	b, _ := json.Marshal(
		map[string]any{
			"metadata": map[string]any{
				"annotations": map[string]any{remediateMachineAnnotation: ""},
			},
		},
	)
	return b
}

type remediateMachineAction struct{ actionBase }

func NewRemediateMachineAction() action.Action { return &remediateMachineAction{} }

type remediateMachineActionModel struct {
	Machine   types.String `tfsdk:"machine"`
	Namespace types.String `tfsdk:"namespace"`
}

func (a *remediateMachineAction) Metadata(
	_ context.Context,
	req action.MetadataRequest,
	resp *action.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_remediate_machine"
}

func (a *remediateMachineAction) Schema(
	_ context.Context,
	_ action.SchemaRequest,
	resp *action.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Mark a Machine for remediation (the `cluster.x-k8s.io/remediate-machine` annotation): the MachineHealthCheck that covers it treats it as unhealthy and its owner replaces it, subject to the check's remediation limits.",
		Attributes: map[string]schema.Attribute{
			"machine": schema.StringAttribute{
				MarkdownDescription: "Machine name.",
				Required:            true,
			},
			"namespace": namespaceAttribute(),
		},
	}
}

func (a *remediateMachineAction) Invoke(
	ctx context.Context,
	req action.InvokeRequest,
	resp *action.InvokeResponse,
) {
	var data remediateMachineActionModel
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
	progress(
		resp,
		"Marking machine %s/%s for remediation",
		namespaceOf(data.Namespace),
		data.Machine.ValueString(),
	)
	if _, err := client.Patch(
		ctx,
		gvk,
		namespaceOf(data.Namespace),
		data.Machine.ValueString(),
		k8stypes.MergePatchType,
		remediatePatch(),
	); err != nil {
		resp.Diagnostics.AddError("Marking machine for remediation", err.Error())
	}
}
