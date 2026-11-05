// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
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

func TestUpgradeV1JSON(t *testing.T) {
	out, err := upgradeV1JSON([]byte(v1State))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"workers", "addons"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s must be removed", gone)
		}
	}
	var infra map[string]map[string]json.RawMessage
	if err := json.Unmarshal(got["infrastructure"], &infra); err != nil {
		t.Fatal(err)
	}
	if string(infra["tinkerbell"]["version"]) != `"v0.5.4"` {
		t.Errorf("infrastructure = %s", got["infrastructure"])
	}
	if len(infra["tinkerbell"]) != 10 {
		t.Errorf("provider object must carry all 10 attributes, got %d", len(infra["tinkerbell"]))
	}
	if string(infra["tinkerbell"]["resource_config"]) != "null" {
		t.Errorf("resource_config must be null after upgrade: %s", infra["tinkerbell"]["resource_config"])
	}
	var addon map[string]map[string]json.RawMessage
	if err := json.Unmarshal(got["addon"], &addon); err != nil {
		t.Fatal(err)
	}
	if string(addon["helm"]["version"]) != `"v0.2.12"` || string(addon["helm"]["config_variables"]) != `{"A":"b"}` {
		t.Errorf("addon = %s", got["addon"])
	}
	var fc map[string]json.RawMessage
	if err := json.Unmarshal(addon["helm"]["fetch_config"], &fc); err != nil {
		t.Fatal(err)
	}
	if string(fc["url"]) != `"https://example.com/addon-components.yaml"` || string(fc["owner"]) != "null" || string(fc["repository"]) != "null" {
		t.Errorf("fetch_config = %s", addon["helm"]["fetch_config"])
	}
	wantTopo := `{"control_plane":{"infrastructure":null,"replicas":1},"workers":{"machine_deployments":[{"bootstrap":null,"class":null,"failure_domain":null,"infrastructure":null,"metadata":null,"name":"md-0","replicas":2}]}}`
	if string(got["topology"]) != wantTopo {
		t.Errorf("topology = %s\nwant %s", got["topology"], wantTopo)
	}
	if string(got["ipam"]) != "null" || string(got["core"]) != "null" {
		t.Errorf("ipam/core = %s/%s", got["ipam"], got["core"])
	}
	assertV3Shape(t, got)
	if string(got["management"]) != "null" || string(got["name"]) != `"acc-talos"` {
		t.Error("untouched attributes must pass through")
	}
}

