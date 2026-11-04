// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// BootMethod selects how the node is booted into the installer.
type BootMethod string

// BootMethod values.
const (
	BootMethodAuto         BootMethod = "auto"
	BootMethodVirtualMedia BootMethod = "virtual_media"
	BootMethodHTTP         BootMethod = "http"
)

// Defaults applied by Config.withDefaults.
const (
	DefaultBootTimeout  = 15 * time.Minute
	DefaultBootAttempts = 3
	DefaultArchitecture = "amd64"
	defaultClusterName  = "capi-bootstrap"
)

// MachineConfig identifies the bootstrap machine.
type MachineConfig struct {
	Hostname string
	IP       string
	// Netmask, Gateway, MAC, and Nameservers pin static addressing on the node
	// so its IP does not drift off the certs/etcd/kubeconfig generated against
	// it. Empty Netmask leaves the node on DHCP.
	Netmask     string
	Gateway     string
	MAC         string
	Nameservers []string
	Disk        string
	BMC         BMCCredentials
}

// BootConfig controls installer boot.
type BootConfig struct {
	Method   BootMethod
	Timeout  time.Duration
	Attempts int
	// Device is the firmware boot target for attached virtual media. Empty
	// defaults to CD-ROM. BMCs that expose virtual media as a USB device (many
	// KVM-over-IP dongles, e.g. NanoKVM) need "usb" or the Cd override never
	// boots the media.
	Device BootDevice
}

// TalosConfig controls the Talos version, images, and machine config.
type TalosConfig struct {
	Version       string
	Architecture  string
	Endpoint      string
	Image         ImageSpec
	ConfigPatches []string
}

// Config is the complete bootstrapper configuration.
type Config struct {
	Machine MachineConfig
	Boot    BootConfig
	Talos   TalosConfig
	Addons  Addons
	// StateDir, when set, is where the machine-secrets bundle is cached so a
	// failed apply can be retried without wiping and re-installing: the next
	// create seeds the cached secrets, the node is recognized as ours, and the
	// observe-driven reconciler resumes from the node's actual state. Empty
	// disables caching.
	StateDir string
}

func (c Config) withDefaults() Config {
	if c.Boot.Method == "" {
		c.Boot.Method = BootMethodAuto
	}
	if c.Boot.Timeout == 0 {
		c.Boot.Timeout = DefaultBootTimeout
	}
	if c.Boot.Attempts <= 0 {
		c.Boot.Attempts = DefaultBootAttempts
	}
	if c.Talos.Architecture == "" {
		c.Talos.Architecture = DefaultArchitecture
	}
	if c.Talos.Endpoint == "" && c.Machine.IP != "" {
		c.Talos.Endpoint = "https://" + net.JoinHostPort(c.Machine.IP, "6443")
	}
	if c.Talos.Image.Factory == "" {
		c.Talos.Image.Factory = DefaultFactoryURL
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir()
	}
	c.Talos.Image.Version = c.Talos.Version
	c.Talos.Image.Architecture = c.Talos.Architecture
	return c
}

// Validate reports every missing or invalid field at once.
func (c Config) Validate() error {
	var errs []error
	if c.Machine.IP == "" {
		errs = append(errs, errors.New("machine ip is required"))
	}
	if c.Machine.Disk == "" {
		errs = append(errs, errors.New("machine disk is required"))
	}
	if c.Machine.BMC.Address == "" {
		errs = append(errs, errors.New("bmc address is required"))
	}
	if c.Talos.Version == "" {
		errs = append(errs, errors.New("talos version is required"))
	}
	switch c.Boot.Method {
	case BootMethodAuto, BootMethodVirtualMedia, BootMethodHTTP:
	default:
		errs = append(errs, fmt.Errorf("unknown boot method %q", c.Boot.Method))
	}
	switch c.Talos.Architecture {
	case "amd64", "arm64":
	default:
		errs = append(errs, fmt.Errorf("unknown architecture %q", c.Talos.Architecture))
	}
	return errors.Join(errs...)
}

// Option customizes a Bootstrapper; used by tests and by the provider.
type Option func(*Bootstrapper)

// WithBMC overrides the BMC implementation.
func WithBMC(b BMC) Option { return func(bs *Bootstrapper) { bs.bmc = b } }

// WithNode overrides the Talos API implementation.
func WithNode(n Node) Option { return func(bs *Bootstrapper) { bs.node = n } }

