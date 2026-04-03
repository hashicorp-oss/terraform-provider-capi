// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// --- Schema Tests ---

func TestClusterResource_Schema(t *testing.T) {
	ctx := context.Background()
	r := NewClusterResource()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}
	r.Schema(ctx, req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Schema returned errors: %v", resp.Diagnostics)
	}

	if resp.Schema.Version != 1 {
		t.Errorf("expected schema version 1, got %d", resp.Schema.Version)
	}

	// Verify top-level attributes exist
	for _, attr := range []string{"name", "kubernetes_version", "flavor", "id"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("missing top-level attribute %q", attr)
		}
	}

	// Verify nested attributes exist and are SingleNestedAttribute
	nestedAttrs := []string{"management", "infrastructure", "bootstrap", "control_plane", "core", "workers", "inventory", "wait", "output", "status"}
	for _, name := range nestedAttrs {
		a, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Errorf("missing nested attribute %q", name)
			continue
		}
		if _, isSingle := a.(schema.SingleNestedAttribute); !isSingle {
			t.Errorf("attribute %q should be SingleNestedAttribute, got %T", name, a)
		}
	}
}

func TestClusterResource_SchemaRequiredAttributes(t *testing.T) {
	ctx := context.Background()
	r := NewClusterResource()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}
	r.Schema(ctx, req, resp)

	// name is required
	nameAttr := resp.Schema.Attributes["name"].(schema.StringAttribute)
	if !nameAttr.Required {
		t.Error("name should be Required")
	}

	// infrastructure is required
	infraAttr := resp.Schema.Attributes["infrastructure"].(schema.SingleNestedAttribute)
	if !infraAttr.Required {
		t.Error("infrastructure should be Required")
	}

	// bootstrap is optional
	bsAttr := resp.Schema.Attributes["bootstrap"].(schema.SingleNestedAttribute)
	if !bsAttr.Optional {
		t.Error("bootstrap should be Optional")
	}
}

func TestClusterResource_SchemaStatusComputed(t *testing.T) {
	ctx := context.Background()
	r := NewClusterResource()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}
	r.Schema(ctx, req, resp)

	statusAttr := resp.Schema.Attributes["status"].(schema.SingleNestedAttribute)
	if !statusAttr.Computed {
		t.Error("status should be Computed")
	}
	if statusAttr.Optional {
		t.Error("status should not be Optional")
	}

	// All status children should be computed
	for childName, child := range statusAttr.Attributes {
		childStr, ok := child.(schema.StringAttribute)
		if !ok {
			t.Errorf("status.%s should be StringAttribute", childName)
			continue
		}
		if !childStr.Computed {
			t.Errorf("status.%s should be Computed", childName)
		}
	}
}

// --- Extraction Helper Tests ---

func TestExtractManagement_Null(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{
		Management: types.ObjectNull(managementAttrTypes()),
	}
	mgmt, diags := extractManagement(ctx, data)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	if mgmt != nil {
		t.Error("expected nil for null management")
	}
}

func TestExtractManagement_Populated(t *testing.T) {
	ctx := context.Background()
	mgmt := ManagementModel{
		Kubeconfig:  types.StringValue("/path/to/kubeconfig"),
		SkipInit:    types.BoolValue(false),
		SelfManaged: types.BoolValue(true),
		Namespace:   types.StringValue("capi-system"),
	}
	mgmtVal, diags := types.ObjectValueFrom(ctx, managementAttrTypes(), mgmt)
	if diags.HasError() {
		t.Fatalf("setup error: %v", diags)
	}

	data := &ClusterResourceModel{Management: mgmtVal}
	result, d := extractManagement(ctx, data)
	if d.HasError() {
		t.Fatalf("unexpected error: %v", d)
	}
	if result == nil {
		t.Fatal("expected non-nil management")
	}
	if result.Kubeconfig.ValueString() != "/path/to/kubeconfig" {
		t.Errorf("kubeconfig = %q, want %q", result.Kubeconfig.ValueString(), "/path/to/kubeconfig")
	}
	if !result.SelfManaged.ValueBool() {
		t.Error("self_managed should be true")
	}
	if result.Namespace.ValueString() != "capi-system" {
		t.Errorf("namespace = %q, want %q", result.Namespace.ValueString(), "capi-system")
	}
}

