// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
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
	if len(infra["tinkerbell"]) != 9 {
		t.Errorf("provider object must carry all 9 attributes, got %d", len(infra["tinkerbell"]))
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
	wantTopo := `{"control_plane":{"replicas":1},"workers":{"machine_deployments":[{"class":null,"failure_domain":null,"metadata":null,"name":"md-0","replicas":2}]}}`
	if string(got["topology"]) != wantTopo {
		t.Errorf("topology = %s\nwant %s", got["topology"], wantTopo)
	}
	if string(got["ipam"]) != "null" || string(got["core"]) != "null" || string(got["template"]) != "null" {
		t.Errorf("ipam/core/template = %s/%s/%s", got["ipam"], got["core"], got["template"])
	}
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

// v2Schema returns the current resource schema.
func v2Schema(t *testing.T, ctx context.Context) schema.Schema {
	t.Helper()
	resp := &resource.SchemaResponse{}
	NewClusterResource().Schema(ctx, resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.Schema
}

func TestUpgradeState_V1ToV2_DecodesAgainstSchema(t *testing.T) {
	ctx := context.Background()
	r := &ClusterResource{}
	up := r.UpgradeState(ctx)[1]
	resp := &resource.UpgradeStateResponse{}
	up.StateUpgrader(ctx, resource.UpgradeStateRequest{RawState: &tfprotov6.RawState{JSON: []byte(v1State)}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if resp.DynamicValue == nil {
		t.Fatal("v1 upgrader must set DynamicValue")
	}
	schema := v2Schema(t, ctx)
	val, err := resp.DynamicValue.Unmarshal(schema.Type().TerraformType(ctx))
	if err != nil {
		t.Fatalf("upgraded state does not decode against the v2 schema: %v", err)
	}
	var out ClusterResourceModel
	if d := (tfsdk.State{Schema: schema, Raw: val}).Get(ctx, &out); d.HasError() {
		t.Fatal(d)
	}
	infra, _ := extractProviders(ctx, out.Infrastructure)
	if infra["tinkerbell"].Version.ValueString() != "v0.5.4" {
		t.Errorf("infrastructure = %v", out.Infrastructure)
	}
	cp, mds, _ := extractTopology(ctx, &out)
	if cp == nil || *cp != 1 || len(mds) != 1 || *mds[0].Replicas != 2 {
		t.Errorf("topology = %v %+v", cp, mds)
	}
	addons, _ := extractProviders(ctx, out.Addon)
	if addons["helm"].Version.ValueString() != "v0.2.12" {
		t.Errorf("addon = %v", out.Addon)
	}
}

func TestUpgradeState_V0ToV2(t *testing.T) {
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

	schema := v2Schema(t, ctx)
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
	if cp == nil || *cp != 3 || len(mds) != 1 || mds[0].Name != "md-0" || *mds[0].Replicas != 2 {
		t.Errorf("topology = %v %+v", cp, mds)
	}
}
