// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"os"
	"path/filepath"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// Ensure provider defined types fully satisfy framework interfaces.
var (
	_ action.Action              = &moveAction{}
	_ action.ActionWithConfigure = &moveAction{}
)

type moveAction struct {
	actionBase
	mover capi.Mover
}

func NewMoveAction() action.Action { return &moveAction{} }

type moveActionModel struct {
	Namespace      types.String `tfsdk:"namespace"`
	FromKubeconfig types.String `tfsdk:"from_kubeconfig"`
	ToKubeconfig   types.String `tfsdk:"to_kubeconfig"`
}

func (a *moveAction) Metadata(
	_ context.Context,
	req action.MetadataRequest,
	resp *action.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_move"
}

func (a *moveAction) Schema(
	_ context.Context,
	_ action.SchemaRequest,
	resp *action.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "`clusterctl move`: carry every CAPI object of a namespace, with what they own (Hardware, rufio Machines, Workflows, HelmChartProxies and the rest), from one management cluster to another whose providers are already installed. Clusters are paused on the source and resumed on the target by clusterctl. The source defaults to the management cluster the provider block names.",
		Attributes: map[string]schema.Attribute{
			"namespace": namespaceAttribute(),
			"from_kubeconfig": schema.StringAttribute{
				MarkdownDescription: "Kubeconfig content of the source management cluster. Default: the provider block's.",
				Optional:            true,
			},
			"to_kubeconfig": schema.StringAttribute{
				MarkdownDescription: "Kubeconfig content of the target management cluster.",
				Required:            true,
			},
		},
	}
}

func (a *moveAction) Invoke(
	ctx context.Context,
	req action.InvokeRequest,
	resp *action.InvokeResponse,
) {
	var data moveActionModel
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
	if err := a.mover.Move(
		ctx,
		from,
		to,
		capi.MoveOptions{Namespace: namespaceOf(data.Namespace)},
	); err != nil {
		resp.Diagnostics.AddError("Moving CAPI objects", err.Error())
	}
}
