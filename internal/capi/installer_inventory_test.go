// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

func TestClusterctlInstaller_Installed(t *testing.T) {
	i := NewClusterctlInstaller("")
	i.inventory = func(context.Context, config.Client, string) (*clusterctlv1.ProviderList, error) {
		return &clusterctlv1.ProviderList{Items: []clusterctlv1.Provider{
			{ObjectMeta: metav1.ObjectMeta{Name: "cluster-api", Namespace: "capi-system"}, ProviderName: "cluster-api", Type: string(clusterctlv1.CoreProviderType), Version: "v1.14.0"},
			{ObjectMeta: metav1.ObjectMeta{Name: "infrastructure-tinkerbell", Namespace: "capt-system"}, ProviderName: "tinkerbell", Type: string(clusterctlv1.InfrastructureProviderType), Version: "v0.7.9"},
			{ObjectMeta: metav1.ObjectMeta{Name: "control-plane-talos", Namespace: "cacppt-system"}, ProviderName: "talos", Type: string(clusterctlv1.ControlPlaneProviderType), Version: "v0.7.1"},
		}}, nil
	}
	installed, err := i.Installed(context.Background(), &Cluster{Name: "mgmt", KubeconfigPath: "/nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 3 {
		t.Fatalf("installed = %v", installed)
	}
	if installed[0].Type != ProviderTypeCore || installed[0].Name != "cluster-api" || installed[0].Version != "v1.14.0" || installed[0].Namespace != "capi-system" {
		t.Errorf("core = %+v", installed[0])
	}
	if installed[1].Type != ProviderTypeInfrastructure || installed[2].Type != ProviderTypeControlPlane {
		t.Errorf("types = %s %s", installed[1].Type, installed[2].Type)
	}
	for in, want := range map[clusterctlv1.ProviderType]ProviderType{
		clusterctlv1.BootstrapProviderType: ProviderTypeBootstrap, clusterctlv1.IPAMProviderType: ProviderTypeIPAM, clusterctlv1.AddonProviderType: ProviderTypeAddon,
	} {
		if got := providerTypeFromClusterctl(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestInitOptionsFor_NoDefaults(t *testing.T) {
	set := ProviderSet{}
	set.Add(ProviderConfig{Name: "tinkerbell", Type: ProviderTypeInfrastructure, Version: "v0.7.9"})
	opts := initOptionsFor(InitOptions{Providers: set, NoDefaults: true})
	if len(opts.InfrastructureProviders) != 1 || opts.InfrastructureProviders[0] != "tinkerbell:v0.7.9" {
		t.Errorf("infrastructure = %v", opts.InfrastructureProviders)
	}
	if len(opts.BootstrapProviders) != 1 || opts.BootstrapProviders[0] != "-" || len(opts.ControlPlaneProviders) != 1 || opts.ControlPlaneProviders[0] != "-" {
		t.Errorf("no defaults: bootstrap = %v control plane = %v", opts.BootstrapProviders, opts.ControlPlaneProviders)
	}
	if opts.CoreProvider != "" {
		t.Error("the core is left to clusterctl")
	}
	set.Add(ProviderConfig{Name: "talos", Type: ProviderTypeBootstrap})
	opts = initOptionsFor(InitOptions{Providers: set, NoDefaults: true})
	if len(opts.BootstrapProviders) != 1 || opts.BootstrapProviders[0] != "talos" {
		t.Errorf("configured bootstrap kept: %v", opts.BootstrapProviders)
	}
	opts = initOptionsFor(InitOptions{Providers: set})
	if len(opts.ControlPlaneProviders) != 0 {
		t.Errorf("defaults allowed: %v", opts.ControlPlaneProviders)
	}
}
