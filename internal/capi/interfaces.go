// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import "context"

// Bootstrapper manages the lifecycle of a bootstrap Kubernetes cluster.
// Modeled after EKS Anywhere's Bootstrapper interface.
type Bootstrapper interface {
	// Create creates a new bootstrap cluster and returns its connection info.
	Create(ctx context.Context, opts BootstrapOptions) (*Cluster, error)

	// Delete tears down a bootstrap cluster.
	Delete(ctx context.Context, cluster *Cluster) error

	// Exists checks whether a bootstrap cluster with the given name exists.
	Exists(ctx context.Context, name string) (bool, error)
}

// Installer installs and manages CAPI components on a cluster.
// Modeled after EKS Anywhere's CAPIClient.InitInfrastructure.
type Installer interface {
	// Init initializes CAPI providers on a cluster (clusterctl init).
	Init(ctx context.Context, cluster *Cluster, opts InitOptions) error
}

// TemplateGenerator generates cluster templates.
type TemplateGenerator interface {
	// Generate generates a cluster template YAML manifest.
	Generate(ctx context.Context, cluster *Cluster, opts TemplateOptions) ([]byte, error)
}

// Applier applies and deletes Kubernetes manifests on a cluster.
type Applier interface {
	// Apply applies a YAML manifest to the cluster.
	Apply(ctx context.Context, cluster *Cluster, manifest []byte) error

	// Delete deletes a CAPI cluster by name and namespace.
	Delete(ctx context.Context, cluster *Cluster, clusterName, namespace string) error
}

// Mover moves CAPI management resources between clusters.
// Modeled after EKS Anywhere's ClusterManager.MoveCAPI.
type Mover interface {
	// Move moves CAPI resources from one cluster to another.
	Move(ctx context.Context, from, to *Cluster, opts MoveOptions) error
}

// Waiter waits for cluster readiness conditions.
// Modeled after EKS Anywhere's wait patterns in ClusterManager.
type Waiter interface {
	// WaitForControlPlane waits for the cluster control plane to become ready.
	WaitForControlPlane(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error

	// WaitForWorkers waits for worker nodes to become ready.
	WaitForWorkers(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error

	// WaitForClusterReady waits for the full cluster to be ready (control plane + workers).
	WaitForClusterReady(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
}

// KubeconfigRetriever retrieves kubeconfigs for workload clusters.
type KubeconfigRetriever interface {
	// GetKubeconfig retrieves the kubeconfig for a workload cluster.
	GetKubeconfig(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error)
}

// ClusterDescriber describes the status of a CAPI cluster.
type ClusterDescriber interface {
	// Describe returns a human-readable description of the cluster's status.
	Describe(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error)
}