func TestExtractInfrastructure_Populated(t *testing.T) {
	ctx := context.Background()
	infra := InfrastructureModel{Provider: types.StringValue("docker")}
	infraVal, _ := types.ObjectValueFrom(ctx, infrastructureAttrTypes(), infra)

	data := &ClusterResourceModel{Infrastructure: infraVal}
	result, diags := extractInfrastructure(ctx, data)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	if result.Provider.ValueString() != "docker" {
		t.Errorf("provider = %q, want %q", result.Provider.ValueString(), "docker")
	}
}

func TestExtractControlPlane_WithMachineCount(t *testing.T) {
	ctx := context.Background()
	cp := ControlPlaneModel{
		Provider:     types.StringValue("kubeadm:v1.12.2"),
		MachineCount: types.Int64Value(3),
	}
	cpVal, _ := types.ObjectValueFrom(ctx, controlPlaneAttrTypes(), cp)

	data := &ClusterResourceModel{ControlPlane: cpVal}
	result, diags := extractControlPlane(ctx, data)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	if result.MachineCount.ValueInt64() != 3 {
		t.Errorf("machine_count = %d, want 3", result.MachineCount.ValueInt64())
	}
}

func TestExtractWorkers_Null(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{
		Workers: types.ObjectNull(workersAttrTypes()),
	}
	result, diags := extractWorkers(ctx, data)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	if result != nil {
		t.Error("expected nil for null workers")
	}
}

// --- Status Tests ---

func TestSetStatus(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{}

	result := &capi.ClusterResult{
		Endpoint:           "https://10.0.0.1:6443",
		Kubeconfig:         "apiVersion: v1\nclusters: []",
		CACertificate:      "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----",
		ClusterDescription: "Cluster ready",
		BootstrapCluster:   &capi.Cluster{Name: "kind-bootstrap-1234"},
	}

	diags := setStatus(ctx, data, result)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}

	st, d := extractStatus(ctx, data)
	if d.HasError() {
		t.Fatalf("extract error: %v", d)
	}
	if st.Endpoint.ValueString() != "https://10.0.0.1:6443" {
		t.Errorf("endpoint = %q", st.Endpoint.ValueString())
	}
	if st.BootstrapCluster.ValueString() != "kind-bootstrap-1234" {
		t.Errorf("bootstrap_cluster = %q", st.BootstrapCluster.ValueString())
	}
}

func TestSetStatus_NilBootstrap(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{}

	result := &capi.ClusterResult{
		Endpoint:   "https://10.0.0.1:6443",
		Kubeconfig: "kubeconfig-data",
	}

	diags := setStatus(ctx, data, result)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}

	st, _ := extractStatus(ctx, data)
	if !st.BootstrapCluster.IsNull() {
		t.Error("bootstrap_cluster should be null when bootstrap is nil")
	}
}

func TestNullStatus(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{}

	diags := nullStatus(ctx, data)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}

	st, _ := extractStatus(ctx, data)
	if !st.Endpoint.IsNull() {
		t.Error("expected null endpoint")
	}
	if !st.Kubeconfig.IsNull() {
		t.Error("expected null kubeconfig")
	}
}

// --- BuildCreateOptions Tests ---

func TestBuildCreateOptions_Minimal(t *testing.T) {
	ctx := context.Background()

	infraVal, _ := types.ObjectValueFrom(ctx, infrastructureAttrTypes(), InfrastructureModel{
		Provider: types.StringValue("docker"),
	})

	data := &ClusterResourceModel{
		Name:              types.StringValue("test-cluster"),
		KubernetesVersion: types.StringNull(),
		Flavor:            types.StringNull(),
		Infrastructure:    infraVal,
		Management:        types.ObjectNull(managementAttrTypes()),
		Bootstrap:         types.ObjectNull(bootstrapAttrTypes()),
		ControlPlane:      types.ObjectNull(controlPlaneAttrTypes()),
		Core:              types.ObjectNull(coreAttrTypes()),
		Workers:           types.ObjectNull(workersAttrTypes()),
		Inventory:         types.ObjectNull(inventoryAttrTypes()),
		Wait:              types.ObjectNull(waitAttrTypes()),
		Output:            types.ObjectNull(outputAttrTypes()),
	}

	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}

	if opts.Name != "test-cluster" {
		t.Errorf("name = %q, want %q", opts.Name, "test-cluster")
	}
	if opts.InfrastructureProvider != "docker" {
		t.Errorf("infra = %q, want %q", opts.InfrastructureProvider, "docker")
	}
	if opts.WaitForReady != true {
		t.Error("WaitForReady should default to true")
	}
}

