// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// inPlaceData builds a Tinkerbell + Talos cluster whose bootstrap node is
// adopted in place. cpPatches become control_plane["talos"].resource_config
// strategic patches; endpointHost, when set, becomes network.control_plane_endpoint.
func inPlaceData(ctx context.Context, t *testing.T, mode string, cpPatches []string, talosEndpoint, endpointHost string, userPatches []string) *ClusterResourceModel {
	t.Helper()
	tm := testTalosModel(ctx, t, nil, userPatches)
	if talosEndpoint != "" {
		tm.Endpoint = types.StringValue(talosEndpoint)
	}
	bsVal, d := types.ObjectValueFrom(ctx, managementBootstrapAttrTypes(), ManagementBootstrapModel{
		Type: types.StringValue("talos"), Mode: types.StringValue(mode), StateDir: types.StringNull(),
		Machine: types.StringValue("cp-1"), Boot: types.ObjectNull(bootAttrTypes()),
		Talos: mustObject(t, ctx, talosAttrTypes(), *tm), Kairos: types.ObjectNull(kairosAttrTypes()), Addons: types.ObjectNull(bootstrapAddonsAttrTypes()),
	})
	if d.HasError() {
		t.Fatal(d)
	}
	data := talosTestData(ctx, t, bsVal, goodMachine)

	if cpPatches != nil {
		rc, err := json.Marshal(map[string]any{"controlPlaneConfig": map[string]any{"controlplane": map[string]any{"strategicPatches": cpPatches}}})
		if err != nil {
			t.Fatal(err)
		}
		cp := nullProviderModel()
		cp.ResourceConfig = types.StringValue(string(rc))
		data.ControlPlane = mustProviderMap(t, ctx, map[string]ProviderModel{"talos": cp})
	}
	if endpointHost != "" {
		ep := mustObject(t, ctx, endpointAttrTypes(), EndpointModel{Host: types.StringValue(endpointHost), Port: types.Int64Null()})
		data.Network = mustObject(t, ctx, clusterNetworkAttrTypes(), ClusterNetworkModel{
			ControlPlaneEndpoint: ep, Pods: types.ObjectNull(networkRangesAttrTypes()),
			Services: types.ObjectNull(networkRangesAttrTypes()), ServiceDomain: types.StringNull(),
		})
	}
	return data
}

func mustObject(t *testing.T, ctx context.Context, attrTypes map[string]attr.Type, v any) types.Object {
	t.Helper()
	obj, d := types.ObjectValueFrom(ctx, attrTypes, v)
	if d.HasError() {
		t.Fatalf("building object: %v", d)
	}
	return obj
}

func TestInPlaceContract_ComposesControlPlanePatchesAndSubstitutesProviderID(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place",
		[]string{"machine:\n  install:\n    disk: /dev/sda\n", "machine:\n  kubelet:\n    extraArgs:\n      provider-id: PROVIDER_ID\n"},
		"", "", []string{"user: patch\n"})

	cfg, diags := buildTalosBootstrapConfig(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	want := []string{
		"machine:\n  install:\n    disk: /dev/sda\n",
		"machine:\n  kubelet:\n    extraArgs:\n      provider-id: tinkerbell://default/cp-1\n",
		"user: patch\n",
	}
	if strings.Join(cfg.Talos.ConfigPatches, "|") != strings.Join(want, "|") {
		t.Errorf("patches = %q\nwant      %q", cfg.Talos.ConfigPatches, want)
	}
	if len(diags.Warnings()) != 1 || !strings.Contains(diags.Warnings()[0].Detail(), "allowSchedulingOnControlPlanes") {
		t.Errorf("expected the scheduling warning, got %v", diags.Warnings())
	}
}

