// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func i64(v int64) *int64 { return &v }

func dur(d time.Duration) *time.Duration { return &d }

func directInputs(core string) nodeGroupInputs {
	return nodeGroupInputs{
		Cluster:       "prod",
		Namespace:     "capi",
		Name:          "workers",
		CoreVersion:   core,
		Replicas:      i64(3),
		Version:       "v1.34.0",
		FailureDomain: "rack-1",
		Infrastructure: &objectRef{
			APIGroup:   "infrastructure.cluster.x-k8s.io",
			APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
			Kind:       "TinkerbellMachineTemplate",
			Name:       "worker-abcd1234",
			Namespace:  "capi",
		},
		Bootstrap: &objectRef{
			APIGroup:   "bootstrap.cluster.x-k8s.io",
			APIVersion: "bootstrap.cluster.x-k8s.io/v1alpha3",
			Kind:       "TalosConfigTemplate",
			Name:       "worker-ef567890",
			Namespace:  "capi",
		},
		Labels: map[string]string{
			"tier": "worker",
		},
		Annotations:    map[string]string{"note": "x"},
		Strategy:       "RollingUpdate",
		MaxSurge:       i64(1),
		MaxUnavailable: i64(0),
		NodeDrain:      dur(10 * time.Minute),
		NodeDeletion:   dur(2 * time.Minute),
	}
}

func nested(t *testing.T, obj map[string]any, fields ...string) any {
	t.Helper()
	v, ok, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if err != nil || !ok {
		t.Fatalf("missing %s: %v", strings.Join(fields, "."), err)
	}
	return v
}

func nestedMap(t *testing.T, obj map[string]any, fields ...string) map[string]any {
	t.Helper()
	m, ok := nested(t, obj, fields...).(map[string]any)
	if !ok {
		t.Fatalf("%s is not a map", strings.Join(fields, "."))
	}
	return m
}

func nestedSlice(t *testing.T, obj map[string]any, fields ...string) []any {
	t.Helper()
	l, ok := nested(t, obj, fields...).([]any)
	if !ok {
		t.Fatalf("%s is not a list", strings.Join(fields, "."))
	}
	return l
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not a map", v)
	}
	return m
}

func TestMachineDeploymentResources_Schema(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		res      resource.Resource
		typeName string
		attrs    []string
	}{
		{NewMachineDeploymentResource(), "capi_machine_deployment", []string{"cluster", "replicas", "autoscaling", "template", "class", "rollout", "deletion", "timeouts", "wait", "scale_to_zero_on_delete", "object_name", "status"}},
		{NewMachineHealthCheckResource(), "capi_machine_health_check", []string{"cluster", "machine_deployment", "control_plane", "selector", "unhealthy_node_conditions", "remediation", "wait", "status"}},
	} {
		resp := &resource.SchemaResponse{}
		tc.res.Schema(ctx, resource.SchemaRequest{}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", tc.typeName, resp.Diagnostics)
		}
		for _, a := range tc.attrs {
			if _, ok := resp.Schema.Attributes[a]; !ok {
				t.Errorf("%s: missing attribute %s", tc.typeName, a)
			}
		}
		meta := &resource.MetadataResponse{}
		tc.res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "capi"}, meta)
		if meta.TypeName != tc.typeName {
			t.Errorf("type name = %s", meta.TypeName)
		}
	}
}