func TestBuildCreateOptions_Full(t *testing.T) {
	ctx := context.Background()

	infraVal, _ := types.ObjectValueFrom(ctx, infrastructureAttrTypes(), InfrastructureModel{
		Provider: types.StringValue("tinkerbell:v0.5.4"),
	})
	mgmtVal, _ := types.ObjectValueFrom(ctx, managementAttrTypes(), ManagementModel{
		Kubeconfig:  types.StringNull(),
		SkipInit:    types.BoolValue(false),
		SelfManaged: types.BoolValue(true),
		Namespace:   types.StringValue("capi-ns"),
	})
	bsVal, _ := types.ObjectValueFrom(ctx, bootstrapAttrTypes(), BootstrapModel{
		Provider: types.StringValue("kubeadm:v1.12.2"),
	})
	cpVal, _ := types.ObjectValueFrom(ctx, controlPlaneAttrTypes(), ControlPlaneModel{
		Provider:     types.StringValue("kubeadm:v1.12.2"),
		MachineCount: types.Int64Value(3),
	})
	coreVal, _ := types.ObjectValueFrom(ctx, coreAttrTypes(), CoreModel{
		Provider: types.StringValue("cluster-api:v1.12.2"),
	})
	wVal, _ := types.ObjectValueFrom(ctx, workersAttrTypes(), WorkersModel{
		MachineCount: types.Int64Value(5),
	})
	waitVal, _ := types.ObjectValueFrom(ctx, waitAttrTypes(), WaitModel{
		Enabled: types.BoolValue(true),
		Timeout: types.StringValue("60m"),
	})
	outVal, _ := types.ObjectValueFrom(ctx, outputAttrTypes(), OutputModel{
		KubeconfigPath: types.StringValue("/tmp/test.kubeconfig"),
	})

	data := &ClusterResourceModel{
		Name:              types.StringValue("prod"),
		KubernetesVersion: types.StringValue("v1.31.0"),
		Flavor:            types.StringValue("ha"),
		Infrastructure:    infraVal,
		Management:        mgmtVal,
		Bootstrap:         bsVal,
		ControlPlane:      cpVal,
		Core:              coreVal,
		Workers:           wVal,
		Inventory:         types.ObjectNull(inventoryAttrTypes()),
		Wait:              waitVal,
		Output:            outVal,
	}

	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}

	if opts.InfrastructureProvider != "tinkerbell:v0.5.4" {
		t.Errorf("infra = %q", opts.InfrastructureProvider)
	}
	if !opts.SelfManaged {
		t.Error("SelfManaged should be true")
	}
	if opts.Namespace != "capi-ns" {
		t.Errorf("namespace = %q", opts.Namespace)
	}
	if opts.BootstrapProvider != "kubeadm:v1.12.2" {
		t.Errorf("bootstrap = %q", opts.BootstrapProvider)
	}
	if opts.ControlPlaneProvider != "kubeadm:v1.12.2" {
		t.Errorf("control_plane = %q", opts.ControlPlaneProvider)
	}
	if opts.CoreProvider != "cluster-api:v1.12.2" {
		t.Errorf("core = %q", opts.CoreProvider)
	}
	if opts.ControlPlaneMachineCount == nil || *opts.ControlPlaneMachineCount != 3 {
		t.Error("control plane machine count should be 3")
	}
	if opts.WorkerMachineCount == nil || *opts.WorkerMachineCount != 5 {
		t.Error("worker machine count should be 5")
	}
	if opts.KubernetesVersion != "v1.31.0" {
		t.Errorf("k8s version = %q", opts.KubernetesVersion)
	}
	if opts.Flavor != "ha" {
		t.Errorf("flavor = %q", opts.Flavor)
	}
	if opts.KubeconfigOutputPath != "/tmp/test.kubeconfig" {
		t.Errorf("kubeconfig path = %q", opts.KubeconfigOutputPath)
	}
}

// --- Validation Tests ---

func TestValidateLifecycleConfig_Docker(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}

	infraVal, _ := types.ObjectValueFrom(ctx, infrastructureAttrTypes(), InfrastructureModel{
		Provider: types.StringValue("docker"),
	})
	data := &ClusterResourceModel{
		Name:           types.StringValue("test"),
		Infrastructure: infraVal,
		Management:     types.ObjectNull(managementAttrTypes()),
		Bootstrap:      types.ObjectNull(bootstrapAttrTypes()),
		ControlPlane:   types.ObjectNull(controlPlaneAttrTypes()),
		Workers:        types.ObjectNull(workersAttrTypes()),
		Inventory:      types.ObjectNull(inventoryAttrTypes()),
	}

	var diags diag.Diagnostics
	r.validateLifecycleConfig(ctx, data, &diags)
	if diags.HasError() {
		t.Errorf("Docker should pass validation, got: %v", diags)
	}
}

