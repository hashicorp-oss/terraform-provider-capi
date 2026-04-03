// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"time"
)

// Cluster represents a Kubernetes cluster with connection information.
type Cluster struct {
	// Name is the cluster name.
	Name string

	// KubeconfigPath is the path to the kubeconfig file for this cluster.
	KubeconfigPath string

	// Namespace is the namespace where CAPI resources are managed.
	Namespace string
}

// BootstrapOptions configures bootstrap cluster creation.
type BootstrapOptions struct {
	// Name is the name for the bootstrap cluster. Auto-generated if empty.
	Name string

	// KubernetesVersion is the Kubernetes version for the bootstrap cluster.
	KubernetesVersion string

	// ExtraPortMappings adds port mappings to the bootstrap cluster nodes.
	ExtraPortMappings []PortMapping
}

// PortMapping represents a port mapping for bootstrap cluster nodes.
type PortMapping struct {
	ContainerPort int32
	HostPort      int32
	Protocol      string
}

// InitOptions configures CAPI provider installation.
type InitOptions struct {
	// Kubeconfig is the path to the cluster's kubeconfig.
	Kubeconfig string

	// CoreProvider is the core provider version (e.g., "cluster-api:v1.7.0").
	CoreProvider string

	// BootstrapProviders is the list of bootstrap providers to install.
	BootstrapProviders []string

	// ControlPlaneProviders is the list of control plane providers to install.
	ControlPlaneProviders []string

	// InfrastructureProviders is the list of infrastructure providers to install.
	InfrastructureProviders []string
}

// TemplateOptions configures cluster template generation.
type TemplateOptions struct {
	// Kubeconfig is the management cluster kubeconfig path.
	Kubeconfig string

	// ClusterName is the name of the cluster to generate.
	ClusterName string

	// Namespace is the target namespace for the cluster.
	Namespace string

	// KubernetesVersion is the Kubernetes version for the workload cluster.
	KubernetesVersion string

	// InfrastructureProvider is the infrastructure provider to use.
	InfrastructureProvider string

	// Flavor is the template flavor.
	Flavor string

	// ControlPlaneMachineCount is the number of control plane machines.
	ControlPlaneMachineCount *int64

	// WorkerMachineCount is the number of worker machines.
	WorkerMachineCount *int64
}

// MoveOptions configures CAPI management move operations.
type MoveOptions struct {
	// FromKubeconfig is the source cluster kubeconfig.
	FromKubeconfig string

	// ToKubeconfig is the target cluster kubeconfig.
	ToKubeconfig string

	// Namespace is the namespace to move resources from.
	Namespace string
}

// WaitOptions configures cluster readiness waiting.
type WaitOptions struct {
	// Timeout is the maximum time to wait for readiness.
	Timeout time.Duration

	// PollInterval is how frequently to check readiness.
	PollInterval time.Duration
}

// DefaultWaitOptions returns sensible default wait options.
func DefaultWaitOptions() WaitOptions {
	return WaitOptions{
		Timeout:      30 * time.Minute,
		PollInterval: 15 * time.Second,
	}
}

// ClusterResult contains the result of a cluster creation operation.
type ClusterResult struct {
	// Cluster is the workload cluster information.
	Cluster *Cluster

	// BootstrapCluster is the bootstrap cluster (if still running).
	BootstrapCluster *Cluster

	// Kubeconfig is the workload cluster kubeconfig content.
	Kubeconfig string

	// Endpoint is the API server endpoint.
	Endpoint string

	// CACertificate is the cluster CA certificate.
	CACertificate string

	// ClusterDescription is a human-readable cluster status description.
	ClusterDescription string
}

// CreateClusterOptions configures the full cluster creation workflow.
type CreateClusterOptions struct {
	// Name is the cluster name.
	Name string

	// Namespace is the target namespace.
	Namespace string

	// InfrastructureProvider is the infrastructure provider (e.g., "docker").
	InfrastructureProvider string

	// BootstrapProvider is the bootstrap provider (e.g., "kubeadm").
	BootstrapProvider string

	// ControlPlaneProvider is the control plane provider (e.g., "kubeadm").
	ControlPlaneProvider string

	// CoreProvider is the core provider version.
	CoreProvider string

	// KubernetesVersion is the Kubernetes version.
	KubernetesVersion string

	// ControlPlaneMachineCount is the number of control plane nodes.
	ControlPlaneMachineCount *int64

	// WorkerMachineCount is the number of worker nodes.
	WorkerMachineCount *int64

	// Flavor is the template flavor.
	Flavor string

	// ManagementKubeconfig is an existing management cluster kubeconfig.
	// If empty, a bootstrap cluster will be created.
	ManagementKubeconfig string

	// SkipInit skips running clusterctl init.
	SkipInit bool

	// WaitForReady waits for the cluster to become ready.
	WaitForReady bool

	// SelfManaged moves CAPI management to the workload cluster.
	SelfManaged bool

	// Wait configures timeout and poll options.
	Wait WaitOptions

	// KubeconfigOutputPath is where to write the workload cluster kubeconfig.
	KubeconfigOutputPath string
}

// DeleteClusterOptions configures cluster deletion.
type DeleteClusterOptions struct {
	// Name is the cluster name.
	Name string

	// Namespace is the cluster namespace.
	Namespace string

	// ManagementKubeconfig is the management cluster kubeconfig.
	ManagementKubeconfig string

	// DeleteBootstrap indicates whether to delete the bootstrap cluster too.
	DeleteBootstrap bool

	// BootstrapName is the name of the bootstrap cluster to delete.
	BootstrapName string
}
