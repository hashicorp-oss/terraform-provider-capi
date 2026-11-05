// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package kairos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/bmc"
)

const k3sKubeconfig = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0t
    server: https://127.0.0.1:6443
  name: default
contexts:
- context:
    cluster: default
    user: default
  name: default
current-context: default
kind: Config
users:
- name: default
  user:
    client-certificate-data: LS0t
    client-key-data: LS0t
`

// fakeNode is the live node as seen over SSH: it answers once booted, and
// publishes the k3s kubeconfig after kubeconfigDelay reads.
type fakeNode struct {
	mu              sync.Mutex
	up              bool
	kubeconfigDelay int
	cmds            []string
}

func (n *fakeNode) Run(_ context.Context, cmd string) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.up {
		return nil, errors.New("dial tcp: connection refused")
	}
	n.cmds = append(n.cmds, cmd)
	switch {
	case cmd == "true":
		return nil, nil
	case strings.Contains(cmd, "k3s.yaml"):
		if n.kubeconfigDelay > 0 {
			n.kubeconfigDelay--
			return nil, errors.New("cat: /etc/rancher/k3s/k3s.yaml: No such file or directory")
		}
		return []byte(k3sKubeconfig), nil
	}
	return nil, fmt.Errorf("unexpected command %q", cmd)
}

func (n *fakeNode) setUp(up bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.up = up
}

// fakeBMC records BMC calls and boots the node when it is powered with media
// attached. bootsAfter boots are ignored first (a silent firmware).
type fakeBMC struct {
	mu         sync.Mutex
	node       *fakeNode
	power      bmc.PowerState
	media      string
	redirected bool
	bootsAfter int
	calls      []string
	insertErr  error
}

func (f *fakeBMC) record(c string) { f.calls = append(f.calls, c) }

func (f *fakeBMC) boot() {
	f.power = bmc.PowerOn
	if f.media == "" && !f.redirected {
		return
	}
	if f.bootsAfter > 0 {
		f.bootsAfter--
		return
	}
	f.node.setUp(true)
}

func (f *fakeBMC) PowerState(context.Context) (bmc.PowerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.power, nil
}

func (f *fakeBMC) PowerOn(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("power-on")
	f.boot()
	return nil
}

func (f *fakeBMC) PowerOff(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("power-off")
	f.power = bmc.PowerOff
	f.node.setUp(false)
	return nil
}

func (f *fakeBMC) PowerCycle(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("power-cycle")
	f.node.setUp(false)
	f.boot()
	return nil
}

func (f *fakeBMC) SetBootDevice(_ context.Context, dev bmc.BootDevice, _, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("boot-device:" + string(dev))
	return nil
}

func (f *fakeBMC) InsertMedia(_ context.Context, iso string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("insert")
	if f.insertErr != nil {
		return false, f.insertErr
	}
	f.media = iso
	return false, nil
}

func (f *fakeBMC) EjectMedia(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("eject")
	f.media = ""
	return nil
}

func (f *fakeBMC) SetHTTPBootURI(_ context.Context, uri string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("http-uri:" + uri)
	return false, nil
}

func (f *fakeBMC) PostCode(context.Context) (string, error) { return "fake", nil }

func (f *fakeBMC) sequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeAMT is a fakeBMC that streams the ISO over storage redirection.
type fakeAMT struct {
	*fakeBMC
	handle *fakeHandle
}

type fakeHandle struct {
	mu     sync.Mutex
	closed bool
}

func (h *fakeHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return nil
}

func (f *fakeAMT) RedirectSupported(context.Context) bool { return true }

func (f *fakeAMT) RedirectISO(_ context.Context, iso string) (bmc.RedirectHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("redirect:" + iso)
	f.redirected = true
	f.handle = &fakeHandle{}
	return f.handle, nil
}

type fakeKube struct {
	mu          sync.Mutex
	notReadyFor int
}

func (k *fakeKube) APIReachable(context.Context) (bool, error) { return true, nil }

func (k *fakeKube) NodeReady(context.Context) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.notReadyFor > 0 {
		k.notReadyFor--
		return false, nil
	}
	return true, nil
}

type fakeInstaller struct {
	kubeconfig string
	addons     capi.AddonSet
	calls      int
}

func (i *fakeInstaller) Install(_ context.Context, cluster *capi.Cluster, addons capi.AddonSet) error {
	i.calls++
	i.kubeconfig = cluster.KubeconfigPath
	i.addons = addons
	return nil
}

func testKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Machine: Machine{Hostname: "nuc", Address: "10.0.0.50", BMC: bmc.Credentials{Address: "10.0.1.50", Username: "admin", Password: "x"}},
		Boot:    bmc.BootConfig{Method: bmc.BootMethodVirtualMedia, Timeout: 300 * time.Millisecond, Attempts: 2, Device: bmc.BootDeviceUSB},
		ISO:     "http://files.example/kairos.iso.custom.iso",
		SSH:     SSHConfig{PrivateKey: testKey(t)},
		Addons:  capi.AddonSet{Helm: []capi.HelmRelease{{Name: "tinkerbell", Namespace: "tink", Chart: "oci://x/tinkerbell"}}},
	}
}

type harness struct {
	b         *Bootstrapper
	node      *fakeNode
	bmc       *fakeBMC
	kube      *fakeKube
	installer *fakeInstaller
	dir       string
}

func newHarness(t *testing.T, cfg Config, client bmc.Client, node *fakeNode) *harness {
	t.Helper()
	if node == nil {
		node = &fakeNode{}
	}
	var base *fakeBMC
	switch b := client.(type) {
	case *fakeBMC:
		base = b
	case *fakeAMT:
		base = b.fakeBMC
	case nil:
		base = &fakeBMC{}
		client = base
	}
	base.node = node
	if base.power == "" {
		base.power = bmc.PowerOff
	}
	h := &harness{node: node, bmc: base, kube: &fakeKube{}, installer: &fakeInstaller{}, dir: t.TempDir()}
	h.b = New(cfg,
		WithBMC(client),
		WithSSHFactory(func(SSHConfig, string) SSH { return node }),
		WithKubeFactory(func(string) (Kube, error) { return h.kube, nil }),
		WithAddonInstaller(h.installer),
		WithLogger(log.New(io.Discard, "", 0)),
		WithTempDir(h.dir),
		WithTimeouts(Timeouts{
			Kubeconfig: time.Second, Kubernetes: time.Second, Addons: time.Second, PowerDown: 50 * time.Millisecond,
			Poll: 5 * time.Millisecond, Probe: 100 * time.Millisecond, BMCRetries: 1, RetryBase: time.Millisecond, RetryMax: time.Millisecond,
		}),
	)
	return h
}

func TestCreate_BootsReadsKubeconfigAndInstallsAddons(t *testing.T) {
	h := newHarness(t, testConfig(t), nil, &fakeNode{kubeconfigDelay: 2})
	h.kube.notReadyFor = 1

	cluster, err := h.b.Create(context.Background(), capi.BootstrapOptions{Name: "bm-bootstrap"})
	if err != nil {
		t.Fatal(err)
	}
	if cluster.Name != "bm-bootstrap" || cluster.KubeconfigPath != filepath.Join(h.dir, "bm-bootstrap-kubeconfig") {
		t.Errorf("cluster = %+v", cluster)
	}
	want := []string{"eject", "insert", "boot-device:usb", "power-on"}
	if got := h.bmc.sequence(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bmc calls = %v, want %v", got, want)
	}
	raw, err := os.ReadFile(cluster.KubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if kc.Clusters["default"].Server != "https://10.0.0.50:6443" {
		t.Errorf("server = %q, want the node address", kc.Clusters["default"].Server)
	}
	if h.installer.calls != 1 || h.installer.kubeconfig != cluster.KubeconfigPath || len(h.installer.addons.Helm) != 1 {
		t.Errorf("addons installed = %d on %q (%+v)", h.installer.calls, h.installer.kubeconfig, h.installer.addons)
	}
	if cluster.ProviderSecrets != nil {
		t.Error("a disposable node keeps no provider secrets")
	}
}

func TestCreate_PowersARunningNodeOffFirst(t *testing.T) {
	bmc := &fakeBMC{power: bmc.PowerOn}
	h := newHarness(t, testConfig(t), bmc, nil)
	if _, err := h.b.Create(context.Background(), capi.BootstrapOptions{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"power-off", "eject", "insert", "boot-device:usb", "power-cycle"}
	if got := h.bmc.sequence(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bmc calls = %v, want %v", got, want)
	}
}

func TestCreate_RetriesASilentBoot(t *testing.T) {
	h := newHarness(t, testConfig(t), &fakeBMC{bootsAfter: 1}, nil)
	if _, err := h.b.Create(context.Background(), capi.BootstrapOptions{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	seq := h.bmc.sequence()
	if n := strings.Count(strings.Join(seq, ","), "insert"); n != 2 {
		t.Errorf("want two boot attempts, got calls %v", seq)
	}
}

func TestCreate_GivesUpAfterTheAttempts(t *testing.T) {
	h := newHarness(t, testConfig(t), &fakeBMC{bootsAfter: 5}, nil)
	_, err := h.b.Create(context.Background(), capi.BootstrapOptions{Name: "x"})
	var be *capi.BootstrapError
	if !errors.As(err, &be) || be.Operation != "boot" || !errors.Is(err, ErrAttemptsExhausted) || !errors.Is(err, ErrBootTimeout) {
		t.Fatalf("err = %v", err)
	}
	if n := strings.Count(strings.Join(h.bmc.sequence(), ","), "insert"); n != 2 {
		t.Errorf("want exactly Boot.Attempts boots, got %d", n)
	}
}

func TestCreate_BMCFailureIsFinal(t *testing.T) {
	h := newHarness(t, testConfig(t), &fakeBMC{insertErr: errors.New("redfish: 500")}, nil)
	_, err := h.b.Create(context.Background(), capi.BootstrapOptions{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "redfish: 500") {
		t.Fatalf("err = %v", err)
	}
	if n := strings.Count(strings.Join(h.bmc.sequence(), ","), "insert"); n != 1 {
		t.Errorf("a BMC error must not be retried as a boot attempt, got %v", h.bmc.sequence())
	}
}

func TestCreate_ReusesALiveNode(t *testing.T) {
	h := newHarness(t, testConfig(t), nil, &fakeNode{up: true})
	if _, err := h.b.Create(context.Background(), capi.BootstrapOptions{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if seq := h.bmc.sequence(); len(seq) != 0 {
		t.Errorf("a node that answers is not rebooted; bmc calls = %v", seq)
	}
}

func TestCreate_KeepsStorageRedirectionUntilDelete(t *testing.T) {
	amt := &fakeAMT{fakeBMC: &fakeBMC{}}
	h := newHarness(t, testConfig(t), amt, nil)
	cluster, err := h.b.Create(context.Background(), capi.BootstrapOptions{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"eject", "redirect:http://files.example/kairos.iso.custom.iso", "power-on"}
	if got := h.bmc.sequence(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bmc calls = %v, want %v (no insert, no boot override: AMT arms the boot)", got, want)
	}
	if amt.handle == nil || amt.handle.closed {
		t.Fatal("the live system reads its cloud-config (and without kairos.ram its root filesystem) from the ISO: the session must stay open")
	}
	if err := h.b.Delete(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	if !amt.handle.closed {
		t.Error("delete releases the session before powering off")
	}
}

func TestCreate_KubeconfigTimeout(t *testing.T) {
	h := newHarness(t, testConfig(t), nil, &fakeNode{kubeconfigDelay: 1 << 20})
	h.b.timeouts.Kubeconfig = 50 * time.Millisecond
	_, err := h.b.Create(context.Background(), capi.BootstrapOptions{Name: "x"})
	var be *capi.BootstrapError
	if !errors.As(err, &be) || be.Operation != "kubeconfig" || !errors.Is(err, ErrKubeconfigTimeout) || !strings.Contains(err.Error(), "No such file") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreate_ValidatesFirst(t *testing.T) {
	cfg := testConfig(t)
	cfg.ISO = ""
	cfg.Boot.Method = bmc.BootMethodHTTP
	cfg.SSH.PrivateKey = "not a key"
	h := newHarness(t, cfg, nil, nil)
	_, err := h.b.Create(context.Background(), capi.BootstrapOptions{})
	var be *capi.BootstrapError
	if !errors.As(err, &be) || be.Operation != "validate" {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"iso is required", `boot method "http"`, "ssh private key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if seq := h.bmc.sequence(); len(seq) != 0 {
		t.Errorf("nothing is booted on invalid config: %v", seq)
	}
}

func TestConfig_DefaultsAndValidation(t *testing.T) {
	cfg := Config{SSH: SSHConfig{PrivateKey: testKey(t), HostKey: "ssh-ed25519 AAAA"}}.withDefaults()
	if cfg.SSH.User != "kairos" || cfg.SSH.Port != 22 || cfg.Boot.Method != bmc.BootMethodAuto || cfg.Boot.Attempts != 3 || cfg.Boot.Timeout != bmc.DefaultBootTimeout {
		t.Errorf("defaults = %+v", cfg)
	}
	err := cfg.Validate()
	for _, want := range []string{"machine address", "bmc address", "iso is required", "ssh host key"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validation lacks %q: %v", want, err)
		}
	}
	if err := testConfig(t).withDefaults().Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestDelete_EjectsAndPowersOff(t *testing.T) {
	h := newHarness(t, testConfig(t), nil, nil)
	cluster, err := h.b.Create(context.Background(), capi.BootstrapOptions{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.b.Delete(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	seq := h.bmc.sequence()
	tail := seq[len(seq)-2:]
	if strings.Join(tail, ",") != "eject,power-off" {
		t.Errorf("delete calls = %v", seq)
	}
	if _, err := os.Stat(cluster.KubeconfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("kubeconfig must be removed: %v", err)
	}
	if up, _ := h.b.Exists(context.Background(), "x"); up {
		t.Error("a powered-off node does not exist")
	}
}

func TestDelete_WithoutACreateStillPowersOff(t *testing.T) {
	h := newHarness(t, testConfig(t), &fakeBMC{power: bmc.PowerOn}, &fakeNode{up: true})
	if err := h.b.Delete(context.Background(), &capi.Cluster{Name: "x-bootstrap"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.bmc.sequence(), ","); got != "eject,power-off" {
		t.Errorf("delete calls = %v", got)
	}
}

func TestExists_ProbesSSH(t *testing.T) {
	h := newHarness(t, testConfig(t), nil, &fakeNode{up: true})
	if up, err := h.b.Exists(context.Background(), "x"); err != nil || !up {
		t.Errorf("exists = %v, %v", up, err)
	}
}

func TestRewriteServer(t *testing.T) {
	out, err := rewriteServer([]byte(k3sKubeconfig), "https://10.0.0.50:6443")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "server: https://10.0.0.50:6443") || strings.Contains(string(out), "127.0.0.1") {
		t.Errorf("rewritten kubeconfig:\n%s", out)
	}
	if _, err := rewriteServer([]byte("apiVersion: v1\nkind: Config\n"), "https://x"); err == nil {
		t.Error("a kubeconfig without clusters is rejected")
	}
}
