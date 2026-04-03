// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"time"

	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// DynamicWaiter waits for cluster readiness using the client-go dynamic client.
// Uses direct API polling instead of shelling out to kubectl wait.
// This mirrors EKS Anywhere's ClusterManager wait patterns.
type DynamicWaiter struct{}

// NewDynamicWaiter creates a new DynamicWaiter.
func NewDynamicWaiter() *DynamicWaiter {
	return &DynamicWaiter{}
}

// WaitForControlPlane waits for the cluster's control plane to become ready.
// Equivalent to EKS Anywhere's WaitForControlPlaneReady.
func (w *DynamicWaiter) WaitForControlPlane(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	if namespace == "" {
		namespace = "default"
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}

	pollInterval := opts.PollInterval
	if pollInterval == 0 {
		pollInterval = 15 * time.Second
	}

	dynClient, err := buildDynamicClient(mgmtCluster.KubeconfigPath)
	if err != nil {
		return &CAPIError{
			Operation: "wait-control-plane",
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: %v", ErrClusterNotReady, err),
		}
	}

	gvr := schema.GroupVersionResource{
		Group:    "controlplane.cluster.x-k8s.io",
		Version:  "v1beta1",
		Resource: "kubeadmcontrolplanes",
	}

	// Wait for KubeadmControlPlane to be ready
	cpName := fmt.Sprintf("%s-control-plane", clusterName)
	err = w.waitForCondition(ctx, dynClient, gvr, cpName, namespace, "Available", timeout, pollInterval)
	if err != nil {
		return &CAPIError{
			Operation: "wait-control-plane",
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: waiting for control plane: %v", ErrClusterNotReady, err),
		}
	}

	return nil
}

// WaitForWorkers waits for worker machine deployments to be ready.
func (w *DynamicWaiter) WaitForWorkers(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	if namespace == "" {
		namespace = "default"
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}

	pollInterval := opts.PollInterval
	if pollInterval == 0 {
		pollInterval = 15 * time.Second
	}

	dynClient, err := buildDynamicClient(mgmtCluster.KubeconfigPath)
	if err != nil {
		return &CAPIError{
			Operation: "wait-workers",
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: %v", ErrClusterNotReady, err),
		}
	}

	gvr := schema.GroupVersionResource{
		Group:    "cluster.x-k8s.io",
		Version:  "v1beta1",
		Resource: "machinedeployments",
	}

	// Poll for machine deployments to have the correct number of ready replicas
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ready, checkErr := w.areMachineDeploymentsReady(ctx, dynClient, gvr, clusterName, namespace)
		if checkErr == nil && ready {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	return &CAPIError{
		Operation: "wait-workers",
		Cluster:   clusterName,
		Err:       fmt.Errorf("%w: worker nodes did not become ready within %v", ErrClusterNotReady, timeout),
	}
}

// WaitForClusterReady waits for the full cluster (control plane + workers) to be ready.
// This mirrors EKS Anywhere's combined wait approach.
func (w *DynamicWaiter) WaitForClusterReady(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	if namespace == "" {
		namespace = "default"
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}

	pollInterval := opts.PollInterval
	if pollInterval == 0 {
		pollInterval = 15 * time.Second
	}

	dynClient, err := buildDynamicClient(mgmtCluster.KubeconfigPath)
	if err != nil {
		return &CAPIError{
			Operation: "wait-cluster-ready",
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: %v", ErrClusterNotReady, err),
		}
	}

	gvr := schema.GroupVersionResource{
		Group:    "cluster.x-k8s.io",
		Version:  "v1beta1",
		Resource: "clusters",
	}

	// First wait for the Cluster resource to have the Ready condition
	err = w.waitForCondition(ctx, dynClient, gvr, clusterName, namespace, "Ready", timeout, pollInterval)
	if err != nil {
		return &CAPIError{
			Operation: "wait-cluster-ready",
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: waiting for cluster Ready condition: %v", ErrClusterNotReady, err),
		}
	}

	return nil
}

// waitForCondition polls a Kubernetes resource until the specified condition is True.
func (w *DynamicWaiter) waitForCondition(ctx context.Context, dynClient dynamic.Interface, gvr schema.GroupVersionResource, resourceName, namespace, conditionType string, timeout, pollInterval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obj, getErr := dynClient.Resource(gvr).Namespace(namespace).Get(ctx, resourceName, meta_v1.GetOptions{})
		if getErr == nil && hasCondition(obj, conditionType, "True") {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	return fmt.Errorf("timed out waiting for %s/%s condition %s", gvr.Resource, resourceName, conditionType)
}

// areMachineDeploymentsReady checks if all machine deployments for the cluster have desired replicas ready.
func (w *DynamicWaiter) areMachineDeploymentsReady(ctx context.Context, dynClient dynamic.Interface, gvr schema.GroupVersionResource, clusterName, namespace string) (bool, error) {
	list, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx, meta_v1.ListOptions{
		LabelSelector: fmt.Sprintf("cluster.x-k8s.io/cluster-name=%s", clusterName),
	})
	if err != nil {
		return false, fmt.Errorf("listing machine deployments: %w", err)
	}

	if len(list.Items) == 0 {
		return false, fmt.Errorf("no machine deployments found")
	}

	for _, item := range list.Items {
		replicas, _, _ := unstructured.NestedInt64(item.Object, "spec", "replicas")
		readyReplicas, _, _ := unstructured.NestedInt64(item.Object, "status", "readyReplicas")

		if replicas == 0 || readyReplicas != replicas {
			return false, nil
		}
	}

	return true, nil
}

// hasCondition checks if an unstructured object has the specified condition with the given status.
func hasCondition(obj *unstructured.Unstructured, conditionType, status string) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}

	for _, c := range conditions {
		condition, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		cType, _, _ := unstructured.NestedString(condition, "type")
		cStatus, _, _ := unstructured.NestedString(condition, "status")
		if cType == conditionType && cStatus == status {
			return true
		}
	}

	return false
}

// buildDynamicClient creates a dynamic Kubernetes client from a kubeconfig path.
func buildDynamicClient(kubeconfigPath string) (dynamic.Interface, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return dynamic.NewForConfig(config)
}
