// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResourceSetRender(t *testing.T) {
	in := resourceSetInputs{
		Name:            "gateway",
		Namespace:       "capi",
		APIVersion:      "addons.cluster.x-k8s.io/v1beta2",
		ClusterSelector: map[string]string{clusterNameLabel: "prod"},
		Strategy:        "Reconcile",
		Manifests: []string{
			crsManifests,
		},
		Secrets: []string{
			"apiVersion: v1\nkind: Secret\nmetadata:\n  name: token\n  namespace: gateway-system\nstringData:\n  t: x\n",
		},
	}
	objects, err := in.objects()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objects {
		got = append(got, o.String())
	}
	if strings.Join(
		got,
		";",
	) != "Namespace gateway-system;Deployment gateway-system/gateway;Secret gateway-system/token" {
		t.Errorf("objects = %v", got)
	}
	crs, holders := in.render()
	if crs.GetAPIVersion() != in.APIVersion || crs.GetKind() != "ClusterResourceSet" ||
		crs.GetName() != "gateway" {
		t.Errorf("identity = %s %s %s", crs.GetAPIVersion(), crs.GetKind(), crs.GetName())
	}
	spec := nestedMap(t, crs.Object, "spec")
	if spec["strategy"] != "Reconcile" ||
		nestedMap(t, spec, "clusterSelector", "matchLabels")[clusterNameLabel] != "prod" {
		t.Errorf("spec = %v", spec)
	}
	refs := resourceRefs(crs)
	if len(refs) != 2 || refs[0].Kind != "ConfigMap" || refs[0].Name != "gateway-0" ||
		refs[1].Kind != "Secret" ||
		refs[1].Name != "gateway-secret-0" {
		t.Errorf("refs = %v", refs)
	}
	if len(holders) != 2 {
		t.Fatalf("holders = %d", len(holders))
	}
	cm, secret := holders[0], holders[1]
	if cm.GetKind() != "ConfigMap" || cm.GetName() != "gateway-0" ||
		cm.GetLabels()[resourceSetLabel] != "gateway" ||
		nestedMap(t, cm.Object, "data")[resourceSetManifestKey] != crsManifests {
		t.Errorf("configmap = %v", cm.Object)
	}
	if secret.GetKind() != "Secret" || secret.GetName() != "gateway-secret-0" ||
		secret.Object["type"] != resourceSetSecretType ||
		nestedMap(t, secret.Object, "stringData")[resourceSetManifestKey] == "" {
		t.Errorf("secret = %v", secret.Object)
	}
	// Default strategy.
	in.Strategy = ""
	crs, _ = in.render()
	if nestedMap(t, crs.Object, "spec")["strategy"] != "ApplyOnce" {
		t.Error("strategy defaults to ApplyOnce")
	}
}

func TestValidateManifest(t *testing.T) {
	if err := validateManifest(crsManifests); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := validateManifest(
		"apiVersion: v1\nkind: ConfigMap\n",
	); err == nil ||
		!strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("nameless: %v", err)
	}
	if err := validateManifest("metadata:\n  name: x\n"); err == nil {
		t.Error("a document without a kind must fail")
	}
	if err := validateManifest(
		strings.Repeat("x", configMapLimit+1),
	); err == nil ||
		!strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("too large: %v", err)
	}
}

