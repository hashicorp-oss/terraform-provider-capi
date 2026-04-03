// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"os/exec"
	"testing"
)

func TestKindBootstrapper_CheckAvailability(t *testing.T) {
	b := NewKindBootstrapper()
	// Check if kind is available (test may skip if not installed)
	_, err := exec.LookPath("kind")
	if err != nil {
		t.Skip("kind not available in PATH, skipping")
	}

	err = b.checkKindAvailable(context.Background())
	if err != nil {
		t.Errorf("expected kind to be available: %v", err)
	}
}

func TestKindBootstrapper_CheckAvailability_MissingBinary(t *testing.T) {
	b := NewKindBootstrapperWithBinary("/nonexistent/kind")
	err := b.checkKindAvailable(context.Background())
	if err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestKindBootstrapper_GenerateKindConfig(t *testing.T) {
	b := NewKindBootstrapper()

	opts := BootstrapOptions{
		Name: "test",
		ExtraPortMappings: []PortMapping{
			{ContainerPort: 80, HostPort: 8080, Protocol: "TCP"},
			{ContainerPort: 443, HostPort: 8443},
		},
	}

	config, err := b.generateKindConfig("test", opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 80
    hostPort: 8080
    protocol: TCP
  - containerPort: 443
    hostPort: 8443
    protocol: TCP
`
	if config != expected {
		t.Errorf("config mismatch:\nexpected:\n%s\ngot:\n%s", expected, config)
	}
}

func TestKindBootstrapper_GenerateKindConfig_NoPortMappings(t *testing.T) {
	b := NewKindBootstrapper()
	config, err := b.generateKindConfig("test", BootstrapOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
`
	if config != expected {
		t.Errorf("config mismatch:\nexpected:\n%s\ngot:\n%s", expected, config)
	}
}

func TestKindBootstrapper_Exists_ClusterNotFound(t *testing.T) {
	_, err := exec.LookPath("kind")
	if err != nil {
		t.Skip("kind not available in PATH, skipping")
	}

	b := NewKindBootstrapper()
	exists, err := b.Exists(context.Background(), "nonexistent-cluster-12345")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected cluster to not exist")
	}
}
