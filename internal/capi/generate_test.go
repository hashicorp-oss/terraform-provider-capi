// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBuildSpec_MergesNestedMapsReplacesListsDeletesNull(t *testing.T) {
	defaults := map[string]any{
		"controlPlaneConfig": map[string]any{
			"controlplane": map[string]any{"generateType": "controlplane", "keep": true},
		},
		"rolloutStrategy": map[string]any{"type": "RollingUpdate"},
		"patches":         []any{"a", "b"},
	}
	rc := map[string]any{
		"controlPlaneConfig": map[string]any{
			"controlplane": map[string]any{"talosVersion": "v1.13"},
		},
		"rolloutStrategy": nil,
		"patches":         []any{"c"},
	}

	got := buildSpec(defaults, rc)

	want := map[string]any{
		"controlPlaneConfig": map[string]any{
			"controlplane": map[string]any{"generateType": "controlplane", "keep": true, "talosVersion": "v1.13"},
		},
		"patches": []any{"c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildSpec = %#v\nwant %#v", got, want)
	}
	if _, still := defaults["rolloutStrategy"]; !still {
		t.Error("buildSpec mutated its defaults argument")
	}
}

func TestBuildSpec_NilInputsYieldEmptyObject(t *testing.T) {
	got := buildSpec(nil, nil)
	if got == nil || len(got) != 0 {
		t.Errorf("buildSpec(nil, nil) = %#v, want an empty, non-nil map", got)
	}
}

func TestBuildSpec_IntegralNumbersStayIntegers(t *testing.T) {
	got := buildSpec(nil, map[string]any{"replicas": int64(3), "ratio": 1.5})
	if v, ok := got["replicas"].(int64); !ok || v != 3 {
		t.Errorf("replicas = %#v (%T), want int64 3", got["replicas"], got["replicas"])
	}
	if v, ok := got["ratio"].(float64); !ok || v != 1.5 {
		t.Errorf("ratio = %#v (%T), want float64 1.5", got["ratio"], got["ratio"])
	}
}

func TestProviderDefaults(t *testing.T) {
	cases := []struct {
		key  ProviderKey
		want map[string]any
	}{
		{ProviderKey{ProviderTypeControlPlane, "talos"}, map[string]any{"controlPlaneConfig": map[string]any{"controlplane": map[string]any{"generateType": "controlplane"}}}},
		{ProviderKey{ProviderTypeBootstrap, "talos"}, map[string]any{"generateType": "worker"}},
		{ProviderKey{ProviderTypeControlPlane, "kubeadm"}, map[string]any{"kubeadmConfigSpec": map[string]any{}}},
		{ProviderKey{ProviderTypeBootstrap, "kubeadm"}, nil},
		{ProviderKey{ProviderTypeInfrastructure, "tinkerbell"}, nil},
		{ProviderKey{ProviderTypeInfrastructure, "docker"}, nil},
	}
	for _, tc := range cases {
		if got := providerDefaults(tc.key); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("providerDefaults(%v) = %#v, want %#v", tc.key, got, tc.want)
		}
	}
}

// staticKindResolver serves a fixed catalog regardless of the management cluster.
type staticKindResolver struct{ catalog *KindCatalog }

func (s staticKindResolver) Kinds(context.Context, *Cluster) (*KindCatalog, error) {
	return s.catalog, nil
}

func generatorFor(t *testing.T, crdFiles ...string) *CatalogManifestGenerator {
	t.Helper()
	return NewManifestGenerator(staticKindResolver{NewKindCatalog(loadTestCRDs(t, crdFiles...))})
}

// assertManifestMatchesGolden compares the generated multi-document YAML with
// testdata/golden/<name>.yaml object by object, in order, ignoring formatting.
func assertManifestMatchesGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", "golden", name+".yaml"))
	if err != nil {
		t.Fatalf("reading golden: %v", err)
	}
	gotObjs, err := parseMultiDocYAML(got)
	if err != nil {
		t.Fatalf("parsing generated manifest: %v\n%s", err, got)
	}
	wantObjs, err := parseMultiDocYAML(want)
	if err != nil {
		t.Fatalf("parsing golden: %v", err)
	}
	if diff := cmp.Diff(wantObjs, gotObjs); diff != "" {
		t.Errorf("manifest differs from golden %s (-want +got):\n%s\n--- generated ---\n%s", name, diff, got)
	}
}

