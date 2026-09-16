// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// loadTestCRDs decodes the CRD fixtures under testdata/crds.
func loadTestCRDs(t *testing.T, files ...string) []apiextv1.CustomResourceDefinition {
	t.Helper()
	var out []apiextv1.CustomResourceDefinition
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join("testdata", "crds", f))
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		out = append(out, decodeCRDs(t, data)...)
	}
	return out
}

func decodeCRDs(t *testing.T, data []byte) []apiextv1.CustomResourceDefinition {
	t.Helper()
	var out []apiextv1.CustomResourceDefinition
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var crd apiextv1.CustomResourceDefinition
		if err := dec.Decode(&crd); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decoding CRD: %v", err)
		}
		if crd.Name != "" {
			out = append(out, crd)
		}
	}
	return out
}

// crdYAML builds one CRD fixture from a compact description. contract is the
// value of the cluster.x-k8s.io/v1beta2 label ("" omits it); versions are
// "name" or "name*" for the storage version.
func crdYAML(t *testing.T, label, kind, contract string, versions ...string) apiextv1.CustomResourceDefinition {
	t.Helper()
	const group = "infrastructure.cluster.x-k8s.io"
	labels := map[string]string{"cluster.x-k8s.io/provider": label}
	if contract != "" {
		labels["cluster.x-k8s.io/v1beta2"] = contract
	}
	var vers []apiextv1.CustomResourceDefinitionVersion
	for _, v := range versions {
		storage := strings.HasSuffix(v, "*")
		vers = append(vers, apiextv1.CustomResourceDefinitionVersion{Name: strings.TrimSuffix(v, "*"), Served: true, Storage: storage})
	}
	crd := apiextv1.CustomResourceDefinition{}
	crd.Name = strings.ToLower(kind) + "s." + group
	crd.Labels = labels
	crd.Spec.Group = group
	crd.Spec.Names = apiextv1.CustomResourceDefinitionNames{Kind: kind, Plural: strings.ToLower(kind) + "s"}
	crd.Spec.Versions = vers
	// Round-trip through YAML so the fixture is built the same way the real ones are decoded.
	b, err := yaml.Marshal(crd)
	if err != nil {
		t.Fatal(err)
	}
	return decodeCRDs(t, b)[0]
}

// parseGVK parses the schema.GroupVersionKind String() form ("g/v, Kind=K").
func parseGVK(t *testing.T, s string) schema.GroupVersionKind {
	t.Helper()
	gv, kind, ok := strings.Cut(s, ", Kind=")
	if !ok {
		t.Fatalf("bad GVK %q", s)
	}
	parsed, err := schema.ParseGroupVersion(gv)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.WithKind(kind)
}

func talosTinkerbellCatalog(t *testing.T) *KindCatalog {
	t.Helper()
	return NewKindCatalog(loadTestCRDs(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml"))
}

func TestKindCatalog_CoreVersion_IsClustersStorageVersion(t *testing.T) {
	got, err := talosTinkerbellCatalog(t).CoreVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1beta2" {
		t.Errorf("CoreVersion() = %q, want v1beta2", got)
	}
}

func TestKindCatalog_CoreVersion_MissingCoreCRD(t *testing.T) {
	_, err := NewKindCatalog(loadTestCRDs(t, "capt.yaml")).CoreVersion()
	if err == nil || !strings.Contains(err.Error(), "clusters.cluster.x-k8s.io") {
		t.Fatalf("expected error naming the core CRD, got %v", err)
	}
}

func TestKindCatalog_Resolve_ByProviderLabelAndSuffix(t *testing.T) {
	c := talosTinkerbellCatalog(t)
	cases := []struct {
		key    ProviderKey
		suffix string
		want   string // group/version, Kind
	}{
		{ProviderKey{ProviderTypeInfrastructure, "tinkerbell"}, "Cluster", "infrastructure.cluster.x-k8s.io/v1beta2, Kind=TinkerbellCluster"},
		{ProviderKey{ProviderTypeInfrastructure, "tinkerbell"}, "ClusterTemplate", "infrastructure.cluster.x-k8s.io/v1beta2, Kind=TinkerbellClusterTemplate"},
		{ProviderKey{ProviderTypeInfrastructure, "tinkerbell"}, "MachineTemplate", "infrastructure.cluster.x-k8s.io/v1beta2, Kind=TinkerbellMachineTemplate"},
		{ProviderKey{ProviderTypeControlPlane, "talos"}, "ControlPlane", "controlplane.cluster.x-k8s.io/v1beta1, Kind=TalosControlPlane"},
		{ProviderKey{ProviderTypeControlPlane, "talos"}, "ControlPlaneTemplate", "controlplane.cluster.x-k8s.io/v1beta1, Kind=TalosControlPlaneTemplate"},
		{ProviderKey{ProviderTypeBootstrap, "talos"}, "ConfigTemplate", "bootstrap.cluster.x-k8s.io/v1beta1, Kind=TalosConfigTemplate"},
		{ProviderKey{ProviderTypeCore, "cluster-api"}, "Cluster", "cluster.x-k8s.io/v1beta2, Kind=Cluster"},
		{ProviderKey{ProviderTypeCore, "cluster-api"}, "MachineDeployment", "cluster.x-k8s.io/v1beta2, Kind=MachineDeployment"},
	}
	for _, tc := range cases {
		got, err := c.Resolve(tc.key, tc.suffix)
		if err != nil {
			t.Errorf("Resolve(%v, %q): %v", tc.key, tc.suffix, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("Resolve(%v, %q) = %s, want %s", tc.key, tc.suffix, got, tc.want)
		}
	}
}

func TestKindCatalog_Resolve_ContractLabelWinsOverStorageVersion(t *testing.T) {
	c := NewKindCatalog(append(loadTestCRDs(t, "core.yaml"),
		crdYAML(t, "infrastructure-foo", "FooCluster", "v1beta9", "v1alpha1*", "v1beta9")))
	got, err := c.Resolve(ProviderKey{ProviderTypeInfrastructure, "foo"}, "Cluster")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v1beta9" {
		t.Errorf("version = %q, want the contract-labelled v1beta9 over storage v1alpha1", got.Version)
	}
}

func TestKindCatalog_Resolve_ContractLabelListPicksNewestServed(t *testing.T) {
	c := NewKindCatalog(append(loadTestCRDs(t, "core.yaml"),
		crdYAML(t, "infrastructure-foo", "FooCluster", "v1alpha1_v1beta2_v1beta1", "v1alpha1*", "v1beta1", "v1beta2")))
	got, err := c.Resolve(ProviderKey{ProviderTypeInfrastructure, "foo"}, "Cluster")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v1beta2" {
		t.Errorf("version = %q, want newest served v1beta2 from the _-separated label", got.Version)
	}
}

