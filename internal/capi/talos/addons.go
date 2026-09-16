// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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

// HelmRelease describes one chart to install on the bootstrap cluster.
type HelmRelease struct {
	Name      string
	Namespace string
	// Chart is an oci:// reference or a chart name used with Repository.
	Chart      string
	Repository string
	Version    string
	// Values is a YAML document of chart values.
	Values  string
	Timeout time.Duration
}

// Addons is everything to install after the cluster comes up, in order:
// Helm releases first, then raw manifests.
type Addons struct {
	Helm      []HelmRelease
	Manifests []string
	// PreHelmManifestURLs are URLs of raw manifests (e.g. Gateway API CRDs)
	// fetched and applied BEFORE the Helm releases, so a chart that renders a
	// custom resource (Cilium's GatewayClass) finds its CRD already present.
	PreHelmManifestURLs []string
}

// Empty reports whether nothing is configured.
func (a Addons) Empty() bool {
	return len(a.Helm) == 0 && len(a.Manifests) == 0 && len(a.PreHelmManifestURLs) == 0
}

// HelmClient abstracts the Helm SDK for tests.
type HelmClient interface {
	Exists(ctx context.Context, kubeconfigPath string, rel HelmRelease) (bool, error)
	Install(ctx context.Context, kubeconfigPath string, rel HelmRelease) error
	Upgrade(ctx context.Context, kubeconfigPath string, rel HelmRelease) error
}

// AddonInstaller installs addons idempotently.
type AddonInstaller interface {
	Install(ctx context.Context, kubeconfigPath string, addons Addons) error
}

// DefaultAddonInstaller installs Helm releases through a HelmClient and raw
// manifests through a capi.Applier.
type DefaultAddonInstaller struct {
	helm    HelmClient
	applier capi.Applier
	logger  *log.Logger
	// httpGet fetches a URL's body; overridable in tests.
	httpGet func(ctx context.Context, url string) ([]byte, error)
}

// NewAddonInstaller wires a HelmClient and an Applier.
func NewAddonInstaller(helm HelmClient, applier capi.Applier, logger *log.Logger) *DefaultAddonInstaller {
	if logger == nil {
		logger = log.New(log.Writer(), "[talos-addons] ", log.LstdFlags)
	}
	return &DefaultAddonInstaller{helm: helm, applier: applier, logger: logger, httpGet: fetchURL}
}

// Install implements AddonInstaller. A release that already exists is
// upgraded so retries converge instead of failing on "already exists".
func (d *DefaultAddonInstaller) Install(ctx context.Context, kubeconfigPath string, addons Addons) error {
	preCluster := &capi.Cluster{Name: "bootstrap", KubeconfigPath: kubeconfigPath}
	for _, url := range addons.PreHelmManifestURLs {
		d.logger.Printf("fetching pre-helm manifest %s", url)
		body, err := d.httpGet(ctx, url)
		if err != nil {
			return fmt.Errorf("%w: fetching pre-helm manifest %s: %v", ErrAddons, url, err)
		}
		if err := d.applier.Apply(ctx, preCluster, body); err != nil {
			return fmt.Errorf("%w: applying pre-helm manifest %s: %v", ErrAddons, url, err)
		}
	}

	for _, rel := range addons.Helm {
		exists, err := d.helm.Exists(ctx, kubeconfigPath, rel)
		if err != nil {
			return fmt.Errorf("%w: checking release %s/%s: %v", ErrAddons, rel.Namespace, rel.Name, err)
		}
		if exists {
			d.logger.Printf("upgrading helm release %s/%s", rel.Namespace, rel.Name)
			err = d.helm.Upgrade(ctx, kubeconfigPath, rel)
		} else {
			d.logger.Printf("installing helm release %s/%s", rel.Namespace, rel.Name)
			err = d.helm.Install(ctx, kubeconfigPath, rel)
		}
		if err != nil {
			return fmt.Errorf("%w: helm release %s/%s: %v", ErrAddons, rel.Namespace, rel.Name, err)
		}
	}

	cluster := &capi.Cluster{Name: "bootstrap", KubeconfigPath: kubeconfigPath}
	for i, m := range addons.Manifests {
		d.logger.Printf("applying manifest %d/%d", i+1, len(addons.Manifests))
		if err := d.applier.Apply(ctx, cluster, []byte(m)); err != nil {
			return fmt.Errorf("%w: manifest %d: %v", ErrAddons, i+1, err)
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

func (h *SDKHelmClient) loadChart(cpo *action.ChartPathOptions, rel HelmRelease) (*chart.Chart, map[string]any, error) {
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
func (h *SDKHelmClient) Exists(_ context.Context, kubeconfigPath string, rel HelmRelease) (bool, error) {
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
func (h *SDKHelmClient) Install(ctx context.Context, kubeconfigPath string, rel HelmRelease) error {
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
func (h *SDKHelmClient) Upgrade(ctx context.Context, kubeconfigPath string, rel HelmRelease) error {
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
