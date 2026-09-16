// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	yaml "gopkg.in/yaml.v3"
)

// ProviderSecretsKey is the key under capi.Cluster.ProviderSecrets that holds
// the Talos machine-secrets bundle (YAML). It is provider-scoped so the generic
// secrets map can carry secrets for any bootstrap provider.
const ProviderSecretsKey = "talos_machine_secrets"

// defaultStateDir is where the resume secrets cache lives when no state_dir is
// configured: a persistent per-user location so a failed apply can resume on the
// next run. It falls back to the temp dir when no user cache dir is available.
func defaultStateDir() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "terraform-provider-capi")
	}
	return filepath.Join(os.TempDir(), "terraform-provider-capi")
}

// secretsCachePath returns the file where the machine-secrets bundle is cached
// for resume, keyed by node so a failed apply can be retried without wiping and
// re-installing. It returns "" when stateDir is empty (caching disabled).
func secretsCachePath(stateDir, nodeIP string) string {
	if stateDir == "" {
		return ""
	}
	safe := strings.NewReplacer("/", "_", ":", "_").Replace(nodeIP)
	return filepath.Join(stateDir, "talos-"+safe+"-secrets.yaml")
}

// readSecretsCache reads a cached bundle YAML; ok is false when path is empty or
// the file is absent/unreadable.
func readSecretsCache(path string) (yaml string, ok bool) {
	if path == "" {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// writeSecretsCache writes the bundle YAML (0600), creating stateDir as needed.
// A "" path disables caching and is a no-op.
func writeSecretsCache(path, bundleYAML string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(bundleYAML), 0o600)
}

// ConfigInput is what the bootstrapper needs to render a controlplane config.
type ConfigInput struct {
	ClusterName       string
	Endpoint          string
	KubernetesVersion string
	TalosVersion      string
	NodeIP            string
	InstallDisk       string
	InstallerImage    string
	// Hostname and the network fields pin the node's identity so a single-node,
	// no-pivot cluster keeps the IP its certs/etcd/kubeconfig are generated
	// against. Empty Netmask leaves the node on DHCP (hostname still pinned).
	Hostname    string
	Netmask     string
	Gateway     string
	MAC         string
	Nameservers []string
	// Patches are strategic-merge or RFC6902 patches applied in order after
	// the built-in wipe patch.
	Patches []string
}

// GeneratedConfig is a rendered machine config and the matching talosconfig.
type GeneratedConfig struct {
	MachineConfig []byte
	Talosconfig   *clientconfig.Config
}

// wipePatch forces a clean install; the node is disposable.
const wipePatch = "machine:\n  install:\n    wipe: true\n"

// NewSecretsBundle creates fresh cluster secrets for the given Talos version.
func NewSecretsBundle(talosVersion string) (*secrets.Bundle, error) {
	contract, err := config.ParseContractFromVersion(talosVersion)
	if err != nil {
		return nil, fmt.Errorf("parsing talos version %q: %w", talosVersion, err)
	}
	return secrets.NewBundle(secrets.NewFixedClock(time.Now()), contract)
}

// MarshalSecretsBundle serializes a secrets bundle to the YAML form used by
// `talosctl gen secrets` (and by the official Talos Terraform provider), so it
// can be persisted in state and later restored to recognize and reset a node we
// own.
func MarshalSecretsBundle(b *secrets.Bundle) (string, error) {
	out, err := yaml.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("marshaling secrets bundle: %w", err)
	}
	return string(out), nil
}

// SecretsBundleFromYAML restores a secrets bundle previously produced by
// MarshalSecretsBundle. The clock is reset to now so any certificate the
// generator regenerates is dated from the current time.
func SecretsBundleFromYAML(s string) (*secrets.Bundle, error) {
	var b secrets.Bundle
	if err := yaml.Unmarshal([]byte(s), &b); err != nil {
		return nil, fmt.Errorf("unmarshaling secrets bundle: %w", err)
	}
	b.Clock = secrets.NewFixedClock(time.Now())
	return &b, nil
}

