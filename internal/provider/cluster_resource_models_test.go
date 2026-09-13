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

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// --- Schema Tests ---

func TestClusterResource_Schema_ProviderMaps(t *testing.T) {
	ctx := context.Background()
	r := NewClusterResource()
	resp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	if resp.Schema.Version != 2 {
		t.Errorf("schema version = %d, want 2", resp.Schema.Version)
	}

	var reference map[string]schema.Attribute
	for _, name := range []string{"core", "infrastructure", "bootstrap", "control_plane", "ipam", "addon"} {
		attr, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Fatalf("%s attribute missing", name)
		}
		m, ok := attr.(schema.MapNestedAttribute)
		if !ok {
			t.Fatalf("%s must be MapNestedAttribute, got %T", name, attr)
		}
		if name == "infrastructure" && !m.Required {
			t.Error("infrastructure must be required")
		}
		if name != "infrastructure" && !m.Optional {
			t.Errorf("%s must be optional", name)
		}
		if len(m.PlanModifiers) == 0 {
			t.Errorf("%s must RequiresReplace", name)
		}
		if reference == nil {
			reference = m.NestedObject.Attributes
			continue
		}
		if len(reference) != len(m.NestedObject.Attributes) {
			t.Errorf("%s nested object differs from the shared provider object", name)
		}
	}
	for _, name := range []string{"version", "fetch_config", "config_variables", "secret_config_variables", "deployment", "manager", "additional_manifests", "manifest_patches", "patches"} {
		if _, ok := reference[name]; !ok {
			t.Errorf("provider object missing %s", name)
		}
	}
	fc, ok := reference["fetch_config"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("fetch_config must be a SingleNestedAttribute")
	}
	for _, name := range []string{"owner", "repository", "url", "oci"} {
		if _, ok := fc.Attributes[name]; !ok {
			t.Errorf("fetch_config missing %s", name)
		}
	}

	for _, gone := range []string{"workers", "addons"} {
		if _, ok := resp.Schema.Attributes[gone]; ok {
			t.Errorf("%s must be removed", gone)
		}
	}
	tmpl, ok := resp.Schema.Attributes["template"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("template must be a SingleNestedAttribute")
	}
	if _, ok := tmpl.Attributes["source"].(schema.StringAttribute); !ok {
		t.Error("template.source must be a string")
	}
	if _, ok := tmpl.Attributes["variables"].(schema.MapAttribute); !ok {
		t.Error("template.variables must be a map")
	}
	topo, ok := resp.Schema.Attributes["topology"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("topology must be a SingleNestedAttribute")
	}
	cp, ok := topo.Attributes["control_plane"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("topology.control_plane must be a SingleNestedAttribute")
	}
	if _, ok := cp.Attributes["replicas"].(schema.Int64Attribute); !ok {
		t.Error("topology.control_plane.replicas must be Int64")
	}
	workers, ok := topo.Attributes["workers"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("topology.workers must be a SingleNestedAttribute")
	}
	mds, ok := workers.Attributes["machine_deployments"].(schema.ListNestedAttribute)
	if !ok {
		t.Fatal("topology.workers.machine_deployments must be a ListNestedAttribute")
	}
	for _, name := range []string{"name", "class", "replicas", "failure_domain", "metadata"} {
		if _, ok := mds.NestedObject.Attributes[name]; !ok {
			t.Errorf("machine_deployments missing %s", name)
		}
	}
	if nameAttr, ok := mds.NestedObject.Attributes["name"].(schema.StringAttribute); !ok || !nameAttr.Required {
		t.Error("machine_deployments.name must be a required string")
	}
}