func TestInPlaceContract_AddsProviderIDWhenPatchesLackIt(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place", []string{"cluster:\n  allowSchedulingOnControlPlanes: true\n"}, "", "", nil)
	cfg, diags := buildTalosBootstrapConfig(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if len(cfg.Talos.ConfigPatches) != 2 || !strings.Contains(cfg.Talos.ConfigPatches[1], "provider-id: tinkerbell://default/cp-1") {
		t.Errorf("expected a kubelet provider-id patch appended, got %q", cfg.Talos.ConfigPatches)
	}
	if len(diags.Warnings()) != 0 {
		t.Errorf("patches allow scheduling; no warning expected, got %v", diags.Warnings())
	}
}

func TestInPlaceContract_EndpointDefaultsToClusterEndpoint(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place", []string{}, "", "10.1.1.100", nil)
	cfg, diags := buildTalosBootstrapConfig(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if cfg.Talos.Endpoint != "https://10.1.1.100:6443" {
		t.Errorf("endpoint = %q, want the cluster endpoint", cfg.Talos.Endpoint)
	}
}

func TestInPlaceContract_EndpointConflictIsAnError(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place", []string{}, "https://10.9.9.9:6443", "10.1.1.100", nil)
	_, diags := buildTalosBootstrapConfig(ctx, data)
	assertErrorContains(t, diags, "name different hosts")

	same := inPlaceData(ctx, t, "in_place", []string{}, "https://10.1.1.100:6443", "10.1.1.100", nil)
	if _, diags := buildTalosBootstrapConfig(ctx, same); diags.HasError() {
		t.Errorf("same host must be accepted: %v", diags)
	}
}

func TestInPlaceContract_RequiresTalosControlPlane(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place", nil, "", "", nil)
	_, diags := buildTalosBootstrapConfig(ctx, data)
	assertErrorContains(t, diags, `control_plane["talos"]`)
}

func TestInPlaceContract_PivotModeLeavesPatchesAlone(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "pivot", []string{"machine: {}\n"}, "", "10.1.1.100", []string{"user: patch\n"})
	cfg, diags := buildTalosBootstrapConfig(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if len(cfg.Talos.ConfigPatches) != 1 || cfg.Talos.ConfigPatches[0] != "user: patch\n" || cfg.Talos.Endpoint != "" {
		t.Errorf("pivot must not compose control-plane patches or endpoints: %+v", cfg.Talos)
	}
}

func TestBuildCreateOptions_InventoryAndManifests(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place", []string{}, "", "", nil)
	manifests, d := types.ListValueFrom(ctx, types.StringType, []string{"kind: Secret\n", "   "})
	if d.HasError() {
		t.Fatal(d)
	}
	data.Manifests = mustObject(t, ctx, manifestsAttrTypes(), ManifestsModel{Management: manifests})

	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if !opts.ManageInventory || opts.BootstrapMachine != "cp-1" || !opts.InPlace {
		t.Errorf("inventory wiring: manage=%v bootstrap=%q inPlace=%v", opts.ManageInventory, opts.BootstrapMachine, opts.InPlace)
	}
	if len(opts.Inventory) != 1 || opts.Inventory[0].Hostname != "cp-1" || opts.Inventory[0].IP != "10.0.0.5" ||
		opts.Inventory[0].MAC != "aa:bb:cc:dd:ee:01" || opts.Inventory[0].Disk != "/dev/sda" || opts.Inventory[0].BMC == nil ||
		opts.Inventory[0].BMC.Address != "10.0.1.5" || opts.Inventory[0].Labels["type"] != "cp" {
		t.Errorf("inventory machine = %+v", opts.Inventory)
	}
	if len(opts.ManagementManifests) != 1 || string(opts.ManagementManifests[0]) != "kind: Secret\n" {
		t.Errorf("management manifests = %q (blank entries dropped)", opts.ManagementManifests)
	}
}

func TestValidateManagementBootstrap_InPlaceRequiresTalos(t *testing.T) {
	ctx := context.Background()
	bs, d := types.ObjectValueFrom(ctx, managementBootstrapAttrTypes(), ManagementBootstrapModel{
		Type: types.StringValue("kind"), Mode: types.StringValue("in_place"), StateDir: types.StringNull(), Machine: types.StringNull(),
		Boot: types.ObjectNull(bootAttrTypes()), Talos: types.ObjectNull(talosAttrTypes()), Kairos: types.ObjectNull(kairosAttrTypes()), Addons: types.ObjectNull(bootstrapAddonsAttrTypes()),
	})
	if d.HasError() {
		t.Fatal(d)
	}
	diags := runValidate(t, talosTestData(ctx, t, bs, nil))
	assertErrorContains(t, diags, `requires type = "talos"`)
}

func TestUpgradeInventoryAndAddonsJSON(t *testing.T) {
	raw := []byte(`{"name":"x","inventory":{"source":null,"machine":[]},"management":{"kubeconfig":null,"bootstrap":{"type":"talos","addons":{"helm":[],"manifests":null}}}}`)
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if err := upgradeInventoryAndAddonsJSON(state); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(state)
	for _, want := range []string{`"manage":true`, `"prerequisites":null`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("upgraded state lacks %s:\n%s", want, out)
		}
	}
	// Absent objects stay absent.
	state = map[string]json.RawMessage{"name": json.RawMessage(`"x"`)}
	if err := upgradeInventoryAndAddonsJSON(state); err != nil || len(state) != 1 {
		t.Errorf("nothing to upgrade must be a no-op: %v %v", err, state)
	}
}