// WithKubeFactory overrides how the Kubernetes probe is built.
func WithKubeFactory(f KubeFactory) Option { return func(bs *Bootstrapper) { bs.kubeFactory = f } }

// WithImageResolver overrides image resolution.
func WithImageResolver(r ImageResolver) Option { return func(bs *Bootstrapper) { bs.images = r } }

// WithAddonInstaller overrides addon installation.
func WithAddonInstaller(a AddonInstaller) Option { return func(bs *Bootstrapper) { bs.addons = a } }

// WithLogger sets the logger.
func WithLogger(l *log.Logger) Option { return func(bs *Bootstrapper) { bs.logger = l } }

// WithTimeouts overrides phase timeouts; zero fields keep their defaults.
func WithTimeouts(t Timeouts) Option { return func(bs *Bootstrapper) { bs.timeouts = t } }

// WithTempDir sets where kubeconfig and talosconfig files are written.
func WithTempDir(dir string) Option { return func(bs *Bootstrapper) { bs.tempDir = dir } }

// WithNodeFactory overrides how Talos API clients for other cluster members
// are built (in-place teardown).
func WithNodeFactory(f func(ip string) Node) Option {
	return func(bs *Bootstrapper) { bs.nodeFactory = f }
}

// WithBMCFactory overrides how BMC clients for other cluster members are built
// (in-place teardown).
func WithBMCFactory(f func(BMCCredentials) BMC) Option {
	return func(bs *Bootstrapper) { bs.bmcFactory = f }
}

// Bootstrapper implements capi.Bootstrapper for a Talos bare-metal node.
type Bootstrapper struct {
	cfg         Config
	bmc         BMC
	node        Node
	kubeFactory KubeFactory
	images      ImageResolver
	addons      AddonInstaller
	logger      *log.Logger
	timeouts    Timeouts
	tempDir     string
	nodeFactory func(ip string) Node
	bmcFactory  func(BMCCredentials) BMC

	mu   sync.Mutex
	last *reconciler
}

var (
	_ capi.Bootstrapper          = (*Bootstrapper)(nil)
	_ capi.PreTemplateManifester = (*Bootstrapper)(nil)
	_ capi.InPlaceTeardowner     = (*Bootstrapper)(nil)
)

// New builds a Bootstrapper with real bmclib, Talos, Helm, and client-go
// implementations unless overridden by options.
func New(cfg Config, opts ...Option) *Bootstrapper {
	cfg = cfg.withDefaults()
	b := &Bootstrapper{cfg: cfg, tempDir: os.TempDir()}
	for _, o := range opts {
		o(b)
	}
	b.timeouts = b.timeouts.withDefaults(cfg.Boot.Timeout)
	if b.logger == nil {
		b.logger = log.New(os.Stderr, "[talos-bootstrap] ", log.LstdFlags)
	}
	if b.bmc == nil {
		b.bmc = NewBMCLib(cfg.Machine.BMC, b.logger)
	}
	if b.node == nil {
		b.node = NewMachineryNode(cfg.Machine.IP)
	}
	if b.kubeFactory == nil {
		b.kubeFactory = NewClientGoKube
	}
	if b.images == nil {
		b.images = NewFactoryResolver(nil)
	}
	if b.addons == nil {
		b.addons = NewAddonInstaller(NewSDKHelmClient(b.logger), capi.NewDynamicApplier(), b.logger)
	}
	if b.nodeFactory == nil {
		b.nodeFactory = func(ip string) Node { return NewMachineryNode(ip) }
	}
	if b.bmcFactory == nil {
		b.bmcFactory = func(creds BMCCredentials) BMC { return NewBMCLib(creds, b.logger) }
	}
	return b
}

func (b *Bootstrapper) newReconciler(name, k8sVersion string) *reconciler {
	return &reconciler{
		name: name, k8sVersion: k8sVersion, cfg: b.cfg,
		bmc: b.bmc, node: b.node, kubeFactory: b.kubeFactory, addons: b.addons,
		logger: b.logger, timeouts: b.timeouts, tempDir: b.tempDir,
		sess: &session{}, failures: map[ActionKind]int{},
	}
}

