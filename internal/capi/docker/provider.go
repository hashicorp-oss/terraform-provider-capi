// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

// Package docker provides Docker-specific CAPI provider configuration.
// The Docker infrastructure provider (CAPD) is used primarily for
// development and testing, allowing CAPI clusters to run entirely
// within Docker containers on a single host.
package docker

import "github.com/tinkerbell-community/terraform-provider-capi/internal/capi"

const (
	// InfrastructureProviderName is the CAPI infrastructure provider name for Docker.
	InfrastructureProviderName = "docker"

	// BootstrapProviderName is the default bootstrap provider used with Docker.
	BootstrapProviderName = "kubeadm"

	// ControlPlaneProviderName is the default control plane provider used with Docker.
	ControlPlaneProviderName = "kubeadm"

	// DefaultKubernetesVersion is the default Kubernetes version for Docker clusters.
	DefaultKubernetesVersion = "v1.31.0"

	// DefaultControlPlaneCount is the default number of control plane nodes.
	DefaultControlPlaneCount int64 = 1

	// DefaultWorkerCount is the default number of worker nodes.
	DefaultWorkerCount int64 = 1
)

// DefaultCreateOptions returns sensible defaults for creating a Docker-based CAPI cluster.
func DefaultCreateOptions(name string) capi.CreateClusterOptions {
	cpCount := DefaultControlPlaneCount
	workerCount := DefaultWorkerCount

	return capi.CreateClusterOptions{
		Name:                     name,
		Namespace:                "default",
		InfrastructureProvider:   InfrastructureProviderName,
		BootstrapProvider:        BootstrapProviderName,
		ControlPlaneProvider:     ControlPlaneProviderName,
		KubernetesVersion:        DefaultKubernetesVersion,
		ControlPlaneMachineCount: &cpCount,
		WorkerMachineCount:       &workerCount,
		WaitForReady:             true,
		SelfManaged:              false,
		Wait:                     capi.DefaultWaitOptions(),
	}
}
