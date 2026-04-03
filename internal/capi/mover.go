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

// ClusterctlMover moves CAPI management resources between clusters using clusterctl.
// This mirrors EKS Anywhere's ClusterManager.MoveCAPI and moveClusterManagementTask.
type ClusterctlMover struct {
	configPath string
}

// NewClusterctlMover creates a new mover.
func NewClusterctlMover(configPath string) *ClusterctlMover {
	if configPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			configPath = filepath.Join(home, ".cluster-api")
		}
	}
	return &ClusterctlMover{configPath: configPath}
}

// Move moves CAPI resources from the source cluster to the target cluster.
// This corresponds to EKS Anywhere's moveClusterManagementTask:
//  1. Pauses reconciliation (if needed)
//  2. Calls clusterctl move from source to target
//  3. Waits for control plane readiness on target (handled by Manager)
func (m *ClusterctlMover) Move(ctx context.Context, from, to *Cluster, opts MoveOptions) error {
	client, err := clusterctlclient.New(ctx, m.configPath)
	if err != nil {
		return &CAPIError{
			Operation: "move",
			Cluster:   from.Name,
			Err:       fmt.Errorf("%w: creating clusterctl client: %v", ErrCAPIMove, err),
		}
	}

	moveOpts := clusterctlclient.MoveOptions{
		FromKubeconfig: clusterctlclient.Kubeconfig{
			Path: from.KubeconfigPath,
		},
		ToKubeconfig: clusterctlclient.Kubeconfig{
			Path: to.KubeconfigPath,
		},
	}

	if opts.Namespace != "" {
		moveOpts.Namespace = opts.Namespace
	}

	err = client.Move(ctx, moveOpts)
	if err != nil {
		return &CAPIError{
			Operation: "move",
			Cluster:   from.Name,
			Err:       fmt.Errorf("%w: moving CAPI management from %q to %q: %v", ErrCAPIMove, from.Name, to.Name, err),
		}
	}

	return nil
}
