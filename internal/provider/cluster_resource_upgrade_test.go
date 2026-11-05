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
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

const v1State = `{
  "id": "acc-talos", "name": "acc-talos", "kubernetes_version": "v1.34.0", "flavor": null,
  "provider_secrets": null,
  "infrastructure": {"provider": "tinkerbell:v0.5.4"},
  "bootstrap": {"provider": "talos:v0.6.7"},
  "control_plane": {"provider": "talos:v0.6.7", "machine_count": 1},
  "core": null,
  "workers": {"machine_count": 2},
  "addons": [{
    "provider": "helm:v0.2.12",
    "config_variables": {"A": "b"}, "secret_config_variables": null,
    "fetch_config": {"url": "https://example.com/addon-components.yaml", "oci": null},
    "deployment": null,
    "manager": {"profiler_address": null, "max_concurrent_reconciles": null, "verbosity": 3, "feature_gates": {"X": true}, "additional_args": null},
    "additional_manifests": null, "manifest_patches": null, "patches": null
  }],
  "management": null, "inventory": null, "wait": null, "output": null, "status": null
}`

const v2State = `{
  "id": "acc", "name": "acc", "kubernetes_version": "v1.34.0", "flavor": "ha",
  "provider_secrets": null,
  "core": null, "ipam": null, "addon": null,
  "infrastructure": {"tinkerbell": {"version": "v0.7.9", "fetch_config": {"owner": "tinkerbell-community", "repository": null, "url": null, "oci": null}, "config_variables": null, "secret_config_variables": null, "deployment": null, "manager": null, "additional_manifests": null, "manifest_patches": null, "patches": null}},
  "bootstrap": {"talos": {"version": "v0.8.2", "fetch_config": null, "config_variables": null, "secret_config_variables": null, "deployment": null, "manager": null, "additional_manifests": null, "manifest_patches": null, "patches": null}},
  "control_plane": {"talos": {"version": "v0.7.1", "fetch_config": null, "config_variables": null, "secret_config_variables": null, "deployment": null, "manager": null, "additional_manifests": null, "manifest_patches": null, "patches": null}},
  "topology": {"control_plane": {"replicas": 1}, "workers": {"machine_deployments": [{"name": "md-0", "class": null, "replicas": 2, "failure_domain": null, "metadata": {"labels": {"tier": "worker"}, "annotations": null}}]}},
  "cni": null,
  "template": {"source": "/srv/cluster-template-talos.yaml", "variables": {"CONTROL_PLANE_VIP": "10.0.0.160"}},
  "management": null, "inventory": null, "wait": null, "output": null, "status": null
}`

// v4State is a self-managed Tinkerbell cluster as schema version 4 stored it.
const v4State = `{
  "id": "prod", "name": "prod", "kubernetes_version": "v1.34.0", "cluster_class": false,
  "provider_secrets": {"value": {"prod-talos/bundle": "x"}, "type": ["map", "string"]},
  "network": {"control_plane_endpoint": {"host": "10.0.0.160", "port": 6443}, "pods": null, "services": null, "service_domain": null},
  "core": {"cluster-api": {"version": "v1.14.0", "fetch_config": null, "config_variables": null, "secret_config_variables": null, "deployment": null, "manager": {"profiler_address": null, "max_concurrent_reconciles": null, "verbosity": null, "feature_gates": {"ClusterTopology": true}, "additional_args": null}, "additional_manifests": null, "manifest_patches": null, "patches": null, "resource_config": null}},
  "infrastructure": {"tinkerbell": {"version": "v0.7.9", "fetch_config": {"owner": "tinkerbell-community", "repository": null, "url": null, "oci": null}, "config_variables": null, "secret_config_variables": null, "deployment": null, "manager": null, "additional_manifests": null, "manifest_patches": null, "patches": null, "resource_config": "templateInline: x\n"}},
  "bootstrap": {"talos": {"version": "v0.8.2", "fetch_config": null, "config_variables": null, "secret_config_variables": null, "deployment": null, "manager": null, "additional_manifests": null, "manifest_patches": null, "patches": null, "resource_config": null}},
  "control_plane": {"talos": {"version": "v0.7.1", "fetch_config": null, "config_variables": null, "secret_config_variables": null, "deployment": null, "manager": null, "additional_manifests": null, "manifest_patches": null, "patches": null, "resource_config": "controlPlaneConfig: {}\n"}},
  "ipam": null, "addon": {"helm": {"version": null, "fetch_config": null, "config_variables": null, "secret_config_variables": null, "deployment": null, "manager": null, "additional_manifests": null, "manifest_patches": null, "patches": null, "resource_config": null}},
  "topology": {"control_plane": {"replicas": 3, "infrastructure": "hardwareAffinity: {}\n"}, "workers": {"machine_deployments": [{"name": "md-0", "class": null, "replicas": 2, "failure_domain": null, "metadata": null, "infrastructure": null, "bootstrap": null}]}},
  "management": {"kubeconfig": null, "skip_init": false, "uninstall": false, "self_managed": true, "namespace": "capi",
    "bootstrap": {"type": "kairos", "machine": "nuc", "boot": {"method": "manual", "timeout": "15m", "attempts": 3, "device": "cdrom"}, "kairos": null, "addons": null}},
  "manifests": null, "inventory": null,
  "wait": {"enabled": false, "timeout": "90m"},
  "output": {"kubeconfig_path": "/home/x/.kube/prod.kubeconfig"},
  "status": {"endpoint": "https://10.0.0.160:6443", "kubeconfig": "apiVersion: v1\n", "ca_certificate": "-----BEGIN", "description": "tree", "bootstrap_cluster": null}
}`

