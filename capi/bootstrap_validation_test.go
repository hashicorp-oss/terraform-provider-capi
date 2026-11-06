// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// --- builders ---

func testHelmList(ctx context.Context, t *testing.T, rels []helmReleaseModel) types.List {
	t.Helper()
	l, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: helmReleaseAttrTypes()}, rels)
	if d.HasError() {
		t.Fatalf("build helm list: %v", d)
	}
	return l
}

func helmRel(name, ns, chart, timeout, values string) helmReleaseModel {
	str := func(s string) types.String {
		if s == "" {
			return types.StringNull()
		}
		return types.StringValue(s)
	}
	return helmReleaseModel{
		Name: types.StringValue(
			name,
		),
		Namespace:  types.StringValue(ns),
		Chart:      types.StringValue(chart),
		Repository: types.StringNull(),
		Version:    types.StringNull(),
		Values:     str(values),
		Timeout:    str(timeout),
	}
}

// buildTestBootstrap builds a bare management.bootstrap object of the given
// type; kairosData builds the full kairos one.
func buildTestBootstrap(ctx context.Context, t *testing.T, typ string) types.Object {
	t.Helper()
	bs := managementBootstrapModel{
		Type:      types.StringValue(typ),
		Machine:   types.StringNull(),
		OnFailure: types.StringNull(),
		Boot:      types.ObjectNull(bootAttrTypes()),
		Kairos:    types.ObjectNull(kairosAttrTypes()),
		Addons:    types.ObjectNull(bootstrapAddonsAttrTypes()),
	}
	return mustObject(t, ctx, managementBootstrapAttrTypes(), bs)
}

// tinkerbellTestData builds a self-managed Tinkerbell cluster model with the
// given bootstrap object and inventory.
func tinkerbellTestData(
	ctx context.Context,
	t *testing.T,
	bootstrap types.Object,
	machines []testMachine,
) *clusterResourceModel {
	t.Helper()
	tink := nullProviderModel()
	tink.Version = types.StringValue("v0.5.4")
	providers := providersValue(t, ctx, map[string]types.Map{
		"infrastructure": mustProviderMap(t, ctx, map[string]providerModel{"tinkerbell": tink}),
		"control_plane": mustProviderMap(
			t,
			ctx,
			map[string]providerModel{"talos": nullProviderModel()},
		),
		"bootstrap": mustProviderMap(
			t,
			ctx,
			map[string]providerModel{"talos": nullProviderModel()},
		),
	})
	mgmtVal := selfManagedWith(t, ctx, providers, bootstrap)
	inv := types.ObjectNull(inventoryAttrTypes())
	if machines != nil {
		inv = mustObject(t, ctx, inventoryAttrTypes(), inventoryModel{
			Manage: types.BoolValue(
				true,
			),
			Source:  types.StringNull(),
			Machine: buildTestMachineList(ctx, t, machines),
		})
	}
	data := baseModel(t, ctx)
	data.Infrastructure = mustObject(
		t,
		ctx,
		infrastructureSpecAttrTypes(),
		infrastructureSpecModel{
			Provider: types.StringValue("tinkerbell"),
			Spec:     types.DynamicNull(),
		},
	)
	data.ControlPlane = controlPlaneValue(
		t,
		ctx,
		"talos",
		1,
		templateRef(t, ctx, "TinkerbellMachineTemplate", "cp-1"),
		types.DynamicNull(),
	)
	data.Management = mgmtVal
	data.Inventory = inv
	return data
}

func mustObject(
	t *testing.T,
	ctx context.Context,
	attrTypes map[string]attr.Type,
	v any,
) types.Object {
	t.Helper()
	obj, d := types.ObjectValueFrom(ctx, attrTypes, v)
	if d.HasError() {
		t.Fatalf("building object: %v", d)
	}
	return obj
}

func runValidate(t *testing.T, data *clusterResourceModel) diag.Diagnostics {
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
	data := tinkerbellTestData(ctx, t, buildTestBootstrap(ctx, t, "kind"), nil)
	if diags := runValidate(t, data); diags.HasError() {
		t.Fatalf("kind should validate, got %v", diags)
	}
}

func TestValidateManagementBootstrap_NullBootstrapIsNoop(t *testing.T) {
	ctx := context.Background()
	data := tinkerbellTestData(ctx, t, types.ObjectNull(managementBootstrapAttrTypes()), nil)
	if diags := runValidate(t, data); diags.HasError() {
		t.Fatalf("null bootstrap should validate, got %v", diags)
	}
}

