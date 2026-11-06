// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// --- Schema tests ---

func TestClusterResource_Schema_V5Shape(t *testing.T) {
	ctx := context.Background()
	resp := &resource.SchemaResponse{}
	NewClusterResource().Schema(ctx, resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	if resp.Schema.Version != 5 {
		t.Errorf("schema version = %d, want 5", resp.Schema.Version)
	}
	for _, gone := range []string{"cluster_class", "topology", "output", "core", "bootstrap", "ipam", "addon", "cni", "template", "flavor"} {
		if _, ok := resp.Schema.Attributes[gone]; ok {
			t.Errorf("%s must be gone from the top level", gone)
		}
	}
	if ns, ok := resp.Schema.Attributes["namespace"].(schema.StringAttribute); !ok || !ns.Optional || !ns.Computed || len(ns.PlanModifiers) == 0 {
		t.Error("namespace must be an optional, computed, RequiresReplace string")
	}
	infra, ok := resp.Schema.Attributes["infrastructure"].(schema.SingleNestedAttribute)
	if !ok || !infra.Optional {
		t.Fatal("infrastructure must be an optional SingleNestedAttribute")
	}
	if p, ok := infra.Attributes["provider"].(schema.StringAttribute); !ok || !p.Required {
		t.Error("infrastructure.provider must be required")
	}
	if _, ok := infra.Attributes["spec"].(schema.DynamicAttribute); !ok {
		t.Error("infrastructure.spec must be dynamic")
	}
	cp, ok := resp.Schema.Attributes["control_plane"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("control_plane must be a SingleNestedAttribute")
	}
	for _, name := range []string{"provider", "replicas", "infrastructure", "spec"} {
		if _, ok := cp.Attributes[name]; !ok {
			t.Errorf("control_plane missing %s", name)
		}
	}
	if ref, ok := cp.Attributes["infrastructure"].(schema.SingleNestedAttribute); !ok || ref.Required {
		t.Error("control_plane.infrastructure must be an optional template ref")
	}
	class, ok := resp.Schema.Attributes["class"].(schema.SingleNestedAttribute)
	if !ok || !class.Optional {
		t.Fatal("class must be an optional SingleNestedAttribute")
	}
	if n, ok := class.Attributes["name"].(schema.StringAttribute); !ok || !n.Required {
		t.Error("class.name must be required")
	}
	mgmt, ok := resp.Schema.Attributes["management"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management must be a SingleNestedAttribute")
	}
	for _, gone := range []string{"kubeconfig", "skip_init", "uninstall", "namespace", "addons"} {
		if _, ok := mgmt.Attributes[gone]; ok {
			t.Errorf("management.%s must be gone", gone)
		}
	}
	providers, ok := mgmt.Attributes["providers"].(schema.SingleNestedAttribute)
	if !ok || !providers.Optional {
		t.Fatal("management.providers must be an optional SingleNestedAttribute")
	}
	reference := providerNestedObject().Attributes
	for _, name := range providerMapAttrs {
		m, ok := providers.Attributes[name].(schema.MapNestedAttribute)
		if !ok {
			t.Fatalf("management.providers.%s must be a MapNestedAttribute", name)
		}
		if m.Required {
			t.Errorf("management.providers.%s must be optional", name)
		}
		if len(m.NestedObject.Attributes) != len(reference) {
			t.Errorf("management.providers.%s nested object differs from the shared provider object", name)
		}
		switch name {
		case "infrastructure", "bootstrap", "control_plane":
			if len(m.PlanModifiers) != 1 {
				t.Errorf("management.providers.%s must RequiresReplace on a name change", name)
			}
		default:
			if len(m.PlanModifiers) != 0 {
				t.Errorf("management.providers.%s must be fully mutable", name)
			}
		}
	}
	bs, ok := mgmt.Attributes["bootstrap"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("management.bootstrap must be a SingleNestedAttribute")
	}
	if of, ok := bs.Attributes["on_failure"].(schema.StringAttribute); !ok || !of.Computed {
		t.Error("management.bootstrap.on_failure must be a computed string with a default")
	}
	wait, ok := resp.Schema.Attributes["wait"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatal("wait must be a SingleNestedAttribute")
	}
	if _, ok := wait.Attributes["enabled"]; ok {
		t.Error("wait.enabled is gone: wait.conditions replaces it")
	}
	if c, ok := wait.Attributes["conditions"].(schema.ListAttribute); !ok || !c.Computed {
		t.Error("wait.conditions must be a computed list with a default")
	}
	status, ok := resp.Schema.Attributes["status"].(schema.SingleNestedAttribute)
	if !ok || !status.Computed || status.Optional {
		t.Fatal("status must be computed only")
	}
	for _, name := range []string{"phase", "error", "endpoint", "ca_certificate", "description", "bootstrap_cluster", "conditions"} {
		if _, ok := status.Attributes[name]; !ok {
			t.Errorf("status missing %s", name)
		}
	}
	if _, ok := status.Attributes["kubeconfig"]; ok {
		t.Error("status.kubeconfig is gone: ephemeral.capi_kubeconfig replaces it")
	}
	if _, ok := resp.Schema.Attributes["provider_secrets"].(schema.DynamicAttribute); !ok {
		t.Error("provider_secrets must be dynamic")
	}
}

// --- builders ---

func mustProviderMap(t *testing.T, ctx context.Context, entries map[string]ProviderModel) types.Map {
	t.Helper()
	v, diags := types.MapValueFrom(ctx, providerMapType().ElemType, entries)
	if diags.HasError() {
		t.Fatalf("building provider map: %v", diags)
	}
	return v
}

func emptyProviderModel() ProviderModel { return nullProviderModel() }

func templateRef(t *testing.T, ctx context.Context, kind, name string) types.Object {
	t.Helper()
	return mustObject(t, ctx, templateRefAttrTypes(), TemplateRefModel{
		APIGroup: types.StringValue("infrastructure.cluster.x-k8s.io"), APIVersion: types.StringValue("infrastructure.cluster.x-k8s.io/v1beta1"),
		Kind: types.StringValue(kind), Name: types.StringValue(name), Namespace: types.StringValue("default"),
	})
}

func controlPlaneValue(t *testing.T, ctx context.Context, provider string, replicas int64, ref types.Object, spec types.Dynamic) types.Object {
	t.Helper()
	cp := ControlPlaneSpecModel{Provider: stringOrNull(provider), Replicas: types.Int64Value(replicas), Infrastructure: ref, MachineSpec: types.DynamicNull(), Spec: spec}
	if replicas == 0 {
		cp.Replicas = types.Int64Null()
	}
	return mustObject(t, ctx, controlPlaneSpecAttrTypes(), cp)
}

// baseModel is a direct-mode docker cluster on an external management cluster.
func baseModel(t *testing.T, ctx context.Context) *ClusterResourceModel {
	t.Helper()
	return &ClusterResourceModel{
		Name:            types.StringValue("test"),
		Namespace:       types.StringValue("default"),
		Network:         types.ObjectNull(clusterNetworkAttrTypes()),
		Infrastructure:  mustObject(t, ctx, infrastructureSpecAttrTypes(), InfrastructureSpecModel{Provider: types.StringValue("docker"), Spec: types.DynamicNull()}),
		ControlPlane:    controlPlaneValue(t, ctx, "kubeadm", 1, templateRef(t, ctx, "DockerMachineTemplate", "cp-aaaa"), types.DynamicNull()),
		Class:           types.ObjectNull(classRefAttrTypes()),
		Management:      types.ObjectNull(managementAttrTypes()),
		Manifests:       types.ObjectNull(manifestsAttrTypes()),
		Inventory:       types.ObjectNull(inventoryAttrTypes()),
		Wait:            types.ObjectNull(waitAttrTypes()),
		Status:          types.ObjectNull(statusAttrTypes()),
		ProviderSecrets: types.DynamicNull(),
	}
}

// providersValue builds management.providers from per-type maps.
func providersValue(t *testing.T, ctx context.Context, maps map[string]types.Map) types.Object {
	t.Helper()
	null := types.MapNull(providerMapType().ElemType)
	pv := ProvidersModel{Core: null, Infrastructure: null, Bootstrap: null, ControlPlane: null, IPAM: null, Addon: null}
	for name, m := range maps {
		switch name {
		case "core":
			pv.Core = m
		case "infrastructure":
			pv.Infrastructure = m
		case "bootstrap":
			pv.Bootstrap = m
		case "control_plane":
			pv.ControlPlane = m
		case "ipam":
			pv.IPAM = m
		case "addon":
			pv.Addon = m
		default:
			t.Fatalf("unknown provider map %s", name)
		}
	}
	return mustObject(t, ctx, providersAttrTypes(), pv)
}

// selfManagedWith builds a self-managed management block with the given providers.
func selfManagedWith(t *testing.T, ctx context.Context, providers types.Object, bootstrap types.Object) types.Object {
	t.Helper()
	return mustObject(t, ctx, managementAttrTypes(), ManagementModel{SelfManaged: types.BoolValue(true), Providers: providers, Bootstrap: bootstrap})
}

func selfManaged(t *testing.T, ctx context.Context) types.Object {
	t.Helper()
	return selfManagedWith(t, ctx, types.ObjectNull(providersAttrTypes()), types.ObjectNull(managementBootstrapAttrTypes()))
}

func networkValue(t *testing.T, ctx context.Context, host string, port int64, pods, services []string, domain string) types.Object {
	t.Helper()
	ep, d := types.ObjectValueFrom(ctx, endpointAttrTypes(), EndpointModel{Host: types.StringValue(host), Port: types.Int64Value(port)})
	if d.HasError() {
		t.Fatal(d)
	}
	ranges := func(cidrs []string) types.Object {
		l, d := types.ListValueFrom(ctx, types.StringType, cidrs)
		if d.HasError() {
			t.Fatal(d)
		}
		o, d := types.ObjectValueFrom(ctx, networkRangesAttrTypes(), NetworkRangesModel{CIDRBlocks: l})
		if d.HasError() {
			t.Fatal(d)
		}
		return o
	}
	net, d := types.ObjectValueFrom(ctx, clusterNetworkAttrTypes(), ClusterNetworkModel{
		ControlPlaneEndpoint: ep, Pods: ranges(pods), Services: ranges(services), ServiceDomain: types.StringValue(domain),
	})
	if d.HasError() {
		t.Fatal(d)
	}
	return net
}

// --- extraction ---

func TestExtractManagement_Null(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{Management: types.ObjectNull(managementAttrTypes())}
	mgmt, diags := extractManagement(ctx, data)
	if mgmt != nil || diags.HasError() {
		t.Errorf("null management = %v %v", mgmt, diags)
	}
}

func TestExtractManagement_Populated(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{Management: selfManaged(t, ctx)}
	mgmt, diags := extractManagement(ctx, data)
	if diags.HasError() || mgmt == nil || !mgmt.SelfManaged.ValueBool() || !mgmt.Providers.IsNull() {
		t.Errorf("management = %+v %v", mgmt, diags)
	}
}

func TestProviderMaps_ReadFromManagementProviders(t *testing.T) {
	ctx := context.Background()
	data := baseModel(t, ctx)
	if maps := data.providerMaps(ctx); len(maps) != 6 {
		t.Fatalf("maps = %d", len(maps))
	}
	for _, pm := range data.providerMaps(ctx) {
		if !pm.Map.IsNull() {
			t.Errorf("%s must be null without management.providers", pm.Type)
		}
	}
	tink := emptyProviderModel()
	tink.Version = types.StringValue("v0.7.9")
	data.Management = selfManagedWith(t, ctx, providersValue(t, ctx, map[string]types.Map{
		"infrastructure": mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": tink}),
		"addon":          mustProviderMap(t, ctx, map[string]ProviderModel{"helm": emptyProviderModel()}),
	}), types.ObjectNull(managementBootstrapAttrTypes()))
	set, diags := extractProviderSet(ctx, data)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if infra, ok := set.Infrastructure(); !ok || infra.InitString() != "tinkerbell:v0.7.9" {
		t.Errorf("infrastructure = %+v", infra)
	}
	if got := set.InitStrings(capi.ProviderTypeAddon); len(got) != 1 || got[0] != "helm" {
		t.Errorf("addon = %v", got)
	}
}

func TestExtractProviders_Null(t *testing.T) {
	ctx := context.Background()
	entries, diags := extractProviders(ctx, types.MapNull(providerMapType().ElemType))
	if entries != nil || diags.HasError() {
		t.Errorf("null map = %v %v", entries, diags)
	}
}

func TestExtractControlPlaneAndClass(t *testing.T) {
	ctx := context.Background()
	data := baseModel(t, ctx)
	cp, diags := extractControlPlane(ctx, data)
	if diags.HasError() || cp == nil || cp.Provider.ValueString() != "kubeadm" || cp.Replicas.ValueInt64() != 1 {
		t.Errorf("control plane = %+v %v", cp, diags)
	}
	if class, _ := extractClass(ctx, data); class != nil {
		t.Error("no class by default")
	}
	data.Class = mustObject(t, ctx, classRefAttrTypes(), ClassRefModel{Name: types.StringValue("main"), Namespace: types.StringValue("classes")})
	class, diags := extractClass(ctx, data)
	if diags.HasError() || class == nil || class.Name != "main" || class.Namespace != "classes" {
		t.Errorf("class = %+v %v", class, diags)
	}
}

// --- status ---

func TestSetStatusWithFallback(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{}
	result := &capi.ClusterResult{
		Endpoint:         "https://new-endpoint:6443",
		BootstrapCluster: &capi.Cluster{Name: "test-bootstrap"},
		Conditions:       []capi.Condition{{Type: "Available", Status: "True"}},
	}
	prev := &StatusModel{
		Endpoint: types.StringValue("https://old-endpoint:6443"), CACertificate: types.StringValue("old-ca"),
		Description: types.StringNull(), BootstrapCluster: types.StringNull(), Conditions: types.ListNull(types.ObjectType{AttrTypes: conditionAttrTypes()}),
	}
	if diags := setStatusWithFallback(ctx, data, result, prev, capi.PhaseReady); diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	st := extractStatus(ctx, data)
	if st.Endpoint.ValueString() != "https://new-endpoint:6443" || st.CACertificate.ValueString() != "old-ca" || st.Phase.ValueString() != "ready" || !st.Error.IsNull() {
		t.Errorf("status = %+v", st)
	}
	if st.BootstrapCluster.ValueString() != "test-bootstrap" || st.Conditions.IsNull() {
		t.Errorf("status = %+v", st)
	}
	// Without a bootstrap in the result, the bootstrap is gone.
	result.BootstrapCluster = nil
	_ = setStatusWithFallback(ctx, data, result, prev, capi.PhaseReady)
	st = extractStatus(ctx, data)
	if !st.BootstrapCluster.IsNull() {
		t.Error("bootstrap_cluster must clear once the bootstrap is gone")
	}
}

func TestSetPartialStatus(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{}
	pe := &capi.PhaseError{Phase: capi.PhasePaused, Bootstrap: &capi.Cluster{Name: "test-bootstrap"}, Err: context.DeadlineExceeded}
	if diags := setPartialStatus(ctx, data, pe, nil, false); diags.HasError() {
		t.Fatal(diags)
	}
	st := extractStatus(ctx, data)
	if st.Phase.ValueString() != "paused" || !strings.Contains(st.Error.ValueString(), "deadline") || st.BootstrapCluster.ValueString() != "test-bootstrap" {
		t.Errorf("partial status = %+v", st)
	}
}

func TestNullStatus(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{}
	if diags := nullStatus(ctx, data); diags.HasError() {
		t.Fatal(diags)
	}
	st := extractStatus(ctx, data)
	if !st.Endpoint.IsNull() || !st.Phase.IsNull() || !st.Conditions.IsNull() {
		t.Errorf("null status = %+v", st)
	}
}

// --- yamlObject ---

func TestYAMLObject_ConvertsYAMLAndJSON(t *testing.T) {
	want := map[string]any{"a": map[string]any{"b": int64(1)}, "list": []any{"x"}}
	for _, in := range []string{"a:\n  b: 1\nlist: [x]\n", `{"a": {"b": 1}, "list": ["x"]}`} {
		got, diags := yamlObject(types.StringValue(in), "x")
		if diags.HasError() {
			t.Fatal(diags)
		}
		if got["a"].(map[string]any)["b"] != want["a"].(map[string]any)["b"] { //nolint:forcetypeassert // test fixture
			t.Errorf("yamlObject(%q) = %#v", in, got)
		}
	}
}

func TestYAMLObject_NullUnknownAndBlankYieldNil(t *testing.T) {
	for _, in := range []types.String{types.StringNull(), types.StringUnknown(), types.StringValue("  \n")} {
		if got, diags := yamlObject(in, "x"); got != nil || diags.HasError() {
			t.Errorf("yamlObject(%v) = %v %v", in, got, diags)
		}
	}
}

func TestYAMLObject_RejectsNonObjectAndInvalidYAML(t *testing.T) {
	for _, in := range []string{"- a\n- b\n", "just a string", "a: [unclosed"} {
		if _, diags := yamlObject(types.StringValue(in), "x"); !diags.HasError() {
			t.Errorf("yamlObject(%q) must fail", in)
		}
	}
}

// --- provider map plan modifier ---

func TestProviderMapRequiresReplace_IgnoresResourceConfig(t *testing.T) {
	ctx := context.Background()
	a := emptyProviderModel()
	a.ResourceConfig = types.StringValue("x: 1")
	b := emptyProviderModel()
	b.ResourceConfig = types.StringValue("x: 2")
	state := mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": a})
	plan := mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": b})
	if requiresReplace(t, ctx, state, plan) {
		t.Error("a resource_config change must not replace the cluster")
	}
	renamed := mustProviderMap(t, ctx, map[string]ProviderModel{"docker": b})
	if !requiresReplace(t, ctx, state, renamed) {
		t.Error("a provider name change must replace the cluster")
	}
}

func requiresReplace(t *testing.T, ctx context.Context, state, plan types.Map) bool {
	t.Helper()
	nonNull := tftypes.NewValue(tftypes.String, "x")
	resp := &planmodifier.MapResponse{PlanValue: plan}
	providerMapRequiresReplace().PlanModifyMap(ctx, planmodifier.MapRequest{
		StateValue: state, PlanValue: plan,
		State: tfsdk.State{Raw: nonNull}, Plan: tfsdk.Plan{Raw: nonNull},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.RequiresReplace
}

// --- buildCreateOptions ---

func TestBuildCreateOptions_Minimal(t *testing.T) {
	ctx := context.Background()
	opts, diags := buildCreateOptions(ctx, baseModel(t, ctx))
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	if opts.Infrastructure.Provider != "docker" || opts.Infrastructure.Spec != nil {
		t.Errorf("infrastructure = %+v", opts.Infrastructure)
	}
	if opts.ControlPlane.Provider != "kubeadm" || opts.ControlPlane.Replicas == nil || *opts.ControlPlane.Replicas != 1 || opts.ControlPlane.Template == nil || opts.ControlPlane.Template.Name != "cp-aaaa" || opts.ControlPlane.Template.Kind != "DockerMachineTemplate" {
		t.Errorf("control plane = %+v", opts.ControlPlane)
	}
	if !opts.WaitForReady || opts.Wait.Conditions != nil {
		t.Errorf("wait defaults: ready=%v conditions=%v", opts.WaitForReady, opts.Wait.Conditions)
	}
	if opts.Class != nil || opts.SelfManaged || len(opts.Providers) != 0 || opts.Namespace != "default" {
		t.Errorf("defaults: class=%v self=%v providers=%v ns=%s", opts.Class, opts.SelfManaged, opts.Providers, opts.Namespace)
	}
	if opts.Network.ControlPlaneEndpoint != nil || opts.Network.PodCIDRs != nil {
		t.Errorf("network must be unset by default: %+v", opts.Network)
	}
}

func TestBuildCreateOptions_Full(t *testing.T) {
	ctx := context.Background()
	data := baseModel(t, ctx)
	data.Namespace = types.StringValue("capi")
	data.KubernetesVersion = types.StringValue("v1.34.0")
	data.Network = networkValue(t, ctx, "10.0.0.160", 6443, []string{"10.244.0.0/16"}, []string{"10.96.0.0/12"}, "cluster.local")

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
	tink.ResourceConfig = types.StringValue(`{"templateInline": "version: \"0.1\""}`)
	talosBS := emptyProviderModel()
	talosBS.Version = types.StringValue("v0.8.2")
	talosCP := emptyProviderModel()
	talosCP.Version = types.StringValue("v0.7.1")
	core := emptyProviderModel()
	core.Version = types.StringValue("v1.12.2")
	data.Management = selfManagedWith(t, ctx, providersValue(t, ctx, map[string]types.Map{
		"infrastructure": mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": tink}),
		"bootstrap":      mustProviderMap(t, ctx, map[string]ProviderModel{"talos": talosBS}),
		"control_plane":  mustProviderMap(t, ctx, map[string]ProviderModel{"talos": talosCP}),
		"core":           mustProviderMap(t, ctx, map[string]ProviderModel{"cluster-api": core}),
		"addon":          mustProviderMap(t, ctx, map[string]ProviderModel{"helm": emptyProviderModel()}),
	}), types.ObjectNull(managementBootstrapAttrTypes()))
	data.Infrastructure = mustObject(t, ctx, infrastructureSpecAttrTypes(), InfrastructureSpecModel{
		Provider: types.StringValue("tinkerbell"), Spec: types.DynamicValue(mustObj(t, map[string]attr.Value{"imageLookupFormat": types.StringValue("x")})),
	})
	data.ControlPlane = controlPlaneValue(t, ctx, "talos", 3, templateRef(t, ctx, "TinkerbellMachineTemplate", "cp-1234"),
		types.DynamicValue(mustObj(t, map[string]attr.Value{"controlPlaneConfig": mustObj(t, map[string]attr.Value{"controlplane": mustObj(t, map[string]attr.Value{"talosVersion": types.StringValue("v1.13")})})})))
	conditions, _ := types.ListValueFrom(ctx, types.StringType, []string{"ControlPlaneInitialized", "Available"})
	data.Wait = mustObject(t, ctx, waitAttrTypes(), WaitModel{Conditions: conditions, Timeout: types.StringValue("90m")})

	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("unexpected error: %v", diags)
	}
	if opts.Namespace != "capi" || !opts.SelfManaged || opts.KubernetesVersion != "v1.34.0" {
		t.Errorf("identity = %s self=%v version=%s", opts.Namespace, opts.SelfManaged, opts.KubernetesVersion)
	}
	infra, _ := opts.Providers.Infrastructure()
	if infra.InitString() != "tinkerbell:v0.7.9" || infra.FetchConfig == nil || infra.FetchConfig.Owner != "tinkerbell-community" || infra.ResourceConfig["templateInline"] != "version: \"0.1\"" {
		t.Errorf("infra = %+v", infra)
	}
	if infra.Manager == nil || !infra.Manager.FeatureGates["ClusterTopology"] {
		t.Errorf("infra manager = %+v", infra.Manager)
	}
	for typ, want := range map[capi.ProviderType]string{capi.ProviderTypeBootstrap: "talos:v0.8.2", capi.ProviderTypeControlPlane: "talos:v0.7.1", capi.ProviderTypeCore: "cluster-api:v1.12.2", capi.ProviderTypeAddon: "helm"} {
		if got := opts.Providers.InitStrings(typ); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %s", typ, got, want)
		}
	}
	if opts.Infrastructure.Provider != "tinkerbell" || opts.Infrastructure.Spec["imageLookupFormat"] != "x" {
		t.Errorf("infrastructure = %+v", opts.Infrastructure)
	}
	if opts.ControlPlane.Provider != "talos" || *opts.ControlPlane.Replicas != 3 || opts.ControlPlane.Template.Name != "cp-1234" || opts.ControlPlane.Template.APIVersion != "infrastructure.cluster.x-k8s.io/v1beta1" {
		t.Errorf("control plane = %+v", opts.ControlPlane)
	}
	if v, _ := opts.ControlPlane.Spec["controlPlaneConfig"].(map[string]any); v == nil || v["controlplane"].(map[string]any)["talosVersion"] != "v1.13" { //nolint:forcetypeassert // test fixture
		t.Errorf("control plane spec = %v", opts.ControlPlane.Spec)
	}
	if ep := opts.Network.ControlPlaneEndpoint; ep == nil || ep.Host != "10.0.0.160" || ep.Port != 6443 || opts.Network.ServiceDomain != "cluster.local" {
		t.Errorf("network = %+v", opts.Network)
	}
	if !opts.WaitForReady || len(opts.Wait.Conditions) != 2 || opts.Wait.Conditions[1] != "Available" || opts.Wait.Timeout.Minutes() != 90 {
		t.Errorf("wait = %v %v %v", opts.WaitForReady, opts.Wait.Conditions, opts.Wait.Timeout)
	}
}

func TestBuildCreateOptions_OwnMachineSpec(t *testing.T) {
	ctx := context.Background()
	data := baseModel(t, ctx)
	data.ControlPlane = mustObject(t, ctx, controlPlaneSpecAttrTypes(), ControlPlaneSpecModel{
		Provider: types.StringValue("kubeadm"), Replicas: types.Int64Value(1), Infrastructure: types.ObjectNull(templateRefAttrTypes()),
		MachineSpec: types.DynamicValue(mustObj(t, map[string]attr.Value{"hardwareName": types.StringValue("cp-1")})), Spec: types.DynamicNull(),
	})
	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if opts.ControlPlane.Template != nil || opts.ControlPlane.TemplateSpec["hardwareName"] != "cp-1" {
		t.Errorf("control plane = %+v", opts.ControlPlane)
	}
}

func TestBuildCreateOptions_ClassAndNoWait(t *testing.T) {
	ctx := context.Background()
	data := baseModel(t, ctx)
	data.KubernetesVersion = types.StringValue("v1.34.0")
	data.Infrastructure = types.ObjectNull(infrastructureSpecAttrTypes())
	data.Class = mustObject(t, ctx, classRefAttrTypes(), ClassRefModel{Name: types.StringValue("main"), Namespace: types.StringNull()})
	data.ControlPlane = controlPlaneValue(t, ctx, "", 3, types.ObjectNull(templateRefAttrTypes()), types.DynamicNull())
	empty, _ := types.ListValueFrom(ctx, types.StringType, []string{})
	data.Wait = mustObject(t, ctx, waitAttrTypes(), WaitModel{Conditions: empty, Timeout: types.StringNull()})
	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if opts.Class == nil || opts.Class.Name != "main" || opts.Infrastructure.Provider != "" || opts.ControlPlane.Template != nil || *opts.ControlPlane.Replicas != 3 {
		t.Errorf("class options = %+v %+v %+v", opts.Class, opts.Infrastructure, opts.ControlPlane)
	}
	if opts.WaitForReady {
		t.Error("an empty condition list waits for nothing")
	}
}

func TestBuildCreateOptions_ProviderOrderIsDeterministic(t *testing.T) {
	ctx := context.Background()
	data := baseModel(t, ctx)
	data.Management = selfManagedWith(t, ctx, providersValue(t, ctx, map[string]types.Map{
		"infrastructure": mustProviderMap(t, ctx, map[string]ProviderModel{"docker": emptyProviderModel()}),
		"addon":          mustProviderMap(t, ctx, map[string]ProviderModel{"zeta": emptyProviderModel(), "alpha": emptyProviderModel(), "mid": emptyProviderModel()}),
	}), types.ObjectNull(managementBootstrapAttrTypes()))
	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatal(diags)
	}
	got := opts.Providers.InitStrings(capi.ProviderTypeAddon)
	if len(got) != 3 || got[0] != "alpha" || got[1] != "mid" || got[2] != "zeta" {
		t.Errorf("addon order = %v, want sorted by name", got)
	}
}