// decodeUpgraded decodes a raw-JSON upgrade result against the current schema.
func decodeUpgraded(t *testing.T, ctx context.Context, resp *resource.UpgradeStateResponse) ClusterResourceModel {
	t.Helper()
	if resp.DynamicValue == nil {
		t.Fatal("raw-JSON upgrader must set DynamicValue")
	}
	schema := currentSchema(t, ctx)
	val, err := resp.DynamicValue.Unmarshal(schema.Type().TerraformType(ctx))
	if err != nil {
		t.Fatalf("upgraded state does not decode against the current schema: %v\n%s", err, resp.DynamicValue.JSON)
	}
	var out ClusterResourceModel
	if d := (tfsdk.State{Schema: schema, Raw: val}).Get(ctx, &out); d.HasError() {
		t.Fatal(d)
	}
	return out
}

// currentSchema returns the current resource schema.
func currentSchema(t *testing.T, ctx context.Context) schema.Schema {
	t.Helper()
	resp := &resource.SchemaResponse{}
	NewClusterResource().Schema(ctx, resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.Schema
}

func upgradeFrom(t *testing.T, ctx context.Context, version int64, raw string) ClusterResourceModel {
	t.Helper()
	up := (&ClusterResource{}).UpgradeState(ctx)[version]
	if up.PriorSchema != nil {
		t.Fatalf("v%d upgrader must rewrite raw JSON without a prior schema", version)
	}
	resp := &resource.UpgradeStateResponse{}
	up.StateUpgrader(ctx, resource.UpgradeStateRequest{RawState: &tfprotov6.RawState{JSON: []byte(raw)}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return decodeUpgraded(t, ctx, resp)
}

func providersOf(t *testing.T, ctx context.Context, out *ClusterResourceModel) ProvidersModel {
	t.Helper()
	mgmt, _ := extractManagement(ctx, out)
	if mgmt == nil || mgmt.Providers.IsNull() {
		t.Fatal("management.providers must be set")
	}
	var pv ProvidersModel
	if d := mgmt.Providers.As(ctx, &pv, basetypes.ObjectAsOptions{}); d.HasError() {
		t.Fatal(d)
	}
	return pv
}

func TestUpgradeState_V4ToV5(t *testing.T) {
	ctx := context.Background()
	out := upgradeFrom(t, ctx, 4, v4State)

	if out.Namespace.ValueString() != "capi" || out.Name.ValueString() != "prod" || out.KubernetesVersion.ValueString() != "v1.34.0" {
		t.Errorf("identity = %s/%s %s", out.Namespace.ValueString(), out.Name.ValueString(), out.KubernetesVersion.ValueString())
	}
	mgmt, _ := extractManagement(ctx, &out)
	if mgmt == nil || !mgmt.SelfManaged.ValueBool() {
		t.Fatalf("management = %+v", mgmt)
	}
	pv := providersOf(t, ctx, &out)
	infra, _ := extractProviders(ctx, pv.Infrastructure)
	if infra["tinkerbell"].Version.ValueString() != "v0.7.9" || infra["tinkerbell"].ResourceConfig.ValueString() != "templateInline: x\n" {
		t.Errorf("providers.infrastructure = %v", pv.Infrastructure)
	}
	core, _ := extractProviders(ctx, pv.Core)
	if core["cluster-api"].Version.ValueString() != "v1.14.0" {
		t.Errorf("providers.core = %v", pv.Core)
	}
	if addon, _ := extractProviders(ctx, pv.Addon); len(addon) != 1 || !pv.IPAM.IsNull() {
		t.Errorf("providers.addon/ipam = %v %v", pv.Addon, pv.IPAM)
	}
	bs, _ := extractManagementBootstrap(ctx, mgmt)
	if bs == nil || bs.Type.ValueString() != "kairos" || bs.OnFailure.ValueString() != "resume" {
		t.Errorf("bootstrap = %+v", bs)
	}
	infraSpec, _ := extractInfrastructure(ctx, &out)
	if infraSpec == nil || infraSpec.Provider.ValueString() != "tinkerbell" || !infraSpec.Spec.IsNull() {
		t.Errorf("infrastructure = %+v", infraSpec)
	}
	cp, _ := extractControlPlane(ctx, &out)
	if cp == nil || cp.Provider.ValueString() != "talos" || cp.Replicas.ValueInt64() != 3 || !cp.Infrastructure.IsNull() || !cp.Spec.IsNull() {
		t.Errorf("control_plane = %+v", cp)
	}
	machineSpec, d := dynamicToMap(cp.MachineSpec)
	if d.HasError() || machineSpec == nil {
		t.Fatalf("the generated control-plane template carries on as machine_spec: %v %v", machineSpec, d)
	}
	if _, ok := machineSpec["hardwareAffinity"].(map[string]any); !ok {
		t.Errorf("machine_spec = %v", machineSpec)
	}
	if !out.Class.IsNull() {
		t.Error("a direct-mode cluster has no class")
	}
	wait, _ := extractWait(ctx, &out)
	conditions, _ := stringList(ctx, wait.Conditions)
	if len(conditions) != 0 || wait.Timeout.ValueString() != "90m" {
		t.Errorf("wait = %+v (enabled=false becomes an empty condition list)", wait)
	}
	status := extractStatus(ctx, &out)
	if status.Phase.ValueString() != "ready" || status.Endpoint.ValueString() != "https://10.0.0.160:6443" || !status.Error.IsNull() || !status.Conditions.IsNull() {
		t.Errorf("status = %+v", status)
	}
	secrets, _ := providerSecretsFromDynamic(ctx, out.ProviderSecrets)
	if secrets["prod-talos/bundle"] != "x" {
		t.Errorf("provider_secrets = %v", secrets)
	}
	if ep, _ := extractNetwork(ctx, &out); ep.ControlPlaneEndpoint == nil || ep.ControlPlaneEndpoint.Host != "10.0.0.160" {
		t.Errorf("network = %+v", ep)
	}
}

func TestUpgradeState_V4ToV5_ClusterClass(t *testing.T) {
	ctx := context.Background()
	raw := strings.Replace(v4State, `"cluster_class": false`, `"cluster_class": true`, 1)
	out := upgradeFrom(t, ctx, 4, raw)
	class, _ := extractClass(ctx, &out)
	if class == nil || class.Name != "prod" || class.Namespace != "" {
		t.Errorf("class = %+v (a generated ClusterClass carried the cluster's name)", class)
	}
	if !out.Infrastructure.IsNull() {
		t.Error("a class cluster has no infrastructure block")
	}
	cp, _ := extractControlPlane(ctx, &out)
	if cp == nil || cp.Replicas.ValueInt64() != 3 || !cp.Provider.IsNull() || !cp.MachineSpec.IsNull() {
		t.Errorf("control_plane = %+v (only replicas survive with a class)", cp)
	}
}

func TestDynamicJSON(t *testing.T) {
	got, err := dynamicJSON(map[string]any{"a": "x", "n": float64(2), "b": true, "list": []any{"y", float64(1)}, "nested": map[string]any{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Value map[string]any  `json:"value"`
		Type  json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Value["a"] != "x" || decoded.Value["b"] != true {
		t.Errorf("value = %v", decoded.Value)
	}
	typ := string(decoded.Type)
	for _, want := range []string{`"object"`, `"a":"string"`, `"n":"number"`, `"b":"bool"`, `"list":["tuple",["string","number"]]`, `"nested":["object",{"k":"string"}]`} {
		if !strings.Contains(typ, want) {
			t.Errorf("type %s lacks %s", typ, want)
		}
	}
}

func TestUpgradeState_V2ToV5(t *testing.T) {
	ctx := context.Background()
	out := upgradeFrom(t, ctx, 2, v2State)
	pv := providersOf(t, ctx, &out)
	infra, _ := extractProviders(ctx, pv.Infrastructure)
	if infra["tinkerbell"].Version.ValueString() != "v0.7.9" || !infra["tinkerbell"].ResourceConfig.IsNull() {
		t.Errorf("infrastructure = %v", pv.Infrastructure)
	}
	if !out.Network.IsNull() || !out.Class.IsNull() || out.Namespace.ValueString() != "default" {
		t.Errorf("network/class/namespace = %v %v %s", out.Network, out.Class, out.Namespace.ValueString())
	}
	cp, _ := extractControlPlane(ctx, &out)
	if cp == nil || cp.Replicas.ValueInt64() != 1 || cp.Provider.ValueString() != "talos" {
		t.Errorf("control_plane = %+v", cp)
	}
	infraSpec, _ := extractInfrastructure(ctx, &out)
	if infraSpec == nil || infraSpec.Provider.ValueString() != "tinkerbell" {
		t.Errorf("infrastructure = %+v", infraSpec)
	}
	mgmt, _ := extractManagement(ctx, &out)
	if mgmt == nil || mgmt.SelfManaged.ValueBool() {
		t.Errorf("a v2 state without management is not self-managed: %+v", mgmt)
	}
}

func TestUpgradeState_V1ToV5(t *testing.T) {
	ctx := context.Background()
	out := upgradeFrom(t, ctx, 1, v1State)
	pv := providersOf(t, ctx, &out)
	infra, _ := extractProviders(ctx, pv.Infrastructure)
	if infra["tinkerbell"].Version.ValueString() != "v0.5.4" {
		t.Errorf("infrastructure = %v", pv.Infrastructure)
	}
	addons, _ := extractProviders(ctx, pv.Addon)
	if addons["helm"].Version.ValueString() != "v0.2.12" {
		t.Errorf("addon = %v", pv.Addon)
	}
	var fc FetchConfigModel
	if d := addons["helm"].FetchConfig.As(ctx, &fc, basetypes.ObjectAsOptions{}); d.HasError() || fc.URL.ValueString() != "https://example.com/addon-components.yaml" || !fc.Owner.IsNull() {
		t.Errorf("fetch_config = %+v", fc)
	}
	cp, _ := extractControlPlane(ctx, &out)
	if cp == nil || cp.Replicas.ValueInt64() != 1 || cp.Provider.ValueString() != "talos" {
		t.Errorf("control_plane = %+v", cp)
	}
}

func TestUpgradeState_V1ToV5_NoCountsNoAddons(t *testing.T) {
	ctx := context.Background()
	out := upgradeFrom(t, ctx, 1, `{"name":"x","infrastructure":{"provider":"docker"},"bootstrap":null,"control_plane":null,"core":null,"workers":null,"addons":null}`)
	cp, _ := extractControlPlane(ctx, &out)
	if cp == nil || !cp.Replicas.IsNull() || cp.Provider.ValueString() != "kubeadm" {
		t.Errorf("control_plane = %+v (defaults to kubeadm with no count)", cp)
	}
	pv := providersOf(t, ctx, &out)
	infra, _ := extractProviders(ctx, pv.Infrastructure)
	if !infra["docker"].Version.IsNull() || !pv.Addon.IsNull() || !pv.Bootstrap.IsNull() {
		t.Errorf("providers = %+v", pv)
	}
}

func TestUpgradeState_V0ToV5(t *testing.T) {
	ctx := context.Background()
	out := upgradeFrom(t, ctx, 0, `{
	  "id": "c", "name": "c", "kubernetes_version": "v1.31.0", "flavor": null,
	  "management_kubeconfig": null, "skip_init": false, "self_managed": true, "target_namespace": null,
	  "infrastructure_provider": "tinkerbell:v0.5.4", "bootstrap_provider": "kubeadm", "control_plane_provider": "kubeadm:v1.12.2",
	  "control_plane_machine_count": 3, "core_provider": "cluster-api:v1.12.2", "worker_machine_count": 2,
	  "wait_for_ready": true, "kubeconfig_path": null,
	  "endpoint": null, "kubeconfig": null, "cluster_ca_certificate": null, "cluster_description": null, "bootstrap_cluster_name": null
	}`)
	pv := providersOf(t, ctx, &out)
	infra, _ := extractProviders(ctx, pv.Infrastructure)
	if infra["tinkerbell"].Version.ValueString() != "v0.5.4" {
		t.Errorf("infrastructure = %v", pv.Infrastructure)
	}
	bs, _ := extractProviders(ctx, pv.Bootstrap)
	if _, ok := bs["kubeadm"]; !ok || !bs["kubeadm"].Version.IsNull() {
		t.Errorf("bootstrap = %v", pv.Bootstrap)
	}
	core, _ := extractProviders(ctx, pv.Core)
	if core["cluster-api"].Version.ValueString() != "v1.12.2" {
		t.Errorf("core = %v", pv.Core)
	}
	if !pv.IPAM.IsNull() || !pv.Addon.IsNull() {
		t.Error("ipam and addon must be null after a v0 upgrade")
	}
	mgmt, _ := extractManagement(ctx, &out)
	if mgmt == nil || !mgmt.SelfManaged.ValueBool() {
		t.Errorf("management = %+v", mgmt)
	}
	cp, _ := extractControlPlane(ctx, &out)
	if cp == nil || cp.Replicas.ValueInt64() != 3 || cp.Provider.ValueString() != "kubeadm" {
		t.Errorf("control_plane = %+v", cp)
	}
	if !out.Class.IsNull() || !out.Network.IsNull() || out.Namespace.ValueString() != "default" {
		t.Error("v0 state must upgrade without class or network and with the default namespace")
	}
	wait, _ := extractWait(ctx, &out)
	if conditions, _ := stringList(ctx, wait.Conditions); len(conditions) != 1 || conditions[0] != "Available" {
		t.Errorf("wait = %+v", wait)
	}
}

func TestUpgradeV4JSON_RawShape(t *testing.T) {
	out, err := upgradeV4JSON([]byte(v4State))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"cluster_class", "topology", "output", "core", "infrastructure_provider", "addon", "ipam", "bootstrap"} {
		if v, ok := got[gone]; ok && gone != "infrastructure" {
			if gone == "bootstrap" || gone == "core" || gone == "addon" || gone == "ipam" {
				t.Errorf("%s must move under management.providers, still at top level: %s", gone, v)
			} else {
				t.Errorf("%s must be removed: %s", gone, v)
			}
		}
	}
	if string(got["namespace"]) != `"capi"` {
		t.Errorf("namespace = %s", got["namespace"])
	}
	if strings.Contains(string(got["management"]), `"kubeconfig"`) || strings.Contains(string(got["management"]), `"skip_init"`) {
		t.Errorf("management must drop kubeconfig and skip_init: %s", got["management"])
	}
	if strings.Contains(string(got["status"]), `"kubeconfig"`) || !strings.Contains(string(got["status"]), `"phase":"ready"`) {
		t.Errorf("status = %s", got["status"])
	}
}

func TestUpgradeState_RefusesInPlaceV3(t *testing.T) {
	ctx := context.Background()
	up := (&ClusterResource{}).UpgradeState(ctx)[3]
	resp := &resource.UpgradeStateResponse{}
	up.StateUpgrader(ctx, resource.UpgradeStateRequest{RawState: &tfprotov6.RawState{JSON: []byte(`{"name":"x","management":{"bootstrap":{"type":"talos","mode":"in_place"}}}`)}}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a v3 in-place cluster cannot be upgraded")
	}
	if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "no longer supports") {
		t.Errorf("error = %v", resp.Diagnostics)
	}
}

var _ = types.StringNull