func TestClusterResource_SchemaRequiredAttributes(t *testing.T) {
	ctx := context.Background()
	r := NewClusterResource()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}
	r.Schema(ctx, req, resp)

	// name is required
	nameAttr, ok := resp.Schema.Attributes["name"].(schema.StringAttribute)
	if !ok || !nameAttr.Required {
		t.Error("name should be Required")
	}

	// infrastructure is required
	infraAttr, ok := resp.Schema.Attributes["infrastructure"].(schema.MapNestedAttribute)
	if !ok || !infraAttr.Required {
		t.Error("infrastructure should be Required")
	}

	// bootstrap is optional
	bsAttr, ok := resp.Schema.Attributes["bootstrap"].(schema.MapNestedAttribute)
	if !ok || !bsAttr.Optional {
		t.Error("bootstrap should be Optional")
	}
}

func TestClusterResource_SchemaStatusComputed(t *testing.T) {
	ctx := context.Background()
	r := NewClusterResource()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}
	r.Schema(ctx, req, resp)

	statusAttr, ok := resp.Schema.Attributes["status"].(schema.SingleNestedAttribute)
	if !ok || !statusAttr.Computed {
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
		Bootstrap:   types.ObjectNull(managementBootstrapAttrTypes()),
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

func TestExtractProviders_Populated(t *testing.T) {
	ctx := context.Background()
	pm := emptyProviderModel()
	pm.Version = types.StringValue("v0.7.9")
	m := mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": pm})
	got, diags := extractProviders(ctx, m)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	if got["tinkerbell"].Version.ValueString() != "v0.7.9" {
		t.Errorf("version = %q", got["tinkerbell"].Version.ValueString())
	}
}

func TestExtractProviders_Null(t *testing.T) {
	got, diags := extractProviders(context.Background(), types.MapNull(providerMapType().ElemType))
	if diags.HasError() || got != nil {
		t.Errorf("null map must yield nil, got %v %v", got, diags)
	}
}

func TestExtractTopology_Null(t *testing.T) {
	ctx := context.Background()
	cp, mds, diags := extractTopology(ctx, &ClusterResourceModel{Topology: types.ObjectNull(topologyAttrTypes())})
	if diags.HasError() || cp != nil || mds != nil {
		t.Errorf("null topology must yield nils, got %v %v %v", cp, mds, diags)
	}
}

func TestLegacyTopologyObject(t *testing.T) {
	ctx := context.Background()
	obj, diags := legacyTopologyObject(ctx, types.Int64Value(3), types.Int64Value(2))
	if diags.HasError() {
		t.Fatal(diags)
	}
	cp, mds, diags := extractTopology(ctx, &ClusterResourceModel{Topology: obj})
	if diags.HasError() {
		t.Fatal(diags)
	}
	if cp == nil || *cp != 3 {
		t.Errorf("cp = %v", cp)
	}
	if len(mds) != 1 || mds[0].Name != "md-0" || mds[0].Replicas == nil || *mds[0].Replicas != 2 {
		t.Errorf("mds = %+v", mds)
	}
	if obj, _ := legacyTopologyObject(ctx, types.Int64Null(), types.Int64Null()); !obj.IsNull() {
		t.Error("no counts must yield a null topology")
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

func mustProviderMap(t *testing.T, ctx context.Context, entries map[string]ProviderModel) types.Map {
	t.Helper()
	v, diags := types.MapValueFrom(ctx, providerMapType().ElemType, entries)
	if diags.HasError() {
		t.Fatalf("building provider map: %v", diags)
	}
	return v
}

func emptyProviderModel() ProviderModel { return nullProviderModel() }

func baseModel(t *testing.T, ctx context.Context) *ClusterResourceModel {
	t.Helper()
	return &ClusterResourceModel{
		Name:           types.StringValue("test"),
		Core:           types.MapNull(providerMapType().ElemType),
		Infrastructure: mustProviderMap(t, ctx, map[string]ProviderModel{"docker": emptyProviderModel()}),
		Bootstrap:      types.MapNull(providerMapType().ElemType),
		ControlPlane:   types.MapNull(providerMapType().ElemType),
		IPAM:           types.MapNull(providerMapType().ElemType),
		Addon:          types.MapNull(providerMapType().ElemType),
		Topology:       types.ObjectNull(topologyAttrTypes()),
		Template:       types.ObjectNull(templateAttrTypes()),
		Management:     types.ObjectNull(managementAttrTypes()),
		Inventory:      types.ObjectNull(inventoryAttrTypes()),
		Wait:           types.ObjectNull(waitAttrTypes()),
		Output:         types.ObjectNull(outputAttrTypes()),
	}
}

func selfManaged(t *testing.T, ctx context.Context) types.Object {
	t.Helper()
	v, d := types.ObjectValueFrom(ctx, managementAttrTypes(), ManagementModel{
		Kubeconfig: types.StringNull(), SkipInit: types.BoolValue(false), SelfManaged: types.BoolValue(true),
		Namespace: types.StringNull(), Bootstrap: types.ObjectNull(managementBootstrapAttrTypes()),
	})
	if d.HasError() {
		t.Fatal(d)
	}
	return v
}

func topologyValue(t *testing.T, ctx context.Context, cpReplicas int64, mds []MachineDeploymentModel) types.Object {
	t.Helper()
	cp, d := types.ObjectValueFrom(ctx, controlPlaneTopologyAttrTypes(), ControlPlaneTopologyModel{Replicas: types.Int64Value(cpReplicas)})
	if d.HasError() {
		t.Fatal(d)
	}
	workers := types.ObjectNull(workersTopologyAttrTypes())
	if mds != nil {
		list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: machineDeploymentAttrTypes()}, mds)
		if d.HasError() {
			t.Fatal(d)
		}
		workers, d = types.ObjectValueFrom(ctx, workersTopologyAttrTypes(), WorkersTopologyModel{MachineDeployments: list})
		if d.HasError() {
			t.Fatal(d)
		}
	}
	topo, d := types.ObjectValueFrom(ctx, topologyAttrTypes(), TopologyModel{ControlPlane: cp, Workers: workers})
	if d.HasError() {
		t.Fatal(d)
	}
	return topo
}

func machineDeployment(t *testing.T, ctx context.Context, name string, replicas int64, labels map[string]string) MachineDeploymentModel {
	t.Helper()
	meta := types.ObjectNull(topologyMetadataAttrTypes())
	if labels != nil {
		l, d := types.MapValueFrom(ctx, types.StringType, labels)
		if d.HasError() {
			t.Fatal(d)
		}
		meta, d = types.ObjectValueFrom(ctx, topologyMetadataAttrTypes(), TopologyMetadataModel{Labels: l, Annotations: types.MapNull(types.StringType)})
		if d.HasError() {
			t.Fatal(d)
		}
	}
	return MachineDeploymentModel{
		Name: types.StringValue(name), Class: types.StringNull(), Replicas: types.Int64Value(replicas),
		FailureDomain: types.StringNull(), Metadata: meta,
	}
}

func TestBuildCreateOptions_Minimal(t *testing.T) {
	ctx := context.Background()
	opts, diags := buildCreateOptions(ctx, baseModel(t, ctx))
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	infra, ok := opts.Providers.Infrastructure()
	if !ok || infra.InitString() != "docker" {
		t.Errorf("infra = %+v %v", infra, ok)
	}
	if !opts.WaitForReady {
		t.Error("WaitForReady should default to true")
	}
	if opts.ControlPlaneMachineCount != nil || opts.MachineDeployments != nil {
		t.Error("counts must be nil without topology")
	}
	if opts.TemplateSource != "" || opts.Variables != nil {
		t.Error("template source/variables must be empty without a template block")
	}
}

func TestBuildCreateOptions_Full(t *testing.T) {
	ctx := context.Background()
	data := baseModel(t, ctx)
	data.KubernetesVersion = types.StringValue("v1.34.0")
	data.Flavor = types.StringValue("ha")

	fetch, _ := types.ObjectValueFrom(ctx, fetchConfigAttrTypes(), FetchConfigModel{
		Owner: types.StringValue("tinkerbell-community"), Repository: types.StringNull(), URL: types.StringNull(), OCI: types.StringNull(),
	})
	gates, _ := types.MapValueFrom(ctx, types.BoolType, map[string]bool{"ClusterTopology": true})
	mgr, _ := types.ObjectValueFrom(ctx, managerAttrTypes(), ManagerModel{
		ProfilerAddress: types.StringNull(), MaxConcurrentReconciles: types.Int64Null(), Verbosity: types.Int64Null(),
		FeatureGates: gates, AdditionalArgs: types.MapNull(types.StringType),
	})
	tink := emptyProviderModel()
	tink.Version = types.StringValue("v0.7.9")
	tink.FetchConfig = fetch
	tink.Manager = mgr
	data.Infrastructure = mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": tink})

	talosBS := emptyProviderModel()
	talosBS.Version = types.StringValue("v0.8.2")
	data.Bootstrap = mustProviderMap(t, ctx, map[string]ProviderModel{"talos": talosBS})
	talosCP := emptyProviderModel()
	talosCP.Version = types.StringValue("v0.7.1")
	data.ControlPlane = mustProviderMap(t, ctx, map[string]ProviderModel{"talos": talosCP})
	core := emptyProviderModel()
	core.Version = types.StringValue("v1.12.2")
	data.Core = mustProviderMap(t, ctx, map[string]ProviderModel{"cluster-api": core})
	data.Addon = mustProviderMap(t, ctx, map[string]ProviderModel{"helm": emptyProviderModel()})
	data.Topology = topologyValue(t, ctx, 3, []MachineDeploymentModel{machineDeployment(t, ctx, "md-0", 5, map[string]string{"tier": "worker"})})
	vars, _ := types.MapValueFrom(ctx, types.StringType, map[string]string{"CONTROL_PLANE_VIP": "10.0.0.160"})
	data.Template, _ = types.ObjectValueFrom(ctx, templateAttrTypes(), TemplateModel{Source: types.StringValue("/srv/cluster-template-talos.yaml"), Variables: vars})

	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	infra, _ := opts.Providers.Infrastructure()
	if infra.InitString() != "tinkerbell:v0.7.9" || infra.FetchConfig == nil || infra.FetchConfig.Owner != "tinkerbell-community" {
		t.Errorf("infra = %+v", infra)
	}
	if infra.Manager == nil || !infra.Manager.FeatureGates["ClusterTopology"] {
		t.Errorf("infra manager = %+v", infra.Manager)
	}
	if got := opts.Providers.InitStrings(capi.ProviderTypeBootstrap); len(got) != 1 || got[0] != "talos:v0.8.2" {
		t.Errorf("bootstrap = %v", got)
	}
	if got := opts.Providers.InitStrings(capi.ProviderTypeControlPlane); len(got) != 1 || got[0] != "talos:v0.7.1" {
		t.Errorf("control plane = %v", got)
	}
	if got := opts.Providers.InitStrings(capi.ProviderTypeCore); len(got) != 1 || got[0] != "cluster-api:v1.12.2" {
		t.Errorf("core = %v", got)
	}
	if got := opts.Providers.InitStrings(capi.ProviderTypeAddon); len(got) != 1 || got[0] != "helm" {
		t.Errorf("addon = %v", got)
	}
	if opts.ControlPlaneMachineCount == nil || *opts.ControlPlaneMachineCount != 3 {
		t.Error("control plane replicas should be 3")
	}
	if len(opts.MachineDeployments) != 1 || *opts.MachineDeployments[0].Replicas != 5 || opts.MachineDeployments[0].Labels["tier"] != "worker" {
		t.Errorf("machine deployments = %+v", opts.MachineDeployments)
	}
	if got := opts.WorkerMachineCount(); got == nil || *got != 5 {
		t.Errorf("WorkerMachineCount() = %v", got)
	}
	if opts.TemplateSource != "/srv/cluster-template-talos.yaml" || opts.Variables["CONTROL_PLANE_VIP"] != "10.0.0.160" {
		t.Errorf("template source/variables = %q %v", opts.TemplateSource, opts.Variables)
	}
	if opts.KubernetesVersion != "v1.34.0" || opts.Flavor != "ha" {
		t.Errorf("k8s/flavor = %q/%q", opts.KubernetesVersion, opts.Flavor)
	}
}

func TestBuildCreateOptions_ProviderOrderIsDeterministic(t *testing.T) {
	ctx := context.Background()
	data := baseModel(t, ctx)
	data.Addon = mustProviderMap(t, ctx, map[string]ProviderModel{"zeta": emptyProviderModel(), "alpha": emptyProviderModel(), "mid": emptyProviderModel()})
	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatal(diags)
	}
	got := opts.Providers.InitStrings(capi.ProviderTypeAddon)
	if len(got) != 3 || got[0] != "alpha" || got[1] != "mid" || got[2] != "zeta" {
		t.Errorf("addon order = %v, want sorted by name", got)
	}
}

