// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"testing"
)

func TestKindBootstrapperConstructors(t *testing.T) {
	b := NewKindBootstrapper()
	if b.nodeImage != "" {
		t.Errorf("expected empty nodeImage, got %q", b.nodeImage)
	}

	b2 := NewKindBootstrapperWithNodeImage("kindest/node:v1.31.0")
	if b2.nodeImage != "kindest/node:v1.31.0" {
		t.Errorf("expected nodeImage 'kindest/node:v1.31.0', got %q", b2.nodeImage)
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
	b := NewKindBootstrapper()
	exists, err := b.Exists(context.Background(), "nonexistent-cluster-12345")
	if err != nil {
		// Docker may not be available - skip gracefully
		t.Skip("Docker may not be available:", err)
	}
	if exists {
		t.Error("expected cluster to not exist")
	}
}
