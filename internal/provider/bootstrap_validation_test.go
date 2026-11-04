// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// --- builders ---

func testTalosModel(ctx context.Context, t *testing.T, image *TalosImageModel, patches []string) *TalosModel {
	t.Helper()
	tm := &TalosModel{
		Version:       types.StringValue("v1.13.6"),
		Architecture:  types.StringValue("amd64"),
		Endpoint:      types.StringNull(),
		Image:         types.ObjectNull(talosImageAttrTypes()),
		ConfigPatches: types.ListNull(types.StringType),
	}
	if image != nil {
		v, d := types.ObjectValueFrom(ctx, talosImageAttrTypes(), *image)
		if d.HasError() {
			t.Fatalf("build image: %v", d)
		}
		tm.Image = v
	}
	if patches != nil {
		l, d := types.ListValueFrom(ctx, types.StringType, patches)
		if d.HasError() {
			t.Fatalf("build patches: %v", d)
		}
		tm.ConfigPatches = l
	}
	return tm
}

func testImageModel(ctx context.Context, t *testing.T, iso, installer, schematic string, extensions []string) *TalosImageModel {
	t.Helper()
	str := func(s string) types.String {
		if s == "" {
			return types.StringNull()
		}
		return types.StringValue(s)
	}
	img := &TalosImageModel{
		Factory:    types.StringValue("https://factory.talos.dev"),
		Schematic:  str(schematic),
		Extensions: types.ListNull(types.StringType),
		KernelArgs: types.ListNull(types.StringType),
		ISO:        str(iso),
		Installer:  str(installer),
	}
	if extensions != nil {
		l, d := types.ListValueFrom(ctx, types.StringType, extensions)
		if d.HasError() {
			t.Fatalf("build extensions: %v", d)
		}
		img.Extensions = l
	}
	return img
}

func testHelmList(ctx context.Context, t *testing.T, rels []HelmReleaseModel) types.List {
	t.Helper()
	l, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: helmReleaseAttrTypes()}, rels)
	if d.HasError() {
		t.Fatalf("build helm list: %v", d)
	}
	return l
}

func helmRel(name, ns, chart, timeout, values string) HelmReleaseModel {
	str := func(s string) types.String {
		if s == "" {
			return types.StringNull()
		}
		return types.StringValue(s)
	}
	return HelmReleaseModel{
		Name: types.StringValue(name), Namespace: types.StringValue(ns), Chart: types.StringValue(chart),
		Repository: types.StringNull(), Version: types.StringNull(), Values: str(values), Timeout: str(timeout),
	}
}

func buildTestBootstrap(ctx context.Context, t *testing.T, typ, machine string, boot *BootModel, talos *TalosModel, addons *BootstrapAddonsModel) types.Object {
	t.Helper()
	bs := ManagementBootstrapModel{
		Type:    types.StringValue(typ),
		Machine: types.StringValue(machine),
		Boot:    types.ObjectNull(bootAttrTypes()),
		Talos:   types.ObjectNull(talosAttrTypes()),
		Kairos:  types.ObjectNull(kairosAttrTypes()),
		Addons:  types.ObjectNull(bootstrapAddonsAttrTypes()),
	}
	if machine == "" {
		bs.Machine = types.StringNull()
	}
	if boot != nil {
		v, d := types.ObjectValueFrom(ctx, bootAttrTypes(), *boot)
		if d.HasError() {
			t.Fatalf("build boot: %v", d)
		}
		bs.Boot = v
	}
	if talos != nil {
		v, d := types.ObjectValueFrom(ctx, talosAttrTypes(), *talos)
		if d.HasError() {
			t.Fatalf("build talos: %v", d)
		}
		bs.Talos = v
	}
	if addons != nil {
		v, d := types.ObjectValueFrom(ctx, bootstrapAddonsAttrTypes(), *addons)
		if d.HasError() {
			t.Fatalf("build addons: %v", d)
		}
		bs.Addons = v
	}
	v, d := types.ObjectValueFrom(ctx, managementBootstrapAttrTypes(), bs)
	if d.HasError() {
		t.Fatalf("build bootstrap: %v", d)
	}
	return v
}

// talosTestData builds a Tinkerbell cluster model with the given bootstrap object and inventory.
func talosTestData(ctx context.Context, t *testing.T, bootstrap types.Object, machines []testMachine) *ClusterResourceModel {
	t.Helper()
	tink := nullProviderModel()
	tink.Version = types.StringValue("v0.5.4")
	infraVal := mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": tink})
	mgmtVal, d := types.ObjectValueFrom(ctx, managementAttrTypes(), ManagementModel{
		Kubeconfig: types.StringNull(), SkipInit: types.BoolValue(false), SelfManaged: types.BoolValue(true),
		Namespace: types.StringNull(), Bootstrap: bootstrap, Addons: types.ObjectNull(bootstrapAddonsAttrTypes()),
	})
	if d.HasError() {
		t.Fatalf("build management: %v", d)
	}
	inv := types.ObjectNull(inventoryAttrTypes())
	if machines != nil {
		invVal, d := types.ObjectValueFrom(ctx, inventoryAttrTypes(), InventoryModel{
			Source: types.StringNull(), Machine: buildTestMachineList(ctx, t, machines),
		})
		if d.HasError() {
			t.Fatalf("build inventory: %v", d)
		}
		inv = invVal
	}
	data := baseModel(t, ctx)
	data.Infrastructure = infraVal
	data.Management = mgmtVal
	data.Inventory = inv
	return data
}