// --- Validation ---

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

func TestValidateLifecycleConfig_SelfManagedNeedsOneInfrastructure(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Management = selfManaged(t, ctx)
	})
	if !hasErrorContaining(diags, "exactly one provider") {
		t.Errorf("a self-managed cluster without management.providers.infrastructure must be rejected: %v", diags)
	}
	diags = validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Management = selfManagedWith(t, ctx, providersValue(t, ctx, map[string]types.Map{
			"infrastructure": mustProviderMap(t, ctx, map[string]ProviderModel{"docker": emptyProviderModel(), "aws": emptyProviderModel()}),
		}), types.ObjectNull(managementBootstrapAttrTypes()))
	})
	if !hasErrorContaining(diags, "exactly one provider") {
		t.Errorf("two infrastructure providers must be rejected: %v", diags)
	}
}

func TestValidateLifecycleConfig_UnsupportedProvider(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Infrastructure = mustObject(t, ctx, infrastructureSpecAttrTypes(), InfrastructureSpecModel{Provider: types.StringValue("metal3"), Spec: types.DynamicNull()})
	})
	if !hasErrorContaining(diags, "not supported") {
		t.Errorf("unsupported provider must be rejected: %v", diags)
	}
}

func TestValidateLifecycleConfig_ProvidersMustAgree(t *testing.T) {
	ctx := context.Background()
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Management = selfManagedWith(t, ctx, providersValue(t, ctx, map[string]types.Map{
			"infrastructure": mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": emptyProviderModel()}),
			"control_plane":  mustProviderMap(t, ctx, map[string]ProviderModel{"talos": emptyProviderModel()}),
		}), types.ObjectNull(managementBootstrapAttrTypes()))
	})
	if !hasErrorContaining(diags, `infrastructure.provider "docker"`) || !hasErrorContaining(diags, `control_plane.provider "kubeadm"`) {
		t.Errorf("the cluster's providers must be the ones management.providers installs: %v", diags)
	}
}

