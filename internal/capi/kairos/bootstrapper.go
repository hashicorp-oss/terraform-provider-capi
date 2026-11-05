// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package kairos

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/addons"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/bmc"
)

// kubeconfigCommand reads the k3s admin kubeconfig on the node. k3s writes it
// with the mode its --write-kubeconfig-mode flag names, so a plain read is
// tried first and passwordless sudo second.
const kubeconfigCommand = "cat /etc/rancher/k3s/k3s.yaml 2>/dev/null || sudo -n cat /etc/rancher/k3s/k3s.yaml"

// Timeouts bounds each phase of a create.
type Timeouts struct {
	// Boot bounds each wait for SSH after a boot (Config.Boot.Timeout).
	Boot time.Duration
	// Kubeconfig bounds the wait for k3s to publish its kubeconfig once the
	// node answers over SSH.
	Kubeconfig time.Duration
	// Kubernetes bounds the wait for the API server and a Ready node.
	Kubernetes time.Duration
	// Addons bounds the addon install.
	Addons time.Duration
	// PowerDown bounds the wait for the node to report off before a boot.
	PowerDown time.Duration
	// Poll is the interval between probes inside a wait.
	Poll time.Duration
	// Probe bounds one SSH probe.
	Probe time.Duration
	// BMCRetries, RetryBase and RetryMax configure the retry policy around
	// BMC calls.
	BMCRetries int
	RetryBase  time.Duration
	RetryMax   time.Duration
}

// DefaultTimeouts returns production timeouts for a boot timeout.
func DefaultTimeouts(boot time.Duration) Timeouts {
	return Timeouts{
		Boot: boot, Kubeconfig: 10 * time.Minute, Kubernetes: 10 * time.Minute, Addons: 15 * time.Minute,
		PowerDown: 2 * time.Minute, Poll: 5 * time.Second, Probe: 15 * time.Second,
		BMCRetries: 3, RetryBase: 500 * time.Millisecond, RetryMax: 5 * time.Second,
	}
}

func (t Timeouts) withDefaults(boot time.Duration) Timeouts {
	d := DefaultTimeouts(boot)
	pick := func(v, def time.Duration) time.Duration {
		if v == 0 {
			return def
		}
		return v
	}
	t.Boot = pick(t.Boot, d.Boot)
	t.Kubeconfig = pick(t.Kubeconfig, d.Kubeconfig)
	t.Kubernetes = pick(t.Kubernetes, d.Kubernetes)
	t.Addons = pick(t.Addons, d.Addons)
	t.PowerDown = pick(t.PowerDown, d.PowerDown)
	t.Poll = pick(t.Poll, d.Poll)
	t.Probe = pick(t.Probe, d.Probe)
	t.RetryBase = pick(t.RetryBase, d.RetryBase)
	t.RetryMax = pick(t.RetryMax, d.RetryMax)
	if t.BMCRetries <= 0 {
		t.BMCRetries = d.BMCRetries
	}
	return t
}

// Option customizes a Bootstrapper; used by tests and by the provider.
type Option func(*Bootstrapper)

// WithBMC overrides the BMC implementation.
func WithBMC(b bmc.Client) Option { return func(bs *Bootstrapper) { bs.client = b } }

// WithSSHFactory overrides how the node is reached over SSH.
func WithSSHFactory(f SSHFactory) Option { return func(bs *Bootstrapper) { bs.newSSH = f } }

// WithKubeFactory overrides how the Kubernetes probe is built.
func WithKubeFactory(f KubeFactory) Option { return func(bs *Bootstrapper) { bs.kube = f } }

// WithAddonInstaller overrides addon installation.
func WithAddonInstaller(a capi.AddonInstaller) Option {
	return func(bs *Bootstrapper) { bs.addons = a }
}

// WithLogger sets the logger.
func WithLogger(l *log.Logger) Option { return func(bs *Bootstrapper) { bs.logger = l } }

// WithTimeouts overrides phase timeouts; zero fields keep their defaults.
func WithTimeouts(t Timeouts) Option { return func(bs *Bootstrapper) { bs.timeouts = t } }

// WithTempDir sets where the kubeconfig is written.
func WithTempDir(dir string) Option { return func(bs *Bootstrapper) { bs.tempDir = dir } }

// Bootstrapper implements capi.Bootstrapper for a disposable Kairos node.
type Bootstrapper struct {
	cfg      Config
	client   bmc.Client
	newSSH   SSHFactory
	kube     KubeFactory
	addons   capi.AddonInstaller
	logger   *log.Logger
	timeouts Timeouts
	tempDir  string

	mu    sync.Mutex
	media *bmc.MediaBoot
}

var _ capi.Bootstrapper = (*Bootstrapper)(nil)