func tinkerbellTalosOptions() GenerateOptions {
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "tinkerbell", Type: ProviderTypeInfrastructure, ResourceConfig: map[string]any{
		"templateInline": "version: \"0.1\"\nname: talos\n",
	}})
	providers.Add(ProviderConfig{Name: "talos", Type: ProviderTypeControlPlane, ResourceConfig: map[string]any{
		"controlPlaneConfig": map[string]any{
			"controlplane": map[string]any{
				"talosVersion":     "v1.13",
				"strategicPatches": []any{"machine:\n  install:\n    disk: /dev/sda\n"},
			},
		},
		"rolloutStrategy": map[string]any{"type": "OnDelete"},
	}})
	providers.Add(ProviderConfig{Name: "talos", Type: ProviderTypeBootstrap, ResourceConfig: map[string]any{
		"talosVersion": "v1.13",
	}})
	return GenerateOptions{
		ClusterName:       "acc",
		Namespace:         "capi",
		KubernetesVersion: "v1.34.0",
		Providers:         providers,
		Network: NetworkConfig{
			ControlPlaneEndpoint: &APIEndpoint{Host: "10.0.0.160", Port: 6443},
			PodCIDRs:             []string{"10.244.0.0/16"},
			ServiceCIDRs:         []string{"10.96.0.0/12"},
			ServiceDomain:        "cluster.local",
		},
		ControlPlane: ControlPlaneTopology{
			Replicas: ptrInt64(1),
			Infrastructure: map[string]any{
				"hardwareAffinity": map[string]any{"required": []any{map[string]any{"labelSelector": map[string]any{"matchLabels": map[string]any{"type": "cp"}}}}},
			},
		},
		MachineDeployments: []MachineDeploymentTopology{
			{
				Name: "md-0", Replicas: ptrInt64(2), FailureDomain: "rack-1",
				Labels: map[string]string{"tier": "worker"}, Annotations: map[string]string{"note": "x"},
				Infrastructure: map[string]any{
					"hardwareAffinity": map[string]any{"required": []any{map[string]any{"labelSelector": map[string]any{"matchLabels": map[string]any{"type": "worker"}}}}},
				},
			},
			{
				Name: "gpu", Replicas: ptrInt64(1),
				Bootstrap: map[string]any{"generateType": "worker", "talosVersion": "v1.14"},
			},
		},
	}
}