// --- Validation Tests ---

func validateWith(t *testing.T, ctx context.Context, mutate func(*ClusterResourceModel)) diag.Diagnostics {
	t.Helper()
	data := baseModel(t, ctx)
	mutate(data)
	var diags diag.Diagnostics
	(&ClusterResource{}).validateLifecycleConfig(ctx, data, &diags)
	return diags
}

func hasErrorContaining(diags diag.Diagnostics, s string) bool {
	for _, d := range diags.Errors() {
		if strings.Contains(d.Summary()+" "+d.Detail(), s) {
			return true
		}
	}
	return false
}

func TestValidateLifecycleConfig_Docker(t *testing.T) {
	ctx := context.Background()
	if diags := validateWith(t, ctx, func(*ClusterResourceModel) {}); diags.HasError() {
		t.Errorf("docker should be valid: %v", diags)
	}
}

func TestValidateLifecycleConfig_InfrastructureExactlyOne(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Infrastructure = mustProviderMap(t, ctx, map[string]ProviderModel{"docker": emptyProviderModel(), "aws": emptyProviderModel()})
	})
	if !hasErrorContaining(diags, "exactly one") {
		t.Errorf("two infrastructure providers must be rejected: %v", diags)
	}
	diags = validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Infrastructure = mustProviderMap(t, ctx, map[string]ProviderModel{})
	})
	if !hasErrorContaining(diags, "exactly one") {
		t.Errorf("empty infrastructure map must be rejected: %v", diags)
	}
}

