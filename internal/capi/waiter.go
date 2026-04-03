// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// KubectlWaiter waits for cluster readiness using kubectl.
// This mirrors EKS Anywhere's ClusterManager wait patterns.
type KubectlWaiter struct {
	kubectlBinary string
}

// NewKubectlWaiter creates a new KubectlWaiter.
func NewKubectlWaiter() *KubectlWaiter {
	return &KubectlWaiter{kubectlBinary: "kubectl"}
}

// WaitForControlPlane waits for the cluster's control plane to become ready.
// Equivalent to EKS Anywhere's WaitForControlPlaneReady.
func (w *KubectlWaiter) WaitForControlPlane(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	if namespace == "" {
		namespace = "default"
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}

	// Wait for KubeadmControlPlane to be ready
	err := w.waitForCondition(ctx, mgmtCluster, "kubeadmcontrolplane",
		fmt.Sprintf("%s-control-plane", clusterName), namespace,
		"Available", timeout)
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
func (w *KubectlWaiter) WaitForWorkers(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
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

	// Poll for machine deployments to have the correct number of ready replicas
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ready, err := w.areMachineDeploymentsReady(ctx, mgmtCluster, clusterName, namespace)
		if err == nil && ready {
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
func (w *KubectlWaiter) WaitForClusterReady(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	if namespace == "" {
		namespace = "default"
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}

	// First wait for the Cluster resource to have the Ready condition
	err := w.waitForCondition(ctx, mgmtCluster, "cluster.cluster.x-k8s.io",
		clusterName, namespace, "Ready", timeout)
	if err != nil {
		return &CAPIError{
			Operation: "wait-cluster-ready",
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: waiting for cluster Ready condition: %v", ErrClusterNotReady, err),
		}
	}

	return nil
}

// waitForCondition uses kubectl wait to wait for a condition on a resource.
func (w *KubectlWaiter) waitForCondition(ctx context.Context, cluster *Cluster, resourceType, resourceName, namespace, condition string, timeout time.Duration) error {
	args := []string{
		"wait", fmt.Sprintf("%s/%s", resourceType, resourceName),
		"--kubeconfig", cluster.KubeconfigPath,
		"--namespace", namespace,
		"--for", fmt.Sprintf("condition=%s", condition),
		"--timeout", fmt.Sprintf("%ds", int(timeout.Seconds())),
	}

	cmd := exec.CommandContext(ctx, w.kubectlBinary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("waiting for %s/%s condition %s: %s", resourceType, resourceName, condition, stderr.String())
	}

	return nil
}

// areMachineDeploymentsReady checks if all machine deployments for the cluster have desired replicas ready.
func (w *KubectlWaiter) areMachineDeploymentsReady(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (bool, error) {
	// Get machine deployments for this cluster
	args := []string{
		"get", "machinedeployment",
		"--kubeconfig", mgmtCluster.KubeconfigPath,
		"--namespace", namespace,
		"-l", fmt.Sprintf("cluster.x-k8s.io/cluster-name=%s", clusterName),
		"-o", "jsonpath={range .items[*]}{.status.readyReplicas}/{.spec.replicas} {end}",
	}

	cmd := exec.CommandContext(ctx, w.kubectlBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("getting machine deployments: %s", stderr.String())
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return false, fmt.Errorf("no machine deployments found")
	}

	for _, pair := range strings.Fields(output) {
		parts := strings.SplitN(pair, "/", 2)
		if len(parts) != 2 || parts[0] != parts[1] || parts[0] == "" || parts[0] == "0" {
			return false, nil
		}
	}

	return true, nil
}