func TestValidateLifecycleConfig_UnsupportedProvider(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}

	infraVal, _ := types.ObjectValueFrom(ctx, infrastructureAttrTypes(), InfrastructureModel{
		Provider: types.StringValue("badprovider"),
	})
	data := &ClusterResourceModel{
		Name:           types.StringValue("test"),
		Infrastructure: infraVal,
		Management:     types.ObjectNull(managementAttrTypes()),
		Bootstrap:      types.ObjectNull(bootstrapAttrTypes()),
		ControlPlane:   types.ObjectNull(controlPlaneAttrTypes()),
		Workers:        types.ObjectNull(workersAttrTypes()),
		Inventory:      types.ObjectNull(inventoryAttrTypes()),
	}

	var diags diag.Diagnostics
	r.validateLifecycleConfig(ctx, data, &diags)
	if !diags.HasError() {
		t.Error("expected error for unsupported provider")
	}
}

func TestValidateLifecycleConfig_TinkerbellRequiresSelfManaged(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}

	infraVal, _ := types.ObjectValueFrom(ctx, infrastructureAttrTypes(), InfrastructureModel{
		Provider: types.StringValue("tinkerbell:v0.5.4"),
	})
	mgmtVal, _ := types.ObjectValueFrom(ctx, managementAttrTypes(), ManagementModel{
		Kubeconfig:  types.StringNull(),
		SkipInit:    types.BoolValue(false),
		SelfManaged: types.BoolValue(false),
		Namespace:   types.StringNull(),
	})
	data := &ClusterResourceModel{
		Name:           types.StringValue("test"),
		Infrastructure: infraVal,
		Management:     mgmtVal,
		Bootstrap:      types.ObjectNull(bootstrapAttrTypes()),
		ControlPlane:   types.ObjectNull(controlPlaneAttrTypes()),
		Workers:        types.ObjectNull(workersAttrTypes()),
		Inventory:      types.ObjectNull(inventoryAttrTypes()),
	}

	var diags diag.Diagnostics
	r.validateLifecycleConfig(ctx, data, &diags)
	if !diags.HasError() {
		t.Error("Tinkerbell without self_managed=true should fail validation")
	}
}

func TestValidateLifecycleConfig_TinkerbellWithTalos(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}

	infraVal, _ := types.ObjectValueFrom(ctx, infrastructureAttrTypes(), InfrastructureModel{
		Provider: types.StringValue("tinkerbell:v0.5.4"),
	})
	mgmtVal, _ := types.ObjectValueFrom(ctx, managementAttrTypes(), ManagementModel{
		Kubeconfig:  types.StringNull(),
		SkipInit:    types.BoolValue(false),
		SelfManaged: types.BoolValue(true),
		Namespace:   types.StringNull(),
	})
	bsVal, _ := types.ObjectValueFrom(ctx, bootstrapAttrTypes(), BootstrapModel{
		Provider: types.StringValue("talos:v0.6.7"),
	})
	cpVal, _ := types.ObjectValueFrom(ctx, controlPlaneAttrTypes(), ControlPlaneModel{
		Provider:     types.StringValue("talos:v0.6.7"),
		MachineCount: types.Int64Value(3),
	})

	data := &ClusterResourceModel{
		Name:           types.StringValue("test"),
		Infrastructure: infraVal,
		Management:     mgmtVal,
		Bootstrap:      bsVal,
		ControlPlane:   cpVal,
		Workers:        types.ObjectNull(workersAttrTypes()),
		Inventory:      types.ObjectNull(inventoryAttrTypes()),
	}

	var diags diag.Diagnostics
	r.validateLifecycleConfig(ctx, data, &diags)
	if diags.HasError() {
		t.Errorf("Tinkerbell + Talos + self_managed should be valid, got: %v", diags)
	}
}