func TestRenderMachineDeployment_V1Beta2(t *testing.T) {
	in := directInputs("v1beta2")
	obj := renderMachineDeployment(in)
	if obj.GetAPIVersion() != "cluster.x-k8s.io/v1beta2" || obj.GetKind() != "MachineDeployment" ||
		obj.GetName() != "workers" ||
		obj.GetNamespace() != "capi" {
		t.Errorf(
			"identity = %s %s %s/%s",
			obj.GetAPIVersion(),
			obj.GetKind(),
			obj.GetNamespace(),
			obj.GetName(),
		)
	}
	if obj.GetLabels()[clusterNameLabel] != "prod" {
		t.Errorf("labels = %v", obj.GetLabels())
	}
	if _, ok := obj.GetAnnotations()[autoscalerMinSizeAnnotation]; ok {
		t.Error("no autoscaler annotations without autoscaling")
	}
	if nested(t, obj.Object, "spec", "replicas") != int64(3) ||
		nested(t, obj.Object, "spec", "clusterName") != "prod" {
		t.Errorf("spec = %v", obj.Object["spec"])
	}
	sel := nestedMap(t, obj.Object, "spec", "selector", "matchLabels")
	tmplLabels := nestedMap(t, obj.Object, "spec", "template", "metadata", "labels")
	for _, m := range []map[string]any{sel, tmplLabels} {
		if m[clusterNameLabel] != "prod" || m[deploymentNameLabel] != "workers" {
			t.Errorf("selector labels = %v", m)
		}
	}
	if tmplLabels["tier"] != "worker" {
		t.Errorf("user labels missing: %v", tmplLabels)
	}
	if nestedMap(t, obj.Object, "spec", "template", "metadata", "annotations")["note"] != "x" {
		t.Error("user annotations go on the machine template")
	}
	infra := nestedMap(t, obj.Object, "spec", "template", "spec", "infrastructureRef")
	if infra["apiGroup"] != "infrastructure.cluster.x-k8s.io" ||
		infra["kind"] != "TinkerbellMachineTemplate" ||
		infra["name"] != "worker-abcd1234" {
		t.Errorf("v1beta2 ref = %v", infra)
	}
	if _, ok := infra["apiVersion"]; ok {
		t.Error("v1beta2 refs carry apiGroup, not apiVersion")
	}
	boot := nestedMap(t, obj.Object, "spec", "template", "spec", "bootstrap", "configRef")
	if boot["kind"] != "TalosConfigTemplate" {
		t.Errorf("bootstrap ref = %v", boot)
	}
	if nested(t, obj.Object, "spec", "template", "spec", "version") != "v1.34.0" ||
		nested(t, obj.Object, "spec", "template", "spec", "failureDomain") != "rack-1" {
		t.Errorf("machine spec = %v", nested(t, obj.Object, "spec", "template", "spec"))
	}
	ru := nestedMap(t, obj.Object, "spec", "rollout", "strategy", "rollingUpdate")
	if ru["maxSurge"] != int64(1) || ru["maxUnavailable"] != int64(0) {
		t.Errorf("rollingUpdate = %v", ru)
	}
	if nested(t, obj.Object, "spec", "rollout", "strategy", "type") != "RollingUpdate" {
		t.Error("strategy type")
	}
	if nested(t, obj.Object, "spec", "deletion", "nodeDrainTimeoutSeconds") != int64(600) ||
		nested(t, obj.Object, "spec", "deletion", "nodeDeletionTimeoutSeconds") != int64(120) {
		t.Errorf("deletion = %v", nested(t, obj.Object, "spec", "deletion"))
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(
		obj.Object,
		"spec",
		"deletion",
		"nodeVolumeDetachTimeoutSeconds",
	); ok {
		t.Error("unset timeouts are not rendered")
	}
}

func TestRenderMachineDeployment_V1Beta1(t *testing.T) {
	in := directInputs("v1beta1")
	in.Infrastructure.Namespace = ""
	obj := renderMachineDeployment(in)
	if obj.GetAPIVersion() != "cluster.x-k8s.io/v1beta1" {
		t.Errorf("apiVersion = %s", obj.GetAPIVersion())
	}
	infra := nestedMap(t, obj.Object, "spec", "template", "spec", "infrastructureRef")
	if infra["apiVersion"] != "infrastructure.cluster.x-k8s.io/v1beta1" ||
		infra["namespace"] != "capi" ||
		infra["name"] != "worker-abcd1234" {
		t.Errorf("legacy ref = %v", infra)
	}
	if _, ok := infra["apiGroup"]; ok {
		t.Error("legacy refs carry apiVersion, not apiGroup")
	}
	if nested(t, obj.Object, "spec", "strategy", "type") != "RollingUpdate" {
		t.Errorf("legacy strategy = %v", obj.Object["spec"])
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "rollout"); ok {
		t.Error("legacy shape has no rollout")
	}
	if nested(t, obj.Object, "spec", "template", "spec", "nodeDrainTimeout") != "10m0s" {
		t.Errorf(
			"legacy timeouts live on the machine spec: %v",
			nested(t, obj.Object, "spec", "template", "spec"),
		)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "deletion"); ok {
		t.Error("legacy shape has no deletion block")
	}
}

func TestRenderMachineDeployment_Autoscaling(t *testing.T) {
	in := directInputs("v1beta2")
	in.Replicas = nil
	in.AutoscalingMin, in.AutoscalingMax = i64(1), i64(5)
	obj := renderMachineDeployment(in)
	if _, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "replicas"); ok {
		t.Error("the autoscaler owns replicas")
	}
	ann := obj.GetAnnotations()
	if ann[autoscalerMinSizeAnnotation] != "1" || ann[autoscalerMaxSizeAnnotation] != "5" {
		t.Errorf("autoscaler annotations on the object = %v", ann)
	}
	tmplAnn := nestedMap(t, obj.Object, "spec", "template", "metadata", "annotations")
	if _, ok := tmplAnn[autoscalerMinSizeAnnotation]; ok {
		t.Error("autoscaler annotations do not belong on the machines")
	}
	if tmplAnn["note"] != "x" {
		t.Errorf("template annotations = %v", tmplAnn)
	}
}

