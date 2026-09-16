// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"testing"
	"time"
)

func TestDefaultWaitOptions(t *testing.T) {
	opts := DefaultWaitOptions()
	if opts.Timeout != 30*time.Minute {
		t.Errorf("expected 30m timeout, got %v", opts.Timeout)
	}
	if opts.PollInterval != 15*time.Second {
		t.Errorf("expected 15s poll interval, got %v", opts.PollInterval)
	}
}

func TestCluster_Fields(t *testing.T) {
	c := &Cluster{
		Name:           "test",
		KubeconfigPath: "/tmp/kubeconfig",
		Namespace:      "default",
	}
	if c.Name != "test" {
		t.Error("name mismatch")
	}
	if c.KubeconfigPath != "/tmp/kubeconfig" {
		t.Error("kubeconfig path mismatch")
	}
	if c.Namespace != "default" {
		t.Error("namespace mismatch")
	}
}

func TestCreateClusterOptions_Fields(t *testing.T) {
	cpCount := int64(3)
	workerCount := int64(5)
	opts := CreateClusterOptions{
		Name:      "prod",
		Namespace: "prod-ns",
		Providers: ProviderSet{
			ProviderTypeInfrastructure: {{Type: ProviderTypeInfrastructure, Name: "docker"}},
			ProviderTypeCore:           {{Type: ProviderTypeCore, Name: "cluster-api", Version: "v1.7.0"}},
		},
		KubernetesVersion:    "v1.31.0",
		ControlPlane:         ControlPlaneTopology{Replicas: &cpCount},
		MachineDeployments:   []MachineDeploymentTopology{{Name: "md-0", Replicas: &workerCount}},
		ManagementKubeconfig: "/tmp/kubeconfig",
		SkipInit:             false,
		WaitForReady:         true,
		SelfManaged:          true,
	}

	if opts.Name != "prod" {
		t.Error("name mismatch")
	}
	if *opts.ControlPlane.Replicas != 3 {
		t.Errorf("cp count mismatch: got %d", *opts.ControlPlane.Replicas)
	}
	if *opts.MachineDeployments[0].Replicas != 5 {
		t.Errorf("worker count mismatch: got %d", *opts.MachineDeployments[0].Replicas)
	}
}

func TestProviderType_ComponentsFile(t *testing.T) {
	cases := map[ProviderType]string{
		ProviderTypeCore:           "core-components.yaml",
		ProviderTypeInfrastructure: "infrastructure-components.yaml",
		ProviderTypeBootstrap:      "bootstrap-components.yaml",
		ProviderTypeControlPlane:   "control-plane-components.yaml",
		ProviderTypeIPAM:           "ipam-components.yaml",
		ProviderTypeAddon:          "addon-components.yaml",
	}
	for typ, want := range cases {
		if got := typ.ComponentsFile(); got != want {
			t.Errorf("%s.ComponentsFile() = %q, want %q", typ, got, want)
		}
		back, ok := ProviderTypeFromClusterctl(typ.Clusterctl())
		if !ok || back != typ {
			t.Errorf("round trip through clusterctl type failed for %s: %q %v", typ, back, ok)
		}
	}
}

func TestProviderConfig_InitString(t *testing.T) {
	if got := (ProviderConfig{Name: "helm"}).InitString(); got != "helm" {
		t.Errorf("got %q", got)
	}
	if got := (ProviderConfig{Name: "talos", Version: "v0.8.2"}).InitString(); got != "talos:v0.8.2" {
		t.Errorf("got %q", got)
	}
}

func TestProviderConfig_NeedsURLOverride(t *testing.T) {
	if (ProviderConfig{Name: "helm"}).NeedsURLOverride() {
		t.Error("bare provider must not need an override")
	}
	if !(ProviderConfig{Name: "helm", Version: "v0.2.12"}).NeedsURLOverride() {
		t.Error("versioned provider needs an override")
	}
	if !(ProviderConfig{Name: "helm", FetchConfig: &FetchConfig{Owner: "me"}}).NeedsURLOverride() {
		t.Error("fetch config needs an override")
	}
}

func TestProviderConfig_HasCustomizations_IgnoresFetchAndVersion(t *testing.T) {
	p := ProviderConfig{Name: "helm", Version: "v1", FetchConfig: &FetchConfig{Owner: "me"}}
	if p.HasCustomizations() {
		t.Error("version/fetch config are not component customizations")
	}
	p.Manager = &ManagerConfig{FeatureGates: map[string]bool{"X": true}}
	if !p.HasCustomizations() {
		t.Error("manager config is a customization")
	}
}

func TestProviderSet(t *testing.T) {
	s := ProviderSet{}
	s.Add(ProviderConfig{Type: ProviderTypeInfrastructure, Name: "tinkerbell", Version: "v0.7.9"})
	s.Add(ProviderConfig{Type: ProviderTypeBootstrap, Name: "talos", Manager: &ManagerConfig{Verbosity: ptrInt64(2)}})
	s.Add(ProviderConfig{Type: ProviderTypeControlPlane, Name: "talos", Manager: &ManagerConfig{Verbosity: ptrInt64(5)}})
	s.Add(ProviderConfig{Type: ProviderTypeAddon, Name: "helm"})

	infra, ok := s.Infrastructure()
	if !ok || infra.Name != "tinkerbell" {
		t.Fatalf("Infrastructure() = %+v, %v", infra, ok)
	}
	if got := s.InitStrings(ProviderTypeInfrastructure); len(got) != 1 || got[0] != "tinkerbell:v0.7.9" {
		t.Errorf("InitStrings(infra) = %v", got)
	}
	if got := s.InitStrings(ProviderTypeIPAM); len(got) != 0 {
		t.Errorf("InitStrings(ipam) = %v, want empty", got)
	}
	c := s.Customized()
	if len(c) != 2 {
		t.Fatalf("Customized() has %d entries, want 2", len(c))
	}
	if *c[ProviderKey{ProviderTypeBootstrap, "talos"}].Manager.Verbosity != 2 ||
		*c[ProviderKey{ProviderTypeControlPlane, "talos"}].Manager.Verbosity != 5 {
		t.Error("bootstrap and control-plane talos must be keyed independently")
	}
	if got := s.All(); len(got) != 4 || got[0].Name != "talos" || got[2].Name != "tinkerbell" || got[3].Name != "helm" {
		t.Errorf("All() order = %v", got)
	}
}

func ptrInt64(v int64) *int64 { return &v }