func TestValidateLifecycleConfig_TinkerbellInvalidBootstrap(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}

	infraVal, _ := types.ObjectValueFrom(ctx, infrastructureAttrTypes(), InfrastructureModel{
		Provider: types.StringValue("tinkerbell"),
	})
	mgmtVal, _ := types.ObjectValueFrom(ctx, managementAttrTypes(), ManagementModel{
		Kubeconfig:  types.StringNull(),
		SkipInit:    types.BoolValue(false),
		SelfManaged: types.BoolValue(true),
		Namespace:   types.StringNull(),
	})
	bsVal, _ := types.ObjectValueFrom(ctx, bootstrapAttrTypes(), BootstrapModel{
		Provider: types.StringValue("microk8s"),
	})

	data := &ClusterResourceModel{
		Name:           types.StringValue("test"),
		Infrastructure: infraVal,
		Management:     mgmtVal,
		Bootstrap:      bsVal,
		ControlPlane:   types.ObjectNull(controlPlaneAttrTypes()),
		Workers:        types.ObjectNull(workersAttrTypes()),
		Inventory:      types.ObjectNull(inventoryAttrTypes()),
	}

	var diags diag.Diagnostics
	r.validateLifecycleConfig(ctx, data, &diags)
	if !diags.HasError() {
		t.Error("Tinkerbell with microk8s bootstrap should fail validation")
	}
}

// --- Inventory Validation Tests ---

func TestValidateInventory_SourceAndMachineMutuallyExclusive(t *testing.T) {
	ctx := context.Background()

	machineVal := buildTestMachineList(ctx, t, []testMachine{
		{hostname: "m1", ip: "10.0.0.1", mac: "aa:bb:cc:dd:ee:01"},
	})
	inv := &InventoryModel{
		Source:  types.StringValue("/path/to/hardware.csv"),
		Machine: machineVal,
	}

	var diags diag.Diagnostics
	validateInventory(ctx, inv, 0, 0, &diags)
	if !diags.HasError() {
		t.Error("expected error for source+machine conflict")
	}
}

func TestValidateInventory_DuplicateHostnames(t *testing.T) {
	ctx := context.Background()
	machineVal := buildTestMachineList(ctx, t, []testMachine{
		{hostname: "m1", ip: "10.0.0.1", mac: "aa:bb:cc:dd:ee:01"},
		{hostname: "m1", ip: "10.0.0.2", mac: "aa:bb:cc:dd:ee:02"},
	})

	inv := &InventoryModel{
		Source:  types.StringNull(),
		Machine: machineVal,
	}

	var diags diag.Diagnostics
	validateInventory(ctx, inv, 0, 0, &diags)
	if !diags.HasError() {
		t.Error("expected error for duplicate hostname")
	}
}

func TestValidateInventory_DuplicateIPs(t *testing.T) {
	ctx := context.Background()
	machineVal := buildTestMachineList(ctx, t, []testMachine{
		{hostname: "m1", ip: "10.0.0.1", mac: "aa:bb:cc:dd:ee:01"},
		{hostname: "m2", ip: "10.0.0.1", mac: "aa:bb:cc:dd:ee:02"},
	})

	inv := &InventoryModel{
		Source:  types.StringNull(),
		Machine: machineVal,
	}

	var diags diag.Diagnostics
	validateInventory(ctx, inv, 0, 0, &diags)
	if !diags.HasError() {
		t.Error("expected error for duplicate IP")
	}
}

func TestValidateInventory_DuplicateMACs(t *testing.T) {
	ctx := context.Background()
	machineVal := buildTestMachineList(ctx, t, []testMachine{
		{hostname: "m1", ip: "10.0.0.1", mac: "aa:bb:cc:dd:ee:01"},
		{hostname: "m2", ip: "10.0.0.2", mac: "aa:bb:cc:dd:ee:01"},
	})

	inv := &InventoryModel{
		Source:  types.StringNull(),
		Machine: machineVal,
	}

	var diags diag.Diagnostics
	validateInventory(ctx, inv, 0, 0, &diags)
	if !diags.HasError() {
		t.Error("expected error for duplicate MAC")
	}
}

func TestValidateInventory_InsufficientControlPlane(t *testing.T) {
	ctx := context.Background()
	machineVal := buildTestMachineList(ctx, t, []testMachine{
		{hostname: "cp1", ip: "10.0.0.1", mac: "aa:bb:cc:dd:ee:01", role: "cp"},
		{hostname: "w1", ip: "10.0.0.2", mac: "aa:bb:cc:dd:ee:02", role: "worker"},
	})

	inv := &InventoryModel{
		Source:  types.StringNull(),
		Machine: machineVal,
	}

	var diags diag.Diagnostics
	validateInventory(ctx, inv, 3, 0, &diags)
	if !diags.HasError() {
		t.Error("expected error for insufficient CP machines")
	}
}

