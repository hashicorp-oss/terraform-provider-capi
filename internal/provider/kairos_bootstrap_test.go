// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/bmc"
)

// kairosMachines is an inventory with the disposable node (no disk) and one
// control-plane machine.
var kairosMachines = []testMachine{
	{hostname: "nuc", ip: "10.0.0.50", mac: "aa:bb:cc:dd:ee:50", role: "worker", bmc: "10.0.1.50"},
	{hostname: "cp-1", ip: "10.0.0.5", mac: "aa:bb:cc:dd:ee:01", role: "cp", disk: "/dev/sda", bmc: "10.0.1.5"},
}

// kairosData builds a self-managed Tinkerbell cluster bootstrapped from a
// Kairos live node. mutate runs before the nested objects are built; withKairos
// false leaves the kairos block out.
func kairosData(ctx context.Context, t *testing.T, withKairos bool, mutate func(bs *ManagementBootstrapModel, k *KairosModel, s *KairosSSHModel)) *ClusterResourceModel {
	t.Helper()
	s := KairosSSHModel{User: types.StringValue("kairos"), Port: types.Int64Value(22), PrivateKey: types.StringValue("-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n"), HostKey: types.StringNull()}
	k := KairosModel{ISO: types.StringValue("http://files.example/kairos.iso.custom.iso"), Address: types.StringNull(), SSH: types.ObjectNull(kairosSSHAttrTypes())}
	bs := ManagementBootstrapModel{
		Type: types.StringValue("kairos"), Mode: types.StringValue("pivot"), StateDir: types.StringNull(), Machine: types.StringValue("nuc"),
		Boot: types.ObjectNull(bootAttrTypes()), Talos: types.ObjectNull(talosAttrTypes()), Kairos: types.ObjectNull(kairosAttrTypes()),
		Addons: types.ObjectNull(bootstrapAddonsAttrTypes()),
	}
	if mutate != nil {
		mutate(&bs, &k, &s)
	}
	if withKairos {
		k.SSH = mustObject(t, ctx, kairosSSHAttrTypes(), s)
		bs.Kairos = mustObject(t, ctx, kairosAttrTypes(), k)
	}
	return talosTestData(ctx, t, mustObject(t, ctx, managementBootstrapAttrTypes(), bs), kairosMachines)
}

func withControlPlaneTalos(ctx context.Context, t *testing.T, data *ClusterResourceModel, patches ...string) {
	t.Helper()
	rc, err := json.Marshal(map[string]any{"controlPlaneConfig": map[string]any{"controlplane": map[string]any{"strategicPatches": patches}}})
	if err != nil {
		t.Fatal(err)
	}
	cp := nullProviderModel()
	cp.ResourceConfig = types.StringValue(string(rc))
	data.ControlPlane = mustProviderMap(t, ctx, map[string]ProviderModel{"talos": cp})
}

func withCilium(ctx context.Context, t *testing.T, data *ClusterResourceModel) {
	t.Helper()
	data.CNI = mustObject(t, ctx, cniAttrTypes(), CNIModel{
		Provider: types.StringValue("cilium"), Version: types.StringValue("1.20.1"), Values: types.StringNull(),
		GatewayAPI: types.ObjectNull(gatewayAPIAttrTypes()),
	})
}

