// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

func TestProviderChanges(t *testing.T) {
	ctx := context.Background()
	pinned := func(version string) ProviderModel {
		p := emptyProviderModel()
		p.Version = types.StringValue(version)
		return p
	}

	state := baseModel(t, ctx)
	state.Infrastructure = mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": pinned("v0.7.9")})
	state.ControlPlane = mustProviderMap(t, ctx, map[string]ProviderModel{"talos": pinned("v0.7.1")})
	state.Core = mustProviderMap(t, ctx, map[string]ProviderModel{"cluster-api": pinned("v1.13.0")})
	state.IPAM = mustProviderMap(t, ctx, map[string]ProviderModel{"unifi": pinned("v0.4.1"), "in-cluster": emptyProviderModel()})
	state.Addon = mustProviderMap(t, ctx, map[string]ProviderModel{"helm": emptyProviderModel()})

	plan := baseModel(t, ctx)
	plan.Infrastructure = mustProviderMap(t, ctx, map[string]ProviderModel{"tinkerbell": pinned("v0.8.0")}) // upgrade
	cp := pinned("v0.7.1")
	cp.ResourceConfig = types.StringValue("controlPlaneConfig: {}") // not a provider change
	plan.ControlPlane = mustProviderMap(t, ctx, map[string]ProviderModel{"talos": cp})
	plan.Core = types.MapNull(providerMapType().ElemType)                                    // unpinned, never removed
	plan.IPAM = mustProviderMap(t, ctx, map[string]ProviderModel{"unifi": pinned("v0.4.1")}) // in-cluster removed
	helm := emptyProviderModel()
	fetch, _ := types.ObjectValueFrom(ctx, fetchConfigAttrTypes(), FetchConfigModel{
		Owner: types.StringValue("me"), Repository: types.StringNull(), URL: types.StringNull(), OCI: types.StringNull(),
	})
	helm.FetchConfig = fetch                                                                                    // upgrade: same version, other source
	plan.Addon = mustProviderMap(t, ctx, map[string]ProviderModel{"helm": helm, "other": emptyProviderModel()}) // other added

	upgrade, removed, added, diags := providerChanges(ctx, state, plan)
	if diags.HasError() {
		t.Fatal(diags)
	}
	want := []capi.ProviderKey{{Type: capi.ProviderTypeInfrastructure, Name: "tinkerbell"}, {Type: capi.ProviderTypeAddon, Name: "helm"}}
	if len(upgrade) != len(want) || upgrade[0] != want[0] || upgrade[1] != want[1] {
		t.Errorf("upgrade = %v, want %v", upgrade, want)
	}
	if keys := removed.Keys(); len(keys) != 1 || keys[0] != (capi.ProviderKey{Type: capi.ProviderTypeIPAM, Name: "in-cluster"}) {
		t.Errorf("removed = %v (only ipam and addon entries are deleted)", keys)
	}
	if !added {
		t.Error("the new addon entry must be reported as added")
	}

	// No change at all.
	upgrade, removed, added, diags = providerChanges(ctx, state, state)
	if diags.HasError() || len(upgrade) != 0 || !removed.Empty() || added {
		t.Errorf("identical maps: %v %v %v %v", upgrade, removed.Keys(), added, diags)
	}
}

func TestParseImportID(t *testing.T) {
	cases := map[string][3]string{
		"prod":                              {"", "", "prod"},
		"capi-system/prod":                  {"", "capi-system", "prod"},
		"~/.kube/mgmt:capi-system/prod":     {"~/.kube/mgmt", "capi-system", "prod"},
		"/etc/kube/a:b.yaml:prod":           {"/etc/kube/a:b.yaml", "", "prod"},
		" /home/me/kubeconfig : ns / prod ": {"/home/me/kubeconfig", "ns", "prod"},
		"":                                  {"", "", ""},
		"/path/with/slashes/only:capi/":     {"/path/with/slashes/only", "capi", ""},
		"ns/":                               {"", "ns", ""},
	}
	for id, want := range cases {
		k, ns, name := parseImportID(id)
		if k != want[0] || ns != want[1] || name != want[2] {
			t.Errorf("%q → %q %q %q, want %v", id, k, ns, name, want)
		}
	}
}

func TestClusterResource_ImportState(t *testing.T) {
	ctx := context.Background()
	resp := &resource.SchemaResponse{}
	NewClusterResource().Schema(ctx, resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	r, ok := NewClusterResource().(*ClusterResource)
	if !ok {
		t.Fatal("NewClusterResource must return a *ClusterResource")
	}
	importInto := func(id string) (*resource.ImportStateResponse, tfsdk.State) {
		st := tfsdk.State{Schema: resp.Schema, Raw: tftypes.NewValue(resp.Schema.Type().TerraformType(ctx), nil)}
		out := &resource.ImportStateResponse{State: st}
		r.ImportState(ctx, resource.ImportStateRequest{ID: id}, out)
		return out, out.State
	}

	out, st := importInto("/tmp/mgmt:capi-system/prod")
	if out.Diagnostics.HasError() {
		t.Fatal(out.Diagnostics)
	}
	var name, id string
	_ = st.GetAttribute(ctx, path.Root("name"), &name)
	_ = st.GetAttribute(ctx, path.Root("id"), &id)
	if name != "prod" || id != "prod" {
		t.Errorf("name %q id %q", name, id)
	}
	var mgmt ManagementModel
	if d := st.GetAttribute(ctx, path.Root("management"), &mgmt); d.HasError() {
		t.Fatal(d)
	}
	if mgmt.Kubeconfig.ValueString() != "/tmp/mgmt" || mgmt.Namespace.ValueString() != "capi-system" || !mgmt.SkipInit.IsNull() {
		t.Errorf("management = %+v", mgmt)
	}

	out, st = importInto("prod")
	if out.Diagnostics.HasError() {
		t.Fatal(out.Diagnostics)
	}
	var mgmtObj types.Object
	_ = st.GetAttribute(ctx, path.Root("management"), &mgmtObj)
	if !mgmtObj.IsNull() {
		t.Error("a bare name leaves management unset")
	}

	if out, _ := importInto("ns/"); !out.Diagnostics.HasError() {
		t.Error("an ID without a name is invalid")
	}
}

func TestClusterResource_Schema_ManagementUninstall(t *testing.T) {
	resp := &resource.SchemaResponse{}
	NewClusterResource().Schema(context.Background(), resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	mgmt, _ := resp.Schema.Attributes["management"].(schema.SingleNestedAttribute)
	attr, ok := mgmt.Attributes["uninstall"].(schema.BoolAttribute)
	if !ok || !attr.Optional || !attr.Computed || attr.Default == nil || len(attr.PlanModifiers) != 0 {
		t.Errorf("management.uninstall must be an optional, computed, defaulted, mutable bool: %+v", attr)
	}
}

func TestUpgradeInventoryAndAddonsJSON_AddsUninstall(t *testing.T) {
	raw := []byte(`{"name":"x","management":{"kubeconfig":null,"bootstrap":null,"addons":null}}`)
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if err := upgradeInventoryAndAddonsJSON(state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state["management"]), `"uninstall":false`) {
		t.Errorf("management.uninstall must default to false: %s", state["management"])
	}
}
