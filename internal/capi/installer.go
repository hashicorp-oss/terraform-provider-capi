// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"

	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
)

// ClusterctlInstaller installs CAPI components using the clusterctl client
// library. It always injects an isolated config client (so provider URL
// overrides never touch global viper state) and a repository factory that
// applies capi-operator-style customizations to component YAML.
type ClusterctlInstaller struct {
	configPath string
}

// NewClusterctlInstaller creates a new installer with the given clusterctl
// config path. An empty configPath lets clusterctl fall back to its own
// default config file resolution.
func NewClusterctlInstaller(configPath string) *ClusterctlInstaller {
	return &ClusterctlInstaller{configPath: configPath}
}

// Init initializes CAPI providers on a cluster using clusterctl init.
// This corresponds to EKS Anywhere's installCAPIComponentsTask.
func (i *ClusterctlInstaller) Init(ctx context.Context, cluster *Cluster, opts InitOptions) error {
	client, err := newClusterctlClient(ctx, i.configPath, opts.Providers, opts.Variables)
	if err != nil {
		return &CAPIError{Operation: "init", Cluster: cluster.Name, Err: fmt.Errorf("%w: %v", ErrCAPIInit, err)}
	}

	initOpts := buildInitOptions(opts.Providers)
	initOpts.Kubeconfig = clusterctlclient.Kubeconfig{Path: cluster.KubeconfigPath}

	if _, err := client.Init(ctx, initOpts); err != nil {
		return &CAPIError{Operation: "init", Cluster: cluster.Name, Err: fmt.Errorf("%w: %v", ErrCAPIInit, err)}
	}
	return nil
}

// buildInitOptions assembles the per-type clusterctl init strings for a ProviderSet.
func buildInitOptions(providers ProviderSet) clusterctlclient.InitOptions {
	initOpts := clusterctlclient.InitOptions{
		// Wait for provider deployments (including webhook services) to become
		// ready before returning. Without this, callers can race ahead and
		// apply manifests that hit not-yet-ready admission webhooks.
		WaitProviders:           true,
		BootstrapProviders:      providers.InitStrings(ProviderTypeBootstrap),
		ControlPlaneProviders:   providers.InitStrings(ProviderTypeControlPlane),
		InfrastructureProviders: providers.InitStrings(ProviderTypeInfrastructure),
		IPAMProviders:           providers.InitStrings(ProviderTypeIPAM),
		AddonProviders:          providers.InitStrings(ProviderTypeAddon),
	}
	if core := providers.InitStrings(ProviderTypeCore); len(core) > 0 {
		initOpts.CoreProvider = core[0]
	}
	return initOpts
}
