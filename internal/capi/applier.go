// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// KubectlApplier applies and deletes Kubernetes manifests using kubectl.
type KubectlApplier struct {
	kubectlBinary string
}

// NewKubectlApplier creates a new KubectlApplier.
func NewKubectlApplier() *KubectlApplier {
	return &KubectlApplier{kubectlBinary: "kubectl"}
}

// NewKubectlApplierWithBinary creates a KubectlApplier with a specific kubectl binary path.
func NewKubectlApplierWithBinary(kubectlBinary string) *KubectlApplier {
	return &KubectlApplier{kubectlBinary: kubectlBinary}
}

// Apply applies a YAML manifest to the cluster.
func (a *KubectlApplier) Apply(ctx context.Context, cluster *Cluster, manifest []byte) error {
	if err := a.checkKubectlAvailable(); err != nil {
		return err
	}

	args := []string{"apply", "--kubeconfig", cluster.KubeconfigPath, "-f", "-"}
	cmd := exec.CommandContext(ctx, a.kubectlBinary, args...)
	cmd.Stdin = bytes.NewReader(manifest)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return &CAPIError{
			Operation: "apply-manifest",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %s", ErrManifestApply, stderr.String()),
		}
	}

	return nil
}

// Delete deletes a CAPI cluster by name and namespace from the management cluster.
func (a *KubectlApplier) Delete(ctx context.Context, cluster *Cluster, clusterName, namespace string) error {
	if err := a.checkKubectlAvailable(); err != nil {
		return err
	}

	if namespace == "" {
		namespace = "default"
	}

	// Delete the CAPI Cluster resource - CAPI controllers will cascade delete all owned resources.
	args := []string{
		"delete", "cluster.cluster.x-k8s.io", clusterName,
		"--kubeconfig", cluster.KubeconfigPath,
		"--namespace", namespace,
		"--timeout", "10m",
	}

	cmd := exec.CommandContext(ctx, a.kubectlBinary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return &CAPIError{
			Operation: "delete-cluster",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %s", ErrManifestDelete, stderr.String()),
		}
	}

	return nil
}

func (a *KubectlApplier) checkKubectlAvailable() error {
	_, err := exec.LookPath(a.kubectlBinary)
	if err != nil {
		return fmt.Errorf("%w: kubectl binary not found at %q", ErrCommandNotFound, a.kubectlBinary)
	}
	return nil
}
