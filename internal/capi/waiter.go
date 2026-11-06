// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
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
func (w *DynamicWaiter) WaitForControlPlane(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	const op = "wait-control-plane"
	namespace, timeout, pollInterval := waitDefaults(namespace, opts)

	dynClient, coreVersion, err := w.connect(ctx, mgmtCluster)
	if err != nil {
		return &CAPIError{
			Operation: op,
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: %v", ErrClusterNotReady, err),
		}
	}
	err = w.waitForAnyCondition(
		ctx,
		dynClient,
		clustersGVR(coreVersion),
		clusterName,
		namespace,
		[]string{controlPlaneInitializedCondition},
		timeout,
		pollInterval,
	)
	if err != nil {
		return &CAPIError{
			Operation: op,
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: waiting for control plane: %v", ErrClusterNotReady, err),
		}
	}
	return nil
}

// WaitForWorkers waits for worker machine deployments to be ready.
func (w *DynamicWaiter) WaitForWorkers(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	const op = "wait-workers"
	namespace, timeout, pollInterval := waitDefaults(namespace, opts)

	dynClient, coreVersion, err := w.connect(ctx, mgmtCluster)
	if err != nil {
		return &CAPIError{
			Operation: op,
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: %v", ErrClusterNotReady, err),
		}
	}
	gvr := schema.GroupVersionResource{
		Group:    "cluster.x-k8s.io",
		Version:  coreVersion,
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
		Operation: op,
		Cluster:   clusterName,
		Err: fmt.Errorf(
			"%w: worker nodes did not become ready within %v",
			ErrClusterNotReady,
			timeout,
		),
	}
}

// WaitForClusterReady waits for the Cluster to report Available (v1beta2) or
// Ready (v1beta1), which covers the control plane and the workers.
func (w *DynamicWaiter) WaitForClusterReady(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	const op = "wait-cluster-ready"
	namespace, timeout, pollInterval := waitDefaults(namespace, opts)

	dynClient, coreVersion, err := w.connect(ctx, mgmtCluster)
	if err != nil {
		return &CAPIError{
			Operation: op,
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: %v", ErrClusterNotReady, err),
		}
	}
	if len(opts.Conditions) > 0 {
		err = w.waitForAllConditions(
			ctx,
			dynClient,
			clustersGVR(coreVersion),
			clusterName,
			namespace,
			readinessConditions(opts.Conditions, coreVersion),
			timeout,
			pollInterval,
		)
	} else {
		err = w.waitForAnyCondition(
			ctx,
			dynClient,
			clustersGVR(coreVersion),
			clusterName,
			namespace,
			clusterReadyConditions,
			timeout,
			pollInterval,
		)
	}
	if err != nil {
		return &CAPIError{
			Operation: op,
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: waiting for cluster readiness: %v", ErrClusterNotReady, err),
		}
	}
	return nil
}

// readinessConditions maps the caller's condition names onto the contract
// in use: the v1beta2 Available is the legacy Ready, and the other way round.
func readinessConditions(conditions []string, coreVersion string) []string {
	legacy := version.CompareKubeAwareVersionStrings(coreVersion, "v1beta2") < 0
	out := make([]string, 0, len(conditions))
	for _, c := range conditions {
		switch {
		case legacy && c == "Available":
			c = "Ready"
		case !legacy && c == "Ready":
			c = "Available"
		}
		out = append(out, c)
	}
	return out
}

