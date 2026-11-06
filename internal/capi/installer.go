// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"strings"
	"time"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

// upgradeWaitTimeout bounds how long clusterctl upgrade apply waits for each
// upgraded provider to come back.
const upgradeWaitTimeout = 10 * time.Minute

// ClusterctlInstaller runs the clusterctl provider lifecycle (init, upgrade
// apply, delete) through the clusterctl client library. It always injects an
// isolated config client (so provider URL overrides never touch global viper
// state) and repository factories that apply capi-operator-style
// customizations to component YAML, for init and upgrade alike.
type ClusterctlInstaller struct {
	configPath string
	// inventory lists the providers installed on a cluster; replaced in tests.
	inventory func(ctx context.Context, configClient config.Client, kubeconfigPath string) (*clusterctlv1.ProviderList, error)
}

// NewClusterctlInstaller creates a new installer with the given clusterctl
// config path. An empty configPath lets clusterctl fall back to its own
// default config file resolution.
func NewClusterctlInstaller(configPath string) *ClusterctlInstaller {
	return &ClusterctlInstaller{configPath: configPath, inventory: installedProviders}
}

// installedProviders reads a cluster's clusterctl provider inventory. The
// inventory CRD is installed first, as clusterctl init does, so a fresh
// cluster reads as empty instead of failing.
func installedProviders(
	ctx context.Context,
	configClient config.Client,
	kubeconfigPath string,
) (*clusterctlv1.ProviderList, error) {
	c := cluster.New(cluster.Kubeconfig{Path: kubeconfigPath}, configClient)
	if err := c.ProviderInventory().EnsureCustomResourceDefinitions(ctx); err != nil {
		return nil, fmt.Errorf("ensuring the clusterctl inventory CRD: %w", err)
	}
	return c.ProviderInventory().List(ctx)
}

// Init converges the cluster on opts.Providers (clusterctl init). clusterctl
// itself skips a provider already installed at the requested version but
// refuses one installed at another version, so those are upgraded first;
// everything missing is then installed. A provider without a pinned version
// is left as installed. This corresponds to EKS Anywhere's
// installCAPIComponentsTask.
func (i *ClusterctlInstaller) Init(ctx context.Context, cluster *Cluster, opts InitOptions) error {
	fail := func(err error) error {
		return &CAPIError{
			Operation: "init",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %v", ErrCAPIInit, err),
		}
	}
	client, configClient, err := newClusterctlClient(
		ctx,
		i.configPath,
		opts.Providers,
		opts.Variables,
	)
	if err != nil {
		return fail(err)
	}
	installed, err := i.inventory(ctx, configClient, cluster.KubeconfigPath)
	if err != nil {
		return fail(fmt.Errorf("listing installed providers: %v", err))
	}

	if mismatched := versionMismatches(opts.Providers, installed); len(mismatched) > 0 {
		refs, _ := upgradeRefs(opts.Providers, mismatched, installed)
		if err := applyUpgrade(ctx, client, cluster, refs); err != nil {
			return fail(fmt.Errorf("upgrading providers installed at another version: %v", err))
		}
	}

	initOpts := initOptionsFor(opts)
	initOpts.Kubeconfig = clusterctlclient.Kubeconfig{Path: cluster.KubeconfigPath}
	if _, err := client.Init(ctx, initOpts); err != nil {
		return fail(err)
	}
	return nil
}

// initOptionsFor builds the clusterctl init options: the configured
// providers, and with NoDefaults the "-" provider for the bootstrap and
// control-plane types left unset, which stops clusterctl from adding kubeadm.
func initOptionsFor(opts InitOptions) clusterctlclient.InitOptions {
	initOpts := buildInitOptions(opts.Providers)
	if opts.NoDefaults {
		if len(initOpts.BootstrapProviders) == 0 {
			initOpts.BootstrapProviders = []string{clusterctlclient.NoopProvider}
		}
		if len(initOpts.ControlPlaneProviders) == 0 {
			initOpts.ControlPlaneProviders = []string{clusterctlclient.NoopProvider}
		}
	}
	return initOpts
}