func TestValidateLifecycleConfig_UnsupportedProvider(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Infrastructure = mustProviderMap(t, ctx, map[string]ProviderModel{"gcp": emptyProviderModel()})
	})
	if !hasErrorContaining(diags, "not supported") {
		t.Errorf("gcp must be rejected: %v", diags)
	}
}

func TestValidateLifecycleConfig_TinkerbellRequiresSelfManaged(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Infrastructure = mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": emptyProviderModel()})
	})
	if !hasErrorContaining(diags, "self_managed") {
		t.Errorf("tinkerbell without self_managed must fail: %v", diags)
	}
}

func TestValidateLifecycleConfig_TinkerbellWithTalos(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Infrastructure = mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": emptyProviderModel()})
		d.Management = selfManaged(t, ctx)
		d.Bootstrap = mustProviderMap(t, ctx, map[string]ProviderModel{"talos": emptyProviderModel()})
		d.ControlPlane = mustProviderMap(t, ctx, map[string]ProviderModel{"talos": emptyProviderModel()})
	})
	if diags.HasError() {
		t.Errorf("tinkerbell+talos should be valid: %v", diags)
	}
}

func TestValidateLifecycleConfig_TinkerbellInvalidBootstrap(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Infrastructure = mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": emptyProviderModel()})
		d.Management = selfManaged(t, ctx)
		d.Bootstrap = mustProviderMap(t, ctx, map[string]ProviderModel{"microk8s": emptyProviderModel()})
	})
	if !hasErrorContaining(diags, "bootstrap") {
		t.Errorf("microk8s bootstrap must be rejected for tinkerbell: %v", diags)
	}
}

