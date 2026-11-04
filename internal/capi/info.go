// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
)

// ClusterctlInfoRetriever retrieves cluster information using the clusterctl client library.
type ClusterctlInfoRetriever struct {
	configPath string
}

// NewClusterctlInfoRetriever creates a new info retriever. An empty configPath
// lets clusterctl fall back to its own default config file resolution.
func NewClusterctlInfoRetriever(configPath string) *ClusterctlInfoRetriever {
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

	// The v1beta2 object tree: Available/Ready conditions, machines grouped
	// by state, templates and MachineSets left out, as `clusterctl describe
	// cluster` shows by default.
	opts := clusterctlclient.DescribeClusterOptions{
		Kubeconfig: clusterctlclient.Kubeconfig{
			Path: mgmtCluster.KubeconfigPath,
		},
		Namespace:   namespace,
		ClusterName: clusterName,
		Grouping:    true,
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

	return RenderObjectTree(tree, time.Now()), nil
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