func TestKindCatalog_Resolve_FallsBackToStorageVersionWithoutContractLabel(t *testing.T) {
	c := NewKindCatalog(append(loadTestCRDs(t, "core.yaml"),
		crdYAML(t, "infrastructure-foo", "FooCluster", "", "v1alpha1", "v1beta1*")))
	got, err := c.Resolve(ProviderKey{ProviderTypeInfrastructure, "foo"}, "Cluster")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v1beta1" {
		t.Errorf("version = %q, want storage v1beta1", got.Version)
	}
}

func TestKindCatalog_Resolve_ShortestKindWins(t *testing.T) {
	c := NewKindCatalog(append(loadTestCRDs(t, "core.yaml"),
		crdYAML(t, "infrastructure-aws", "AWSManagedCluster", "v1beta2", "v1beta2*"),
		crdYAML(t, "infrastructure-aws", "AWSCluster", "v1beta2", "v1beta2*")))
	got, err := c.Resolve(ProviderKey{ProviderTypeInfrastructure, "aws"}, "Cluster")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "AWSCluster" {
		t.Errorf("kind = %q, want the shortest match AWSCluster", got.Kind)
	}
}

func TestKindCatalog_Resolve_AmbiguousKindsListCandidates(t *testing.T) {
	c := NewKindCatalog(append(loadTestCRDs(t, "core.yaml"),
		crdYAML(t, "infrastructure-foo", "FooCluster", "v1beta2", "v1beta2*"),
		crdYAML(t, "infrastructure-foo", "BarCluster", "v1beta2", "v1beta2*")))
	_, err := c.Resolve(ProviderKey{ProviderTypeInfrastructure, "foo"}, "Cluster")
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	for _, want := range []string{"BarCluster", "FooCluster", "ambiguous"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestKindCatalog_Resolve_NotFoundNamesLabelAndSuffix(t *testing.T) {
	c := talosTinkerbellCatalog(t)
	_, err := c.Resolve(ProviderKey{ProviderTypeInfrastructure, "docker"}, "Cluster")
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	for _, want := range []string{"infrastructure-docker", "Cluster"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	if !errors.Is(err, ErrKindNotFound) {
		t.Errorf("error should wrap ErrKindNotFound, got %v", err)
	}
}

func TestKindCatalog_ControlPlaneRefShape(t *testing.T) {
	c := NewKindCatalog(loadTestCRDs(t, "core.yaml", "cacppt.yaml", "docker-kubeadm.yaml"))
	// Only concrete control-plane kinds carry the ref: a *ControlPlaneTemplate
	// leaves it to the topology controller, so the generator never asks.
	cases := map[string]RefShape{
		"controlplane.cluster.x-k8s.io/v1beta1, Kind=TalosControlPlane":   RefShapeContract,
		"controlplane.cluster.x-k8s.io/v1beta2, Kind=KubeadmControlPlane": RefShapeContract,
		"controlplane.cluster.x-k8s.io/v1beta1, Kind=KubeadmControlPlane": RefShapeLegacy,
	}
	for gvkStr, want := range cases {
		gvk := parseGVK(t, gvkStr)
		got, err := c.ControlPlaneRefShape(gvk)
		if err != nil {
			t.Errorf("ControlPlaneRefShape(%s): %v", gvkStr, err)
			continue
		}
		if got != want {
			t.Errorf("ControlPlaneRefShape(%s) = %q, want %q", gvkStr, got, want)
		}
	}
}

func TestKindCatalog_ControlPlaneRefShape_UnknownKind(t *testing.T) {
	c := NewKindCatalog(loadTestCRDs(t, "core.yaml"))
	_, err := c.ControlPlaneRefShape(parseGVK(t, "controlplane.cluster.x-k8s.io/v1beta1, Kind=NopeControlPlane"))
	if err == nil {
		t.Fatal("expected an error for a kind the catalog does not hold")
	}
}
