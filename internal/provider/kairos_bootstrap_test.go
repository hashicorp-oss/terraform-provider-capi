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
		Type: types.StringValue("kairos"), Machine: types.StringValue("nuc"), OnFailure: types.StringNull(),
		Boot: types.ObjectNull(bootAttrTypes()), Kairos: types.ObjectNull(kairosAttrTypes()),
		Addons: types.ObjectNull(bootstrapAddonsAttrTypes()),
	}
	if mutate != nil {
		mutate(&bs, &k, &s)
	}
	if withKairos {
		k.SSH = mustObject(t, ctx, kairosSSHAttrTypes(), s)
		bs.Kairos = mustObject(t, ctx, kairosAttrTypes(), k)
	}
	return tinkerbellTestData(ctx, t, mustObject(t, ctx, managementBootstrapAttrTypes(), bs), kairosMachines)
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
	boot, _ := bs.Attributes["boot"].(schema.SingleNestedAttribute)
	if method, _ := boot.Attributes["method"].(schema.StringAttribute); !strings.Contains(method.MarkdownDescription, "`manual`") {
		t.Error("boot.method must document the manual method")
	}
}

func TestBuildKairosBootstrapConfig(t *testing.T) {
	ctx := context.Background()
	data := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, k *KairosModel, s *KairosSSHModel) {
		bs.Boot = mustObject(t, ctx, bootAttrTypes(), BootModel{
			Method: types.StringValue("virtual_media"), Timeout: types.StringValue("10m"), Attempts: types.Int64Value(2), Device: types.StringValue("usb"),
		})
		bs.Addons = mustObject(t, ctx, bootstrapAddonsAttrTypes(), testAddonsModel(ctx, t, nil,
			[]HelmReleaseModel{helmRel("tinkerbell", "tink", "oci://x/tinkerbell", "5m", "")}, nil))
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
	if len(cfg.Addons.Helm) != 1 || cfg.Addons.Helm[0].Name != "tinkerbell" {
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
	kind := tinkerbellTestData(ctx, t, buildTestBootstrap(ctx, t, "kind"), nil)
	if cfg, diags := buildKairosBootstrapConfig(ctx, kind); cfg != nil || diags.HasError() {
		t.Errorf("a kind bootstrap must yield no kairos config: %+v %v", cfg, diags)
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

	badBoot := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) {
		bs.Boot = mustObject(t, ctx, bootAttrTypes(), BootModel{Method: types.StringValue("pxe"), Timeout: types.StringValue("soon"), Attempts: types.Int64Value(0), Device: types.StringNull()})
	})
	diags := runValidate(t, badBoot)
	assertErrorContains(t, diags, "boot.method")
	assertErrorContains(t, diags, "boot.timeout")
	assertErrorContains(t, diags, "boot.attempts")

	noMachine := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) { bs.Machine = types.StringNull() })
	assertErrorContains(t, runValidate(t, noMachine), `machine is required when type = "kairos"`)

	unknownMachine := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) {
		bs.Machine = types.StringValue("cp-9")
	})
	assertErrorContains(t, runValidate(t, unknownMachine), "does not match any inventory.machine")

	notSelfManaged := kairosData(ctx, t, true, nil)
	mgmt, _ := extractManagement(ctx, notSelfManaged)
	mgmt.SelfManaged = types.BoolValue(false)
	notSelfManaged.Management = mustObject(t, ctx, managementAttrTypes(), *mgmt)
	assertErrorContains(t, runValidate(t, notSelfManaged), "requires management.self_managed = true")
}

func TestValidateManagementBootstrap_ManualBootNeedsNoBMC(t *testing.T) {
	ctx := context.Background()
	noBMC := []testMachine{
		{hostname: "nuc", ip: "10.0.0.50", mac: "aa:bb:cc:dd:ee:50", role: "worker"},
		{hostname: "cp-1", ip: "10.0.0.5", mac: "aa:bb:cc:dd:ee:01", role: "cp", disk: "/dev/sda", bmc: "10.0.1.5"},
	}
	boot := func(method string) types.Object {
		return mustObject(t, ctx, bootAttrTypes(), BootModel{Method: types.StringValue(method), Timeout: types.StringNull(), Attempts: types.Int64Null(), Device: types.StringNull()})
	}
	bs := func(method string) types.Object {
		return mustObject(t, ctx, managementBootstrapAttrTypes(), ManagementBootstrapModel{
			Type: types.StringValue("kairos"), Machine: types.StringValue("nuc"), Boot: boot(method),
			Kairos: mustObject(t, ctx, kairosAttrTypes(), KairosModel{
				ISO: types.StringValue("http://x/k.iso"), Address: types.StringNull(),
				SSH: mustObject(t, ctx, kairosSSHAttrTypes(), KairosSSHModel{User: types.StringValue("kairos"), Port: types.Int64Value(22), PrivateKey: types.StringValue("k"), HostKey: types.StringNull()}),
			}),
			Addons: types.ObjectNull(bootstrapAddonsAttrTypes()),
		})
	}
	assertErrorContains(t, runValidate(t, tinkerbellTestData(ctx, t, bs("virtual_media"), noBMC)), "must define bmc")
	if diags := runValidate(t, tinkerbellTestData(ctx, t, bs("manual"), noBMC)); diags.HasError() {
		t.Errorf("a manual boot needs no BMC on the machine: %v", diags)
	}
	cfg, diags := buildKairosBootstrapConfig(ctx, tinkerbellTestData(ctx, t, bs("manual"), noBMC))
	if diags.HasError() || cfg.Boot.Method != bmc.BootMethodManual || cfg.Machine.BMC.Address != "" {
		t.Errorf("manual config: %+v %v", cfg, diags)
	}
}