// Create implements capi.Bootstrapper. It converges the node from whatever
// state it is in to a ready single-node cluster.
func (b *Bootstrapper) Create(ctx context.Context, opts capi.BootstrapOptions) (*capi.Cluster, error) {
	name := opts.Name
	if name == "" {
		name = defaultClusterName
	}
	if err := b.cfg.Validate(); err != nil {
		return nil, &capi.BootstrapError{ClusterName: name, Operation: "validate", Err: err}
	}

	r := b.newReconciler(name, opts.KubernetesVersion)
	b.mu.Lock()
	b.last = r
	b.mu.Unlock()

	urls, err := b.images.Resolve(ctx, b.cfg.Talos.Image)
	if err != nil {
		return nil, &capi.BootstrapError{ClusterName: name, Operation: "resolve-images", Err: err}
	}
	r.sess.images = urls

	// Restore persisted secrets so a node we previously provisioned is recognized
	// as ours across runs and reachable over the Talos API. Prefer secrets from
	// Terraform state; otherwise fall back to the on-disk resume cache, which
	// survives a failed apply so the next create resumes from the node's actual
	// state instead of wiping and re-installing.
	seed := opts.ProviderSecrets[ProviderSecretsKey]
	if seed == "" {
		if cached, ok := readSecretsCache(secretsCachePath(b.cfg.StateDir, b.cfg.Machine.IP)); ok {
			seed = cached
			b.logger.Printf("%s: resuming from cached machine secrets", name)
		}
	}
	if seed != "" {
		if err := r.seedSecrets(seed); err != nil {
			return nil, &capi.BootstrapError{ClusterName: name, Operation: "seed-secrets", Err: err}
		}
	}

	cluster, err := r.run(ctx)
	if err != nil {
		return nil, &capi.BootstrapError{ClusterName: name, Operation: "create", Err: err}
	}

	// Surface the run's secrets bundle so the caller can persist it in state.
	if secretsYAML, serr := r.exportSecrets(); serr != nil {
		b.logger.Printf("%s: warning: exporting talos secrets for persistence: %v", name, serr)
	} else if secretsYAML != "" {
		if cluster.ProviderSecrets == nil {
			cluster.ProviderSecrets = map[string]string{}
		}
		cluster.ProviderSecrets[ProviderSecretsKey] = secretsYAML
	}
	return cluster, nil
}

// PreTemplateManifests implements capi.PreTemplateManifester. After Create, it
// returns the Talos machine-secrets Secret and the cluster kubeconfig Secret,
// both populated from the just-bootstrapped node, so the CAPI Talos providers
// adopt the node's PKI and kubeconfig and the cluster needs no pivot. It returns
// nil when there is no live session (e.g. Create was not run).
func (b *Bootstrapper) PreTemplateManifests(_ context.Context, clusterName, namespace string) ([][]byte, error) {
	b.mu.Lock()
	r := b.last
	b.mu.Unlock()
	if r == nil {
		return nil, nil
	}

	if r.sess.secretsBundle == nil {
		return nil, nil
	}

	var manifests [][]byte
	bundleYAML, err := MarshalSecretsBundle(r.sess.secretsBundle)
	if err != nil {
		return nil, err
	}
	secretManifest, err := talosSecretsBundleManifest(clusterName, namespace, bundleYAML)
	if err != nil {
		return nil, err
	}
	manifests = append(manifests, secretManifest)

	// Derive the cluster kubeconfig statically from the same bundle (no running
	// cluster needed), so CAPI has <cluster>-kubeconfig before the control plane
	// comes up.
	kubeconfig, err := deriveKubeconfigFromBundle(r.sess.secretsBundle, clusterName, r.cfg.Talos.Endpoint)
	if err != nil {
		return nil, err
	}
	kubeconfigManifest, err := clusterKubeconfigManifest(clusterName, namespace, kubeconfig)
	if err != nil {
		return nil, err
	}
	manifests = append(manifests, kubeconfigManifest)

	return manifests, nil
}

// Delete implements capi.Bootstrapper. With a live session the node is reset
// and halted; in every case it is powered off and its media detached.
func (b *Bootstrapper) Delete(ctx context.Context, cluster *capi.Cluster) error {
	name := clusterNameOf(cluster)
	r := b.teardownSession(ctx, cluster, name)
	if err := r.teardown(ctx); err != nil {
		return &capi.BootstrapError{ClusterName: name, Operation: "delete", Err: err}
	}
	b.forget()
	return nil
}