func TestClusterResource_Schema_Kairos(t *testing.T) {
	resp := &resource.SchemaResponse{}
	NewClusterResource().Schema(context.Background(), resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	mgmt, _ := resp.Schema.Attributes["management"].(schema.SingleNestedAttribute)
	bs, _ := mgmt.Attributes["bootstrap"].(schema.SingleNestedAttribute)
	k, ok := bs.Attributes["kairos"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management.bootstrap.kairos must be a SingleNestedAttribute")
	}
	if iso, _ := k.Attributes["iso"].(schema.StringAttribute); !iso.Required {
		t.Error("kairos.iso must be required")
	}
	ssh, ok := k.Attributes["ssh"].(schema.SingleNestedAttribute)
	if !ok || !ssh.Required {
		t.Fatal("kairos.ssh must be a required SingleNestedAttribute")
	}
	if pk, _ := ssh.Attributes["private_key"].(schema.StringAttribute); !pk.Required || !pk.Sensitive {
		t.Error("kairos.ssh.private_key must be required and sensitive")
	}
	for _, name := range []string{"user", "port", "host_key"} {
		if _, ok := ssh.Attributes[name]; !ok {
			t.Errorf("kairos.ssh lacks %q", name)
		}
	}
}

func TestBuildKairosBootstrapConfig(t *testing.T) {
	ctx := context.Background()
	data := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, k *KairosModel, s *KairosSSHModel) {
		bs.Boot = mustObject(t, ctx, bootAttrTypes(), BootModel{
			Method: types.StringValue("virtual_media"), Timeout: types.StringValue("10m"), Attempts: types.Int64Value(2), Device: types.StringValue("usb"),
		})
		bs.Addons = mustObject(t, ctx, bootstrapAddonsAttrTypes(), testAddonsModel(ctx, t, nil,
			[]HelmReleaseModel{helmRel("metrics", "kube-system", "oci://x/metrics", "5m", "")}, nil))
		s.Port = types.Int64Value(2222)
		s.HostKey = types.StringValue("ssh-ed25519 AAAA")
	})

	cfg, diags := buildKairosBootstrapConfig(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if cfg.Machine.Hostname != "nuc" || cfg.Machine.Address != "10.0.0.50" || cfg.Machine.BMC.Address != "10.0.1.50" {
		t.Errorf("machine = %+v", cfg.Machine)
	}
	if cfg.ISO != "http://files.example/kairos.iso.custom.iso" {
		t.Errorf("iso = %q", cfg.ISO)
	}
	if cfg.SSH.User != "kairos" || cfg.SSH.Port != 2222 || cfg.SSH.HostKey != "ssh-ed25519 AAAA" || !strings.Contains(cfg.SSH.PrivateKey, "PRIVATE KEY") {
		t.Errorf("ssh = %+v", cfg.SSH)
	}
	if cfg.Boot.Method != bmc.BootMethodVirtualMedia || cfg.Boot.Attempts != 2 || cfg.Boot.Device != bmc.BootDeviceUSB || cfg.Boot.Timeout.Minutes() != 10 {
		t.Errorf("boot = %+v", cfg.Boot)
	}
	if len(cfg.Addons.Helm) != 1 || cfg.Addons.Helm[0].Name != "metrics" {
		t.Errorf("addons = %+v", cfg.Addons)
	}

	// The address override wins over the inventory address.
	data = kairosData(ctx, t, true, func(_ *ManagementBootstrapModel, k *KairosModel, _ *KairosSSHModel) {
		k.Address = types.StringValue("10.0.0.51")
	})
	cfg, diags = buildKairosBootstrapConfig(ctx, data)
	if diags.HasError() || cfg.Machine.Address != "10.0.0.51" {
		t.Errorf("address override: %+v %v", cfg, diags)
	}

	// Not a Kairos bootstrap: nothing to build.
	if cfg, diags := buildKairosBootstrapConfig(ctx, inPlaceData(ctx, t, "pivot", []string{}, "", "", nil)); cfg != nil || diags.HasError() {
		t.Errorf("talos bootstrap must yield no kairos config: %+v %v", cfg, diags)
	}
}

func TestValidateManagementBootstrap_Kairos(t *testing.T) {
	ctx := context.Background()
	if diags := runValidate(t, kairosData(ctx, t, true, nil)); diags.HasError() {
		t.Fatalf("a well-formed kairos bootstrap (no disk on the node) must validate: %v", diags)
	}

	assertErrorContains(t, runValidate(t, kairosData(ctx, t, false, nil)), "management.bootstrap.kairos is required")

	noKey := kairosData(ctx, t, true, func(_ *ManagementBootstrapModel, _ *KairosModel, s *KairosSSHModel) {
		s.PrivateKey = types.StringValue("")
	})
	assertErrorContains(t, runValidate(t, noKey), "ssh.private_key is required")

	badPort := kairosData(ctx, t, true, func(_ *ManagementBootstrapModel, _ *KairosModel, s *KairosSSHModel) { s.Port = types.Int64Value(0) })
	assertErrorContains(t, runValidate(t, badPort), "ssh.port must be between")

	httpBoot := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) {
		bs.Boot = mustObject(t, ctx, bootAttrTypes(), BootModel{Method: types.StringValue("http"), Timeout: types.StringNull(), Attempts: types.Int64Null(), Device: types.StringNull()})
	})
	assertErrorContains(t, runValidate(t, httpBoot), `boot.method "http"`)

	withTalos := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) {
		bs.Talos = mustObject(t, ctx, talosAttrTypes(), *testTalosModel(ctx, t, nil, nil))
	})
	assertErrorContains(t, runValidate(t, withTalos), `management.bootstrap.talos applies to type = "talos"`)

	inPlace := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) {
		bs.Mode = types.StringValue("in_place")
	})
	assertErrorContains(t, runValidate(t, inPlace), `requires type = "talos"`)

	noMachine := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) { bs.Machine = types.StringNull() })
	assertErrorContains(t, runValidate(t, noMachine), `machine is required when type = "kairos"`)

	notSelfManaged := kairosData(ctx, t, true, nil)
	mgmt, _ := extractManagement(ctx, notSelfManaged)
	mgmt.SelfManaged = types.BoolValue(false)
	notSelfManaged.Management = mustObject(t, ctx, managementAttrTypes(), *mgmt)
	assertErrorContains(t, runValidate(t, notSelfManaged), "requires management.self_managed = true")
}