func TestBuildCreateOptions_KairosPivot(t *testing.T) {
	ctx := context.Background()
	data := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) {
		bs.Addons = mustObject(t, ctx, bootstrapAddonsAttrTypes(), testAddonsModel(ctx, t, nil,
			[]HelmReleaseModel{helmRel("tinkerbell", "tink", "oci://x/tinkerbell", "", "")}, nil))
	})
	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if !opts.SelfManaged || opts.BootstrapMachine != "nuc" {
		t.Errorf("kairos pivots: SelfManaged=%v BootstrapMachine=%q", opts.SelfManaged, opts.BootstrapMachine)
	}
	if len(opts.Inventory) != 2 {
		t.Errorf("the disposable node stays in the inventory (excluded until the pivot by the manager): %d machines", len(opts.Inventory))
	}
	if len(opts.BootstrapAddons.Helm) != 1 || opts.BootstrapAddons.Helm[0].Name != "tinkerbell" {
		t.Errorf("management.bootstrap.addons is the bootstrap cluster's stack: %+v", opts.BootstrapAddons)
	}
}

func TestManagerFor_Kairos(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}
	mgr, diags := r.managerFor(ctx, kairosData(ctx, t, true, nil))
	if diags.HasError() || mgr == nil {
		t.Fatalf("manager = %v, diags = %v", mgr, diags)
	}
	kind := tinkerbellTestData(ctx, t, buildTestBootstrap(ctx, t, "kind"), nil)
	if mgr, diags := r.managerFor(ctx, kind); diags.HasError() || mgr != nil {
		t.Errorf("a kind bootstrap keeps the resource's default manager: %v %v", mgr, diags)
	}
}

func TestUpgradeInventoryAndAddonsJSON_AddsKairos(t *testing.T) {
	raw := []byte(`{"name":"x","management":{"bootstrap":{"type":"kind","addons":null}}}`)
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
	if strings.Contains(string(state["management"]), `"addons":null,"bootstrap"`) || strings.Contains(string(state["management"]), `"management":{"addons"`) {
		t.Errorf("management.addons is gone and must not be added back: %s", state["management"])
	}
}

func TestUpgradeV3JSON(t *testing.T) {
	raw := []byte(`{"name":"prod","cni":{"provider":"cilium"},"management":{"kubeconfig":null,"addons":{"helm":[]},"bootstrap":{"type":"kairos","mode":"pivot","state_dir":null,"talos":null,"kairos":{"iso":"x"},"addons":null}}}`)
	out, err := upgradeV3JSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["cni"]; ok {
		t.Error("cni must be dropped")
	}
	mgmt := string(got["management"])
	for _, gone := range []string{`"addons":{"helm"`, `"mode"`, `"state_dir"`, `"talos"`} {
		if strings.Contains(mgmt, gone) {
			t.Errorf("%s must be dropped from management: %s", gone, mgmt)
		}
	}
	if !strings.Contains(mgmt, `"kairos":{"iso":"x"}`) || !strings.Contains(mgmt, `"type":"kairos"`) {
		t.Errorf("the kairos bootstrap survives: %s", mgmt)
	}

	for _, legacy := range []string{
		`{"name":"bm","management":{"bootstrap":{"type":"talos"}}}`,
		`{"name":"bm","management":{"bootstrap":{"type":"kairos","mode":"in_place"}}}`,
	} {
		if _, err := upgradeV3JSON([]byte(legacy)); err == nil || !strings.Contains(err.Error(), "no longer supports") {
			t.Errorf("a Talos or in-place cluster cannot be upgraded, got %v", err)
		}
	}
	if _, err := upgradeV3JSON([]byte(`{"name":"x","management":null}`)); err != nil {
		t.Errorf("no management block is fine: %v", err)
	}
}