var goodMachine = []testMachine{{hostname: "cp-1", ip: "10.0.0.5", mac: "aa:bb:cc:dd:ee:01", role: "cp", disk: "/dev/sda", bmc: "10.0.1.5"}}

func runValidate(t *testing.T, data *ClusterResourceModel) diag.Diagnostics {
	t.Helper()
	var diags diag.Diagnostics
	validateManagementBootstrap(context.Background(), data, &diags)
	return diags
}

func assertErrorContains(t *testing.T, diags diag.Diagnostics, want string) {
	t.Helper()
	if !diags.HasError() {
		t.Fatalf("expected error containing %q, got none", want)
	}
	for _, d := range diags.Errors() {
		if strings.Contains(d.Detail(), want) || strings.Contains(d.Summary(), want) {
			return
		}
	}
	t.Fatalf("no error contains %q: %v", want, diags)
}

// --- tests ---

func TestValidateManagementBootstrap_KindIsNoop(t *testing.T) {
	ctx := context.Background()
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "kind", "", nil, nil, nil), nil)
	if diags := runValidate(t, data); diags.HasError() {
		t.Fatalf("kind should validate, got %v", diags)
	}
}

func TestValidateManagementBootstrap_NullBootstrapIsNoop(t *testing.T) {
	ctx := context.Background()
	data := talosTestData(ctx, t, types.ObjectNull(managementBootstrapAttrTypes()), nil)
	if diags := runValidate(t, data); diags.HasError() {
		t.Fatalf("null bootstrap should validate, got %v", diags)
	}
}

func TestValidateManagementBootstrap_UnknownType(t *testing.T) {
	ctx := context.Background()
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "vm", "", nil, nil, nil), nil)
	assertErrorContains(t, runValidate(t, data), "not supported")
}

func TestValidateManagementBootstrap_TalosRequiresMachine(t *testing.T) {
	ctx := context.Background()
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "", nil, testTalosModel(ctx, t, nil, nil), nil), goodMachine)
	assertErrorContains(t, runValidate(t, data), "machine is required")
}

func TestValidateManagementBootstrap_TalosMachineMustExist(t *testing.T) {
	ctx := context.Background()
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-9", nil, testTalosModel(ctx, t, nil, nil), nil), goodMachine)
	assertErrorContains(t, runValidate(t, data), "does not match any inventory.machine")
}

func TestValidateManagementBootstrap_TalosMachineNeedsBMCAndDisk(t *testing.T) {
	ctx := context.Background()
	bare := []testMachine{{hostname: "cp-1", ip: "10.0.0.5", mac: "aa:bb:cc:dd:ee:01"}}
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", nil, testTalosModel(ctx, t, nil, nil), nil), bare)
	diags := runValidate(t, data)
	assertErrorContains(t, diags, "bmc")
	assertErrorContains(t, diags, "disk.device")
}

func TestValidateManagementBootstrap_TalosRequiresVersion(t *testing.T) {
	ctx := context.Background()
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", nil, nil, nil), goodMachine)
	assertErrorContains(t, runValidate(t, data), "talos.version is required")
}

func TestValidateManagementBootstrap_InvalidBoot(t *testing.T) {
	ctx := context.Background()
	boot := &BootModel{Method: types.StringValue("pxe"), Timeout: types.StringValue("soon"), Attempts: types.Int64Value(0)}
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", boot, testTalosModel(ctx, t, nil, nil), nil), goodMachine)
	diags := runValidate(t, data)
	assertErrorContains(t, diags, "boot.method")
	assertErrorContains(t, diags, "boot.timeout")
	assertErrorContains(t, diags, "boot.attempts")
}

func TestValidateManagementBootstrap_InvalidArchitecture(t *testing.T) {
	ctx := context.Background()
	tm := testTalosModel(ctx, t, nil, nil)
	tm.Architecture = types.StringValue("riscv64")
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", nil, tm, nil), goodMachine)
	assertErrorContains(t, runValidate(t, data), "architecture")
}

func TestValidateManagementBootstrap_ImageRules(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		img  *TalosImageModel
		want string
	}{
		{"iso without installer", testImageModel(ctx, t, "https://x/iso", "", "", nil), "set together"},
		{"iso and schematic", testImageModel(ctx, t, "https://x/iso", "x/inst:v1", "abc", nil), "conflict"},
		{"schematic and extensions", testImageModel(ctx, t, "", "", "abc", []string{"iscsi-tools"}), "conflicts with"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", nil, testTalosModel(ctx, t, tc.img, nil), nil), goodMachine)
			assertErrorContains(t, runValidate(t, data), tc.want)
		})
	}
}