// InstalledProvider is one entry of a cluster's clusterctl provider inventory.
type InstalledProvider struct {
	Name      string
	Type      ProviderType
	Version   string
	Namespace string
}

// Installed reads a cluster's clusterctl provider inventory.
func (i *ClusterctlInstaller) Installed(
	ctx context.Context,
	cluster *Cluster,
) ([]InstalledProvider, error) {
	_, configClient, err := newClusterctlClient(ctx, i.configPath, ProviderSet{}, nil)
	if err != nil {
		return nil, err
	}
	list, err := i.inventory(ctx, configClient, cluster.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("listing installed providers: %w", err)
	}
	out := make([]InstalledProvider, 0, len(list.Items))
	for _, p := range list.Items {
		out = append(
			out,
			InstalledProvider{
				Name:      p.ProviderName,
				Type:      providerTypeFromClusterctl(p.GetProviderType()),
				Version:   p.Version,
				Namespace: p.Namespace,
			},
		)
	}
	return out, nil
}

// providerTypeFromClusterctl maps clusterctl's provider type names to ours.
func providerTypeFromClusterctl(t clusterctlv1.ProviderType) ProviderType {
	switch t {
	case clusterctlv1.CoreProviderType:
		return ProviderTypeCore
	case clusterctlv1.BootstrapProviderType:
		return ProviderTypeBootstrap
	case clusterctlv1.ControlPlaneProviderType:
		return ProviderTypeControlPlane
	case clusterctlv1.InfrastructureProviderType:
		return ProviderTypeInfrastructure
	case clusterctlv1.IPAMProviderType:
		return ProviderTypeIPAM
	case clusterctlv1.AddonProviderType:
		return ProviderTypeAddon
	}
	return ProviderType(strings.ToLower(string(t)))
}

// Upgrade upgrades or re-renders opts.Targets in place (clusterctl upgrade
// apply). A target that is not installed is installed instead.
func (i *ClusterctlInstaller) Upgrade(
	ctx context.Context,
	cluster *Cluster,
	opts UpgradeOptions,
) error {
	fail := func(err error) error {
		return &CAPIError{
			Operation: "upgrade",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %v", ErrCAPIUpgrade, err),
		}
	}
	if len(opts.Targets) == 0 {
		return nil
	}
	client, configClient, err := newClusterctlClient(
		ctx,
		i.configPath,
		opts.Providers,
		opts.Variables,
	)
	if err != nil {
		return fail(err)
	}
	installed, err := i.inventory(ctx, configClient, cluster.KubeconfigPath)
	if err != nil {
		return fail(fmt.Errorf("listing installed providers: %v", err))
	}

	refs, missing := upgradeRefs(opts.Providers, opts.Targets, installed)
	if len(refs) > 0 {
		if err := applyUpgrade(ctx, client, cluster, refs); err != nil {
			return fail(err)
		}
	}
	if len(missing) > 0 {
		initOpts := buildInitOptions(opts.Providers.Subset(missing))
		initOpts.Kubeconfig = clusterctlclient.Kubeconfig{Path: cluster.KubeconfigPath}
		if _, err := client.Init(ctx, initOpts); err != nil {
			return fail(fmt.Errorf("installing %v: %v", missing, err))
		}
	}
	return nil
}