func TestValidateLifecycleConfig_TinkerbellProviders(t *testing.T) {
	ctx := context.Background()
	tinkerbell := func(d *ClusterResourceModel, bootstrap, cp string) {
		d.Infrastructure = mustObject(t, ctx, infrastructureSpecAttrTypes(), InfrastructureSpecModel{Provider: types.StringValue("tinkerbell"), Spec: types.DynamicNull()})
		d.ControlPlane = controlPlaneValue(t, ctx, cp, 1, templateRef(t, ctx, "TinkerbellMachineTemplate", "cp-1"), types.DynamicNull())
		d.Management = selfManagedWith(t, ctx, providersValue(t, ctx, map[string]types.Map{
			"infrastructure": mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": emptyProviderModel()}),
			"bootstrap":      mustProviderMap(t, ctx, map[string]ProviderModel{bootstrap: emptyProviderModel()}),
			"control_plane":  mustProviderMap(t, ctx, map[string]ProviderModel{cp: emptyProviderModel()}),
		}), types.ObjectNull(managementBootstrapAttrTypes()))
	}
	if diags := validateWith(t, ctx, func(d *ClusterResourceModel) { tinkerbell(d, "talos", "talos") }); diags.HasError() {
		t.Errorf("tinkerbell with talos is valid: %v", diags)
	}
	if diags := validateWith(t, ctx, func(d *ClusterResourceModel) { tinkerbell(d, "microk8s", "talos") }); !hasErrorContaining(diags, "kubeadm or talos") {
		t.Errorf("tinkerbell rejects other bootstrap providers: %v", diags)
	}
	// Tinkerbell on an external management cluster that already runs it is fine.
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Infrastructure = mustObject(t, ctx, infrastructureSpecAttrTypes(), InfrastructureSpecModel{Provider: types.StringValue("tinkerbell"), Spec: types.DynamicNull()})
		d.ControlPlane = controlPlaneValue(t, ctx, "talos", 1, templateRef(t, ctx, "TinkerbellMachineTemplate", "cp-1"), types.DynamicNull())
	})
	if diags.HasError() {
		t.Errorf("tinkerbell on an external management cluster is valid: %v", diags)
	}
}

