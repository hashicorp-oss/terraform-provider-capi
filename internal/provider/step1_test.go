// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

func TestProviderSchema_ManagementConnection(t *testing.T) {
	resp := &provider.SchemaResponse{}
	New("test")().Schema(context.Background(), provider.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	for _, name := range []string{"kubeconfig", "kubernetes"} {
		if _, ok := resp.Schema.Attributes[name]; !ok {
			t.Errorf("provider schema lacks %q", name)
		}
	}
	if _, ok := resp.Schema.Attributes["endpoint"]; ok {
		t.Error("the placeholder endpoint attribute is gone")
	}
	if !resp.Schema.Attributes["kubeconfig"].IsSensitive() {
		t.Error("kubeconfig must be sensitive")
	}
}

func TestEphemeralAndDataSourceSchemas(t *testing.T) {
	ctx := context.Background()
	eresp := &ephemeral.SchemaResponse{}
	NewKubeconfigEphemeralResource().Schema(ctx, ephemeral.SchemaRequest{}, eresp)
	if eresp.Diagnostics.HasError() {
		t.Fatal(eresp.Diagnostics)
	}
	for _, name := range []string{"raw", "client_key", "client_certificate", "token", "secrets"} {
		if !eresp.Schema.Attributes[name].IsSensitive() {
			t.Errorf("capi_kubeconfig.%s must be sensitive", name)
		}
	}
	for _, ds := range []datasource.DataSource{NewClusterDataSource(), NewProviderComponentsDataSource()} {
		resp := &datasource.SchemaResponse{}
		ds.Schema(ctx, datasource.SchemaRequest{}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatal(resp.Diagnostics)
		}
	}
}

func TestPopulateClusterStatus(t *testing.T) {
	ctx := context.Background()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"paused":               true,
			"controlPlaneEndpoint": map[string]any{"host": "10.0.0.160", "port": int64(6443)},
			"topology":             map[string]any{"version": "v1.34.0"},
		},
		"status": map[string]any{
			"phase": "Provisioned",
			"conditions": []any{
				map[string]any{"type": "ControlPlaneInitialized", "status": "True", "reason": "Initialized"},
				map[string]any{"type": "Available", "status": "False", "reason": "NotAvailable", "message": "waiting"},
			},
		},
	}}
	var data ClusterDataSourceModel
	if d := populateClusterStatus(ctx, &data, obj); d.HasError() {
		t.Fatal(d)
	}
	if data.Phase.ValueString() != "Provisioned" || data.Endpoint.ValueString() != "https://10.0.0.160:6443" || data.KubernetesVersion.ValueString() != "v1.34.0" {
		t.Errorf("status = %+v", data)
	}
	if !data.Paused.ValueBool() || !data.ControlPlaneInitialized.ValueBool() || data.Available.ValueBool() {
		t.Errorf("flags = paused %v initialized %v available %v", data.Paused, data.ControlPlaneInitialized, data.Available)
	}
	if len(data.Conditions.Elements()) != 2 {
		t.Errorf("conditions = %v", data.Conditions)
	}
}

func TestProviderTypeFromSchema(t *testing.T) {
	for in, want := range map[string]capi.ProviderType{"core": capi.ProviderTypeCore, "control_plane": capi.ProviderTypeControlPlane, "control-plane": capi.ProviderTypeControlPlane, "addon": capi.ProviderTypeAddon} {
		if got, ok := providerTypeFromSchema(in); !ok || got != want {
			t.Errorf("%s = %v %v", in, got, ok)
		}
	}
	if _, ok := providerTypeFromSchema("runtime"); ok {
		t.Error("unknown type")
	}
	_ = types.StringNull()
}