// TeardownInPlace implements capi.InPlaceTeardowner: every other member that
// answers the cluster's talosconfig is reset (wiped and halted) and powered
// off through its BMC, then the bootstrap node is reset and powered off. A
// member that does not answer is only powered off. Failures are collected so
// one unreachable machine does not leave the others running.
func (b *Bootstrapper) TeardownInPlace(ctx context.Context, cluster *capi.Cluster, machines []capi.InventoryMachine) error {
	name := clusterNameOf(cluster)
	r := b.teardownSession(ctx, cluster, name)

	members := append([]capi.InventoryMachine(nil), machines...)
	sort.Slice(members, func(i, j int) bool { return members[i].Hostname < members[j].Hostname })
	var errs []error
	for _, m := range members {
		if err := b.teardownMember(ctx, r, m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", m.Hostname, err))
		}
	}
	if err := r.teardown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("bootstrap node %s: %w", b.cfg.Machine.Hostname, err))
	}
	b.forget()
	if len(errs) > 0 {
		return &capi.BootstrapError{ClusterName: name, Operation: "teardown-in-place", Err: errors.Join(errs...)}
	}
	return nil
}

// teardownMember resets one other cluster member over the Talos API when it
// answers our talosconfig, then powers it off through its BMC.
func (b *Bootstrapper) teardownMember(ctx context.Context, r *reconciler, m capi.InventoryMachine) error {
	var errs []error
	if m.IP != "" && r.sess.talosconfig != nil {
		node := b.nodeFactory(m.IP)
		if st, _ := node.Probe(ctx, r.sess.talosconfig); st == TalosOurs {
			b.logger.Printf("%s: resetting member %s (%s)", r.name, m.Hostname, m.IP)
			if err := node.Reset(ctx, r.sess.talosconfig); err != nil {
				errs = append(errs, fmt.Errorf("reset: %w", err))
			} else if err := waitFor(ctx, r.timeouts.Reset, r.timeouts.Poll, func(ctx context.Context) (bool, error) {
				st, _ := node.Probe(ctx, r.sess.talosconfig)
				return st != TalosOurs, nil
			}); err != nil {
				errs = append(errs, fmt.Errorf("waiting for reset: %w", err))
			}
		} else {
			b.logger.Printf("%s: member %s (%s) does not answer the cluster talosconfig (%s); powering off only", r.name, m.Hostname, m.IP, st)
		}
	}
	if m.BMC != nil {
		bmc := b.bmcFactory(BMCCredentials{Address: m.BMC.Address, Username: m.BMC.Username, Password: m.BMC.Password})
		if err := retry(ctx, r.timeouts.BMCRetries, r.timeouts.RetryBase, r.timeouts.RetryMax, bmc.PowerOff); err != nil && !errors.Is(err, ErrUnsupported) {
			errs = append(errs, fmt.Errorf("power off: %w", err))
		}
	}
	return errors.Join(errs...)
}

// teardownSession returns the live reconciler or, in a fresh process, one
// seeded from the persisted secrets (state, then the resume cache) so teardown
// can reach the nodes over the Talos API.
func (b *Bootstrapper) teardownSession(ctx context.Context, cluster *capi.Cluster, name string) *reconciler {
	b.mu.Lock()
	r := b.last
	b.mu.Unlock()
	if r != nil {
		return r
	}
	r = b.newReconciler(name, "")
	seed := ""
	if cluster != nil {
		seed = cluster.ProviderSecrets[ProviderSecretsKey]
	}
	if seed == "" {
		if cached, ok := readSecretsCache(secretsCachePath(b.cfg.StateDir, b.cfg.Machine.IP)); ok {
			seed = cached
		}
	}
	if seed != "" {
		if urls, err := b.images.Resolve(ctx, b.cfg.Talos.Image); err == nil {
			r.sess.images = urls
		}
		if err := r.seedSecrets(seed); err != nil {
			b.logger.Printf("%s: seeding secrets for teardown: %v", name, err)
		}
	}
	return r
}

// forget drops the live session and the resume cache after a teardown, so a
// later create starts fresh.
func (b *Bootstrapper) forget() {
	if p := secretsCachePath(b.cfg.StateDir, b.cfg.Machine.IP); p != "" {
		_ = os.Remove(p)
	}
	b.mu.Lock()
	b.last = nil
	b.mu.Unlock()
}

func clusterNameOf(cluster *capi.Cluster) string {
	if cluster != nil && cluster.Name != "" {
		return cluster.Name
	}
	return defaultClusterName
}

// Exists implements capi.Bootstrapper. It is true only while this process
// holds a session whose talosconfig the node accepts.
func (b *Bootstrapper) Exists(ctx context.Context, _ string) (bool, error) {
	b.mu.Lock()
	r := b.last
	b.mu.Unlock()
	if r == nil || r.sess.talosconfig == nil {
		return false, nil
	}
	return r.probe(ctx) == TalosOurs, nil
}