func TestValidateInventory_Valid(t *testing.T) {
	ctx := context.Background()
	machineVal := buildTestMachineList(ctx, t, []testMachine{
		{hostname: "cp1", ip: "10.0.0.1", mac: "aa:bb:cc:dd:ee:01", role: "cp"},
		{hostname: "cp2", ip: "10.0.0.2", mac: "aa:bb:cc:dd:ee:02", role: "cp"},
		{hostname: "cp3", ip: "10.0.0.3", mac: "aa:bb:cc:dd:ee:03", role: "cp"},
		{hostname: "w1", ip: "10.0.0.10", mac: "aa:bb:cc:dd:ee:10", role: "worker"},
		{hostname: "w2", ip: "10.0.0.11", mac: "aa:bb:cc:dd:ee:11", role: "worker"},
	})

	inv := &InventoryModel{
		Source:  types.StringNull(),
		Machine: machineVal,
	}

	var diags diag.Diagnostics
	validateInventory(ctx, inv, 3, 2, &diags)
	if diags.HasError() {
		t.Errorf("expected no errors, got: %v", diags)
	}
}

// --- StatusWithFallback Tests ---

func TestSetStatusWithFallback_UsesResultFirst(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{}

	result := &capi.ClusterResult{
		Endpoint:   "https://new-endpoint:6443",
		Kubeconfig: "new-kubeconfig",
	}
	prev := &StatusModel{
		Endpoint:         types.StringValue("https://old-endpoint:6443"),
		Kubeconfig:       types.StringValue("old-kubeconfig"),
		CACertificate:    types.StringValue("old-ca"),
		Description:      types.StringNull(),
		BootstrapCluster: types.StringNull(),
	}

	diags := setStatusWithFallback(ctx, data, result, prev)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}

	st, _ := extractStatus(ctx, data)
	if st.Endpoint.ValueString() != "https://new-endpoint:6443" {
		t.Errorf("endpoint should be from result, got %q", st.Endpoint.ValueString())
	}
	// CA cert should fall back since result has empty
	if st.CACertificate.ValueString() != "old-ca" {
		t.Errorf("ca_cert should fallback, got %q", st.CACertificate.ValueString())
	}
}

// --- Test Machine Builder ---

type testMachine struct {
	hostname string
	ip       string
	mac      string
	role     string // "cp" or "worker"
}

func buildTestMachineList(ctx context.Context, t *testing.T, machines []testMachine) types.List {
	t.Helper()

	var machineObjects []MachineModel
	for _, m := range machines {
		netVal, d := types.ObjectValueFrom(ctx, networkAttrTypes(), NetworkModel{
			IPAddress:   types.StringValue(m.ip),
			Netmask:     types.StringValue("255.255.255.0"),
			Gateway:     types.StringValue("10.0.0.1"),
			MACAddress:  types.StringValue(m.mac),
			Nameservers: types.ListNull(types.StringType),
			VLANID:      types.StringNull(),
		})
		if d.HasError() {
			t.Fatalf("build network: %v", d)
		}

		var labels types.Map
		if m.role != "" {
			labelsMap, d := types.MapValueFrom(ctx, types.StringType, map[string]string{"type": m.role})
			if d.HasError() {
				t.Fatalf("build labels: %v", d)
			}
			labels = labelsMap
		} else {
			labels = types.MapNull(types.StringType)
		}

		machineObjects = append(machineObjects, MachineModel{
			Hostname: types.StringValue(m.hostname),
			Network:  netVal,
			Disk:     types.ObjectNull(diskAttrTypes()),
			BMC:      types.ObjectNull(bmcAttrTypes()),
			Labels:   labels,
		})
	}

	machineList, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: machineAttrTypes()}, machineObjects)
	if d.HasError() {
		t.Fatalf("build machine list: %v", d)
	}

	return machineList
}

// --- StringOrNull Tests ---

func TestStringOrNull(t *testing.T) {
	if v := stringOrNull("hello"); v.ValueString() != "hello" {
		t.Errorf("expected hello, got %q", v.ValueString())
	}
	if v := stringOrNull(""); !v.IsNull() {
		t.Error("expected null for empty string")
	}
}

// --- Upgrade State Tests ---

func TestClusterResource_UpgradeState(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}
	upgraders := r.UpgradeState(ctx)

	if _, ok := upgraders[0]; !ok {
		t.Error("expected v0 upgrader")
	}
	if upgraders[0].PriorSchema == nil {
		t.Error("v0 upgrader should have a PriorSchema")
	}
	if upgraders[0].StateUpgrader == nil {
		t.Error("v0 upgrader should have a StateUpgrader function")
	}
}
