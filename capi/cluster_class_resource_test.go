// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func classInputs(core string) clusterClassInputs {
	return clusterClassInputs{
		Name:        "prod",
		Namespace:   "capi",
		CoreVersion: core,
		Infrastructure: &objectRef{
			APIGroup:   "infrastructure.cluster.x-k8s.io",
			APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
			Kind:       "TinkerbellClusterTemplate",
			Name:       "prod-1234abcd",
			Namespace:  "capi",
		},
		ControlPlane: &objectRef{
			APIGroup:   "controlplane.cluster.x-k8s.io",
			APIVersion: "controlplane.cluster.x-k8s.io/v1alpha3",
			Kind:       "TalosControlPlaneTemplate",
			Name:       "prod-cp-aaaa1111",
		},
		ControlPlaneInfra: &objectRef{
			APIGroup:   "infrastructure.cluster.x-k8s.io",
			APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
			Kind:       "TinkerbellMachineTemplate",
			Name:       "cp-bbbb2222",
		},
		ControlPlaneLabels: map[string]string{"role": "cp"},
		Workers: []workerClassInputs{
			{
				Name: "worker-arm64",
				Infrastructure: &objectRef{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "TinkerbellMachineTemplate",
					Name:       "arm-1",
				},
				Bootstrap: &objectRef{
					APIVersion: "bootstrap.cluster.x-k8s.io/v1alpha3",
					Kind:       "TalosConfigTemplate",
					Name:       "worker-1",
				},
			},
			{
				Name: "worker-amd64",
				Infrastructure: &objectRef{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "TinkerbellMachineTemplate",
					Name:       "amd-1",
				},
				Bootstrap: &objectRef{
					APIVersion: "bootstrap.cluster.x-k8s.io/v1alpha3",
					Kind:       "TalosConfigTemplate",
					Name:       "worker-1",
				},
				FailureDomain: "rack-1",
				Labels:        map[string]string{"tier": "worker"},
			},
		},
		Variables: []any{
			map[string]any{
				"name":     "diskSize",
				"required": false,
				"schema":   map[string]any{"openAPIV3Schema": map[string]any{"type": "string"}},
			},
		},
		Patches: []any{map[string]any{"name": "diskSize", "definitions": []any{}}},
	}
}

func TestRenderClusterClass_V1Beta2(t *testing.T) {
	in := classInputs("v1beta2")
	obj := renderClusterClass(in)
	if obj.GetAPIVersion() != "cluster.x-k8s.io/v1beta2" || obj.GetKind() != "ClusterClass" ||
		obj.GetName() != "prod" ||
		obj.GetNamespace() != "capi" {
		t.Errorf(
			"identity = %s %s %s/%s",
			obj.GetAPIVersion(),
			obj.GetKind(),
			obj.GetNamespace(),
			obj.GetName(),
		)
	}
	infra := nestedMap(t, obj.Object, "spec", "infrastructure", "templateRef")
	if infra["kind"] != "TinkerbellClusterTemplate" || infra["name"] != "prod-1234abcd" ||
		infra["apiVersion"] != "infrastructure.cluster.x-k8s.io/v1beta1" {
		t.Errorf("infrastructure = %v", infra)
	}
	if _, ok := infra["namespace"]; ok {
		t.Error("v1beta2 template refs carry no namespace")
	}
	if nestedMap(t, obj.Object, "spec", "controlPlane", "templateRef")["name"] != "prod-cp-aaaa1111" ||
		nestedMap(t, obj.Object, "spec", "controlPlane", "machineInfrastructure", "templateRef")["name"] != "cp-bbbb2222" {
		t.Errorf("controlPlane = %v", nestedMap(t, obj.Object, "spec", "controlPlane"))
	}
	if nestedMap(t, obj.Object, "spec", "controlPlane", "metadata", "labels")["role"] != "cp" {
		t.Error("control plane labels")
	}
	mds := nestedSlice(t, obj.Object, "spec", "workers", "machineDeployments")
	if len(mds) != 2 || asMap(t, mds[0])["class"] != "worker-amd64" ||
		asMap(t, mds[1])["class"] != "worker-arm64" {
		t.Fatalf("classes are sorted: %v", mds)
	}
	amd := asMap(t, mds[0])
	if nestedMap(t, amd, "infrastructure", "templateRef")["name"] != "amd-1" ||
		nestedMap(t, amd, "bootstrap", "templateRef")["name"] != "worker-1" ||
		amd["failureDomain"] != "rack-1" ||
		nestedMap(t, amd, "metadata", "labels")["tier"] != "worker" {
		t.Errorf("amd64 class = %v", amd)
	}
	if _, ok := amd["template"]; ok {
		t.Error("v1beta2 classes have no template holder")
	}
	if len(nestedSlice(t, obj.Object, "spec", "variables")) != 1 ||
		len(nestedSlice(t, obj.Object, "spec", "patches")) != 1 {
		t.Error("variables and patches pass through")
	}
	live := readLiveClusterClass(obj)
	if live.Infrastructure != "prod-1234abcd" || live.ControlPlane != "prod-cp-aaaa1111" ||
		live.ControlPlaneInfra != "cp-bbbb2222" {
		t.Errorf("live = %+v", live)
	}
	if w := live.Workers["worker-amd64"]; w.Infrastructure != "amd-1" ||
		w.Bootstrap != "worker-1" ||
		w.FailureDomain != "rack-1" {
		t.Errorf("live worker = %+v", w)
	}
	if len(live.Variables) != 1 || len(live.Patches) != 1 {
		t.Errorf("live variables/patches = %v %v", live.Variables, live.Patches)
	}
}

