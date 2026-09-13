// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"

	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

// newClusterctlClient builds a clusterctl client that knows every provider in
// providers: URL overrides (pinned versions, fetch_config, and names clusterctl
// only knows in its "<name>-<name>" form) are registered through an isolated
// config reader, and component customizations are applied through the
// injected repository factory. variables become clusterctl variables, ahead
// of the config file and environment. Every clusterctl operation that
// resolves a provider by name (init, template generation) must go through
// this so the same names work everywhere.
func newClusterctlClient(ctx context.Context, configPath string, providers ProviderSet, variables map[string]string) (clusterctlclient.Client, error) {
	configClient, _, err := newProviderConfigClient(ctx, configPath, providers)
	if err != nil {
		return nil, err
	}
	for k, v := range variables {
		configClient.Variables().Set(k, v)
	}
	return clusterctlclient.New(ctx, configPath,
		clusterctlclient.InjectConfig(configClient),
		clusterctlclient.InjectRepositoryFactory(NewCustomizingRepoFactory(configClient, providers.Customized())),
	)
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