func TestGenerate_Direct_TinkerbellTalos(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml")
	got, err := gen.Generate(context.Background(), &Cluster{Name: "mgmt"}, tinkerbellTalosOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertManifestMatchesGolden(t, "direct-tinkerbell-talos", got)
}

func TestGenerate_Direct_DockerKubeadmMinimal(t *testing.T) {
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "docker", Type: ProviderTypeInfrastructure})
	providers.Add(ProviderConfig{Name: "kubeadm", Type: ProviderTypeControlPlane})
	providers.Add(ProviderConfig{Name: "kubeadm", Type: ProviderTypeBootstrap})
	gen := generatorFor(t, "core.yaml", "docker-kubeadm.yaml")
	got, err := gen.Generate(context.Background(), &Cluster{Name: "mgmt"}, GenerateOptions{
		ClusterName: "dev", Namespace: "default", KubernetesVersion: "v1.31.0", Providers: providers,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertManifestMatchesGolden(t, "direct-docker-kubeadm", got)
}

func TestGenerate_Direct_LegacyV1beta1Contract(t *testing.T) {
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "docker", Type: ProviderTypeInfrastructure})
	providers.Add(ProviderConfig{Name: "kubeadm", Type: ProviderTypeControlPlane})
	providers.Add(ProviderConfig{Name: "kubeadm", Type: ProviderTypeBootstrap})
	gen := generatorFor(t, "legacy-v1beta1.yaml")
	got, err := gen.Generate(context.Background(), &Cluster{Name: "mgmt"}, GenerateOptions{
		ClusterName: "old", Namespace: "default", KubernetesVersion: "v1.29.0", Providers: providers,
		Network:            NetworkConfig{ControlPlaneEndpoint: &APIEndpoint{Host: "192.168.1.100"}},
		MachineDeployments: []MachineDeploymentTopology{{Name: "md-0", Replicas: ptrInt64(1)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertManifestMatchesGolden(t, "direct-legacy-v1beta1", got)
}

func TestGenerate_Direct_TemplateRotationFollowsSpecChanges(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml")
	first, err := gen.Generate(context.Background(), &Cluster{}, tinkerbellTalosOptions())
	if err != nil {
		t.Fatal(err)
	}
	again, err := gen.Generate(context.Background(), &Cluster{}, tinkerbellTalosOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Fatal("generation is not deterministic for identical input")
	}

	changed := tinkerbellTalosOptions()
	changed.ControlPlane.Infrastructure = map[string]any{"hardwareName": "cp-1"}
	rotated, err := gen.Generate(context.Background(), &Cluster{}, changed)
	if err != nil {
		t.Fatal(err)
	}
	before := findObject(t, first, "TalosControlPlane", "acc-control-plane")
	after := findObject(t, rotated, "TalosControlPlane", "acc-control-plane")
	refBefore, _, _ := unstructured.NestedString(before.Object, "spec", "machineTemplate", "spec", "infrastructureRef", "name")
	refAfter, _, _ := unstructured.NestedString(after.Object, "spec", "machineTemplate", "spec", "infrastructureRef", "name")
	if refBefore == refAfter {
		t.Errorf("control-plane machine template ref did not rotate: %q", refAfter)
	}
	if findObject(t, rotated, "TinkerbellMachineTemplate", refAfter) == nil {
		t.Errorf("rotated manifest lacks the machine template %q the control plane references", refAfter)
	}
	if o := findObject(t, rotated, "TinkerbellMachineTemplate", refBefore); o != nil {
		t.Errorf("stale machine template %q is still emitted", refBefore)
	}
}

func TestGenerate_Direct_RequiresControlPlaneProvider(t *testing.T) {
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "docker", Type: ProviderTypeInfrastructure})
	gen := generatorFor(t, "core.yaml", "docker-kubeadm.yaml")
	_, err := gen.Generate(context.Background(), &Cluster{}, GenerateOptions{ClusterName: "dev", Namespace: "default", Providers: providers})
	if err == nil || !strings.Contains(err.Error(), "control_plane") {
		t.Fatalf("expected an error naming the missing control_plane provider, got %v", err)
	}
}

func TestGenerate_Direct_RequiresBootstrapProviderForMachineDeployments(t *testing.T) {
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "docker", Type: ProviderTypeInfrastructure})
	providers.Add(ProviderConfig{Name: "kubeadm", Type: ProviderTypeControlPlane})
	gen := generatorFor(t, "core.yaml", "docker-kubeadm.yaml")
	_, err := gen.Generate(context.Background(), &Cluster{}, GenerateOptions{
		ClusterName: "dev", Namespace: "default", Providers: providers,
		MachineDeployments: []MachineDeploymentTopology{{Name: "md-0"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("expected an error naming the missing bootstrap provider, got %v", err)
	}
}

func TestGenerate_Direct_MissingKindWrapsErrKindNotFound(t *testing.T) {
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "docker", Type: ProviderTypeInfrastructure})
	providers.Add(ProviderConfig{Name: "talos", Type: ProviderTypeControlPlane})
	gen := generatorFor(t, "core.yaml", "cacppt.yaml") // no docker CRDs installed
	_, err := gen.Generate(context.Background(), &Cluster{}, GenerateOptions{ClusterName: "dev", Namespace: "default", Providers: providers})
	if !errors.Is(err, ErrKindNotFound) {
		t.Fatalf("expected ErrKindNotFound, got %v", err)
	}
}

// findObject returns the object with the given kind and name, or nil.
func findObject(t *testing.T, manifest []byte, kind, name string) *unstructured.Unstructured {
	t.Helper()
	objs, err := parseMultiDocYAML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for i := range objs {
		if objs[i].GetKind() == kind && objs[i].GetName() == name {
			return &objs[i]
		}
	}
	return nil
}

