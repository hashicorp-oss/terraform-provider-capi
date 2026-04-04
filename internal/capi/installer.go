// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

// ClusterctlInstaller installs CAPI components using the clusterctl client library.
// This mirrors EKS Anywhere's CAPIClient.InitInfrastructure.
// When addon providers have customizations, the installer injects a custom
// RepositoryClientFactory that applies capi-operator-style modifications
// (deployment overrides, manager config, JSON patches) to the provider
// component YAML before installation.
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
//
// When InitOptions.Addons contains entries with customizations (deployment
// overrides, manager config, config variables, patches), the installer
// wraps the clusterctl client's repository factory via InjectRepositoryFactory.
// This inserts a custom Processor for variable injection and a
// ComponentsClient wrapper that calls repository.AlterComponents after the
// standard processing pipeline completes.
func (i *ClusterctlInstaller) Init(ctx context.Context, cluster *Cluster, opts InitOptions) error {
	var clientOpts []clusterctlclient.Option

	// Build customization map for addons that need component modifications.
	customizations := CustomizedAddons(opts.Addons)

	if len(customizations) > 0 {
		// Create the config client so we can inject both it and a custom
		// repo factory that shares it. This ensures clusterctl's provider
		// configuration resolution still works normally.
		configClient, err := config.New(ctx, i.configPath)
		if err != nil {
			return &CAPIError{
				Operation: "init",
				Cluster:   cluster.Name,
				Err:       fmt.Errorf("%w: creating config client: %v", ErrCAPIInit, err),
			}
		}

		clientOpts = append(clientOpts,
			clusterctlclient.InjectConfig(configClient),
			clusterctlclient.InjectRepositoryFactory(
				NewCustomizingRepoFactory(configClient, customizations),
			),
		)
	}

	client, err := clusterctlclient.New(ctx, i.configPath, clientOpts...)
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

	if len(opts.AddonProviders) > 0 {
		initOpts.AddonProviders = opts.AddonProviders
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
