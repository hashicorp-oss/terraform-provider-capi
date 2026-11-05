// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// providerChanges compares the provider maps of the prior state and the plan
// and reports what the management cluster must do about it: upgrade (an
// entry present in both whose settings other than resource_config differ),
// removed (entries dropped from the ipam and addon maps; a dropped core
// entry only means "stop pinning", and a dropped infrastructure, bootstrap
// or control-plane entry replaces the cluster), and added (any new entry,
// which clusterctl init installs).
func providerChanges(ctx context.Context, state, plan *ClusterResourceModel) (upgrade []capi.ProviderKey, removed capi.ProviderSet, added bool, diags diag.Diagnostics) {
	removed = capi.ProviderSet{}
	stateMaps := state.providerMaps(ctx)
	for i, pm := range plan.providerMaps(ctx) {
		before, d := extractProviders(ctx, stateMaps[i].Map)
		diags.Append(d...)
		after, d := extractProviders(ctx, pm.Map)
		diags.Append(d...)
		stripped := map[string]types.Object{}
		for name, el := range withoutResourceConfig(ctx, pm.Map).Elements() {
			if obj, ok := el.(types.Object); ok {
				stripped[name] = obj
			}
		}
		strippedBefore := map[string]types.Object{}
		for name, el := range withoutResourceConfig(ctx, stateMaps[i].Map).Elements() {
			if obj, ok := el.(types.Object); ok {
				strippedBefore[name] = obj
			}
		}
		for _, name := range sortedProviderNames(after) {
			prev, existed := strippedBefore[name]
			if !existed {
				added = true
				continue
			}
			if !prev.Equal(stripped[name]) {
				upgrade = append(upgrade, capi.ProviderKey{Type: pm.Type, Name: name})
			}
		}
		if pm.Type != capi.ProviderTypeIPAM && pm.Type != capi.ProviderTypeAddon {
			continue
		}
		for _, name := range sortedProviderNames(before) {
			if _, kept := after[name]; !kept {
				removed.Add(capi.ProviderConfig{Name: name, Type: pm.Type})
			}
		}
	}
	return upgrade, removed, added, diags
}

// parseImportID splits an import ID of the form [kubeconfig:][namespace/]name.
// The kubeconfig path ends at the last colon, so a path may contain colons;
// a Windows drive letter cannot be told apart from one and is not supported.
func parseImportID(id string) (kubeconfig, namespace, name string) {
	rest := strings.TrimSpace(id)
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		kubeconfig, rest = strings.TrimSpace(rest[:i]), rest[i+1:]
	}
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		namespace, rest = strings.TrimSpace(rest[:i]), rest[i+1:]
	}
	return kubeconfig, namespace, strings.TrimSpace(rest)
}
