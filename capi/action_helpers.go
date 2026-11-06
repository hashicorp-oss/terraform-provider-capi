// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
)

// remediateMachineAnnotation asks the MachineHealthCheck controller to
// remediate a Machine first.

// actionBase carries the provider connection.

type actionBase struct {
	pd *providerData
}

func (a *actionBase) Configure(
	_ context.Context,
	req action.ConfigureRequest,
	resp *action.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Action Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData),
		)
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

func (a *actionBase) coreGVK(
	ctx context.Context,
	client *capi.ObjectClient,
	kind string,
) (k8sschema.GroupVersionKind, error) {
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
	return schema.StringAttribute{
		MarkdownDescription: "Namespace. Default `default`.",
		Optional:            true,
	}
}

func namespaceOf(s types.String) string {
	if s.IsNull() || s.IsUnknown() || s.ValueString() == "" {
		return "default"
	}
	return s.ValueString()
}

// pausePatch sets or clears Cluster.spec.paused.

// rolloutRestartPatch asks for a rollout after now: `spec.rollout.after` on
// the v1beta2 contract, `spec.rolloutAfter` before it.

// remediatePatch marks a Machine for remediation.

// isContract reports whether a core API version is on the v1beta2 contract.

// pauseAction pauses or resumes a Cluster's reconciliation.

// NewPauseAction returns the capi_pause action.

// NewResumeAction returns the capi_resume action.

// pauseActionModel is the capi_pause and capi_resume configuration.

// rolloutRestartAction triggers a rollout of a MachineDeployment or a
// control plane without a spec change.

// NewRolloutRestartAction returns the capi_rollout_restart action.

// rolloutRestartActionModel is the capi_rollout_restart configuration.

// remediateMachineAction marks a Machine for remediation.

// NewRemediateMachineAction returns the capi_remediate_machine action.

// remediateMachineActionModel is the capi_remediate_machine configuration.

// moveAction moves CAPI objects between management clusters.

// NewMoveAction returns the capi_move action.

// moveActionModel is the capi_move configuration.