func TestValidateLifecycle_CNIWithKairosNeedsTalosControlPlane(t *testing.T) {
	ctx := context.Background()
	data := kairosData(ctx, t, true, nil)
	withCilium(ctx, t, data)
	assertErrorContains(t, runFullValidate(t, data), `requires a control_plane["talos"] entry`)

	withControlPlaneTalos(ctx, t, data, "machine: {}\n")
	if diags := runFullValidate(t, data); diags.HasError() {
		t.Errorf("cni with kairos and a Talos control plane must validate: %v", diags)
	}

	kind := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "kind", "", nil, nil, nil), nil)
	withCilium(ctx, t, kind)
	assertErrorContains(t, runFullValidate(t, kind), `requires management.bootstrap.type = "talos" or "kairos"`)
}

func TestBuildCreateOptions_KairosPivot(t *testing.T) {
	ctx := context.Background()
	data := kairosData(ctx, t, true, nil)
	withControlPlaneTalos(ctx, t, data, "machine: {}\n")
	withCilium(ctx, t, data)

	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if opts.InPlace || !opts.SelfManaged || opts.BootstrapMachine != "nuc" {
		t.Errorf("kairos pivots: InPlace=%v SelfManaged=%v BootstrapMachine=%q", opts.InPlace, opts.SelfManaged, opts.BootstrapMachine)
	}
	if len(opts.Inventory) != 2 {
		t.Errorf("the disposable node stays in the inventory (excluded until the pivot by the manager): %d machines", len(opts.Inventory))
	}
	cp := opts.Providers[capi.ProviderTypeControlPlane][0].ResourceConfig
	patches, _ := cp["controlPlaneConfig"].(map[string]any)["controlplane"].(map[string]any)["strategicPatches"].([]any)
	if len(patches) != 2 || patches[0] != ciliumCNIPatch {
		t.Errorf("the cni Talos patch must reach the CAPI render with a kairos bootstrap: %q", patches)
	}
	set := workloadAddonsFor(ctx, data)
	if len(set.Helm) != 1 || set.Helm[0].Name != "cilium" {
		t.Errorf("the cni release goes to the workload cluster: %+v", set)
	}
}

func TestManagerFor_Kairos(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}
	mgr, diags := r.managerFor(ctx, kairosData(ctx, t, true, nil))
	if diags.HasError() || mgr == nil {
		t.Fatalf("manager = %v, diags = %v", mgr, diags)
	}
	if mgr, diags := r.managerFor(ctx, talosTestData(ctx, t, buildTestBootstrap(ctx, t, "kind", "", nil, nil, nil), nil)); diags.HasError() || mgr != nil {
		t.Errorf("a kind bootstrap keeps the resource's default manager: %v %v", mgr, diags)
	}
}

func TestUpgradeInventoryAndAddonsJSON_AddsKairos(t *testing.T) {
	raw := []byte(`{"name":"x","management":{"bootstrap":{"type":"talos","addons":null}}}`)
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if err := upgradeInventoryAndAddonsJSON(state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state["management"]), `"kairos":null`) {
		t.Errorf("management.bootstrap.kairos must be added as null: %s", state["management"])
	}
}
