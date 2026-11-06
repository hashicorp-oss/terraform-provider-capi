// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

func TestTemplateResources_Schema(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		res          resource.Resource
		providerAttr string
		hasRole      bool
		typeName     string
	}{
		{NewMachineTemplateResource(), "infrastructure", true, "capi_machine_template"},
		{NewControlPlaneTemplateResource(), "control_plane", false, "capi_control_plane_template"},
		{NewBootstrapTemplateResource(), "bootstrap", false, "capi_bootstrap_template"},
	} {
		resp := &resource.SchemaResponse{}
		tc.res.Schema(ctx, resource.SchemaRequest{}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatal(resp.Diagnostics)
		}
		mresp := &resource.MetadataResponse{}
		tc.res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "capi"}, mresp)
		if mresp.TypeName != tc.typeName {
			t.Errorf("type name = %s", mresp.TypeName)
		}
		attrs := resp.Schema.Attributes
		for _, name := range []string{tc.providerAttr, "name", "namespace", "api_version", "kind", "spec", "revision_history", "object_name", "revision", "ref", "revisions"} {
			if _, ok := attrs[name]; !ok {
				t.Errorf("%s lacks %q", tc.typeName, name)
			}
		}
		if _, ok := attrs["role"]; ok != tc.hasRole {
			t.Errorf("%s role attribute present = %v", tc.typeName, ok)
		}
		if _, ok := attrs["spec"].(schema.DynamicAttribute); !ok {
			t.Errorf("%s spec must be dynamic", tc.typeName)
		}
		if _, ok := attrs["provider"]; ok {
			t.Errorf("%s must not use the reserved attribute name provider", tc.typeName)
		}
	}
	oresp := &resource.SchemaResponse{}
	NewObjectResource().Schema(ctx, resource.SchemaRequest{}, oresp)
	if oresp.Diagnostics.HasError() {
		t.Fatal(oresp.Diagnostics)
	}
}

func TestTemplateResource_Render(t *testing.T) {
	r := &templateResource{kind: machineTemplateKind}
	in := templateInputs{
		provider:    "tinkerbell",
		role:        "machine",
		name:        "worker",
		namespace:   "capi",
		labels:      map[string]string{"tier": "worker"},
		annotations: map[string]string{"a": "b"},
		spec: map[string]any{
			"hardwareAffinity": map[string]any{
				"required": []any{
					map[string]any{
						"labelSelector": map[string]any{
							"matchLabels": map[string]any{"type": "worker"},
						},
					},
				},
			},
		},
	}
	gvk := gvkFor("infrastructure.cluster.x-k8s.io/v1beta1", "TinkerbellMachineTemplate")
	obj, hash, err := r.render(in, gvk)
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != 8 || obj.GetName() != "worker-"+hash || obj.GetNamespace() != "capi" {
		t.Errorf("name = %s hash = %s", obj.GetName(), hash)
	}
	if obj.GetLabels()[templateLabel] != "worker" || obj.GetLabels()["tier"] != "worker" ||
		obj.GetAnnotations()[revisionAnnotation] != hash ||
		obj.GetAnnotations()["a"] != "b" {
		t.Errorf("metadata = %v %v", obj.GetLabels(), obj.GetAnnotations())
	}
	spec, _ := obj.Object["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	if _, ok := tmpl["spec"].(map[string]any)["hardwareAffinity"]; !ok {
		t.Errorf("spec = %v", obj.Object["spec"])
	}
	// The hash is of the content only: same spec, same revision.
	_, again, _ := r.render(in, gvk)
	if again != hash {
		t.Error("hash must be deterministic")
	}
	in.spec = map[string]any{"hardwareAffinity": map[string]any{}}
	_, changed, _ := r.render(in, gvk)
	if changed == hash {
		t.Error("a spec change is a new revision")
	}
	want, _ := capi.SpecHash(templateSpec(in.spec))
	if changed != want {
		t.Error("the hash is SpecHash of spec.template.spec")
	}
}

func TestTemplateKinds_RoleSuffixes(t *testing.T) {
	if machineTemplateKind.roles["machine"] != "MachineTemplate" ||
		machineTemplateKind.roles["cluster"] != "ClusterTemplate" {
		t.Error("machine template roles")
	}
	if controlPlaneTemplateKind.roles[controlPlaneTemplateKind.defaultRole] != "ControlPlaneTemplate" {
		t.Error("control plane template suffix")
	}
	if bootstrapTemplateKind.roles[bootstrapTemplateKind.defaultRole] != "ConfigTemplate" {
		t.Error("bootstrap template suffix")
	}
	for _, k := range []templateKind{machineTemplateKind, controlPlaneTemplateKind, bootstrapTemplateKind} {
		if !strings.HasPrefix(k.typeSuffix, "_") {
			t.Errorf("type suffix %q", k.typeSuffix)
		}
	}
}
