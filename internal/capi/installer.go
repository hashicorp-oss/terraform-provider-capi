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

// ClusterctlInstaller installs CAPI components using the clusterctl client library.
// This mirrors EKS Anywhere's CAPIClient.InitInfrastructure.
type ClusterctlInstaller struct {
	configPath string
}

// NewClusterctlInstaller creates a new installer with the given clusterctl config path.
func NewClusterctlInstaller(configPath string) *ClusterctlInstaller {
	if configPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			configPath = filepath.Join(home, ".cluster-api")
		}
	}
	return &ClusterctlInstaller{configPath: configPath}
}

// Init initializes CAPI providers on a cluster using clusterctl init.
// This corresponds to EKS Anywhere's installCAPIComponentsTask.
func (i *ClusterctlInstaller) Init(ctx context.Context, cluster *Cluster, opts InitOptions) error {
	client, err := clusterctlclient.New(ctx, i.configPath)
	if err != nil {
		return &CAPIError{
			Operation: "init",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: creating clusterctl client: %v", ErrCAPIInit, err),
		}
	}

	initOpts := clusterctlclient.InitOptions{
		Kubeconfig: clusterctlclient.Kubeconfig{
			Path: cluster.KubeconfigPath,
		},
	}

	if opts.CoreProvider != "" {
		initOpts.CoreProvider = opts.CoreProvider
	}

	if len(opts.BootstrapProviders) > 0 {
		initOpts.BootstrapProviders = opts.BootstrapProviders
	}

	if len(opts.ControlPlaneProviders) > 0 {
		initOpts.ControlPlaneProviders = opts.ControlPlaneProviders
	}

	if len(opts.InfrastructureProviders) > 0 {
		initOpts.InfrastructureProviders = opts.InfrastructureProviders
	}

	_, err = client.Init(ctx, initOpts)
	if err != nil {
		return &CAPIError{
			Operation: "init",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %v", ErrCAPIInit, err),
		}
	}

	return nil
}
