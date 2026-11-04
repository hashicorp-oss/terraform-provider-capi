// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"testing"
)

func TestProviderConfigClient_FillsEveryTypeAndRegistersOverrides(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	providers := ProviderSet{}
	providers.Add(ProviderConfig{Type: ProviderTypeCore, Name: "cluster-api"})
	providers.Add(ProviderConfig{Type: ProviderTypeInfrastructure, Name: "tinkerbell", Version: "v0.7.9", FetchConfig: &FetchConfig{Owner: "tinkerbell-community"}})
	providers.Add(ProviderConfig{Type: ProviderTypeBootstrap, Name: "talos", Version: "v0.8.2"})
	providers.Add(ProviderConfig{Type: ProviderTypeControlPlane, Name: "talos", Version: "v0.7.1"})
	providers.Add(ProviderConfig{Type: ProviderTypeIPAM, Name: "unifi", Version: "v0.4.1", FetchConfig: &FetchConfig{Owner: "ubiquiti-community", Repository: "cluster-api-ipam-provider-unifi"}})
	providers.Add(ProviderConfig{Type: ProviderTypeAddon, Name: "helm"})

	_, reader, err := newProviderConfigClient(context.Background(), "", providers)
	if err != nil {
		t.Fatal(err)
	}
	initOpts := buildInitOptions(providers)
	if initOpts.CoreProvider != "cluster-api" {
		t.Errorf("core = %q", initOpts.CoreProvider)
	}
	if len(initOpts.InfrastructureProviders) != 1 || initOpts.InfrastructureProviders[0] != "tinkerbell:v0.7.9" {
		t.Errorf("infra = %v", initOpts.InfrastructureProviders)
	}
	if len(initOpts.BootstrapProviders) != 1 || initOpts.BootstrapProviders[0] != "talos:v0.8.2" {
		t.Errorf("bootstrap = %v", initOpts.BootstrapProviders)
	}
	if len(initOpts.ControlPlaneProviders) != 1 || initOpts.ControlPlaneProviders[0] != "talos:v0.7.1" {
		t.Errorf("control plane = %v", initOpts.ControlPlaneProviders)
	}
	if len(initOpts.IPAMProviders) != 1 || initOpts.IPAMProviders[0] != "unifi:v0.4.1" {
		t.Errorf("ipam = %v", initOpts.IPAMProviders)
	}
	if len(initOpts.AddonProviders) != 1 || initOpts.AddonProviders[0] != "helm" {
		t.Errorf("addon = %v", initOpts.AddonProviders)
	}
	if !initOpts.WaitProviders {
		t.Error("WaitProviders must stay on")
	}

	got := map[string]string{}
	for _, o := range reader.overrides {
		got[o.Name+"/"+string(o.Type)] = o.URL
	}
	if got["tinkerbell/InfrastructureProvider"] != "https://github.com/tinkerbell-community/cluster-api-provider-tinkerbell/releases/v0.7.9/infrastructure-components.yaml" {
		t.Errorf("tinkerbell override = %q", got["tinkerbell/InfrastructureProvider"])
	}
	if got["unifi/IPAMProvider"] != "https://github.com/ubiquiti-community/cluster-api-ipam-provider-unifi/releases/v0.4.1/ipam-components.yaml" {
		t.Errorf("unifi override = %q", got["unifi/IPAMProvider"])
	}
	if _, ok := got["helm/AddonProvider"]; ok {
		t.Error("bare providers must not get an override")
	}
	// Both talos entries pin a version, so both types get overrides from clusterctl defaults.
	if got["talos/BootstrapProvider"] != "https://github.com/siderolabs/cluster-api-bootstrap-provider-talos/releases/v0.8.2/bootstrap-components.yaml" {
		t.Errorf("talos bootstrap override = %q", got["talos/BootstrapProvider"])
	}
	if got["talos/ControlPlaneProvider"] != "https://github.com/siderolabs/cluster-api-control-plane-provider-talos/releases/v0.7.1/control-plane-components.yaml" {
		t.Errorf("talos control-plane override = %q", got["talos/ControlPlaneProvider"])
	}
}

func TestProviderConfigClient_AliasedBareProviderGetsDefaultURL(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Type: ProviderTypeInfrastructure, Name: "tinkerbell"})
	providers.Add(ProviderConfig{Type: ProviderTypeInfrastructure, Name: "docker"})
	_, reader, err := newProviderConfigClient(context.Background(), "", providers)
	if err != nil {
		t.Fatal(err)
	}
	initOpts := buildInitOptions(providers)
	if len(reader.overrides) != 1 || reader.overrides[0].Name != "tinkerbell" ||
		reader.overrides[0].URL != "https://github.com/tinkerbell/cluster-api-provider-tinkerbell/releases/latest/infrastructure-components.yaml" {
		t.Errorf("tinkerbell (known to clusterctl as tinkerbell-tinkerbell) must be registered under its plain name; docker must not: %+v", reader.overrides)
	}
	if len(initOpts.InfrastructureProviders) != 2 || initOpts.InfrastructureProviders[0] != "tinkerbell" {
		t.Errorf("infra = %v", initOpts.InfrastructureProviders)
	}
}

func TestProviderConfigClient_UnknownProviderWithoutRepository(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Type: ProviderTypeIPAM, Name: "unifi", Version: "v0.4.1"})
	_, _, err := newProviderConfigClient(context.Background(), "", providers)
	if !errors.Is(err, ErrUnknownProviderRepository) {
		t.Fatalf("err = %v", err)
	}
}

// TestNewClusterctlClient_ResolvesTemplateProvider reproduces the failure seen
// in the field: init knew "tinkerbell" but template generation, using a plain
// clusterctl client, did not. Both must resolve through newClusterctlClient.
func TestNewClusterctlClient_ResolvesTemplateProvider(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Type: ProviderTypeInfrastructure, Name: "tinkerbell", Version: "v0.7.9", FetchConfig: &FetchConfig{Owner: "tinkerbell-community"}})
	client, _, err := newClusterctlClient(context.Background(), "", providers, map[string]string{"CONTROL_PLANE_VIP": "10.0.0.160"})
	if err != nil {
		t.Fatal(err)
	}
	// GetProvidersConfig lists what the client can resolve by name.
	list, err := client.GetProvidersConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.Name() == "tinkerbell" && p.Type() == ProviderTypeInfrastructure.Clusterctl() {
			if p.URL() != "https://github.com/tinkerbell-community/cluster-api-provider-tinkerbell/releases/v0.7.9/infrastructure-components.yaml" {
				t.Errorf("tinkerbell URL = %s", p.URL())
			}
			return
		}
	}
	t.Fatal("clusterctl client cannot resolve infrastructure provider \"tinkerbell\"")
}

func TestNewProviderConfigClient_VariablesWinOverEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTROL_PLANE_VIP", "from-env")
	configClient, _, err := newProviderConfigClient(context.Background(), "", ProviderSet{})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := configClient.Variables().Get("CONTROL_PLANE_VIP"); err != nil || v != "from-env" {
		t.Fatalf("env fallback: %q %v", v, err)
	}
	configClient.Variables().Set("CONTROL_PLANE_VIP", "10.0.0.160")
	if v, _ := configClient.Variables().Get("CONTROL_PLANE_VIP"); v != "10.0.0.160" {
		t.Errorf("explicit variable must win: %q", v)
	}
}