func TestRenderMachineDeployment_OnDelete(t *testing.T) {
	in := directInputs("v1beta2")
	in.Strategy = "OnDelete"
	obj := renderMachineDeployment(in)
	strategy := nestedMap(t, obj.Object, "spec", "rollout", "strategy")
	if strategy["type"] != "OnDelete" {
		t.Errorf("strategy = %v", strategy)
	}
	if _, ok := strategy["rollingUpdate"]; ok {
		t.Error("OnDelete has no rollingUpdate settings")
	}
	in.Strategy, in.MaxSurge, in.MaxUnavailable = "", nil, nil
	obj = renderMachineDeployment(in)
	if _, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "rollout"); ok {
		t.Error("no rollout block without settings")
	}
}

func TestRenderTopologyEntry(t *testing.T) {
	in := nodeGroupInputs{
		Cluster:        "prod",
		Namespace:      "capi",
		Name:           "workers",
		CoreVersion:    "v1beta2",
		Class:          "worker-amd64",
		AutoscalingMin: i64(2),
		AutoscalingMax: i64(8),
		FailureDomain:  "rack-1",
		Variables: map[string]any{
			"zeta":  "z",
			"alpha": int64(1),
		},
		Labels:    map[string]string{"tier": "worker"},
		Strategy:  "RollingUpdate",
		MaxSurge:  i64(1),
		NodeDrain: dur(time.Minute),
	}
	entry, patch := renderTopologyEntry(in)
	if entry["name"] != "workers" || entry["class"] != "worker-amd64" ||
		entry["failureDomain"] != "rack-1" {
		t.Errorf("entry = %v", entry)
	}
	if _, ok := entry["replicas"]; ok {
		t.Error("the autoscaler owns replicas")
	}
	ann := nestedMap(t, entry, "metadata", "annotations")
	if ann[autoscalerMinSizeAnnotation] != "2" || ann[autoscalerMaxSizeAnnotation] != "8" {
		t.Errorf(
			"the topology controller copies the entry's annotations onto the MachineDeployment: %v",
			ann,
		)
	}
	if nestedMap(t, entry, "metadata", "labels")["tier"] != "worker" {
		t.Error("labels")
	}
	overrides := nestedSlice(t, entry, "variables", "overrides")
	if len(overrides) != 2 || asMap(t, overrides[0])["name"] != "alpha" ||
		asMap(t, overrides[1])["name"] != "zeta" {
		t.Errorf("overrides are sorted by name: %v", overrides)
	}
	if nested(t, entry, "rollout", "strategy", "rollingUpdate", "maxSurge") != int64(1) ||
		nested(t, entry, "deletion", "nodeDrainTimeoutSeconds") != int64(60) {
		t.Errorf("entry = %v", entry)
	}
	if patch.GetKind() != "Cluster" || patch.GetName() != "prod" ||
		patch.GetAPIVersion() != "cluster.x-k8s.io/v1beta2" {
		t.Errorf("patch targets the Cluster: %s %s", patch.GetKind(), patch.GetName())
	}
	mds := nestedSlice(t, patch.Object, "spec", "topology", "workers", "machineDeployments")
	if len(mds) != 1 || asMap(t, mds[0])["name"] != "workers" {
		t.Errorf("patch carries only this entry: %v", mds)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(patch.Object, "spec", "topology", "class"); ok {
		t.Error("the patch must not touch fields the cluster resource owns")
	}
	removal := renderTopologyRemoval(in)
	if got := nestedSlice(
		t,
		removal.Object,
		"spec",
		"topology",
		"workers",
		"machineDeployments",
	); len(
		got,
	) != 0 {
		t.Errorf("removal names no entries: %v", got)
	}
	if nodeGroupFieldManager("workers") == nodeGroupFieldManager("other") {
		t.Error("one field manager per group")
	}
}

func TestRenderTopologyEntry_Legacy(t *testing.T) {
	in := nodeGroupInputs{
		Cluster:      "prod",
		Namespace:    "capi",
		Name:         "workers",
		CoreVersion:  "v1beta1",
		Class:        "w",
		Replicas:     i64(2),
		Strategy:     "OnDelete",
		NodeDeletion: dur(time.Minute),
	}
	entry, _ := renderTopologyEntry(in)
	if entry["replicas"] != int64(2) || nested(t, entry, "strategy", "type") != "OnDelete" ||
		entry["nodeDeletionTimeout"] != "1m0s" {
		t.Errorf("legacy entry = %v", entry)
	}
	if _, ok := entry["metadata"]; ok {
		t.Error("no metadata without labels or annotations")
	}
}

func TestNodeGroupInputs_Validate(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*nodeGroupInputs)
		want   string
	}{
		"ok":                 {func(*nodeGroupInputs) {}, ""},
		"class and template": {func(in *nodeGroupInputs) { in.Class = "w" }, "class and template are exclusive"},
		"missing templates":  {func(in *nodeGroupInputs) { in.Bootstrap = nil }, "template.infrastructure and template.bootstrap are required"},
		"missing version":    {func(in *nodeGroupInputs) { in.Version = "" }, "version is required"},
		"replicas and autoscaling": {func(in *nodeGroupInputs) {
			in.AutoscalingMin, in.AutoscalingMax = i64(1), i64(2)
		}, "replicas and autoscaling are exclusive"},
		"half autoscaling": {func(in *nodeGroupInputs) { in.Replicas = nil; in.AutoscalingMin = i64(1) }, "both min and max"},
		"min above max":    {func(in *nodeGroupInputs) { in.Replicas = nil; in.AutoscalingMin, in.AutoscalingMax = i64(3), i64(2) }, "must not exceed"},
		"bad strategy":     {func(in *nodeGroupInputs) { in.Strategy = "Recreate" }, "not RollingUpdate or OnDelete"},
	} {
		t.Run(name, func(t *testing.T) {
			in := directInputs("v1beta2")
			tc.mutate(&in)
			err := in.validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func clusterObject(
	core string,
	topology map[string]any,
	status map[string]any,
) *unstructured.Unstructured {
	obj := newObject(gvkFor("cluster.x-k8s.io/"+core, "Cluster"), "capi", "prod", nil, nil)
	spec := map[string]any{}
	if topology != nil {
		spec["topology"] = topology
	}
	obj.Object["spec"] = spec
	if status != nil {
		obj.Object["status"] = status
	}
	return obj
}

func TestReadClusterFacts(t *testing.T) {
	class := newObject(gvkFor("cluster.x-k8s.io/v1beta2", "ClusterClass"), "capi", "main", nil, nil)
	class.Object["spec"] = map[string]any{"workers": map[string]any{"machineDeployments": []any{
		map[string]any{"class": "worker-arm64"}, map[string]any{"class": "worker-amd64"},
	}}}
	topo := clusterObject(
		"v1beta2",
		map[string]any{
			"classRef": map[string]any{"name": "main"},
			"version":  "v1.34.1",
			"workers": map[string]any{
				"machineDeployments": []any{
					map[string]any{
						"name":          "workers",
						"class":         "worker-amd64",
						"replicas":      int64(3),
						"failureDomain": "rack-1",
					},
				},
			},
		},
		map[string]any{"failureDomains": []any{"rack-2", "rack-1"}},
	)
	facts := readClusterFacts(topo, class)
	if facts.CoreVersion != "v1beta2" || !facts.TopologyMode || facts.Version != "v1.34.1" {
		t.Errorf("facts = %+v", facts)
	}
	if strings.Join(facts.Classes, ",") != "worker-amd64,worker-arm64" ||
		strings.Join(facts.FailureDomains, ",") != "rack-2,rack-1" {
		t.Errorf("classes = %v domains = %v", facts.Classes, facts.FailureDomains)
	}
	name, ns := clusterClassRef(topo)
	if name != "main" || ns != "" {
		t.Errorf("classRef = %s/%s", ns, name)
	}
	entry := findTopologyEntry(topo, "workers")
	if entry == nil || findTopologyEntry(topo, "other") != nil {
		t.Fatal("findTopologyEntry")
	}
	live := readLiveTopologyEntry(entry)
	if live.Replicas == nil || *live.Replicas != 3 || live.Class != "worker-amd64" ||
		live.FailureDomain != "rack-1" {
		t.Errorf("live = %+v", live)
	}

	// Legacy: class by name, failure domains as a map.
	direct := clusterObject(
		"v1beta1",
		map[string]any{"class": "main", "classNamespace": "classes"},
		map[string]any{
			"failureDomains": map[string]any{"b": map[string]any{}, "a": map[string]any{}},
		},
	)
	facts = readClusterFacts(direct, nil)
	if facts.CoreVersion != "v1beta1" || !facts.TopologyMode ||
		strings.Join(facts.FailureDomains, ",") != "a,b" ||
		len(facts.Classes) != 0 {
		t.Errorf("legacy facts = %+v", facts)
	}
	if name, ns := clusterClassRef(direct); name != "main" || ns != "classes" {
		t.Errorf("legacy classRef = %s/%s", ns, name)
	}
	plain := clusterObject("v1beta2", nil, nil)
	if facts := readClusterFacts(plain, nil); facts.TopologyMode || facts.Version != "" {
		t.Errorf("direct-mode facts = %+v", facts)
	}
}

func TestCheckAgainstCluster(t *testing.T) {
	topo := clusterFacts{
		CoreVersion:    "v1beta2",
		TopologyMode:   true,
		Classes:        []string{"worker-amd64"},
		FailureDomains: []string{"rack-1"},
	}
	direct := clusterFacts{CoreVersion: "v1beta2"}
	in := directInputs("v1beta2")
	if err := in.checkAgainstCluster(direct); err != nil {
		t.Errorf("direct against direct: %v", err)
	}
	if err := in.checkAgainstCluster(
		topo,
	); err == nil ||
		!strings.Contains(err.Error(), "uses a ClusterClass") {
		t.Errorf("template against topology: %v", err)
	}
	cls := nodeGroupInputs{Cluster: "prod", Class: "worker-amd64", FailureDomain: "rack-1"}
	if err := cls.checkAgainstCluster(topo); err != nil {
		t.Errorf("class against topology: %v", err)
	}
	if err := cls.checkAgainstCluster(
		direct,
	); err == nil ||
		!strings.Contains(err.Error(), "has no topology") {
		t.Errorf("class against direct: %v", err)
	}
	cls.Class = "nope"
	if err := cls.checkAgainstCluster(
		topo,
	); err == nil ||
		!strings.Contains(err.Error(), "not a worker class") {
		t.Errorf("unknown class: %v", err)
	}
	cls.Class, cls.FailureDomain = "worker-amd64", "rack-9"
	if err := cls.checkAgainstCluster(
		topo,
	); err == nil ||
		!strings.Contains(err.Error(), "not one of the cluster's failure domains") {
		t.Errorf("unknown failure domain: %v", err)
	}
	// Nothing to check against is not an error.
	if err := cls.checkAgainstCluster(clusterFacts{TopologyMode: true}); err != nil {
		t.Errorf("no facts: %v", err)
	}
}

func TestReadLiveMachineDeployment(t *testing.T) {
	obj := renderMachineDeployment(directInputs("v1beta2"))
	obj.Object["status"] = map[string]any{
		"phase":             "Running",
		"replicas":          int64(3),
		"readyReplicas":     int64(2),
		"availableReplicas": int64(2),
		"upToDateReplicas":  int64(3),
		"conditions": []any{
			map[string]any{"type": "Available", "status": "True", "reason": "Available"},
		},
	}
	live := readLiveMachineDeployment(obj)
	if live.Replicas == nil || *live.Replicas != 3 || live.Version != "v1.34.0" ||
		live.FailureDomain != "rack-1" ||
		live.Infrastructure != "worker-abcd1234" ||
		live.Bootstrap != "worker-ef567890" {
		t.Errorf("live = %+v", live)
	}
	st := readNodeGroupStatus(obj)
	if st.ObjectName != "workers" || st.Phase != "Running" || st.Replicas != 3 ||
		st.ReadyReplicas != 2 ||
		st.AvailableReplicas != 2 ||
		st.UpToDateReplicas != 3 {
		t.Errorf("status = %+v", st)
	}
	if len(st.Conditions) != 1 || st.Conditions[0].Type != "Available" ||
		st.Conditions[0].Status != "True" {
		t.Errorf("conditions = %+v", st.Conditions)
	}
}

func machineObject(
	name, phase string,
	deleting bool,
	conds []map[string]any,
	spec map[string]any,
) unstructured.Unstructured {
	obj := newObject(gvkFor("cluster.x-k8s.io/v1beta2", "Machine"), "capi", name, nil, nil)
	if spec == nil {
		spec = map[string]any{}
	}
	obj.Object["spec"] = spec
	raw := make([]any, 0, len(conds))
	for _, c := range conds {
		raw = append(raw, c)
	}
	obj.Object["status"] = map[string]any{"phase": phase, "conditions": raw}
	if deleting {
		_ = unstructured.SetNestedField(
			obj.Object,
			"2026-11-05T00:00:00Z",
			"metadata",
			"deletionTimestamp",
		)
	}
	return *obj
}

func TestReadMachineFailures(t *testing.T) {
	machines := []unstructured.Unstructured{
		machineObject(
			"ready",
			"Running",
			false,
			[]map[string]any{
				{"type": "Ready", "status": "True"},
				{"type": "UpToDate", "status": "False"},
			},
			nil,
		),
		machineObject("booting", "Provisioning", false, []map[string]any{
			{"type": "Ready", "status": "False", "reason": "NotReady", "message": "Node not ready"},
			{"type": "BootstrapConfigReady", "status": "True"},
			{"type": "Paused", "status": "False"},
		}, nil),
		machineObject("draining", "Deleting", true, []map[string]any{
			{"type": "Ready", "status": "True"},
			{
				"type":    "Deleting",
				"status":  "True",
				"reason":  "DrainingNode",
				"message": "Drain not completed yet: pods x",
			},
		}, map[string]any{"deletion": map[string]any{"nodeDrainTimeoutSeconds": int64(600)}}),
		machineObject("detaching", "Deleting", true, []map[string]any{
			{
				"type":    "Deleting",
				"status":  "True",
				"reason":  "WaitingForVolumeDetach",
				"message": "Waiting for volumes",
			},
		}, map[string]any{"nodeVolumeDetachTimeout": "5m0s"}),
		machineObject("node-gone", "Deleting", true, nil, nil),
	}
	failures := readMachineFailures(machines)
	names := make([]string, 0, len(failures))
	for _, f := range failures {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "booting,draining,detaching,node-gone" {
		t.Fatalf("failures = %v", names)
	}
	booting := failures[0]
	if booting.Phase != "Provisioning" || len(booting.Conditions) != 1 ||
		booting.Conditions[0].Type != "Ready" ||
		booting.Deleting {
		t.Errorf("booting = %+v", booting)
	}
	if s := booting.String(); !strings.Contains(s, "Ready=False (NotReady): Node not ready") {
		t.Errorf("booting string = %s", s)
	}
	if f := failures[1]; !f.Deleting || f.Timeout != "deletion.node_drain_timeout 600s" {
		t.Errorf("draining = %+v", f)
	}
	if f := failures[2]; f.Timeout != "deletion.node_volume_detach_timeout 5m0s" {
		t.Errorf("detaching = %+v", f)
	}
	if f := failures[3]; f.Timeout != "deletion.node_deletion_timeout default" ||
		!strings.Contains(f.String(), "[deletion.node_deletion_timeout default]") {
		t.Errorf("node-gone = %+v", f)
	}
	desc := describeMachineFailures(failures)
	if !strings.HasPrefix(desc, "machines not ready:\n  booting") ||
		strings.Count(desc, "\n") != 4 {
		t.Errorf("description = %q", desc)
	}
	if describeMachineFailures(nil) != "" {
		t.Error("nothing to describe")
	}
}
