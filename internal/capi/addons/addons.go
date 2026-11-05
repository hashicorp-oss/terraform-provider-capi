// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

// Package addons installs and removes addon sets (raw manifests and Helm
// releases) on a cluster through the provider's own Helm and apply clients,
// with no CAPI addon provider involved. It is how a bootstrap node gets its
// stack before CAPI is initialized, and how that stack is taken down again
// before management moves away.
package addons

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/storage/driver"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// Sentinel errors.
var (
	ErrInstall   = errors.New("addon installation failed")
	ErrUninstall = errors.New("addon removal failed")
)

// HelmClient abstracts the Helm SDK for tests.
type HelmClient interface {
	Exists(ctx context.Context, kubeconfigPath string, rel capi.HelmRelease) (bool, error)
	Install(ctx context.Context, kubeconfigPath string, rel capi.HelmRelease) error
	Upgrade(ctx context.Context, kubeconfigPath string, rel capi.HelmRelease) error
	// Uninstall removes a release; a release that does not exist is not an
	// error.
	Uninstall(ctx context.Context, kubeconfigPath string, rel capi.HelmRelease) error
}

// Installer installs Helm releases through a HelmClient and raw manifests
// through a capi.ManifestApplier. It implements capi.AddonInstaller.
type Installer struct {
	helm    HelmClient
	applier capi.ManifestApplier
	logger  *log.Logger
	// httpGet fetches a URL's body; overridable in tests.
	httpGet func(ctx context.Context, url string) ([]byte, error)
}

var _ capi.AddonInstaller = (*Installer)(nil)

// New wires a HelmClient and an applier.
func New(helm HelmClient, applier capi.ManifestApplier, logger *log.Logger) *Installer {
	if logger == nil {
		logger = log.New(log.Writer(), "[addons] ", log.LstdFlags)
	}
	return &Installer{helm: helm, applier: applier, logger: logger, httpGet: fetchURL}
}

// NewInstaller returns an Installer using the Helm SDK and the dynamic
// server-side applier.
func NewInstaller(logger *log.Logger) *Installer {
	return New(NewSDKHelmClient(logger), capi.NewDynamicApplier(), logger)
}

// Install installs a set in its fixed order: PreHelmManifestURLs,
// Prerequisites, Helm, Manifests. A release that already exists is upgraded
// so retries converge instead of failing on "already exists".
func (d *Installer) Install(ctx context.Context, cluster *capi.Cluster, set capi.AddonSet) error {
	for _, url := range set.PreHelmManifestURLs {
		d.logger.Printf("fetching pre-helm manifest %s", url)
		body, err := d.httpGet(ctx, url)
		if err != nil {
			return fmt.Errorf("%w: fetching pre-helm manifest %s: %v", ErrInstall, url, err)
		}
		if err := d.applier.Apply(ctx, cluster, body); err != nil {
			return fmt.Errorf("%w: applying pre-helm manifest %s: %v", ErrInstall, url, err)
		}
	}
	if err := d.applyManifests(ctx, cluster, "prerequisite", set.Prerequisites); err != nil {
		return err
	}
	if err := d.installReleases(ctx, cluster.KubeconfigPath, set.Helm); err != nil {
		return err
	}
	return d.applyManifests(ctx, cluster, "manifest", set.Manifests)
}

// Uninstall removes the set's Helm releases in reverse order. Raw manifests
// are left in place: they are objects a cluster that is about to be torn
// down or handed over does not need removed, and deleting them blindly could
// take namespaces with them.
func (d *Installer) Uninstall(ctx context.Context, cluster *capi.Cluster, set capi.AddonSet) error {
	for _, rel := range slices.Backward(set.Helm) {
		d.logger.Printf("uninstalling helm release %s/%s", rel.Namespace, rel.Name)
		if err := d.helm.Uninstall(ctx, cluster.KubeconfigPath, rel); err != nil {
			return fmt.Errorf("%w: helm release %s/%s: %v", ErrUninstall, rel.Namespace, rel.Name, err)
		}
	}
	return nil
}

// installReleases installs or upgrades each release in order.
func (d *Installer) installReleases(ctx context.Context, kubeconfigPath string, rels []capi.HelmRelease) error {
	for _, rel := range rels {
		exists, err := d.helm.Exists(ctx, kubeconfigPath, rel)
		if err != nil {
			return fmt.Errorf("%w: checking release %s/%s: %v", ErrInstall, rel.Namespace, rel.Name, err)
		}
		if exists {
			d.logger.Printf("upgrading helm release %s/%s", rel.Namespace, rel.Name)
			err = d.helm.Upgrade(ctx, kubeconfigPath, rel)
		} else {
			d.logger.Printf("installing helm release %s/%s", rel.Namespace, rel.Name)
			err = d.helm.Install(ctx, kubeconfigPath, rel)
		}
		if err != nil {
			return fmt.Errorf("%w: helm release %s/%s: %v", ErrInstall, rel.Namespace, rel.Name, err)
		}
	}
	return nil
}