// WaitForClusterDeleted waits for a deleted Cluster object to disappear. CAPI
// removes it only once every owned object is gone, so this is when the
// machines and their infrastructure have been released.
func (w *DynamicWaiter) WaitForClusterDeleted(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	const op = "wait-cluster-deleted"
	namespace, timeout, pollInterval := waitDefaults(namespace, opts)

	dynClient, coreVersion, err := w.connect(ctx, mgmtCluster)
	if err != nil {
		return &CAPIError{
			Operation: op,
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: %v", ErrClusterNotDeleted, err),
		}
	}
	clusters := dynClient.Resource(clustersGVR(coreVersion)).Namespace(namespace)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, getErr := clusters.Get(ctx, clusterName, meta_v1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return &CAPIError{
		Operation: op,
		Cluster:   clusterName,
		Err: fmt.Errorf(
			"%w: %s/%s still exists after %v",
			ErrClusterNotDeleted,
			namespace,
			clusterName,
			timeout,
		),
	}
}

// CAAPH resources. HelmChartProxy.status.matchingClusters names the clusters
// a proxy selected; each gets a HelmReleaseProxy owned by the proxy whose
// spec.clusterRef names the cluster and whose Ready condition reports the
// Helm release.
var (
	helmChartProxiesGVR = schema.GroupVersionResource{
		Group:    "addons.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "helmchartproxies",
	}
	helmReleaseProxiesGVR = schema.GroupVersionResource{
		Group:    "addons.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "helmreleaseproxies",
	}
)

// WaitForAddons waits until every HelmChartProxy that matches the cluster
// has a Ready HelmReleaseProxy for it. Without the CAAPH CRDs there is
// nothing to wait for.
func (w *DynamicWaiter) WaitForAddons(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	const op = "wait-addons"
	namespace, timeout, pollInterval := waitDefaults(namespace, opts)
	dynClient, err := w.clientFor(mgmtCluster.KubeconfigPath)
	if err != nil {
		return &CAPIError{
			Operation: op,
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: %v", ErrClusterNotReady, err),
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		pending, err := pendingAddons(ctx, dynClient, clusterName, namespace)
		if err != nil {
			return &CAPIError{Operation: op, Cluster: clusterName, Err: err}
		}
		if len(pending) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return &CAPIError{
				Operation: op,
				Cluster:   clusterName,
				Err: fmt.Errorf(
					"%w: addons not ready after %v: %s",
					ErrClusterNotReady,
					timeout,
					strings.Join(pending, "; "),
				),
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// pendingAddons lists the matching HelmChartProxies without a Ready release
// for the cluster, each with the reason it is pending.
func pendingAddons(
	ctx context.Context,
	dynClient dynamic.Interface,
	clusterName, namespace string,
) ([]string, error) {
	proxies, err := dynClient.Resource(helmChartProxiesGVR).
		Namespace(namespace).
		List(ctx, meta_v1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing HelmChartProxies in %s: %w", namespace, err)
	}
	releases, err := dynClient.Resource(helmReleaseProxiesGVR).
		Namespace(namespace).
		List(ctx, meta_v1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("listing HelmReleaseProxies in %s: %w", namespace, err)
	}
	var pending []string
	for _, proxy := range proxies.Items {
		if !proxyMatchesCluster(&proxy, clusterName, namespace) {
			continue
		}
		state := "no HelmReleaseProxy yet"
		for i := range releases.Items {
			rel := &releases.Items[i]
			if !releaseBelongsTo(rel, proxy.GetName(), clusterName, namespace) {
				continue
			}
			if hasCondition(rel, "Ready", "True") {
				state = ""
				break
			}
			state = releaseState(rel)
		}
		if state != "" {
			pending = append(pending, fmt.Sprintf("%s: %s", proxy.GetName(), state))
		}
	}
	return pending, nil
}

func proxyMatchesCluster(proxy *unstructured.Unstructured, clusterName, namespace string) bool {
	matching, _, _ := unstructured.NestedSlice(proxy.Object, "status", "matchingClusters")
	for _, m := range matching {
		ref, ok := m.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(ref, "name")
		ns, _, _ := unstructured.NestedString(ref, "namespace")
		if name == clusterName && (ns == "" || ns == namespace) {
			return true
		}
	}
	return false
}

func releaseBelongsTo(
	rel *unstructured.Unstructured,
	proxyName, clusterName, namespace string,
) bool {
	name, _, _ := unstructured.NestedString(rel.Object, "spec", "clusterRef", "name")
	ns, _, _ := unstructured.NestedString(rel.Object, "spec", "clusterRef", "namespace")
	if name != clusterName || (ns != "" && ns != namespace) {
		return false
	}
	for _, owner := range rel.GetOwnerReferences() {
		if owner.Kind == "HelmChartProxy" && owner.Name == proxyName {
			return true
		}
	}
	return false
}

// releaseState summarizes a HelmReleaseProxy that is not Ready: its Helm
// status and the Ready condition's message, which carries Helm's error.
func releaseState(rel *unstructured.Unstructured) string {
	status, _, _ := unstructured.NestedString(rel.Object, "status", "status")
	if status == "" {
		status = "pending"
	}
	conditions, _, _ := unstructured.NestedSlice(rel.Object, "status", "conditions")
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if typ, _, _ := unstructured.NestedString(cond, "type"); typ != "Ready" {
			continue
		}
		if msg, _, _ := unstructured.NestedString(cond, "message"); msg != "" {
			return status + ": " + msg
		}
	}
	return status
}

// connect builds the dynamic client and discovers the core API version.
func (w *DynamicWaiter) connect(
	ctx context.Context,
	mgmtCluster *Cluster,
) (dynamic.Interface, string, error) {
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
	crdGVR := schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
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
	return schema.GroupVersionResource{
		Group:    "cluster.x-k8s.io",
		Version:  version,
		Resource: "clusters",
	}
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
func (w *DynamicWaiter) waitForAnyCondition(
	ctx context.Context,
	dynClient dynamic.Interface,
	gvr schema.GroupVersionResource,
	resourceName, namespace string,
	conditionTypes []string,
	timeout, pollInterval time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obj, getErr := dynClient.Resource(gvr).
			Namespace(namespace).
			Get(ctx, resourceName, meta_v1.GetOptions{})
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

	return fmt.Errorf(
		"timed out waiting for %s/%s condition %v",
		gvr.Resource,
		resourceName,
		conditionTypes,
	)
}

// waitForAllConditions polls until every named condition is True on the
// resource; a timeout names the ones still missing.
func (w *DynamicWaiter) waitForAllConditions(
	ctx context.Context,
	dynClient dynamic.Interface,
	gvr schema.GroupVersionResource,
	resourceName, namespace string,
	conditionTypes []string,
	timeout, pollInterval time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var missing []string
	for time.Now().Before(deadline) {
		missing = missing[:0]
		obj, getErr := dynClient.Resource(gvr).
			Namespace(namespace).
			Get(ctx, resourceName, meta_v1.GetOptions{})
		if getErr == nil {
			for _, conditionType := range conditionTypes {
				if !hasCondition(obj, conditionType, "True") {
					missing = append(missing, conditionType)
				}
			}
			if len(missing) == 0 {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	return fmt.Errorf(
		"timed out waiting for %s/%s conditions %v (not True: %v)",
		gvr.Resource,
		resourceName,
		conditionTypes,
		missing,
	)
}

// areMachineDeploymentsReady checks if all machine deployments for the cluster have desired replicas ready.
func (w *DynamicWaiter) areMachineDeploymentsReady(
	ctx context.Context,
	dynClient dynamic.Interface,
	gvr schema.GroupVersionResource,
	clusterName, namespace string,
) (bool, error) {
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
		condition, ok := c.(map[string]any)
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