func TestGenerate_ClusterClass_TinkerbellTalos(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml")
	opts := tinkerbellTalosOptions()
	opts.ClusterClass = true
	got, err := gen.Generate(context.Background(), &Cluster{Name: "mgmt"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertManifestMatchesGolden(t, "clusterclass-tinkerbell-talos", got)
}

func TestGenerate_ClusterClass_SharedClassEmitsOneTemplateSet(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml")
	opts := tinkerbellTalosOptions()
	opts.ClusterClass = true
	shared := map[string]any{"hardwareName": "shared"}
	opts.MachineDeployments = []MachineDeploymentTopology{
		{Name: "md-a", Class: "worker", Replicas: ptrInt64(1), Infrastructure: shared},
		{Name: "md-b", Class: "worker", Replicas: ptrInt64(2), Infrastructure: shared},
	}
	got, err := gen.Generate(context.Background(), &Cluster{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := parseMultiDocYAML(got)
	if err != nil {
		t.Fatal(err)
	}
	var machineTemplates []string
	for _, o := range objs {
		if o.GetKind() == "TinkerbellMachineTemplate" && strings.HasPrefix(o.GetName(), "acc-worker-") {
			machineTemplates = append(machineTemplates, o.GetName())
		}
	}
	if len(machineTemplates) != 1 {
		t.Errorf("want one machine template for class worker, got %v", machineTemplates)
	}
	cc := findObject(t, got, "ClusterClass", "acc")
	cluster := findObject(t, got, "Cluster", "acc")
	if cc == nil || cluster == nil {
		t.Fatalf("manifest lacks the ClusterClass or Cluster:\n%s", got)
	}
	classes, _, _ := unstructured.NestedSlice(cc.Object, "spec", "workers", "machineDeployments")
	if len(classes) != 1 {
		t.Errorf("want one worker class, got %d", len(classes))
	}
	mds, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "topology", "workers", "machineDeployments")
	if len(mds) != 2 {
		t.Errorf("want two topology machine deployments, got %d", len(mds))
	}
}

func TestGenerate_ClusterClass_ConflictingSharedClassFails(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml")
	opts := tinkerbellTalosOptions()
	opts.ClusterClass = true
	opts.MachineDeployments = []MachineDeploymentTopology{
		{Name: "md-a", Class: "worker", Infrastructure: map[string]any{"hardwareName": "a"}},
		{Name: "md-b", Class: "worker", Infrastructure: map[string]any{"hardwareName": "b"}},
	}
	_, err := gen.Generate(context.Background(), &Cluster{}, opts)
	if err == nil || !strings.Contains(err.Error(), `class "worker"`) {
		t.Fatalf("expected an error naming the conflicting class, got %v", err)
	}
}

func TestGenerate_ClusterClass_ProviderWithoutTemplateKindFails(t *testing.T) {
	var crds []apiextv1.CustomResourceDefinition
	for _, crd := range loadTestCRDs(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml") {
		if crd.Spec.Names.Kind != "TinkerbellClusterTemplate" {
			crds = append(crds, crd)
		}
	}
	gen := NewManifestGenerator(staticKindResolver{NewKindCatalog(crds)})
	opts := tinkerbellTalosOptions()
	opts.ClusterClass = true
	_, err := gen.Generate(context.Background(), &Cluster{}, opts)
	if !errors.Is(err, ErrKindNotFound) || !strings.Contains(err.Error(), "ClusterTemplate") {
		t.Fatalf("expected ErrKindNotFound naming ClusterTemplate, got %v", err)
	}
}

func TestGenerate_ClusterClass_RequiresKubernetesVersion(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml")
	opts := tinkerbellTalosOptions()
	opts.ClusterClass = true
	opts.KubernetesVersion = ""
	_, err := gen.Generate(context.Background(), &Cluster{}, opts)
	if err == nil || !strings.Contains(err.Error(), "kubernetes_version") {
		t.Fatalf("expected an error about kubernetes_version, got %v", err)
	}
}

func TestGenerate_ClusterClass_RequiresV1beta2Core(t *testing.T) {
	gen := generatorFor(t, "legacy-v1beta1.yaml")
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "docker", Type: ProviderTypeInfrastructure})
	providers.Add(ProviderConfig{Name: "kubeadm", Type: ProviderTypeControlPlane})
	_, err := gen.Generate(context.Background(), &Cluster{}, GenerateOptions{
		ClusterName: "old", Namespace: "default", KubernetesVersion: "v1.29.0", Providers: providers, ClusterClass: true,
	})
	if err == nil || !strings.Contains(err.Error(), "v1beta2") {
		t.Fatalf("expected an error about the v1beta2 core contract, got %v", err)
	}
}