func TestValidateLifecycleConfig_FetchConfigExclusivity(t *testing.T) {
	ctx := context.Background()
	mk := func(fc FetchConfigModel) func(*ClusterResourceModel) {
		return func(d *ClusterResourceModel) {
			obj, _ := types.ObjectValueFrom(ctx, fetchConfigAttrTypes(), fc)
			pm := emptyProviderModel()
			pm.FetchConfig = obj
			d.Addon = mustProviderMap(t, ctx, map[string]ProviderModel{"helm": pm})
		}
	}
	null := types.StringNull()
	if diags := validateWith(t, ctx, mk(FetchConfigModel{Owner: types.StringValue("a"), Repository: null, URL: types.StringValue("https://x"), OCI: null})); !hasErrorContaining(diags, "fetch_config") {
		t.Errorf("owner+url must fail: %v", diags)
	}
	if diags := validateWith(t, ctx, mk(FetchConfigModel{Owner: null, Repository: null, URL: types.StringValue("https://x"), OCI: types.StringValue("oci://y")})); !hasErrorContaining(diags, "fetch_config") {
		t.Errorf("url+oci must fail: %v", diags)
	}
	if diags := validateWith(t, ctx, mk(FetchConfigModel{Owner: types.StringValue("a"), Repository: types.StringValue("b"), URL: null, OCI: null})); diags.HasError() {
		t.Errorf("owner+repository is valid: %v", diags)
	}
}

