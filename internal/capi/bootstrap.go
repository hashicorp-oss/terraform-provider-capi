// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	kindcluster "sigs.k8s.io/kind/pkg/cluster"
)

// KindBootstrapper creates bootstrap clusters using kind (Kubernetes in Docker).
// Uses the kind Go library directly instead of shelling out to the kind CLI.
// This mirrors EKS Anywhere's bootstrap cluster approach.
type KindBootstrapper struct {
	// nodeImage overrides the default kind node image (e.g. "kindest/node:v1.31.0").
	nodeImage string
}

// NewKindBootstrapper creates a new KindBootstrapper.
func NewKindBootstrapper() *KindBootstrapper {
	return &KindBootstrapper{}
}

// NewKindBootstrapperWithNodeImage creates a KindBootstrapper with a specific node image.
func NewKindBootstrapperWithNodeImage(nodeImage string) *KindBootstrapper {
	return &KindBootstrapper{
		nodeImage: nodeImage,
	}
}

// Create creates a new kind cluster for use as a CAPI bootstrap cluster.
func (b *KindBootstrapper) Create(ctx context.Context, opts BootstrapOptions) (*Cluster, error) {
	provider := kindcluster.NewProvider()

	name := opts.Name
	if name == "" {
		name = "capi-bootstrap"
	}

	// Check if cluster already exists
	exists, err := b.Exists(ctx, name)
	if err != nil {
		return nil, &BootstrapError{ClusterName: name, Operation: "check-exists", Err: err}
	}
	if exists {
		// Return existing cluster info
		return b.clusterFromName(name)
	}

	// Build kind create options
	var createOpts []kindcluster.CreateOption

	// Set node image from explicit override or kubernetes version
	nodeImage := b.nodeImage
	if opts.KubernetesVersion != "" {
		nodeImage = fmt.Sprintf("kindest/node:%s", opts.KubernetesVersion)
	}
	if nodeImage != "" {
		createOpts = append(createOpts, kindcluster.CreateWithNodeImage(nodeImage))
	}

	// If extra port mappings are needed, generate a kind config
	if len(opts.ExtraPortMappings) > 0 {
		config, err := b.generateKindConfig(name, opts)
		if err != nil {
			return nil, &BootstrapError{ClusterName: name, Operation: "generate-config", Err: err}
		}
		createOpts = append(createOpts, kindcluster.CreateWithRawConfig([]byte(config)))
	}

	// Wait for ready
	createOpts = append(createOpts, kindcluster.CreateWithWaitForReady(5*time.Minute))

	if err := provider.Create(name, createOpts...); err != nil {
		return nil, &BootstrapError{
			ClusterName: name,
			Operation:   "create",
			Err:         fmt.Errorf("%w: %v", ErrBootstrapClusterCreate, err),
		}
	}

	return b.clusterFromName(name)
}

// Delete deletes a kind bootstrap cluster.
func (b *KindBootstrapper) Delete(ctx context.Context, cluster *Cluster) error {
	provider := kindcluster.NewProvider()

	if err := provider.Delete(cluster.Name, ""); err != nil {
		return &BootstrapError{
			ClusterName: cluster.Name,
			Operation:   "delete",
			Err:         fmt.Errorf("%w: %v", ErrBootstrapClusterDelete, err),
		}
	}

	return nil
}

// Exists checks if a kind cluster with the given name exists.
func (b *KindBootstrapper) Exists(ctx context.Context, name string) (bool, error) {
	provider := kindcluster.NewProvider()

	clusters, err := provider.List()
	if err != nil {
		return false, fmt.Errorf("listing kind clusters: %w", err)
	}

	for _, c := range clusters {
		if c == name {
			return true, nil
		}
	}

	return false, nil
}

// clusterFromName creates a Cluster from a kind cluster name.
func (b *KindBootstrapper) clusterFromName(name string) (*Cluster, error) {
	provider := kindcluster.NewProvider()

	kubeconfigPath := filepath.Join(os.TempDir(), fmt.Sprintf("kind-%s-kubeconfig", name))

	// Get kubeconfig content using the kind Go library
	kubeconfig, err := provider.KubeConfig(name, false)
	if err != nil {
		return nil, &BootstrapError{
			ClusterName: name,
			Operation:   "get-kubeconfig",
			Err:         fmt.Errorf("getting kubeconfig: %w", err),
		}
	}

	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0600); err != nil {
		return nil, &BootstrapError{
			ClusterName: name,
			Operation:   "write-kubeconfig",
			Err:         err,
		}
	}

	return &Cluster{
		Name:           name,
		KubeconfigPath: kubeconfigPath,
	}, nil
}

// generateKindConfig generates a kind cluster configuration with port mappings.
func (b *KindBootstrapper) generateKindConfig(name string, opts BootstrapOptions) (string, error) {
	var buf bytes.Buffer
	buf.WriteString("kind: Cluster\n")
	buf.WriteString("apiVersion: kind.x-k8s.io/v1alpha4\n")
	buf.WriteString("nodes:\n")
	buf.WriteString("- role: control-plane\n")

	if len(opts.ExtraPortMappings) > 0 {
		buf.WriteString("  extraPortMappings:\n")
		for _, pm := range opts.ExtraPortMappings {
			protocol := pm.Protocol
			if protocol == "" {
				protocol = "TCP"
			}
			buf.WriteString(fmt.Sprintf("  - containerPort: %d\n", pm.ContainerPort))
			buf.WriteString(fmt.Sprintf("    hostPort: %d\n", pm.HostPort))
			buf.WriteString(fmt.Sprintf("    protocol: %s\n", protocol))
		}
	}

	return buf.String(), nil
}
