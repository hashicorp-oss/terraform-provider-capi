// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// withManagementAddons sets management.addons on data from an addons model.
func withManagementAddons(ctx context.Context, t *testing.T, data *ClusterResourceModel, addons BootstrapAddonsModel) {
	t.Helper()
	mgmt, d := extractManagement(ctx, data)
	if d.HasError() || mgmt == nil {
		t.Fatalf("management missing: %v", d)
	}
	mgmt.Addons = mustObject(t, ctx, bootstrapAddonsAttrTypes(), addons)
	data.Management = mustObject(t, ctx, managementAttrTypes(), *mgmt)
}

func testAddonsModel(ctx context.Context, t *testing.T, prereqs []string, rels []HelmReleaseModel, manifests []string) BootstrapAddonsModel {
	t.Helper()
	list := func(v []string) types.List {
		if v == nil {
			return types.ListNull(types.StringType)
		}
		l, d := types.ListValueFrom(ctx, types.StringType, v)
		if d.HasError() {
			t.Fatal(d)
		}
		return l
	}
	helm := types.ListNull(types.ObjectType{AttrTypes: helmReleaseAttrTypes()})
	if rels != nil {
		helm = testHelmList(ctx, t, rels)
	}
	return BootstrapAddonsModel{Prerequisites: list(prereqs), Helm: helm, Manifests: list(manifests)}
}

func TestClusterResource_Schema_ManagementAddons(t *testing.T) {
	resp := &resource.SchemaResponse{}
	NewClusterResource().Schema(context.Background(), resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	mgmt, ok := resp.Schema.Attributes["management"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management must be a SingleNestedAttribute")
	}
	addons, ok := mgmt.Attributes["addons"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management.addons must be a SingleNestedAttribute")
	}
	for _, name := range []string{"prerequisites", "helm", "manifests"} {
		if _, ok := addons.Attributes[name]; !ok {
			t.Errorf("management.addons lacks %q", name)
		}
	}
	bs, _ := mgmt.Attributes["bootstrap"].(schema.SingleNestedAttribute)
	if _, ok := bs.Attributes["addons"].(schema.SingleNestedAttribute); !ok {
		t.Error("management.bootstrap.addons must keep its own block")
	}
}

func TestBuildCreateOptions_ManagementAddons(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place", []string{}, "", "", nil)
	withManagementAddons(ctx, t, data, testAddonsModel(ctx, t,
		[]string{"kind: CiliumLoadBalancerIPPool\n"},
		[]HelmReleaseModel{helmRel("tinkerbell", "tinkerbell", "oci://ghcr.io/tinkerbell/charts/tinkerbell", "15m", "a: b\n")},
		[]string{"kind: ConfigMap\n"},
	))

	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	set := opts.ManagementAddons
	if len(set.Prerequisites) != 1 || len(set.Helm) != 1 || len(set.Manifests) != 1 {
		t.Fatalf("management addons = %+v", set)
	}
	if set.Helm[0].Name != "tinkerbell" || set.Helm[0].Timeout.Minutes() != 15 || set.Helm[0].Values != "a: b\n" {
		t.Errorf("helm release = %+v", set.Helm[0])
	}
}

func TestBuildCreateOptions_CNIPatchReachesTheCAPIRender(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place", []string{"machine: {}\n"}, "", "", nil)
	data.CNI = mustObject(t, ctx, cniAttrTypes(), CNIModel{
		Provider: types.StringValue("cilium"), Version: types.StringValue("1.20.1"), Values: types.StringNull(),
		GatewayAPI: types.ObjectNull(gatewayAPIAttrTypes()),
	})
	bsTalos := nullProviderModel()
	bsTalos.ResourceConfig = types.StringValue(`{"talosVersion":"v1.13"}`)
	data.Bootstrap = mustProviderMap(t, ctx, map[string]ProviderModel{"talos": bsTalos})
	mdBootstrap, _ := json.Marshal(map[string]any{"strategicPatches": []string{"worker: {}\n"}})
	mds, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: machineDeploymentAttrTypes()}, []MachineDeploymentModel{{
		Name: types.StringValue("md-0"), Class: types.StringNull(), Replicas: types.Int64Value(1), FailureDomain: types.StringNull(),
		Metadata: types.ObjectNull(topologyMetadataAttrTypes()), Infrastructure: types.StringNull(), Bootstrap: types.StringValue(string(mdBootstrap)),
	}})
	if d.HasError() {
		t.Fatal(d)
	}
	workers := mustObject(t, ctx, workersTopologyAttrTypes(), WorkersTopologyModel{MachineDeployments: mds})
	data.Topology = mustObject(t, ctx, topologyAttrTypes(), TopologyModel{ControlPlane: types.ObjectNull(controlPlaneTopologyAttrTypes()), Workers: workers})

	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	cp := opts.Providers[capi.ProviderTypeControlPlane][0].ResourceConfig
	cpPatches, _ := cp["controlPlaneConfig"].(map[string]any)["controlplane"].(map[string]any)["strategicPatches"].([]any)
	if len(cpPatches) != 2 || cpPatches[0] != ciliumCNIPatch || cpPatches[1] != "machine: {}\n" {
		t.Errorf("control plane patches = %q", cpPatches)
	}
	bs := opts.Providers[capi.ProviderTypeBootstrap][0].ResourceConfig
	bsPatches, _ := bs["strategicPatches"].([]any)
	if len(bsPatches) != 1 || bsPatches[0] != ciliumCNIPatch || bs["talosVersion"] != "v1.13" {
		t.Errorf("bootstrap provider resource_config = %v", bs)
	}
	mdPatches, _ := opts.MachineDeployments[0].Bootstrap["strategicPatches"].([]any)
	if len(mdPatches) != 2 || mdPatches[0] != ciliumCNIPatch {
		t.Errorf("machine deployment bootstrap patches = %q", mdPatches)
	}
	// Idempotent: a second pass does not duplicate the patch.
	injectCNIPatch(opts)
	cpPatches, _ = cp["controlPlaneConfig"].(map[string]any)["controlplane"].(map[string]any)["strategicPatches"].([]any)
	if len(cpPatches) != 2 {
		t.Errorf("CNI patch must not be duplicated: %q", cpPatches)
	}
}

