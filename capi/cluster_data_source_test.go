// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

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
				map[string]any{
					"type":   "ControlPlaneInitialized",
					"status": "True",
					"reason": "Initialized",
				},
				map[string]any{
					"type":    "Available",
					"status":  "False",
					"reason":  "NotAvailable",
					"message": "waiting",
				},
			},
		},
	}}
	var data clusterDataSourceModel
	if d := populateClusterStatus(ctx, &data, obj); d.HasError() {
		t.Fatal(d)
	}
	if data.Phase.ValueString() != "Provisioned" ||
		data.Endpoint.ValueString() != "https://10.0.0.160:6443" ||
		data.KubernetesVersion.ValueString() != "v1.34.0" {
		t.Errorf("status = %+v", data)
	}
	if !data.Paused.ValueBool() || !data.ControlPlaneInitialized.ValueBool() ||
		data.Available.ValueBool() {
		t.Errorf(
			"flags = paused %v initialized %v available %v",
			data.Paused,
			data.ControlPlaneInitialized,
			data.Available,
		)
	}
	if len(data.Conditions.Elements()) != 2 {
		t.Errorf("conditions = %v", data.Conditions)
	}
}
