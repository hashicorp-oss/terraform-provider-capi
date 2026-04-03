// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// KindBootstrapper creates bootstrap clusters using kind (Kubernetes in Docker).
// This mirrors EKS Anywhere's bootstrap cluster approach.
type KindBootstrapper struct {
	// kindBinary is the path to the kind binary. Defaults to "kind".
	kindBinary string
}

// NewKindBootstrapper creates a new KindBootstrapper.
func NewKindBootstrapper() *KindBootstrapper {
	return &KindBootstrapper{
		kindBinary: "kind",
	}
}

// NewKindBootstrapperWithBinary creates a KindBootstrapper with a specific kind binary path.
func NewKindBootstrapperWithBinary(kindBinary string) *KindBootstrapper {
	return &KindBootstrapper{
		kindBinary: kindBinary,
	}
}

// Create creates a new kind cluster for use as a CAPI bootstrap cluster.
func (b *KindBootstrapper) Create(ctx context.Context, opts BootstrapOptions) (*Cluster, error) {
	if err := b.checkKindAvailable(ctx); err != nil {
		return nil, err
	}

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

	// Build kind create command
	args := []string{"create", "cluster", "--name", name}

	if opts.KubernetesVersion != "" {
		args = append(args, "--image", fmt.Sprintf("kindest/node:%s", opts.KubernetesVersion))
	}

	// If extra port mappings are needed, generate a kind config
	if len(opts.ExtraPortMappings) > 0 {
		config, err := b.generateKindConfig(name, opts)
		if err != nil {
			return nil, &BootstrapError{ClusterName: name, Operation: "generate-config", Err: err}
		}

		tmpFile, err := os.CreateTemp("", "kind-config-*.yaml")
		if err != nil {
			return nil, &BootstrapError{ClusterName: name, Operation: "write-config", Err: err}
		}
		defer os.Remove(tmpFile.Name())

		if _, err := tmpFile.Write([]byte(config)); err != nil {
			tmpFile.Close()
			return nil, &BootstrapError{ClusterName: name, Operation: "write-config", Err: err}
		}
		tmpFile.Close()

		args = append(args, "--config", tmpFile.Name())
	}

	// Wait for ready
	args = append(args, "--wait", "5m")

	cmd := exec.CommandContext(ctx, b.kindBinary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, &BootstrapError{
			ClusterName: name,
			Operation:   "create",
			Err:         fmt.Errorf("%w: %s", ErrBootstrapClusterCreate, stderr.String()),
		}
	}

	return b.clusterFromName(name)
}

// Delete deletes a kind bootstrap cluster.
func (b *KindBootstrapper) Delete(ctx context.Context, cluster *Cluster) error {
	if err := b.checkKindAvailable(ctx); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, b.kindBinary, "delete", "cluster", "--name", cluster.Name)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return &BootstrapError{
			ClusterName: cluster.Name,
			Operation:   "delete",
			Err:         fmt.Errorf("%w: %s", ErrBootstrapClusterDelete, stderr.String()),
		}
	}

	return nil
}

// Exists checks if a kind cluster with the given name exists.
func (b *KindBootstrapper) Exists(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, b.kindBinary, "get", "clusters")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("listing kind clusters: %s", stderr.String())
	}

	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}

	return false, nil
}

// checkKindAvailable verifies that the kind binary is available.
func (b *KindBootstrapper) checkKindAvailable(ctx context.Context) error {
	_, err := exec.LookPath(b.kindBinary)
	if err != nil {
		return fmt.Errorf("%w: kind binary not found at %q - install from https://kind.sigs.k8s.io/", ErrCommandNotFound, b.kindBinary)
	}
	return nil
}

// clusterFromName creates a Cluster from a kind cluster name.
func (b *KindBootstrapper) clusterFromName(name string) (*Cluster, error) {
	kubeconfigPath := filepath.Join(os.TempDir(), fmt.Sprintf("kind-%s-kubeconfig", name))

	// Export kubeconfig
	cmd := exec.Command(b.kindBinary, "get", "kubeconfig", "--name", name)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, &BootstrapError{
			ClusterName: name,
			Operation:   "get-kubeconfig",
			Err:         fmt.Errorf("getting kubeconfig: %s", stderr.String()),
		}
	}

	if err := os.WriteFile(kubeconfigPath, stdout.Bytes(), 0600); err != nil {
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
