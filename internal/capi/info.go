// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
)

// ClusterctlInfoRetriever retrieves cluster information using the clusterctl client library.
type ClusterctlInfoRetriever struct {
	configPath string
}

// NewClusterctlInfoRetriever creates a new info retriever.
func NewClusterctlInfoRetriever(configPath string) *ClusterctlInfoRetriever {
	if configPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			configPath = filepath.Join(home, ".cluster-api")
		}
	}
	return &ClusterctlInfoRetriever{configPath: configPath}
}

// GetKubeconfig retrieves the kubeconfig for a workload cluster.
func (r *ClusterctlInfoRetriever) GetKubeconfig(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error) {
	client, err := clusterctlclient.New(ctx, r.configPath)
	if err != nil {
		return "", &CAPIError{
			Operation: "get-kubeconfig",
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: creating clusterctl client: %v", ErrKubeconfig, err),
		}
	}

	opts := clusterctlclient.GetKubeconfigOptions{
		Kubeconfig: clusterctlclient.Kubeconfig{
			Path: mgmtCluster.KubeconfigPath,
		},
		Namespace:           namespace,
		WorkloadClusterName: clusterName,
	}

	kubeconfig, err := client.GetKubeconfig(ctx, opts)
	if err != nil {
		return "", &CAPIError{
			Operation: "get-kubeconfig",
			Cluster:   clusterName,
			Err:       fmt.Errorf("%w: %v", ErrKubeconfig, err),
		}
	}

	return kubeconfig, nil
}

// Describe returns a human-readable description of the cluster's status.
func (r *ClusterctlInfoRetriever) Describe(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error) {
	client, err := clusterctlclient.New(ctx, r.configPath)
	if err != nil {
		return "", &CAPIError{
			Operation: "describe",
			Cluster:   clusterName,
			Err:       fmt.Errorf("creating clusterctl client: %v", err),
		}
	}

	opts := clusterctlclient.DescribeClusterOptions{
		Kubeconfig: clusterctlclient.Kubeconfig{
			Path: mgmtCluster.KubeconfigPath,
		},
		Namespace:           namespace,
		ClusterName:         clusterName,
		ShowOtherConditions: "all",
		Grouping:            true,
	}

	tree, err := client.DescribeCluster(ctx, opts)
	if err != nil {
		return "", &CAPIError{
			Operation: "describe",
			Cluster:   clusterName,
			Err:       fmt.Errorf("describing cluster: %v", err),
		}
	}

	if tree == nil || tree.GetRoot() == nil {
		return "", &CAPIError{
			Operation: "describe",
			Cluster:   clusterName,
			Err:       fmt.Errorf("cluster description is empty"),
		}
	}

	root := tree.GetRoot()
	kind := "Cluster"
	if objKind := root.GetObjectKind(); objKind != nil {
		gvk := objKind.GroupVersionKind()
		if gvk.Kind != "" {
			kind = gvk.Kind
		}
	}

	return fmt.Sprintf("NAME: %s\nNAMESPACE: %s\nKIND: %s",
		root.GetName(), root.GetNamespace(), kind), nil
}

// WriteKubeconfig retrieves the workload cluster kubeconfig and writes it to disk.
func (r *ClusterctlInfoRetriever) WriteKubeconfig(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace, outputPath string) error {
	kubeconfig, err := r.GetKubeconfig(ctx, mgmtCluster, clusterName, namespace)
	if err != nil {
		return err
	}

	// Ensure the directory exists
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating kubeconfig directory: %w", err)
	}

	if err := os.WriteFile(outputPath, []byte(kubeconfig), 0600); err != nil {
		return fmt.Errorf("writing kubeconfig to %s: %w", outputPath, err)
	}

	return nil
}