func TestValidateLifecycleConfig_FetchConfigExclusivity(t *testing.T) {
	ctx := context.Background()
	fetch, _ := types.ObjectValueFrom(ctx, fetchConfigAttrTypes(), FetchConfigModel{
		Owner: types.StringValue("me"), Repository: types.StringNull(), URL: types.StringValue("https://x/y.yaml"), OCI: types.StringNull(),
	})
	p := emptyProviderModel()
	p.FetchConfig = fetch
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Management = selfManagedWith(t, ctx, providersValue(t, ctx, map[string]types.Map{
			"infrastructure": mustProviderMap(t, ctx, map[string]ProviderModel{"docker": p}),
		}), types.ObjectNull(managementBootstrapAttrTypes()))
	})
	if !hasErrorContaining(diags, "url") {
		t.Errorf("url and owner are exclusive: %v", diags)
	}
}

func TestValidateLifecycleConfig_ResourceConfigOnlyOnClusterProviders(t *testing.T) {
	ctx := context.Background()
	p := emptyProviderModel()
	p.ResourceConfig = types.StringValue("x: 1")
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Management = selfManagedWith(t, ctx, providersValue(t, ctx, map[string]types.Map{
			"infrastructure": mustProviderMap(t, ctx, map[string]ProviderModel{"docker": emptyProviderModel()}),
			"addon":          mustProviderMap(t, ctx, map[string]ProviderModel{"helm": p}),
		}), types.ObjectNull(managementBootstrapAttrTypes()))
	})
	if !hasErrorContaining(diags, "resource_config") {
		t.Errorf("resource_config on an addon must be rejected: %v", diags)
	}
}