func TestBuildCreateOptions_NoCNIPatchWithoutCNI(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place", []string{"machine: {}\n"}, "", "", nil)
	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	cp := opts.Providers[capi.ProviderTypeControlPlane][0].ResourceConfig
	patches, _ := cp["controlPlaneConfig"].(map[string]any)["controlplane"].(map[string]any)["strategicPatches"].([]any)
	if len(patches) != 1 {
		t.Errorf("no cni block, no injected patch: %q", patches)
	}
}

func TestWorkloadAddonsFor(t *testing.T) {
	ctx := context.Background()
	cni := func(data *ClusterResourceModel) {
		data.CNI = mustObject(t, ctx, cniAttrTypes(), CNIModel{
			Provider: types.StringValue("cilium"), Version: types.StringValue("1.20.1"), Values: types.StringNull(),
			GatewayAPI: mustObject(t, ctx, gatewayAPIAttrTypes(), GatewayAPIModel{Enabled: types.BoolValue(true), Version: types.StringValue("v1.3.0")}),
		})
	}
	pivot := inPlaceData(ctx, t, "pivot", []string{}, "", "", nil)
	cni(pivot)
	set := workloadAddonsFor(ctx, pivot)
	if len(set.Helm) != 1 || set.Helm[0].Name != "cilium" || len(set.PreHelmManifestURLs) != 1 || !strings.Contains(set.PreHelmManifestURLs[0], "v1.3.0") {
		t.Errorf("pivot: the cni release and its CRDs go to the workload cluster, got %+v", set)
	}

	inPlace := inPlaceData(ctx, t, "in_place", []string{}, "", "", nil)
	cni(inPlace)
	if !workloadAddonsFor(ctx, inPlace).Empty() {
		t.Error("in place: the node is the cluster, nothing to install twice")
	}
	if !workloadAddonsFor(ctx, inPlaceData(ctx, t, "pivot", []string{}, "", "", nil)).Empty() {
		t.Error("no cni block, no workload addons")
	}
}

func TestValidateAddonsModel_RejectsBadEntries(t *testing.T) {
	ctx := context.Background()
	data := inPlaceData(ctx, t, "in_place", []string{}, "", "", nil)
	withManagementAddons(ctx, t, data, testAddonsModel(ctx, t,
		[]string{"not: [valid"},
		[]HelmReleaseModel{helmRel("a", "ns", "c", "", ""), helmRel("a", "ns", "c", "", "")},
		nil,
	))
	var diags = runFullValidate(t, data)
	assertErrorContains(t, diags, "management.addons.helm[1] is a duplicate")
	assertErrorContains(t, diags, "management.addons.prerequisites[0] is not valid YAML")
}

func TestUpgradeInventoryAndAddonsJSON_AddsManagementAddons(t *testing.T) {
	raw := []byte(`{"name":"x","management":{"kubeconfig":null,"bootstrap":null}}`)
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if err := upgradeInventoryAndAddonsJSON(state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state["management"]), `"addons":null`) {
		t.Errorf("management.addons must be added as null: %s", state["management"])
	}
}

// runFullValidate runs the resource's lifecycle validation.
func runFullValidate(t *testing.T, data *ClusterResourceModel) (diags diag.Diagnostics) {
	t.Helper()
	r := &ClusterResource{}
	r.validateLifecycleConfig(context.Background(), data, &diags)
	return diags
}