func TestValidateManagementBootstrap_InvalidPatchYAML(t *testing.T) {
	ctx := context.Background()
	tm := testTalosModel(ctx, t, nil, []string{"machine: [broken"})
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", nil, tm, nil), goodMachine)
	assertErrorContains(t, runValidate(t, data), "config_patches[0]")
}

func TestValidateManagementBootstrap_HelmRules(t *testing.T) {
	ctx := context.Background()
	addons := &BootstrapAddonsModel{
		Prerequisites: types.ListNull(types.StringType),
		Helm: testHelmList(ctx, t, []HelmReleaseModel{
			helmRel("cilium", "kube-system", "oci://x", "", ""),
			helmRel("cilium", "kube-system", "oci://y", "later", "a: [b"),
		}),
		Manifests: types.ListNull(types.StringType),
	}
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", nil, testTalosModel(ctx, t, nil, nil), addons), goodMachine)
	diags := runValidate(t, data)
	assertErrorContains(t, diags, "duplicate")
	assertErrorContains(t, diags, "timeout")
	assertErrorContains(t, diags, "values")
}

func TestValidateManagementBootstrap_Valid(t *testing.T) {
	ctx := context.Background()
	boot := &BootModel{Method: types.StringValue("auto"), Timeout: types.StringValue("15m"), Attempts: types.Int64Value(3)}
	img := testImageModel(ctx, t, "", "", "", []string{"iscsi-tools"})
	tm := testTalosModel(ctx, t, img, []string{"cluster:\n  network:\n    cni:\n      name: none\n"})
	addons := &BootstrapAddonsModel{
		Prerequisites: types.ListNull(types.StringType),
		Helm:          testHelmList(ctx, t, []HelmReleaseModel{helmRel("cert-manager", "cert-manager", "oci://quay.io/jetstack/charts/cert-manager", "10m", "crds:\n  enabled: true\n")}),
		Manifests:     types.ListNull(types.StringType),
	}
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", boot, tm, addons), goodMachine)
	if diags := runValidate(t, data); diags.HasError() {
		t.Fatalf("expected valid, got %v", diags)
	}
}

func TestValidateLifecycleConfig_CallsBootstrapValidation(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "", nil, nil, nil), goodMachine)
	var diags diag.Diagnostics
	r.validateLifecycleConfig(ctx, data, &diags)
	assertErrorContains(t, diags, "machine is required")
}

func TestClusterResource_Schema_ManagementBootstrap(t *testing.T) {
	ctx := context.Background()
	r := NewClusterResource()
	resp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Schema returned errors: %v", resp.Diagnostics)
	}

	mgmt, ok := resp.Schema.Attributes["management"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management must be a SingleNestedAttribute")
	}
	bs, ok := mgmt.Attributes["bootstrap"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management.bootstrap must be a SingleNestedAttribute")
	}
	if len(bs.PlanModifiers) != 1 {
		t.Errorf("management.bootstrap should have exactly one plan modifier (RequiresReplace), got %d", len(bs.PlanModifiers))
	}
	for _, name := range []string{"type", "machine", "boot", "talos", "addons"} {
		if _, ok := bs.Attributes[name]; !ok {
			t.Errorf("management.bootstrap missing %q", name)
		}
	}
	talosAttr, ok := bs.Attributes["talos"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management.bootstrap.talos must be a SingleNestedAttribute")
	}
	for _, name := range []string{"version", "architecture", "endpoint", "image", "config_patches"} {
		if _, ok := talosAttr.Attributes[name]; !ok {
			t.Errorf("management.bootstrap.talos missing %q", name)
		}
	}
	addonsAttr, ok := bs.Attributes["addons"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management.bootstrap.addons must be a SingleNestedAttribute")
	}
	if _, ok := addonsAttr.Attributes["helm"].(schema.ListNestedAttribute); !ok {
		t.Error("management.bootstrap.addons.helm must be a ListNestedAttribute")
	}
}

func TestExtractManagementBootstrap_Null(t *testing.T) {
	ctx := context.Background()
	mgmt := &ManagementModel{Bootstrap: types.ObjectNull(managementBootstrapAttrTypes()), Addons: types.ObjectNull(bootstrapAddonsAttrTypes())}
	bs, diags := extractManagementBootstrap(ctx, mgmt)
	if bs != nil || diags.HasError() {
		t.Fatalf("extract null: bs=%v diags=%v", bs, diags)
	}
}

func TestFindInventoryMachine(t *testing.T) {
	ctx := context.Background()
	data := talosTestData(ctx, t, types.ObjectNull(managementBootstrapAttrTypes()), goodMachine)
	m, diags := findInventoryMachine(ctx, data, "cp-1")
	if diags.HasError() || m == nil || m.Hostname.ValueString() != "cp-1" {
		t.Fatalf("findInventoryMachine(cp-1) = %v, %v", m, diags)
	}
	if m, _ := findInventoryMachine(ctx, data, "nope"); m != nil {
		t.Fatal("unknown hostname should return nil")
	}
	noInv := talosTestData(ctx, t, types.ObjectNull(managementBootstrapAttrTypes()), nil)
	if m, _ := findInventoryMachine(ctx, noInv, "cp-1"); m != nil {
		t.Fatal("nil inventory should return nil")
	}
}