// Uninstall deletes providers from the cluster (clusterctl delete). CRDs and
// namespaces are kept, so the objects of other clusters survive.
func (i *ClusterctlInstaller) Uninstall(
	ctx context.Context,
	cluster *Cluster,
	opts UninstallOptions,
) error {
	fail := func(err error) error {
		return &CAPIError{
			Operation: "delete-providers",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %v", ErrCAPIUninstall, err),
		}
	}
	if !opts.All && opts.Providers.Empty() {
		return nil
	}
	client, _, err := newClusterctlClient(ctx, i.configPath, opts.Providers, nil)
	if err != nil {
		return fail(err)
	}
	del := clusterctlclient.DeleteOptions{
		Kubeconfig: clusterctlclient.Kubeconfig{Path: cluster.KubeconfigPath},
		DeleteAll:  opts.All,
	}
	if !opts.All {
		names := func(t ProviderType) []string {
			var out []string
			for _, p := range opts.Providers[t] {
				out = append(out, p.Name)
			}
			return out
		}
		if core := names(ProviderTypeCore); len(core) > 0 {
			del.CoreProvider = core[0]
		}
		del.BootstrapProviders = names(ProviderTypeBootstrap)
		del.ControlPlaneProviders = names(ProviderTypeControlPlane)
		del.InfrastructureProviders = names(ProviderTypeInfrastructure)
		del.IPAMProviders = names(ProviderTypeIPAM)
		del.AddonProviders = names(ProviderTypeAddon)
	}
	if err := client.Delete(ctx, del); err != nil {
		return fail(err)
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

// applyUpgrade runs clusterctl upgrade apply for the given upgrade items,
// grouped by type, waiting for each upgraded provider to come back.
func applyUpgrade(
	ctx context.Context,
	client clusterctlclient.Client,
	cluster *Cluster,
	refs map[ProviderType][]string,
) error {
	up := clusterctlclient.ApplyUpgradeOptions{
		Kubeconfig:              clusterctlclient.Kubeconfig{Path: cluster.KubeconfigPath},
		BootstrapProviders:      refs[ProviderTypeBootstrap],
		ControlPlaneProviders:   refs[ProviderTypeControlPlane],
		InfrastructureProviders: refs[ProviderTypeInfrastructure],
		IPAMProviders:           refs[ProviderTypeIPAM],
		AddonProviders:          refs[ProviderTypeAddon],
		WaitProviders:           true,
		WaitProviderTimeout:     upgradeWaitTimeout,
	}
	if core := refs[ProviderTypeCore]; len(core) > 0 {
		up.CoreProvider = core[0]
	}
	return client.ApplyUpgrade(ctx, up)
}

// installedProvider finds the inventory entry for a configured provider.
func installedProvider(
	p ProviderConfig,
	installed *clusterctlv1.ProviderList,
) (clusterctlv1.Provider, bool) {
	if installed == nil {
		return clusterctlv1.Provider{}, false
	}
	matches := installed.FilterByProviderNameAndType(p.Name, p.Type.Clusterctl())
	if len(matches) == 0 {
		return clusterctlv1.Provider{}, false
	}
	return matches[0], true
}

// versionMismatches returns the configured providers pinned to a version
// other than the installed one. Unpinned providers never mismatch.
func versionMismatches(providers ProviderSet, installed *clusterctlv1.ProviderList) []ProviderKey {
	var out []ProviderKey
	for _, p := range providers.All() {
		if p.Version == "" {
			continue
		}
		inst, ok := installedProvider(p, installed)
		if ok && !sameVersion(inst.Version, p.Version) {
			out = append(out, ProviderKey{Type: p.Type, Name: p.Name})
		}
	}
	return out
}

// sameVersion compares release tags, tolerating a missing "v" prefix.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(
		strings.TrimSpace(a),
		"v",
	) == strings.TrimPrefix(
		strings.TrimSpace(b),
		"v",
	)
}

// upgradeRefs renders the clusterctl upgrade items ("namespace/name:version")
// for the target providers, grouped by type. A target pinned to a version
// upgrades to it; an unpinned target is reinstalled at its installed version,
// which re-renders its components with the current customizations. Targets
// that are not installed are returned separately.
func upgradeRefs(
	providers ProviderSet,
	targets []ProviderKey,
	installed *clusterctlv1.ProviderList,
) (map[ProviderType][]string, []ProviderKey) {
	refs := map[ProviderType][]string{}
	var missing []ProviderKey
	for _, key := range targets {
		p, ok := providers.Find(key)
		if !ok {
			continue
		}
		inst, ok := installedProvider(p, installed)
		if !ok {
			missing = append(missing, key)
			continue
		}
		version := p.Version
		if version == "" {
			version = inst.Version
		}
		refs[p.Type] = append(
			refs[p.Type],
			fmt.Sprintf("%s/%s:%s", inst.Namespace, p.Name, version),
		)
	}
	return refs, missing
}