// --- inventory ---

func inventoryWith(t *testing.T, ctx context.Context, machines []testMachine) types.Object {
	t.Helper()
	return mustObject(t, ctx, inventoryAttrTypes(), InventoryModel{
		Manage: types.BoolValue(true), Source: types.StringNull(), Machine: buildTestMachineList(ctx, t, machines),
	})
}

func TestValidateInventory_SourceAndMachineMutuallyExclusive(t *testing.T) {
	ctx := context.Background()
	inv := &InventoryModel{Manage: types.BoolValue(true), Source: types.StringValue("x.csv"), Machine: buildTestMachineList(ctx, t, []testMachine{{hostname: "a", ip: "10.0.0.1", mac: "aa"}})}
	var diags diag.Diagnostics
	validateInventory(ctx, inv, 0, 0, &diags)
	if !hasErrorContaining(diags, "not both") {
		t.Errorf("source and machine must be exclusive: %v", diags)
	}
}

func TestValidateInventory_Duplicates(t *testing.T) {
	ctx := context.Background()
	for name, machines := range map[string][]testMachine{
		"hostname": {{hostname: "a", ip: "10.0.0.1", mac: "aa"}, {hostname: "a", ip: "10.0.0.2", mac: "bb"}},
		"ip":       {{hostname: "a", ip: "10.0.0.1", mac: "aa"}, {hostname: "b", ip: "10.0.0.1", mac: "bb"}},
		"mac":      {{hostname: "a", ip: "10.0.0.1", mac: "aa"}, {hostname: "b", ip: "10.0.0.2", mac: "aa"}},
	} {
		inv := &InventoryModel{Manage: types.BoolValue(true), Source: types.StringNull(), Machine: buildTestMachineList(ctx, t, machines)}
		var diags diag.Diagnostics
		validateInventory(ctx, inv, 0, 0, &diags)
		if !hasErrorContaining(diags, "Duplicate") {
			t.Errorf("duplicate %s must be rejected: %v", name, diags)
		}
	}
}