func TestUpgradeV1JSON_NoCountsNoAddons(t *testing.T) {
	out, err := upgradeV1JSON([]byte(`{"name":"x","infrastructure":{"provider":"docker"},"bootstrap":null,"control_plane":null,"core":null,"workers":null,"addons":null}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["topology"]) != "null" || string(got["addon"]) != "null" || string(got["bootstrap"]) != "null" {
		t.Errorf("empty inputs must become null: %s", out)
	}
	var infra map[string]map[string]json.RawMessage
	if err := json.Unmarshal(got["infrastructure"], &infra); err != nil {
		t.Fatal(err)
	}
	if string(infra["docker"]["version"]) != "null" {
		t.Errorf("bare provider must have a null version: %s", got["infrastructure"])
	}
}

// assertV3Shape checks the attributes every upgrader must emit for schema v3.
func assertV3Shape(t *testing.T, got map[string]json.RawMessage) {
	t.Helper()
	for _, gone := range []string{"template", "flavor"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s must be removed", gone)
		}
	}
	if string(got["cluster_class"]) != "false" {
		t.Errorf("cluster_class = %s, want false", got["cluster_class"])
	}
	if string(got["network"]) != "null" {
		t.Errorf("network = %s, want null", got["network"])
	}
}

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

func TestUpgradeV2JSON(t *testing.T) {
	out, err := upgradeV2JSON([]byte(v2State))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	assertV3Shape(t, got)
	for _, attr := range []string{"infrastructure", "bootstrap", "control_plane"} {
		var m map[string]map[string]json.RawMessage
		if err := json.Unmarshal(got[attr], &m); err != nil {
			t.Fatalf("%s: %v", attr, err)
		}
		for name, entry := range m {
			if len(entry) != 10 || string(entry["resource_config"]) != "null" {
				t.Errorf("%s[%s] must gain a null resource_config: %s", attr, name, got[attr])
			}
		}
	}
	if string(got["core"]) != "null" || string(got["addon"]) != "null" {
		t.Error("null provider maps must stay null")
	}
	// Untouched sub-objects pass through verbatim, so compare semantically.
	wantTopo := `{"control_plane":{"infrastructure":null,"replicas":1},"workers":{"machine_deployments":[{"bootstrap":null,"class":null,"failure_domain":null,"infrastructure":null,"metadata":{"annotations":null,"labels":{"tier":"worker"}},"name":"md-0","replicas":2}]}}`
	var gotTopo, wantTopoVal any
	if err := json.Unmarshal(got["topology"], &gotTopo); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wantTopo), &wantTopoVal); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotTopo, wantTopoVal) {
		t.Errorf("topology = %s\nwant %s", got["topology"], wantTopo)
	}
	if _, hasCNI := got["cni"]; string(got["kubernetes_version"]) != `"v1.34.0"` || hasCNI {
		t.Error("untouched attributes must pass through")
	}
}

func TestUpgradeState_V2ToV3_DecodesAgainstSchema(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}
	up := r.UpgradeState(ctx)[2]
	if up.PriorSchema != nil {
		t.Fatal("v2 upgrader must rewrite raw JSON without a prior schema")
	}
	resp := &resource.UpgradeStateResponse{}
	up.StateUpgrader(ctx, resource.UpgradeStateRequest{RawState: &tfprotov6.RawState{JSON: []byte(v2State)}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	out := decodeUpgraded(t, ctx, resp)
	infra, _ := extractProviders(ctx, out.Infrastructure)
	if infra["tinkerbell"].Version.ValueString() != "v0.7.9" || !infra["tinkerbell"].ResourceConfig.IsNull() {
		t.Errorf("infrastructure = %v", out.Infrastructure)
	}
	if !out.ClusterClass.IsNull() && out.ClusterClass.ValueBool() {
		t.Error("cluster_class must be false")
	}
	if !out.Network.IsNull() {
		t.Error("network must be null")
	}
	cp, mds, _ := extractTopology(ctx, &out)
	if cp.Replicas == nil || *cp.Replicas != 1 || len(mds) != 1 || *mds[0].Replicas != 2 || mds[0].Labels["tier"] != "worker" {
		t.Errorf("topology = %+v %+v", cp, mds)
	}
}

// decodeUpgraded decodes a raw-JSON upgrade result against the current schema.
func decodeUpgraded(t *testing.T, ctx context.Context, resp *resource.UpgradeStateResponse) ClusterResourceModel {
	t.Helper()
	if resp.DynamicValue == nil {
		t.Fatal("raw-JSON upgrader must set DynamicValue")
	}
	schema := currentSchema(t, ctx)
	val, err := resp.DynamicValue.Unmarshal(schema.Type().TerraformType(ctx))
	if err != nil {
		t.Fatalf("upgraded state does not decode against the current schema: %v", err)
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

func TestUpgradeState_V1ToV3_DecodesAgainstSchema(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}
	up := r.UpgradeState(ctx)[1]
	resp := &resource.UpgradeStateResponse{}
	up.StateUpgrader(ctx, resource.UpgradeStateRequest{RawState: &tfprotov6.RawState{JSON: []byte(v1State)}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	out := decodeUpgraded(t, ctx, resp)
	infra, _ := extractProviders(ctx, out.Infrastructure)
	if infra["tinkerbell"].Version.ValueString() != "v0.5.4" {
		t.Errorf("infrastructure = %v", out.Infrastructure)
	}
	cp, mds, _ := extractTopology(ctx, &out)
	if cp.Replicas == nil || *cp.Replicas != 1 || len(mds) != 1 || *mds[0].Replicas != 2 {
		t.Errorf("topology = %+v %+v", cp, mds)
	}
	if out.ClusterClass.ValueBool() || !out.Network.IsNull() {
		t.Error("v1 state must upgrade with cluster_class=false and no network")
	}
	addons, _ := extractProviders(ctx, out.Addon)
	if addons["helm"].Version.ValueString() != "v0.2.12" {
		t.Errorf("addon = %v", out.Addon)
	}
}

func TestUpgradeState_V0ToV3(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}
	up := r.UpgradeState(ctx)[0]

	v0Type := up.PriorSchema.Type().TerraformType(ctx)
	v0JSON := []byte(`{
	  "id": "c", "name": "c", "kubernetes_version": "v1.31.0", "flavor": null,
	  "management_kubeconfig": null, "skip_init": false, "self_managed": true, "target_namespace": null,
	  "infrastructure_provider": "tinkerbell:v0.5.4", "bootstrap_provider": "kubeadm", "control_plane_provider": "kubeadm:v1.12.2",
	  "control_plane_machine_count": 3, "core_provider": "cluster-api:v1.12.2", "worker_machine_count": 2,
	  "wait_for_ready": true, "kubeconfig_path": null,
	  "endpoint": null, "kubeconfig": null, "cluster_ca_certificate": null, "cluster_description": null, "bootstrap_cluster_name": null
	}`)
	v0Val, err := (&tfprotov6.DynamicValue{JSON: v0JSON}).Unmarshal(v0Type)
	if err != nil {
		t.Fatalf("fixture does not match the v0 schema: %v", err)
	}

	schema := currentSchema(t, ctx)
	resp := &resource.UpgradeStateResponse{State: tfsdk.State{Schema: schema, Raw: tftypes.NewValue(schema.Type().TerraformType(ctx), nil)}}
	up.StateUpgrader(ctx, resource.UpgradeStateRequest{State: &tfsdk.State{Schema: *up.PriorSchema, Raw: v0Val}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}

	var out ClusterResourceModel
	if d := resp.State.Get(ctx, &out); d.HasError() {
		t.Fatal(d)
	}
	infra, _ := extractProviders(ctx, out.Infrastructure)
	if infra["tinkerbell"].Version.ValueString() != "v0.5.4" {
		t.Errorf("infrastructure = %v", out.Infrastructure)
	}
	bs, _ := extractProviders(ctx, out.Bootstrap)
	if _, ok := bs["kubeadm"]; !ok || !bs["kubeadm"].Version.IsNull() {
		t.Errorf("bootstrap = %v", out.Bootstrap)
	}
	core, _ := extractProviders(ctx, out.Core)
	if core["cluster-api"].Version.ValueString() != "v1.12.2" {
		t.Errorf("core = %v", out.Core)
	}
	if !out.IPAM.IsNull() || !out.Addon.IsNull() {
		t.Error("ipam and addon must be null after a v0 upgrade")
	}
	cp, mds, _ := extractTopology(ctx, &out)
	if cp.Replicas == nil || *cp.Replicas != 3 || len(mds) != 1 || mds[0].Name != "md-0" || *mds[0].Replicas != 2 {
		t.Errorf("topology = %+v %+v", cp, mds)
	}
	if out.ClusterClass.ValueBool() || !out.Network.IsNull() {
		t.Error("v0 state must upgrade with cluster_class=false and no network")
	}
}
