// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"

	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
)

// newClusterctlClient builds a clusterctl client that knows every provider in
// providers: URL overrides (pinned versions, fetch_config, and names clusterctl
// only knows in its "<name>-<name>" form) are registered through an isolated
// config reader, and component customizations are applied through the
// injected repository factory. variables become clusterctl variables, ahead
// of the config file and environment. Every clusterctl operation that
// resolves a provider by name (init, upgrade, delete) must go through this so
// the same names work everywhere. The config client is returned for callers
// that need to read the cluster's provider inventory.
func newClusterctlClient(ctx context.Context, configPath string, providers ProviderSet, variables map[string]string) (clusterctlclient.Client, config.Client, error) {
	configClient, _, err := newProviderConfigClient(ctx, configPath, providers)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range variables {
		configClient.Variables().Set(k, v)
	}
	repoFactory := NewCustomizingRepoFactory(configClient, providers.Customized())
	c, err := clusterctlclient.New(ctx, configPath,
		clusterctlclient.InjectConfig(configClient),
		clusterctlclient.InjectRepositoryFactory(repoFactory),
		clusterctlclient.InjectClusterClientFactory(customizingClusterFactory(configClient, repoFactory)),
	)
	if err != nil {
		return nil, nil, err
	}
	return c, configClient, nil
}

// customizingClusterFactory mirrors clusterctl's default cluster client
// factory but routes the cluster client's repository access through the
// customizing factory too. clusterctl only propagates the injected repository
// factory to init; upgrade apply fetches components through the cluster
// client, and without this an upgrade would drop every customization init
// applied.
func customizingClusterFactory(configClient config.Client, repoFactory clusterctlclient.RepositoryClientFactory) clusterctlclient.ClusterClientFactory {
	return func(input clusterctlclient.ClusterClientFactoryInput) (cluster.Client, error) {
		return cluster.New(
			cluster.Kubeconfig(input.Kubeconfig),
			configClient,
			cluster.InjectYamlProcessor(input.Processor),
			cluster.InjectRepositoryFactory(func(ctx context.Context, provider config.Provider, _ config.Client, _ ...repository.Option) (repository.Client, error) {
				return repoFactory(ctx, clusterctlclient.RepositoryClientFactoryInput{
					Provider:  clusterctlclient.Provider(provider),
					Processor: input.Processor,
				})
			}),
		), nil
	}
}

// newProviderConfigClient creates the overlay-backed clusterctl config client
// and registers a URL override for every provider that needs one. The reader
// is returned for tests.
func newProviderConfigClient(ctx context.Context, configPath string, providers ProviderSet) (config.Client, *overlayReader, error) {
	configClient, reader, err := newOverlayConfigClient(ctx, configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("creating clusterctl config client: %w", err)
	}

	for _, p := range providers.All() {
		def, exact := lookupDefaultProvider(configClient, p)
		// A bare entry clusterctl already knows by this exact name needs no
		// override. Everything else (pinned version, fetch config, or a name
		// clusterctl only knows in its "<name>-<name>" form) is registered.
		if exact && !p.NeedsURLOverride() {
			continue
		}
		defaultURL := ""
		if def != nil {
			defaultURL = def.URL()
		}
		u, err := ResolveFetchURL(p, defaultURL)
		if err != nil {
			return nil, nil, err
		}
		reader.AddOverride(p.Name, p.Type.Clusterctl(), u)
	}
	return configClient, reader, nil
}