func TestValidateLifecycleConfig_PatchExclusivity(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		pm := emptyProviderModel()
		pm.ManifestPatches, _ = types.ListValueFrom(ctx, types.StringType, []string{"{}"})
		pm.Patches, _ = types.ListValueFrom(ctx, types.ObjectType{AttrTypes: patchAttrTypes()},
			[]PatchModel{{Patch: types.StringValue("{}"), Target: types.ObjectNull(patchSelectorAttrTypes())}})
		d.Addon = mustProviderMap(t, ctx, map[string]ProviderModel{"helm": pm})
	})
	if !hasErrorContaining(diags, "manifest_patches") {
		t.Errorf("patches and manifest_patches together must fail: %v", diags)
	}
}

func TestValidateLifecycleConfig_DuplicateMachineDeployment(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Topology = topologyValue(t, ctx, 1, []MachineDeploymentModel{
			machineDeployment(t, ctx, "md-0", 1, nil), machineDeployment(t, ctx, "md-0", 2, nil),
		})
	})
	if !hasErrorContaining(diags, "Duplicate machine deployment") {
		t.Errorf("duplicate names must fail: %v", diags)
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
	disk     string // e.g. "/dev/sda"; empty leaves disk null
	bmc      string // BMC address; empty leaves bmc null
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

		diskVal := types.ObjectNull(diskAttrTypes())
		if m.disk != "" {
			v, d := types.ObjectValueFrom(ctx, diskAttrTypes(), DiskModel{Device: types.StringValue(m.disk)})
			if d.HasError() {
				t.Fatalf("build disk: %v", d)
			}
			diskVal = v
		}
		bmcVal := types.ObjectNull(bmcAttrTypes())
		if m.bmc != "" {
			v, d := types.ObjectValueFrom(ctx, bmcAttrTypes(), BMCModel{
				Address:  types.StringValue(m.bmc),
				Username: types.StringValue("admin"),
				Password: types.StringValue("secret"),
			})
			if d.HasError() {
				t.Fatalf("build bmc: %v", d)
			}
			bmcVal = v
		}

		machineObjects = append(machineObjects, MachineModel{
			Hostname: types.StringValue(m.hostname),
			Network:  netVal,
			Disk:     diskVal,
			BMC:      bmcVal,
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

	v0, ok := upgraders[0]
	if !ok || v0.PriorSchema == nil || v0.StateUpgrader == nil {
		t.Error("expected v0 upgrader with PriorSchema and StateUpgrader")
	}
	v1, ok := upgraders[1]
	if !ok || v1.PriorSchema != nil || v1.StateUpgrader == nil {
		t.Error("expected raw-JSON v1 upgrader without PriorSchema")
	}
}