func TestValidateManagementBootstrap_RemovedTypesAreRejected(t *testing.T) {
	ctx := context.Background()
	for _, typ := range []string{"vm", "talos"} {
		data := tinkerbellTestData(ctx, t, buildTestBootstrap(ctx, t, typ), nil)
		assertErrorContains(t, runValidate(t, data), "not supported. Supported: kind, kairos")
	}
}

func TestValidateManagementBootstrap_HelmRules(t *testing.T) {
	ctx := context.Background()
	addons := &bootstrapAddonsModel{
		Prerequisites: types.ListNull(types.StringType),
		Helm: testHelmList(ctx, t, []helmReleaseModel{
			helmRel("tinkerbell", "tink", "oci://x", "", ""),
			helmRel("tinkerbell", "tink", "oci://y", "later", "a: [b"),
		}),
		Manifests: types.ListNull(types.StringType),
	}
	data := kairosData(
		ctx,
		t,
		true,
		func(bs *managementBootstrapModel, _ *kairosModel, _ *kairosSSHModel) {
			bs.Addons = mustObject(t, ctx, bootstrapAddonsAttrTypes(), *addons)
		},
	)
	diags := runValidate(t, data)
	assertErrorContains(t, diags, "duplicate")
	assertErrorContains(t, diags, "timeout")
	assertErrorContains(t, diags, "values")
}

func TestValidateLifecycleConfig_CallsBootstrapValidation(t *testing.T) {
	ctx := context.Background()
	r := &clusterResource{}
	data := kairosData(
		ctx,
		t,
		true,
		func(bs *managementBootstrapModel, _ *kairosModel, _ *kairosSSHModel) { bs.Machine = types.StringNull() },
	)
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
	if resp.Schema.Version != 5 {
		t.Errorf("schema version = %d, want 5", resp.Schema.Version)
	}

	mgmt, ok := resp.Schema.Attributes["management"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management must be a SingleNestedAttribute")
	}
	for _, gone := range []string{"addons"} {
		if _, ok := mgmt.Attributes[gone]; ok {
			t.Errorf("management.%s was removed in schema version 4", gone)
		}
	}
	if _, ok := resp.Schema.Attributes["cni"]; ok {
		t.Error("cni was removed in schema version 4: the CNI is a workload addon")
	}
	bs, ok := mgmt.Attributes["bootstrap"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management.bootstrap must be a SingleNestedAttribute")
	}
	if len(bs.PlanModifiers) != 1 {
		t.Errorf(
			"management.bootstrap should have exactly one plan modifier (RequiresReplace), got %d",
			len(bs.PlanModifiers),
		)
	}
	for _, name := range []string{"type", "machine", "boot", "kairos", "addons"} {
		if _, ok := bs.Attributes[name]; !ok {
			t.Errorf("management.bootstrap missing %q", name)
		}
	}
	for _, gone := range []string{"mode", "state_dir", "talos"} {
		if _, ok := bs.Attributes[gone]; ok {
			t.Errorf("management.bootstrap.%s was removed in schema version 4", gone)
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
	mgmt := &managementModel{Bootstrap: types.ObjectNull(managementBootstrapAttrTypes())}
	bs, diags := extractManagementBootstrap(ctx, mgmt)
	if bs != nil || diags.HasError() {
		t.Fatalf("extract null: bs=%v diags=%v", bs, diags)
	}
}

func TestFindInventoryMachine(t *testing.T) {
	ctx := context.Background()
	machines := []testMachine{
		{
			hostname: "cp-1",
			ip:       "10.0.0.5",
			mac:      "aa:bb:cc:dd:ee:01",
			role:     "cp",
			disk:     "/dev/sda",
			bmc:      "10.0.1.5",
		},
	}
	data := tinkerbellTestData(ctx, t, types.ObjectNull(managementBootstrapAttrTypes()), machines)
	m, diags := findInventoryMachine(ctx, data, "cp-1")
	if diags.HasError() || m == nil || m.Hostname.ValueString() != "cp-1" {
		t.Fatalf("findInventoryMachine(cp-1) = %v, %v", m, diags)
	}
	if m, _ := findInventoryMachine(ctx, data, "nope"); m != nil {
		t.Fatal("unknown hostname should return nil")
	}
	noInv := tinkerbellTestData(ctx, t, types.ObjectNull(managementBootstrapAttrTypes()), nil)
	if m, _ := findInventoryMachine(ctx, noInv, "cp-1"); m != nil {
		t.Fatal("nil inventory should return nil")
	}
}