func TestRenderClusterClass_V1Beta1(t *testing.T) {
	in := classInputs("v1beta1")
	obj := renderClusterClass(in)
	ref := nestedMap(t, obj.Object, "spec", "infrastructure", "ref")
	if ref["name"] != "prod-1234abcd" || ref["namespace"] != "capi" {
		t.Errorf("legacy infrastructure ref = %v", ref)
	}
	if nestedMap(t, obj.Object, "spec", "controlPlane", "ref")["name"] != "prod-cp-aaaa1111" ||
		nestedMap(t, obj.Object, "spec", "controlPlane", "machineInfrastructure", "ref")["namespace"] != "capi" {
		t.Errorf("legacy controlPlane = %v", nestedMap(t, obj.Object, "spec", "controlPlane"))
	}
	amd := asMap(t, nestedSlice(t, obj.Object, "spec", "workers", "machineDeployments")[0])
	if nestedMap(t, amd, "template", "infrastructure", "ref")["name"] != "amd-1" ||
		nestedMap(t, amd, "template", "bootstrap", "ref")["name"] != "worker-1" ||
		nestedMap(t, amd, "template", "metadata", "labels")["tier"] != "worker" {
		t.Errorf("legacy class = %v", amd)
	}
	live := readLiveClusterClass(obj)
	if live.Infrastructure != "prod-1234abcd" ||
		live.Workers["worker-arm64"].Infrastructure != "arm-1" {
		t.Errorf("legacy live = %+v", live)
	}
	// Minimal: no workers, no variables.
	minimal := clusterClassInputs{
		Name:           "x",
		Namespace:      "capi",
		CoreVersion:    "v1beta2",
		Infrastructure: in.Infrastructure,
		ControlPlane:   in.ControlPlane,
	}
	spec := nestedMap(t, renderClusterClass(minimal).Object, "spec")
	for _, absent := range []string{"workers", "variables", "patches"} {
		if _, ok := spec[absent]; ok {
			t.Errorf("%s rendered without a value", absent)
		}
	}
	if _, ok := nestedMap(t, spec, "controlPlane")["machineInfrastructure"]; ok {
		t.Error("no machineInfrastructure without a machine template")
	}
	if err := minimal.validate(); err != nil {
		t.Errorf("minimal is valid: %v", err)
	}
	minimal.ControlPlane = nil
	if err := minimal.validate(); err == nil ||
		!strings.Contains(err.Error(), "control_plane.template") {
		t.Errorf("missing control plane: %v", err)
	}
	minimal.Workers = []workerClassInputs{{Name: "w"}}
	if err := minimal.validate(); err == nil || !strings.Contains(err.Error(), `worker class "w"`) {
		t.Errorf("incomplete worker: %v", err)
	}
}

func TestDynamicListRoundTrip(t *testing.T) {
	list := []any{
		map[string]any{
			"name":     "diskSize",
			"required": true,
			"schema": map[string]any{
				"openAPIV3Schema": map[string]any{"type": "string", "default": "100Gi"},
			},
		},
	}
	d, diags := dynamicFromList(list)
	if diags.HasError() {
		t.Fatal(diags)
	}
	back, diags := dynamicToList(d, "variables")
	if diags.HasError() || len(back) != 1 {
		t.Fatalf("back = %v %v", back, diags)
	}
	if m, ok := back[0].(map[string]any); !ok || m["name"] != "diskSize" || m["required"] != true {
		t.Errorf("back = %v", back)
	}
	if _, diags := dynamicToList(
		types.DynamicValue(types.StringValue("x")),
		"variables",
	); !diags.HasError() {
		t.Error("a string is not a list")
	}
	if l, diags := dynamicToList(types.DynamicNull(), "variables"); diags.HasError() || l != nil {
		t.Error("null is nil")
	}
	if d, _ := dynamicFromList(nil); !d.IsNull() {
		t.Error("nil is null")
	}
}