// New builds a Bootstrapper with real bmclib, SSH, Helm and client-go
// implementations unless overridden by options.
func New(cfg Config, opts ...Option) *Bootstrapper {
	cfg = cfg.withDefaults()
	b := &Bootstrapper{cfg: cfg, tempDir: os.TempDir()}
	for _, o := range opts {
		o(b)
	}
	b.timeouts = b.timeouts.withDefaults(cfg.Boot.Timeout)
	if b.logger == nil {
		b.logger = log.New(os.Stderr, "[kairos-bootstrap] ", log.LstdFlags)
	}
	if b.client == nil && !b.manual() {
		b.client = bmc.New(cfg.Machine.BMC, b.logger)
	}
	if b.newSSH == nil {
		b.newSSH = NewSSH
	}
	if b.kube == nil {
		b.kube = NewClientGoKube
	}
	if b.addons == nil {
		b.addons = addons.NewInstaller(b.logger)
	}
	return b
}

// manual reports whether the machine is booted by hand: no BMC is driven,
// the node is only waited for and the operator powers it off afterwards.
func (b *Bootstrapper) manual() bool { return b.cfg.Boot.Method == bmc.BootMethodManual }

// mediaBoot returns the boot layer, keeping one across Create and Delete so
// a storage-redirection session opened by a boot is the one released later.
func (b *Bootstrapper) mediaBoot() *bmc.MediaBoot {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.media == nil {
		m := bmc.NewMediaBoot(b.client, b.cfg.Boot.Device, b.logger)
		m.PowerDown, m.Poll = b.timeouts.PowerDown, b.timeouts.Poll
		m.Retries, m.RetryBase, m.RetryMax = b.timeouts.BMCRetries, b.timeouts.RetryBase, b.timeouts.RetryMax
		b.media = m
	}
	return b.media
}

// Create implements capi.Bootstrapper. Unless the node already answers over
// SSH (a retried apply), it boots the live ISO through the BMC; then it reads
// the k3s kubeconfig over SSH, rewrites its server to the node's address,
// waits for the API and a Ready node, and installs the addons. The media
// stays attached until Delete.
func (b *Bootstrapper) Create(ctx context.Context, opts capi.BootstrapOptions) (*capi.Cluster, error) {
	name := opts.Name
	if name == "" {
		name = defaultClusterName
	}
	fail := func(op string, err error) (*capi.Cluster, error) {
		return nil, &capi.BootstrapError{ClusterName: name, Operation: op, Err: err}
	}
	if err := b.cfg.Validate(); err != nil {
		return fail("validate", err)
	}

	node := b.newSSH(b.cfg.SSH, b.cfg.Machine.Address)
	if b.answers(ctx, node) {
		b.logger.Printf("%s: %s already answers over SSH; reusing the live node", name, b.cfg.Machine.Address)
	} else if err := b.boot(ctx, name, node); err != nil {
		return fail("boot", err)
	}

	raw, err := b.fetchKubeconfig(ctx, node)
	if err != nil {
		return fail("kubeconfig", err)
	}
	kubeconfigPath := filepath.Join(b.tempDir, name+"-kubeconfig")
	if err := os.WriteFile(kubeconfigPath, raw, 0o600); err != nil {
		return fail("kubeconfig", err)
	}

	if err := b.waitKubernetes(ctx, kubeconfigPath); err != nil {
		return fail("kubernetes", err)
	}

	if !b.cfg.Addons.Empty() {
		actx, cancel := context.WithTimeout(ctx, b.timeouts.Addons)
		defer cancel()
		if err := b.addons.Install(actx, &capi.Cluster{Name: name, KubeconfigPath: kubeconfigPath}, b.cfg.Addons); err != nil {
			return fail("addons", err)
		}
	}
	b.logger.Printf("%s: live node %s ready (kubeconfig: %s)", name, b.cfg.Machine.Address, kubeconfigPath)
	return &capi.Cluster{Name: name, KubeconfigPath: kubeconfigPath}, nil
}

// answers reports whether the node runs a command over SSH.
func (b *Bootstrapper) answers(ctx context.Context, node SSH) bool {
	pctx, cancel := context.WithTimeout(ctx, b.timeouts.Probe)
	defer cancel()
	_, err := node.Run(pctx, "true")
	return err == nil
}

