// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"slices"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	yaml "gopkg.in/yaml.v3"
)

// asMap is a checked map assertion for navigating decoded YAML in tests.
func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T: %v", v, v)
	}
	return m
}

// asSlice is a checked slice assertion for navigating decoded YAML in tests.
func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("expected slice, got %T: %v", v, v)
	}
	return s
}

func testConfigInput() ConfigInput {
	return ConfigInput{
		ClusterName:       "bootstrap",
		Endpoint:          "https://10.0.0.5:6443",
		KubernetesVersion: "v1.34.0",
		TalosVersion:      "v1.13.6",
		NodeIP:            "10.0.0.5",
		InstallDisk:       "/dev/sda",
		InstallerImage:    "factory.talos.dev/metal-installer/abc:v1.13.6",
	}
}

func TestGenerateConfig_SetsInstallAndScheduling(t *testing.T) {
	bundle, err := NewSecretsBundle("v1.13.6")
	if err != nil {
		t.Fatal(err)
	}
	gen, err := GenerateConfig(testConfigInput(), bundle)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := configloader.NewFromBytes(gen.MachineConfig)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if got := cfg.Machine().Install().Disk(); got != "/dev/sda" {
		t.Errorf("install disk = %q", got)
	}
	if got := cfg.Machine().Install().Image(); got != "factory.talos.dev/metal-installer/abc:v1.13.6" {
		t.Errorf("install image = %q", got)
	}
	if !cfg.Machine().Install().Zero() {
		t.Error("install wipe should be true")
	}
	if !cfg.Cluster().ScheduleOnControlPlanes() {
		t.Error("allowSchedulingOnControlPlanes should be true")
	}
	if got := cfg.Cluster().Endpoint().String(); got != "https://10.0.0.5:6443" {
		t.Errorf("endpoint = %q", got)
	}
	if !cfg.Machine().Type().IsControlPlane() {
		t.Error("machine type should be controlplane")
	}

	ctx := gen.Talosconfig.Contexts[gen.Talosconfig.Context]
	if !slices.Equal(ctx.Endpoints, []string{"10.0.0.5"}) {
		t.Errorf("talosconfig endpoints = %v", ctx.Endpoints)
	}
}

