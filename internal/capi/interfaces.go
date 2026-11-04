// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

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

// PreTemplateManifester is an optional Bootstrapper capability. After the
// bootstrap cluster is created, the manager calls it with the target CAPI
// cluster name and namespace to obtain manifests to apply to the management
// cluster BEFORE the cluster template. This lets a bootstrapper pre-create
// resources the CAPI providers would otherwise generate — e.g. the Talos
// machine-secrets Secret populated from the bootstrap node's bundle — so the
// providers adopt the bootstrap node's identity and the cluster needs no pivot.
// A bootstrapper that returns no manifests (or does not implement this) leaves
// the providers to generate their own secrets.
type PreTemplateManifester interface {
	PreTemplateManifests(ctx context.Context, clusterName, namespace string) ([][]byte, error)
}

// InPlaceTeardowner is an optional Bootstrapper capability for in-place
// self-managed clusters, where the bootstrap node is a member of the cluster
// it manages. Deleting the Cluster object from inside such a cluster would
// power off the node running the controllers, so the manager hands teardown to
// the bootstrapper instead: it resets and powers off every other machine, then
// its own node.
type InPlaceTeardowner interface {
	TeardownInPlace(ctx context.Context, cluster *Cluster, machines []InventoryMachine) error
}

// AddonInstaller installs an AddonSet on a cluster through the provider's own
// Helm and apply clients (no CAPI addon provider involved).
type AddonInstaller interface {
	Install(ctx context.Context, cluster *Cluster, addons AddonSet) error
}

// Quiescer waits until the management cluster's provisioning engine is idle,
// so a clusterctl move does not strand work that cannot be handed over (a
// Tinkerbell Workflow mid-flight reports to the tink-server that booted it).
type Quiescer interface {
	WaitForQuiescence(ctx context.Context, mgmtCluster *Cluster, namespace string, opts WaitOptions) error
}

// Installer installs and manages CAPI components on a cluster: the
// clusterctl init / upgrade apply / delete lifecycle.
// Modeled after EKS Anywhere's CAPIClient.InitInfrastructure.
type Installer interface {
	// Init converges the cluster on the configured providers (clusterctl
	// init): missing providers are installed, providers installed at a
	// different pinned version are upgraded, the rest are left alone.
	Init(ctx context.Context, cluster *Cluster, opts InitOptions) error

	// Upgrade upgrades or re-renders the target providers in place
	// (clusterctl upgrade apply).
	Upgrade(ctx context.Context, cluster *Cluster, opts UpgradeOptions) error

	// Uninstall deletes providers from the cluster (clusterctl delete).
	Uninstall(ctx context.Context, cluster *Cluster, opts UninstallOptions) error
}

// ManifestGenerator builds the workload cluster's CAPI objects from the
// configured providers, using the CRDs installed on the management cluster.
type ManifestGenerator interface {
	// Generate returns a multi-document YAML manifest for the cluster.
	Generate(ctx context.Context, mgmt *Cluster, opts GenerateOptions) ([]byte, error)
}

// ManifestApplier applies Kubernetes manifests to a cluster.
type ManifestApplier interface {
	// Apply applies a YAML manifest to the cluster.
	Apply(ctx context.Context, cluster *Cluster, manifest []byte) error
}

// Applier applies manifests and manages the core Cluster object on a
// management cluster.
type Applier interface {
	ManifestApplier

	// Delete deletes a CAPI cluster by name and namespace. A cluster that
	// does not exist returns an error wrapping ErrClusterNotFound.
	Delete(ctx context.Context, cluster *Cluster, clusterName, namespace string) error

	// Get reads the core Cluster object by name and namespace. A cluster
	// that does not exist returns an error wrapping ErrClusterNotFound.
	Get(ctx context.Context, cluster *Cluster, clusterName, namespace string) (*unstructured.Unstructured, error)
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

	// WaitForClusterDeleted waits for a deleted Cluster object to disappear,
	// which is when every owned object (machines, infrastructure) is gone.
	WaitForClusterDeleted(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
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
