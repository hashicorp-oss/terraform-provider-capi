// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"testing"
	"time"
)

func TestDefaultWaitOptions(t *testing.T) {
	opts := DefaultWaitOptions()
	if opts.Timeout != 30*time.Minute {
		t.Errorf("expected 30m timeout, got %v", opts.Timeout)
	}
	if opts.PollInterval != 15*time.Second {
		t.Errorf("expected 15s poll interval, got %v", opts.PollInterval)
	}
}

func TestCluster_Fields(t *testing.T) {
	c := &Cluster{
		Name:           "test",
		KubeconfigPath: "/tmp/kubeconfig",
		Namespace:      "default",
	}
	if c.Name != "test" {
		t.Error("name mismatch")
	}
	if c.KubeconfigPath != "/tmp/kubeconfig" {
		t.Error("kubeconfig path mismatch")
	}
	if c.Namespace != "default" {
		t.Error("namespace mismatch")
	}
}

func TestCreateClusterOptions_Fields(t *testing.T) {
	cpCount := int64(3)
	workerCount := int64(5)
	opts := CreateClusterOptions{
		Name:                     "prod",
		Namespace:                "prod-ns",
		InfrastructureProvider:   "docker",
		BootstrapProvider:        "kubeadm",
		ControlPlaneProvider:     "kubeadm",
		CoreProvider:             "cluster-api:v1.7.0",
		KubernetesVersion:        "v1.31.0",
		ControlPlaneMachineCount: &cpCount,
		WorkerMachineCount:       &workerCount,
		Flavor:                   "development",
		ManagementKubeconfig:     "/tmp/kubeconfig",
		SkipInit:                 false,
		WaitForReady:             true,
		SelfManaged:              true,
	}

	if opts.Name != "prod" {
		t.Error("name mismatch")
	}
	if *opts.ControlPlaneMachineCount != 3 {
		t.Errorf("cp count mismatch: got %d", *opts.ControlPlaneMachineCount)
	}
	if *opts.WorkerMachineCount != 5 {
		t.Errorf("worker count mismatch: got %d", *opts.WorkerMachineCount)
	}
}
