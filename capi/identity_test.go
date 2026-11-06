// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"sort"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestIdentity_EveryResourceHasOne(t *testing.T) {
	ctx := context.Background()
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.GetResourceIdentitySchemas(
		ctx,
		&tfprotov6.GetResourceIdentitySchemasRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for name := range resp.IdentitySchemas {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{
		"capi_bootstrap_template",
		"capi_cluster",
		"capi_cluster_class",
		"capi_cluster_resource_set",
		"capi_control_plane_template",
		"capi_hardware",
		"capi_helm_chart_proxy",
		"capi_machine_deployment",
		"capi_machine_health_check",
		"capi_machine_template",
		"capi_object",
		"capi_provider",
	}
	if len(got) != len(want) {
		t.Fatalf("identity schemas = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("identity schemas = %v, want %v", got, want)
		}
	}
}

// An import by identity runs the same code as one by ID and records the
// identity.
func TestClusterResource_ImportByIdentity(t *testing.T) {
	ctx := context.Background()
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	idType := tftypes.Object{
		AttributeTypes: map[string]tftypes.Type{
			"namespace": tftypes.String,
			"name":      tftypes.String,
		},
	}
	identity, err := tfprotov6.NewDynamicValue(
		idType,
		tftypes.NewValue(idType, map[string]tftypes.Value{
			"namespace": tftypes.NewValue(tftypes.String, "capi"),
			"name":      tftypes.NewValue(tftypes.String, "prod"),
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.ImportResourceState(ctx, &tfprotov6.ImportResourceStateRequest{
		TypeName: "capi_cluster",
		Identity: &tfprotov6.ResourceIdentityData{IdentityData: &identity},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v", resp.Diagnostics[0])
	}
	if len(resp.ImportedResources) != 1 || resp.ImportedResources[0].Identity == nil {
		t.Fatalf("imported = %+v", resp.ImportedResources)
	}
	idVal, err := resp.ImportedResources[0].Identity.IdentityData.Unmarshal(idType)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]tftypes.Value
	if err := idVal.As(&fields); err != nil {
		t.Fatal(err)
	}
	var ns, name string
	_ = fields["namespace"].As(&ns)
	_ = fields["name"].As(&name)
	if ns != "capi" || name != "prod" {
		t.Errorf("identity = %s/%s", ns, name)
	}
}

func TestImportIDFromIdentity(t *testing.T) {
	s := types.StringValue
	if got := namespacedImportID(
		namespacedIdentityModel{Namespace: types.StringNull(), Name: s("prod")},
	); got != "default/prod" {
		t.Errorf("namespaced = %q", got)
	}
	if got := nodeGroupImportID(
		nodeGroupIdentityModel{Namespace: s("capi"), Cluster: s("prod"), Name: s("workers")},
	); got != "capi/prod/workers" {
		t.Errorf("node group = %q", got)
	}
	if got := providerImportID(
		providerIdentityModel{Type: s("infrastructure"), Name: s("tinkerbell")},
	); got != "infrastructure/tinkerbell" {
		t.Errorf("provider = %q", got)
	}
	if got := objectImportID(
		objectIdentityModel{Kind: s("IPPool"), Namespace: s("capi"), Name: s("nodes")},
	); got != "IPPool:capi/nodes" {
		t.Errorf("object = %q", got)
	}
	kind, ns, name, ok := parseObjectImportID(
		objectImportID(
			objectIdentityModel{Kind: s("IPPool"), Namespace: s("capi"), Name: s("nodes")},
		),
	)
	if !ok || kind != "IPPool" || ns != "capi" || name != "nodes" {
		t.Error("an object identity round-trips through the import ID parser")
	}
}