func TestValidateInventory_ControlPlaneSeats(t *testing.T) {
	ctx := context.Background()
	machines := []testMachine{{hostname: "cp-1", ip: "10.0.0.1", mac: "aa", role: "cp"}, {hostname: "w-1", ip: "10.0.0.2", mac: "bb", role: "worker"}}
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.ControlPlane = controlPlaneValue(t, ctx, "kubeadm", 3, templateRef(t, ctx, "DockerMachineTemplate", "cp"), types.DynamicNull())
		d.Inventory = inventoryWith(t, ctx, machines)
	})
	if !hasErrorContaining(diags, "control plane") {
		t.Errorf("three control-plane replicas on one cp machine must be rejected: %v", diags)
	}
	diags = validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.Inventory = inventoryWith(t, ctx, machines)
	})
	if diags.HasError() {
		t.Errorf("one replica on one cp machine is fine: %v", diags)
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

		labels := types.MapNull(types.StringType)
		if m.role != "" {
			labelsMap, d := types.MapValueFrom(ctx, types.StringType, map[string]string{"type": m.role})
			if d.HasError() {
				t.Fatalf("build labels: %v", d)
			}
			labels = labelsMap
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

func TestStringOrNull(t *testing.T) {
	if v := stringOrNull("hello"); v.ValueString() != "hello" {
		t.Errorf("expected hello, got %q", v.ValueString())
	}
	if v := stringOrNull(""); !v.IsNull() {
		t.Error("expected null for empty string")
	}
}

func TestClusterResource_UpgradeState(t *testing.T) {
	ctx := context.Background()
	upgraders := (&ClusterResource{}).UpgradeState(ctx)
	for _, v := range []int64{0, 1, 2, 3, 4} {
		up, ok := upgraders[v]
		if !ok || up.PriorSchema != nil || up.StateUpgrader == nil {
			t.Errorf("expected raw-JSON v%d upgrader without PriorSchema", v)
		}
	}
}