func TestBindingSatisfied(t *testing.T) {
	expected := []appliedObject{
		{Kind: "ConfigMap", Name: "gateway-0"},
		{Kind: "Secret", Name: "gateway-secret-0"},
	}
	binding := newObject(
		gvkFor("addons.cluster.x-k8s.io/v1beta2", "ClusterResourceSetBinding"),
		"capi",
		"prod",
		nil,
		nil,
	)
	binding.Object["spec"] = map[string]any{"bindings": []any{
		map[string]any{
			"clusterResourceSetName": "other",
			"resources": []any{
				map[string]any{"kind": "ConfigMap", "name": "x", "applied": true},
			},
		},
		map[string]any{"clusterResourceSetName": "gateway", "resources": []any{
			map[string]any{
				"kind":    "ConfigMap",
				"name":    "gateway-0",
				"applied": true,
				"hash":    "sha256:abc",
			},
			map[string]any{"kind": "Secret", "name": "gateway-secret-0", "applied": false},
		}},
	}}
	bound, ok := readBinding(binding, "gateway")
	if !ok || len(bound) != 2 || bound[0].Hash != "sha256:abc" || !bound[0].Applied ||
		bound[1].Applied {
		t.Errorf("binding = %v %v", bound, ok)
	}
	if _, ok := readBinding(binding, "missing"); ok {
		t.Error("a set the binding does not name is not bound")
	}
	if ok, why := bindingSatisfied(
		bound,
		expected,
	); ok ||
		!strings.Contains(why, "Secret gateway-secret-0 not applied") {
		t.Errorf("unapplied = %v %q", ok, why)
	}
	bound[1].Applied = true
	if ok, _ := bindingSatisfied(bound, expected); !ok {
		t.Error("all applied")
	}
	if ok, why := bindingSatisfied(
		bound[:1],
		expected,
	); ok ||
		!strings.Contains(why, "not bound yet") {
		t.Errorf("missing = %v %q", ok, why)
	}
}

func TestObjectReady(t *testing.T) {
	mk := func(apiVersion, kind string, generation int64, spec, status map[string]any) *unstructured.Unstructured {
		obj := newObject(gvkFor(apiVersion, kind), "ns", "x", nil, nil)
		obj.SetGeneration(generation)
		if spec != nil {
			obj.Object["spec"] = spec
		}
		if status != nil {
			obj.Object["status"] = status
		}
		return obj
	}
	for name, tc := range map[string]struct {
		obj   *unstructured.Unstructured
		ready bool
		why   string
	}{
		"deployment unobserved": {mk("apps/v1", "Deployment", 2, map[string]any{"replicas": int64(2)}, map[string]any{"observedGeneration": int64(1), "availableReplicas": int64(2)}), false, "not observed"},
		"deployment partial":    {mk("apps/v1", "Deployment", 2, map[string]any{"replicas": int64(2)}, map[string]any{"observedGeneration": int64(2), "availableReplicas": int64(1)}), false, "1 of 2 replicas"},
		"deployment ready":      {mk("apps/v1", "Deployment", 2, map[string]any{"replicas": int64(2)}, map[string]any{"observedGeneration": int64(2), "availableReplicas": int64(2)}), true, ""},
		"deployment default":    {mk("apps/v1", "StatefulSet", 1, map[string]any{}, map[string]any{"observedGeneration": int64(1), "availableReplicas": int64(1)}), true, ""},
		"daemonset":             {mk("apps/v1", "DaemonSet", 1, nil, map[string]any{"desiredNumberScheduled": int64(3), "numberReady": int64(2)}), false, "2 of 3 pods"},
		"job complete":          {mk("batch/v1", "Job", 1, nil, map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}), true, ""},
		"job failed":            {mk("batch/v1", "Job", 1, nil, map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True", "message": "backoff"}}}), false, "failed: backoff"},
		"crd":                   {mk("apiextensions.k8s.io/v1", "CustomResourceDefinition", 1, nil, map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "False", "reason": "Installing"}}}), false, "not established"},
		"ready condition":       {mk("cilium.io/v2alpha1", "CiliumLoadBalancerIPPool", 1, nil, map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "message": "conflict"}}}), false, "Ready=False: conflict"},
		"plain object":          {mk("v1", "Namespace", 1, nil, nil), true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ready, why := objectReady(tc.obj)
			if ready != tc.ready || !strings.Contains(why, tc.why) {
				t.Errorf("ready = %v why = %q", ready, why)
			}
		})
	}
}
