// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestNewDynamicWaiter(t *testing.T) {
	w := NewDynamicWaiter()
	if w == nil {
		t.Fatal("expected non-nil waiter")
	}
}

func TestHasCondition_True(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"status": map[string]any{
				"conditions": []any{
					map[string]any{
						"type":   "Ready",
						"status": "True",
					},
					map[string]any{
						"type":   "Available",
						"status": "False",
					},
				},
			},
		},
	}

	if !hasCondition(obj, "Ready", "True") {
		t.Error("expected Ready=True to match")
	}
	if hasCondition(obj, "Available", "True") {
		t.Error("expected Available=True to not match (status is False)")
	}
	if !hasCondition(obj, "Available", "False") {
		t.Error("expected Available=False to match")
	}
}

func TestHasCondition_NotFound(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"status": map[string]any{
				"conditions": []any{
					map[string]any{
						"type":   "Ready",
						"status": "True",
					},
				},
			},
		},
	}

	if hasCondition(obj, "NonExistent", "True") {
		t.Error("expected NonExistent condition to not match")
	}
}

func TestHasCondition_NoConditions(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{},
	}
	if hasCondition(obj, "Ready", "True") {
		t.Error("expected no conditions to match on empty object")
	}
}

func TestHasCondition_EmptyStatus(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"status": map[string]any{},
		},
	}
	if hasCondition(obj, "Ready", "True") {
		t.Error("expected no conditions to match on empty status")
	}
}

func TestHasCondition_EmptyConditionsList(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"status": map[string]any{
				"conditions": []any{},
			},
		},
	}
	if hasCondition(obj, "Ready", "True") {
		t.Error("expected no conditions to match on empty conditions list")
	}
}

// fakeWaiter returns a DynamicWaiter whose client serves the given objects
// plus a core Cluster CRD whose storage version is coreVersion.
func fakeWaiter(
	t *testing.T,
	coreVersion string,
	objs ...*unstructured.Unstructured,
) *DynamicWaiter {
	t.Helper()
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "clusters.cluster.x-k8s.io"},
		"spec": map[string]any{
			"group": "cluster.x-k8s.io",
			"versions": []any{
				map[string]any{
					"name":    "v1beta1",
					"served":  true,
					"storage": coreVersion == "v1beta1",
				},
				map[string]any{
					"name":    "v1beta2",
					"served":  true,
					"storage": coreVersion == "v1beta2",
				},
			},
		},
	}}
	all := append([]runtime.Object{crd}, toRuntimeObjects(objs)...)
	listKinds := map[schema.GroupVersionResource]string{}
	for _, v := range []string{"v1beta1", "v1beta2"} {
		listKinds[schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: v, Resource: "machinedeployments"}] = "MachineDeploymentList"
		listKinds[schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: v, Resource: "clusters"}] = "ClusterList"
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		listKinds,
		all...)
	return &DynamicWaiter{clientFor: func(string) (dynamic.Interface, error) { return client, nil }}
}

func toRuntimeObjects(objs []*unstructured.Unstructured) []runtime.Object {
	out := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		out = append(out, o)
	}
	return out
}

func clusterObject(
	version, name, namespace string,
	conditions ...string,
) *unstructured.Unstructured {
	var conds []any
	for _, c := range conditions {
		conds = append(conds, map[string]any{"type": c, "status": "True"})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/" + version,
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"status":     map[string]any{"conditions": conds},
	}}
}

func machineDeploymentObject(
	version, name, namespace, clusterName string,
	replicas, ready int64,
) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/" + version,
		"kind":       "MachineDeployment",
		"metadata": map[string]any{
			"name": name, "namespace": namespace,
			"labels": map[string]any{"cluster.x-k8s.io/cluster-name": clusterName},
		},
		"spec":   map[string]any{"replicas": replicas},
		"status": map[string]any{"readyReplicas": ready},
	}}
}

var fastWait = WaitOptions{Timeout: 200 * time.Millisecond, PollInterval: 5 * time.Millisecond}

func TestDynamicWaiter_WaitForControlPlane_UsesClusterInitializedCondition(t *testing.T) {
	w := fakeWaiter(
		t,
		"v1beta2",
		clusterObject("v1beta2", "acc", "capi", "ControlPlaneInitialized"),
	)
	if err := w.WaitForControlPlane(
		context.Background(),
		&Cluster{},
		"acc",
		"capi",
		fastWait,
	); err != nil {
		t.Fatalf("control plane should be ready from the Cluster condition alone: %v", err)
	}
}

func TestDynamicWaiter_WaitForControlPlane_TimesOutWithoutCondition(t *testing.T) {
	w := fakeWaiter(t, "v1beta2", clusterObject("v1beta2", "acc", "capi"))
	err := w.WaitForControlPlane(context.Background(), &Cluster{}, "acc", "capi", fastWait)
	if !errors.Is(err, ErrClusterNotReady) {
		t.Fatalf("expected ErrClusterNotReady, got %v", err)
	}
}

func TestDynamicWaiter_WaitForControlPlane_UsesDiscoveredCoreVersion(t *testing.T) {
	// The Cluster only exists at v1beta1; a hardcoded v1beta2 read would miss it.
	w := fakeWaiter(
		t,
		"v1beta1",
		clusterObject("v1beta1", "old", "default", "ControlPlaneInitialized"),
	)
	if err := w.WaitForControlPlane(
		context.Background(),
		&Cluster{},
		"old",
		"default",
		fastWait,
	); err != nil {
		t.Fatalf("expected the v1beta1 cluster to be found: %v", err)
	}
}

func TestDynamicWaiter_WaitForClusterReady_AcceptsAvailableOrReady(t *testing.T) {
	v2 := fakeWaiter(t, "v1beta2", clusterObject("v1beta2", "acc", "capi", "Available"))
	if err := v2.WaitForClusterReady(
		context.Background(),
		&Cluster{},
		"acc",
		"capi",
		fastWait,
	); err != nil {
		t.Errorf("v1beta2 Available=True should satisfy readiness: %v", err)
	}
	v1 := fakeWaiter(t, "v1beta1", clusterObject("v1beta1", "old", "default", "Ready"))
	if err := v1.WaitForClusterReady(
		context.Background(),
		&Cluster{},
		"old",
		"default",
		fastWait,
	); err != nil {
		t.Errorf("v1beta1 Ready=True should satisfy readiness: %v", err)
	}
	notYet := fakeWaiter(
		t,
		"v1beta2",
		clusterObject("v1beta2", "acc", "capi", "ControlPlaneInitialized"),
	)
	if err := notYet.WaitForClusterReady(
		context.Background(),
		&Cluster{},
		"acc",
		"capi",
		fastWait,
	); !errors.Is(
		err,
		ErrClusterNotReady,
	) {
		t.Errorf("an initialized but unavailable cluster must time out, got %v", err)
	}
}

func TestDynamicWaiter_WaitForWorkers_UsesDiscoveredCoreVersion(t *testing.T) {
	w := fakeWaiter(
		t,
		"v1beta1",
		machineDeploymentObject("v1beta1", "old-md-0", "default", "old", 2, 2),
	)
	if err := w.WaitForWorkers(
		context.Background(),
		&Cluster{},
		"old",
		"default",
		fastWait,
	); err != nil {
		t.Fatalf("expected the v1beta1 machine deployment to count as ready: %v", err)
	}
}
