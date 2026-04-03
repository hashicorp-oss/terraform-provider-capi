// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package docker

import (
	"testing"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

func TestDefaultCreateOptions(t *testing.T) {
	opts := DefaultCreateOptions("my-cluster")

	if opts.Name != "my-cluster" {
		t.Errorf("expected name 'my-cluster', got %q", opts.Name)
	}
	if opts.InfrastructureProvider != InfrastructureProviderName {
		t.Errorf("expected provider %q, got %q", InfrastructureProviderName, opts.InfrastructureProvider)
	}
	if opts.BootstrapProvider != BootstrapProviderName {
		t.Errorf("expected bootstrap %q, got %q", BootstrapProviderName, opts.BootstrapProvider)
	}
	if opts.ControlPlaneProvider != ControlPlaneProviderName {
		t.Errorf("expected control plane %q, got %q", ControlPlaneProviderName, opts.ControlPlaneProvider)
	}
	if opts.KubernetesVersion != DefaultKubernetesVersion {
		t.Errorf("expected k8s version %q, got %q", DefaultKubernetesVersion, opts.KubernetesVersion)
	}
	if *opts.ControlPlaneMachineCount != DefaultControlPlaneCount {
		t.Errorf("expected cp count %d, got %d", DefaultControlPlaneCount, *opts.ControlPlaneMachineCount)
	}
	if *opts.WorkerMachineCount != DefaultWorkerCount {
		t.Errorf("expected worker count %d, got %d", DefaultWorkerCount, *opts.WorkerMachineCount)
	}
	if opts.Namespace != "default" {
		t.Errorf("expected namespace 'default', got %q", opts.Namespace)
	}
	if !opts.WaitForReady {
		t.Error("expected WaitForReady to be true")
	}
	if opts.SelfManaged {
		t.Error("expected SelfManaged to be false")
	}

	// Verify wait options are set
	defaultWait := capi.DefaultWaitOptions()
	if opts.Wait.Timeout != defaultWait.Timeout {
		t.Errorf("expected timeout %v, got %v", defaultWait.Timeout, opts.Wait.Timeout)
	}
}

func TestConstants(t *testing.T) {
	if InfrastructureProviderName != "docker" {
		t.Errorf("expected 'docker', got %q", InfrastructureProviderName)
	}
	if BootstrapProviderName != "kubeadm" {
		t.Errorf("expected 'kubeadm', got %q", BootstrapProviderName)
	}
	if ControlPlaneProviderName != "kubeadm" {
		t.Errorf("expected 'kubeadm', got %q", ControlPlaneProviderName)
	}
}
