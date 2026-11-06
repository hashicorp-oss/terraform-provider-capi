// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

func writeClusterctlConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "clusterctl.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOverlayReader_MergesFileAndOverrides(t *testing.T) {
	path := writeClusterctlConfig(t, `
providers:
  - name: tinkerbell
    type: InfrastructureProvider
    url: https://github.com/file-owner/cluster-api-provider-tinkerbell/releases/latest/infrastructure-components.yaml
  - name: custom
    type: AddonProvider
    url: https://github.com/file-owner/custom/releases/latest/addon-components.yaml
FOO: bar
`)
	r := newOverlayReader()
	r.AddOverride(
		"tinkerbell",
		clusterctlv1.InfrastructureProviderType,
		"https://github.com/overlay/cluster-api-provider-tinkerbell/releases/v1/infrastructure-components.yaml",
	)
	r.AddOverride(
		"unifi",
		clusterctlv1.IPAMProviderType,
		"https://github.com/ubiquiti-community/cluster-api-ipam-provider-unifi/releases/v0.4.1/ipam-components.yaml",
	)

	if err := r.Init(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	client, err := config.New(context.Background(), path, config.InjectReader(r))
	if err != nil {
		t.Fatal(err)
	}

	got, err := client.Providers().Get("tinkerbell", clusterctlv1.InfrastructureProviderType)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL() != "https://github.com/overlay/cluster-api-provider-tinkerbell/releases/v1/infrastructure-components.yaml" {
		t.Errorf("overlay should win over file: %s", got.URL())
	}
	got, err = client.Providers().Get("custom", clusterctlv1.AddonProviderType)
	if err != nil ||
		got.URL() != "https://github.com/file-owner/custom/releases/latest/addon-components.yaml" {
		t.Errorf("file-only provider must survive: %v %v", got, err)
	}
	if _, err := client.Providers().Get("unifi", clusterctlv1.IPAMProviderType); err != nil {
		t.Errorf("overlay-only provider must be registered: %v", err)
	}
	if v, err := client.Variables().Get("FOO"); err != nil || v != "bar" {
		t.Errorf("plain variables must pass through: %q %v", v, err)
	}
}

func TestOverlayReader_OverridesAddedAfterInitAreVisible(t *testing.T) {
	client, r, err := newOverlayConfigClient(context.Background(), writeClusterctlConfig(t, "{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	def, err := client.Providers().Get("docker", clusterctlv1.InfrastructureProviderType)
	if err != nil {
		t.Fatal(err)
	}
	if def.URL() == "" {
		t.Fatal("expected clusterctl default URL")
	}
	r.AddOverride(
		"docker",
		clusterctlv1.InfrastructureProviderType,
		"https://github.com/x/y/releases/v2/infrastructure-components.yaml",
	)
	after, err := client.Providers().Get("docker", clusterctlv1.InfrastructureProviderType)
	if err != nil ||
		after.URL() != "https://github.com/x/y/releases/v2/infrastructure-components.yaml" {
		t.Errorf("override added after Init must be visible: %v %v", after, err)
	}
}

func TestOverlayReader_MissingDefaultConfigIsFine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	r := newOverlayReader()
	if err := r.Init(context.Background(), ""); err != nil {
		t.Fatalf("missing default config must not error: %v", err)
	}
}

func TestOverlayReader_EnvVariables(t *testing.T) {
	t.Setenv("MY_TEST_VAR", "from-env")
	r := newOverlayReader()
	if err := r.Init(context.Background(), writeClusterctlConfig(t, "{}\n")); err != nil {
		t.Fatal(err)
	}
	if v, err := r.Get("my-test-var"); err != nil || v != "from-env" {
		t.Errorf("dash keys must map to underscore env vars: %q %v", v, err)
	}
}

func TestLookupDefaultProvider(t *testing.T) {
	client, _, err := newOverlayConfigClient(context.Background(), writeClusterctlConfig(t, "{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if def, exact := lookupDefaultProvider(
		client,
		ProviderConfig{Type: ProviderTypeInfrastructure, Name: "docker"},
	); def == nil ||
		!exact {
		t.Errorf("docker must be an exact default: %v %v", def, exact)
	}
	def, exact := lookupDefaultProvider(
		client,
		ProviderConfig{Type: ProviderTypeInfrastructure, Name: "tinkerbell"},
	)
	if def == nil || exact || def.Name() != "tinkerbell-tinkerbell" {
		t.Errorf(
			"tinkerbell must resolve through the tinkerbell-tinkerbell alias: %v %v",
			def,
			exact,
		)
	}
	if def, _ := lookupDefaultProvider(
		client,
		ProviderConfig{Type: ProviderTypeIPAM, Name: "unifi"},
	); def != nil {
		t.Errorf("unifi must be unknown: %v", def)
	}
}