// GenerateConfig renders a single-node controlplane machine config.
func GenerateConfig(in ConfigInput, bundle *secrets.Bundle) (*GeneratedConfig, error) {
	contract, err := config.ParseContractFromVersion(in.TalosVersion)
	if err != nil {
		return nil, fmt.Errorf("parsing talos version %q: %w", in.TalosVersion, err)
	}

	k8sVersion := strings.TrimPrefix(in.KubernetesVersion, "v")
	if k8sVersion == "" {
		k8sVersion = constants.DefaultKubernetesVersion
	}

	input, err := generate.NewInput(in.ClusterName, in.Endpoint, k8sVersion,
		generate.WithSecretsBundle(bundle),
		generate.WithVersionContract(contract),
		generate.WithInstallDisk(in.InstallDisk),
		generate.WithInstallImage(in.InstallerImage),
		generate.WithAllowSchedulingOnControlPlanes(true),
		generate.WithEndpointList([]string{in.NodeIP}),
	)
	if err != nil {
		return nil, fmt.Errorf("building config input: %w", err)
	}

	provider, err := input.Config(machine.TypeControlPlane)
	if err != nil {
		return nil, fmt.Errorf("generating controlplane config: %w", err)
	}
	raw, err := provider.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encoding config: %w", err)
	}

	netPatch, err := networkConfigPatch(in.Hostname, in.NodeIP, in.Netmask, in.Gateway, in.MAC, in.Nameservers)
	if err != nil {
		return nil, fmt.Errorf("building network patch: %w", err)
	}
	// Order: wipe, then our network patch (pin hostname + static addressing),
	// then user patches last so they can override.
	base := []string{wipePatch}
	if netPatch != "" {
		base = append(base, netPatch)
	}
	patches, err := configpatcher.LoadPatches(append(base, in.Patches...))
	if err != nil {
		return nil, fmt.Errorf("loading config patches: %w", err)
	}
	out, err := configpatcher.Apply(configpatcher.WithBytes(raw), patches)
	if err != nil {
		return nil, fmt.Errorf("applying config patches: %w", err)
	}
	patched, err := out.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encoding patched config: %w", err)
	}

	tc, err := input.Talosconfig()
	if err != nil {
		return nil, fmt.Errorf("generating talosconfig: %w", err)
	}

	return &GeneratedConfig{MachineConfig: patched, Talosconfig: tc}, nil
}

// netmaskToPrefix converts a dotted-quad IPv4 netmask (e.g. 255.255.0.0) to a
// prefix length (e.g. 16).
func netmaskToPrefix(netmask string) (int, error) {
	ip := net.ParseIP(strings.TrimSpace(netmask))
	if ip == nil || ip.To4() == nil {
		return 0, fmt.Errorf("invalid IPv4 netmask %q", netmask)
	}
	ones, bits := net.IPMask(ip.To4()).Size()
	if bits == 0 {
		return 0, fmt.Errorf("non-contiguous netmask %q", netmask)
	}
	return ones, nil
}

// networkConfigPatch renders a Talos strategic-merge patch that pins the node's
// hostname and, when a netmask is given, static addressing on the primary
// interface (address + default route + nameservers). The interface is selected
// by MAC when provided, else the first physical device. An empty result means
// there was nothing to pin (no hostname and no address).
func networkConfigPatch(hostname, ip, netmask, gateway, mac string, nameservers []string) (string, error) {
	network := map[string]any{}
	if hostname != "" {
		network["hostname"] = hostname
	}
	if len(nameservers) > 0 {
		network["nameservers"] = nameservers
	}
	if ip != "" && netmask != "" {
		prefix, err := netmaskToPrefix(netmask)
		if err != nil {
			return "", err
		}
		selector := map[string]any{"physical": true}
		if mac != "" {
			selector = map[string]any{"hardwareAddr": mac}
		}
		iface := map[string]any{
			"deviceSelector": selector,
			"addresses":      []string{fmt.Sprintf("%s/%d", ip, prefix)},
		}
		if gateway != "" {
			iface["routes"] = []map[string]any{{"network": "0.0.0.0/0", "gateway": gateway}}
		}
		network["interfaces"] = []map[string]any{iface}
	}
	if len(network) == 0 {
		return "", nil
	}
	patch, err := yaml.Marshal(map[string]any{"machine": map[string]any{"network": network}})
	if err != nil {
		return "", fmt.Errorf("marshaling network patch: %w", err)
	}
	return string(patch), nil
}