func TestGenerateConfig_AppliesUserPatchesInOrder(t *testing.T) {
	bundle, err := NewSecretsBundle("v1.13.6")
	if err != nil {
		t.Fatal(err)
	}
	in := testConfigInput()
	// Scalars are overridden by later patches; lists are appended by Talos'
	// strategic merge, so ordering is asserted on the scalar.
	in.Patches = []string{
		"machine:\n  install:\n    disk: /dev/sdb\n    extraKernelArgs: [first]\n",
		"machine:\n  install:\n    disk: /dev/sdc\n",
	}
	gen, err := GenerateConfig(in, bundle)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := configloader.NewFromBytes(gen.MachineConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Machine().Install().Disk(); got != "/dev/sdc" {
		t.Errorf("install disk = %q, want /dev/sdc (last patch wins)", got)
	}
	if got := cfg.Machine().Install().ExtraKernelArgs(); !slices.Equal(got, []string{"first"}) {
		t.Errorf("extraKernelArgs = %v, want [first]", got)
	}
}

func TestGenerateConfig_DefaultsKubernetesVersion(t *testing.T) {
	bundle, err := NewSecretsBundle("v1.13.6")
	if err != nil {
		t.Fatal(err)
	}
	in := testConfigInput()
	in.KubernetesVersion = ""
	if _, err := GenerateConfig(in, bundle); err != nil {
		t.Fatalf("GenerateConfig() with empty kubernetes version: %v", err)
	}
}

func TestGenerateConfig_InvalidPatch(t *testing.T) {
	bundle, err := NewSecretsBundle("v1.13.6")
	if err != nil {
		t.Fatal(err)
	}
	in := testConfigInput()
	in.Patches = []string{"machine: [not, a, map"}
	if _, err := GenerateConfig(in, bundle); err == nil {
		t.Fatal("expected error for malformed patch")
	}
}

func TestNewSecretsBundle_BadVersion(t *testing.T) {
	if _, err := NewSecretsBundle("not-a-version"); err == nil {
		t.Fatal("expected error for bad Talos version")
	}
}

func TestSecretsBundleRoundTrip(t *testing.T) {
	b, err := NewSecretsBundle("v1.13.6")
	if err != nil {
		t.Fatal(err)
	}
	y1, err := MarshalSecretsBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(y1, "secrets") || !strings.Contains(y1, "certs") {
		t.Fatalf("serialized bundle looks empty: %q", y1)
	}
	got, err := SecretsBundleFromYAML(y1)
	if err != nil {
		t.Fatal(err)
	}
	y2, err := MarshalSecretsBundle(got)
	if err != nil {
		t.Fatal(err)
	}
	if y1 != y2 {
		t.Fatalf("round-trip changed the bundle:\n--- first ---\n%s\n--- second ---\n%s", y1, y2)
	}
}

func TestSecretsCache(t *testing.T) {
	if secretsCachePath("", "10.0.0.160") != "" {
		t.Fatal("empty stateDir must disable the cache")
	}
	dir := t.TempDir()
	p := secretsCachePath(dir, "10.0.0.160")
	if p == "" {
		t.Fatal("expected a cache path for a set stateDir")
	}
	if _, ok := readSecretsCache(p); ok {
		t.Fatal("expected a miss before any write")
	}
	if err := writeSecretsCache(p, "bundle-yaml"); err != nil {
		t.Fatal(err)
	}
	got, ok := readSecretsCache(p)
	if !ok || got != "bundle-yaml" {
		t.Fatalf("readSecretsCache = %q, ok=%v; want \"bundle-yaml\", true", got, ok)
	}
	// A "" path is a no-op for both read and write (caching disabled).
	if err := writeSecretsCache("", "x"); err != nil {
		t.Fatalf("write to empty path should be a no-op: %v", err)
	}
	if _, ok := readSecretsCache(""); ok {
		t.Fatal("read of empty path must be a miss")
	}
}

func TestNetmaskToPrefix(t *testing.T) {
	cases := map[string]int{"255.255.0.0": 16, "255.255.255.0": 24, "255.0.0.0": 8, "255.255.255.128": 25}
	for mask, want := range cases {
		got, err := netmaskToPrefix(mask)
		if err != nil || got != want {
			t.Errorf("netmaskToPrefix(%q) = %d, %v; want %d", mask, got, err, want)
		}
	}
	if _, err := netmaskToPrefix("not-a-mask"); err == nil {
		t.Error("invalid netmask must error")
	}
}

func TestNetworkConfigPatch(t *testing.T) {
	patch, err := networkConfigPatch("host1", "10.1.40.35", "255.255.0.0", "10.0.0.1", "b8:ae:ed:76:79:61", []string{"10.0.0.1", "8.8.8.8"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(patch), &m); err != nil {
		t.Fatalf("patch is not valid YAML: %v\n%s", err, patch)
	}
	net := asMap(t, asMap(t, m["machine"])["network"])
	if net["hostname"] != "host1" {
		t.Errorf("hostname = %v", net["hostname"])
	}
	if ns, _ := net["nameservers"].([]any); len(ns) != 2 || ns[0] != "10.0.0.1" {
		t.Errorf("nameservers = %v", net["nameservers"])
	}
	dev := asMap(t, asSlice(t, net["interfaces"])[0])
	if addrs, _ := dev["addresses"].([]any); len(addrs) != 1 || addrs[0] != "10.1.40.35/16" {
		t.Errorf("addresses = %v", dev["addresses"])
	}
	if sel, _ := dev["deviceSelector"].(map[string]any); sel["hardwareAddr"] != "b8:ae:ed:76:79:61" {
		t.Errorf("deviceSelector = %v", dev["deviceSelector"])
	}
	route := asMap(t, asSlice(t, dev["routes"])[0])
	if route["network"] != "0.0.0.0/0" || route["gateway"] != "10.0.0.1" {
		t.Errorf("route = %v", route)
	}
}

func TestNetworkConfigPatch_NoMACUsesPhysicalSelector(t *testing.T) {
	patch, err := networkConfigPatch("host1", "10.1.40.35", "255.255.255.0", "10.0.0.1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch, "physical: true") || strings.Contains(patch, "hardwareAddr") {
		t.Errorf("empty MAC must select the physical interface, got:\n%s", patch)
	}
}

func TestNetworkConfigPatch_EmptyWhenNoNetwork(t *testing.T) {
	patch, err := networkConfigPatch("", "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if patch != "" {
		t.Errorf("no network inputs should yield an empty patch, got:\n%s", patch)
	}
}

func TestGenerateConfig_PinsHostnameAndStaticNetwork(t *testing.T) {
	bundle, err := NewSecretsBundle("v1.13.6")
	if err != nil {
		t.Fatal(err)
	}
	in := testConfigInput()
	in.Hostname = "talos-b8-ae-ed-76-79-61"
	in.NodeIP = "10.1.40.35"
	in.Netmask = "255.255.0.0"
	in.Gateway = "10.0.0.1"
	in.MAC = "b8:ae:ed:76:79:61"
	in.Nameservers = []string{"10.0.0.1"}

	gen, err := GenerateConfig(in, bundle)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(gen.MachineConfig, &doc); err != nil {
		t.Fatalf("generated config not YAML: %v", err)
	}
	net := asMap(t, asMap(t, doc["machine"])["network"])
	if net["hostname"] != "talos-b8-ae-ed-76-79-61" {
		t.Fatalf("machine.network.hostname not pinned: %v", net)
	}
	dev := asMap(t, asSlice(t, net["interfaces"])[0])
	if addrs, _ := dev["addresses"].([]any); len(addrs) == 0 || addrs[0] != "10.1.40.35/16" {
		t.Errorf("static address not set: %v", dev["addresses"])
	}
}
