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
// It watches the core Cluster object's conditions, so it works for any
// control-plane provider, at the API version the management cluster stores.
type DynamicWaiter struct {
	clientFor func(kubeconfigPath string) (dynamic.Interface, error)
}

// NewDynamicWaiter creates a new DynamicWaiter.
func NewDynamicWaiter() *DynamicWaiter {
	return &DynamicWaiter{clientFor: buildDynamicClient}
}

// controlPlaneInitializedCondition is set on the Cluster by both the v1beta1
// and v1beta2 condition sets once the first control plane machine is up.
const controlPlaneInitializedCondition = "ControlPlaneInitialized"

// clusterReadyConditions are the Cluster-level "everything is up" conditions:
// Available (v1beta2) or Ready (v1beta1).
var clusterReadyConditions = []string{"Available", "Ready"}

// WaitForControlPlane waits for the cluster's control plane to be initialized.
func (w *DynamicWaiter) WaitForControlPlane(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	const op = "wait-control-plane"
	namespace, timeout, pollInterval := waitDefaults(namespace, opts)

	dynClient, coreVersion, err := w.connect(ctx, mgmtCluster)
	if err != nil {
		return &CAPIError{Operation: op, Cluster: clusterName, Err: fmt.Errorf("%w: %v", ErrClusterNotReady, err)}
	}
	err = w.waitForAnyCondition(ctx, dynClient, clustersGVR(coreVersion), clusterName, namespace, []string{controlPlaneInitializedCondition}, timeout, pollInterval)
	if err != nil {
		return &CAPIError{Operation: op, Cluster: clusterName, Err: fmt.Errorf("%w: waiting for control plane: %v", ErrClusterNotReady, err)}
	}
	return nil
}

// WaitForWorkers waits for worker machine deployments to be ready.
func (w *DynamicWaiter) WaitForWorkers(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	const op = "wait-workers"
	namespace, timeout, pollInterval := waitDefaults(namespace, opts)

	dynClient, coreVersion, err := w.connect(ctx, mgmtCluster)
	if err != nil {
		return &CAPIError{Operation: op, Cluster: clusterName, Err: fmt.Errorf("%w: %v", ErrClusterNotReady, err)}
	}
	gvr := schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: coreVersion, Resource: "machinedeployments"}

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

	return &CAPIError{Operation: op, Cluster: clusterName, Err: fmt.Errorf("%w: worker nodes did not become ready within %v", ErrClusterNotReady, timeout)}
}

// WaitForClusterReady waits for the Cluster to report Available (v1beta2) or
// Ready (v1beta1), which covers the control plane and the workers.
func (w *DynamicWaiter) WaitForClusterReady(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	const op = "wait-cluster-ready"
	namespace, timeout, pollInterval := waitDefaults(namespace, opts)

	dynClient, coreVersion, err := w.connect(ctx, mgmtCluster)
	if err != nil {
		return &CAPIError{Operation: op, Cluster: clusterName, Err: fmt.Errorf("%w: %v", ErrClusterNotReady, err)}
	}
	err = w.waitForAnyCondition(ctx, dynClient, clustersGVR(coreVersion), clusterName, namespace, clusterReadyConditions, timeout, pollInterval)
	if err != nil {
		return &CAPIError{Operation: op, Cluster: clusterName, Err: fmt.Errorf("%w: waiting for cluster readiness: %v", ErrClusterNotReady, err)}
	}
	return nil
}

// connect builds the dynamic client and discovers the core API version.
func (w *DynamicWaiter) connect(ctx context.Context, mgmtCluster *Cluster) (dynamic.Interface, string, error) {
	dynClient, err := w.clientFor(mgmtCluster.KubeconfigPath)
	if err != nil {
		return nil, "", err
	}
	coreVersion, err := coreClusterVersion(ctx, dynClient)
	if err != nil {
		return nil, "", err
	}
	return dynClient, coreVersion, nil
}

// coreClusterVersion reads the storage version of the core Cluster CRD, the
// version every core object is read at.
func coreClusterVersion(ctx context.Context, dynClient dynamic.Interface) (string, error) {
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	crd, err := dynClient.Resource(crdGVR).Get(ctx, coreClustersCRD, meta_v1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", coreClustersCRD, err)
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if storage, _, _ := unstructured.NestedBool(entry, "storage"); storage {
			name, _, _ := unstructured.NestedString(entry, "name")
			return name, nil
		}
	}
	return "", fmt.Errorf("%s has no storage version", coreClustersCRD)
}

func clustersGVR(version string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: version, Resource: "clusters"}
}

// waitDefaults fills the namespace, timeout and poll interval defaults.
func waitDefaults(namespace string, opts WaitOptions) (string, time.Duration, time.Duration) {
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
	return namespace, timeout, pollInterval
}

// waitForAnyCondition polls a resource until one of the condition types is True.
func (w *DynamicWaiter) waitForAnyCondition(ctx context.Context, dynClient dynamic.Interface, gvr schema.GroupVersionResource, resourceName, namespace string, conditionTypes []string, timeout, pollInterval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obj, getErr := dynClient.Resource(gvr).Namespace(namespace).Get(ctx, resourceName, meta_v1.GetOptions{})
		if getErr == nil {
			for _, conditionType := range conditionTypes {
				if hasCondition(obj, conditionType, "True") {
					return nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	return fmt.Errorf("timed out waiting for %s/%s condition %v", gvr.Resource, resourceName, conditionTypes)
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

// buildDynamicClient creates a dynamic client from a kubeconfig path.
func buildDynamicClient(kubeconfigPath string) (dynamic.Interface, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	return dynamic.NewForConfig(config)
}