// applyManifests server-side-applies each raw manifest in order.
func (d *Installer) applyManifests(ctx context.Context, cluster *capi.Cluster, what string, manifests []string) error {
	for i, m := range manifests {
		d.logger.Printf("applying %s %d/%d", what, i+1, len(manifests))
		if err := d.applier.Apply(ctx, cluster, []byte(m)); err != nil {
			return fmt.Errorf("%w: %s %d: %v", ErrInstall, what, i+1, err)
		}
	}
	return nil
}

// SDKHelmClient implements HelmClient with the Helm SDK.
type SDKHelmClient struct {
	logger *log.Logger
}

// NewSDKHelmClient returns a Helm SDK client.
func NewSDKHelmClient(logger *log.Logger) *SDKHelmClient {
	if logger == nil {
		logger = log.New(log.Writer(), "[helm] ", log.LstdFlags)
	}
	return &SDKHelmClient{logger: logger}
}

func (h *SDKHelmClient) configuration(kubeconfigPath, namespace string) (*action.Configuration, error) {
	cfg := new(action.Configuration)
	getter := kube.GetConfig(kubeconfigPath, "", namespace)
	if err := cfg.Init(getter, namespace, "secret", h.logger.Printf); err != nil {
		return nil, fmt.Errorf("initializing helm: %w", err)
	}
	rc, err := registry.NewClient()
	if err != nil {
		return nil, fmt.Errorf("creating helm registry client: %w", err)
	}
	cfg.RegistryClient = rc
	return cfg, nil
}

func (h *SDKHelmClient) loadChart(cpo *action.ChartPathOptions, rel capi.HelmRelease) (*chart.Chart, map[string]any, error) {
	cpo.Version = rel.Version
	if !registry.IsOCI(rel.Chart) {
		cpo.RepoURL = rel.Repository
	}
	path, err := cpo.LocateChart(rel.Chart, cli.New())
	if err != nil {
		return nil, nil, fmt.Errorf("locating chart %q: %w", rel.Chart, err)
	}
	ch, err := loader.Load(path)
	if err != nil {
		return nil, nil, fmt.Errorf("loading chart %q: %w", rel.Chart, err)
	}
	vals, err := chartutil.ReadValues([]byte(rel.Values))
	if err != nil {
		return nil, nil, fmt.Errorf("parsing values for %s: %w", rel.Name, err)
	}
	return ch, vals, nil
}

// Exists implements HelmClient.
func (h *SDKHelmClient) Exists(_ context.Context, kubeconfigPath string, rel capi.HelmRelease) (bool, error) {
	cfg, err := h.configuration(kubeconfigPath, rel.Namespace)
	if err != nil {
		return false, err
	}
	_, err = action.NewGet(cfg).Run(rel.Name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Install implements HelmClient.
func (h *SDKHelmClient) Install(ctx context.Context, kubeconfigPath string, rel capi.HelmRelease) error {
	cfg, err := h.configuration(kubeconfigPath, rel.Namespace)
	if err != nil {
		return err
	}
	inst := action.NewInstall(cfg)
	inst.ReleaseName = rel.Name
	inst.Namespace = rel.Namespace
	inst.CreateNamespace = true
	inst.Wait = true
	inst.Timeout = rel.Timeout
	inst.SetRegistryClient(cfg.RegistryClient)

	ch, vals, err := h.loadChart(&inst.ChartPathOptions, rel)
	if err != nil {
		return err
	}
	_, err = inst.RunWithContext(ctx, ch, vals)
	return err
}

// Upgrade implements HelmClient.
func (h *SDKHelmClient) Upgrade(ctx context.Context, kubeconfigPath string, rel capi.HelmRelease) error {
	cfg, err := h.configuration(kubeconfigPath, rel.Namespace)
	if err != nil {
		return err
	}
	up := action.NewUpgrade(cfg)
	up.Namespace = rel.Namespace
	up.Wait = true
	up.Timeout = rel.Timeout
	up.SetRegistryClient(cfg.RegistryClient)

	ch, vals, err := h.loadChart(&up.ChartPathOptions, rel)
	if err != nil {
		return err
	}
	_, err = up.RunWithContext(ctx, rel.Name, ch, vals)
	return err
}

// Uninstall implements HelmClient. It waits for the release's resources to
// be deleted, so a stack that must be gone (a DHCP responder) is gone when
// it returns.
func (h *SDKHelmClient) Uninstall(_ context.Context, kubeconfigPath string, rel capi.HelmRelease) error {
	cfg, err := h.configuration(kubeconfigPath, rel.Namespace)
	if err != nil {
		return err
	}
	un := action.NewUninstall(cfg)
	un.Wait = true
	un.Timeout = rel.Timeout
	un.IgnoreNotFound = true
	_, err = un.Run(rel.Name)
	return err
}

// fetchURL GETs url and returns the body. It bounds the request with a
// timeout so a hung endpoint cannot stall the install indefinitely.
func fetchURL(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