// boot boots the ISO until the node answers over SSH, up to Boot.Attempts
// times. A BMC failure is final; a silent boot is retried. With a manual boot
// nothing is driven: the node is waited for once, for the boot timeout.
func (b *Bootstrapper) boot(ctx context.Context, name string, node SSH) error {
	if b.manual() {
		b.logger.Printf("%s: boot method is manual; waiting up to %s for %s to be booted from %s by hand", name, b.timeouts.Boot, b.cfg.Machine.Address, b.cfg.ISO)
		err := bmc.WaitFor(ctx, b.timeouts.Boot, b.timeouts.Poll, func(ctx context.Context) (bool, error) {
			return b.answers(ctx, node), nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrBootTimeout
		}
		return nil
	}
	media := b.mediaBoot()
	for attempt := 1; attempt <= b.cfg.Boot.Attempts; attempt++ {
		b.logger.Printf("%s: booting %s from %s (attempt %d/%d)", name, b.cfg.Machine.Hostname, b.cfg.ISO, attempt, b.cfg.Boot.Attempts)
		if err := media.Boot(ctx, b.cfg.ISO); err != nil {
			return err
		}
		err := bmc.WaitFor(ctx, b.timeouts.Boot, b.timeouts.Poll, func(ctx context.Context) (bool, error) {
			return b.answers(ctx, node), nil
		})
		if err == nil {
			// The media stays attached (a storage-redirection session
			// included) for the life of the node: the cloud-config is read
			// from the ISO at every stage, and without kairos.ram the ISO is
			// the root filesystem too. Delete ejects it.
			b.logger.Printf("%s: %s answers over SSH; the boot media stays attached until the node is deleted", name, b.cfg.Machine.Address)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.logger.Printf("%s: %v", name, ErrBootTimeout)
	}
	return fmt.Errorf("%w after %d attempts: %w", ErrAttemptsExhausted, b.cfg.Boot.Attempts, ErrBootTimeout)
}

// fetchKubeconfig reads the k3s kubeconfig once k3s has written it and
// rewrites its server from the loopback address to the node's.
func (b *Bootstrapper) fetchKubeconfig(ctx context.Context, node SSH) ([]byte, error) {
	server := "https://" + net.JoinHostPort(b.cfg.Machine.Address, apiPort)
	var raw []byte
	var lastErr error
	err := bmc.WaitFor(ctx, b.timeouts.Kubeconfig, b.timeouts.Poll, func(ctx context.Context) (bool, error) {
		pctx, cancel := context.WithTimeout(ctx, b.timeouts.Probe)
		defer cancel()
		out, err := node.Run(pctx, kubeconfigCommand)
		if err != nil {
			lastErr = err
			return false, nil
		}
		rewritten, err := rewriteServer(out, server)
		if err != nil {
			lastErr = err
			return false, nil
		}
		raw = rewritten
		return true, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrKubeconfigTimeout, lastErr)
	}
	return raw, nil
}

// rewriteServer points every cluster in a kubeconfig at server.
func rewriteServer(raw []byte, server string) ([]byte, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	if len(cfg.Clusters) == 0 {
		return nil, errors.New("parsing kubeconfig: no clusters")
	}
	for _, c := range cfg.Clusters {
		c.Server = server
	}
	return clientcmd.Write(*cfg)
}

// waitKubernetes waits for the API server to answer and a node to be Ready.
func (b *Bootstrapper) waitKubernetes(ctx context.Context, kubeconfigPath string) error {
	kube, err := b.kube(kubeconfigPath)
	if err != nil {
		return err
	}
	err = bmc.WaitFor(ctx, b.timeouts.Kubernetes, b.timeouts.Poll, func(ctx context.Context) (bool, error) {
		if ok, err := kube.APIReachable(ctx); err != nil || !ok {
			return false, err
		}
		return kube.NodeReady(ctx)
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrKubernetesTimeout
	}
	return nil
}

// Delete implements capi.Bootstrapper: the media is ejected and the node
// powered off. Nothing was written to its disk, so there is nothing to reset.
func (b *Bootstrapper) Delete(ctx context.Context, cluster *capi.Cluster) error {
	name := defaultClusterName
	if cluster != nil && cluster.Name != "" {
		name = cluster.Name
	}
	if b.manual() {
		b.logger.Printf("%s: boot method is manual; power off %s by hand", name, b.cfg.Machine.Hostname)
	} else {
		b.mediaBoot().Detach(ctx)
		if err := bmc.Retry(ctx, b.timeouts.BMCRetries, b.timeouts.RetryBase, b.timeouts.RetryMax, b.client.PowerOff); err != nil {
			return &capi.BootstrapError{ClusterName: name, Operation: "delete", Err: err}
		}
	}
	if cluster != nil && cluster.KubeconfigPath != "" {
		if err := os.Remove(cluster.KubeconfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.logger.Printf("%s: removing kubeconfig %s: %v", name, cluster.KubeconfigPath, err)
		}
	}
	if !b.manual() {
		b.logger.Printf("%s: live node %s powered off", name, b.cfg.Machine.Hostname)
	}
	return nil
}

// Exists implements capi.Bootstrapper: true while the node answers over SSH.
func (b *Bootstrapper) Exists(ctx context.Context, _ string) (bool, error) {
	return b.answers(ctx, b.newSSH(b.cfg.SSH, b.cfg.Machine.Address)), nil
}
