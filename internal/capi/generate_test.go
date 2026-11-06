// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
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
			"controlplane": map[string]any{
				"generateType": "controlplane",
				"keep":         true,
				"talosVersion": "v1.13",
			},
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
		{
			ProviderKey{ProviderTypeControlPlane, "talos"},
			map[string]any{
				"controlPlaneConfig": map[string]any{
					"controlplane": map[string]any{"generateType": "controlplane"},
				},
			},
		},
		{ProviderKey{ProviderTypeBootstrap, "talos"}, map[string]any{"generateType": "worker"}},
		{
			ProviderKey{ProviderTypeControlPlane, "kubeadm"},
			map[string]any{"kubeadmConfigSpec": map[string]any{}},
		},
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
	path := filepath.Join("testdata", "golden", name+".yaml")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
	}
	want, err := os.ReadFile(path)
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
		t.Errorf(
			"manifest differs from golden %s (-want +got):\n%s\n--- generated ---\n%s",
			name,
			diff,
			got,
		)
	}
}

func tinkerbellTalosOptions() GenerateOptions {
	providers := ProviderSet{}
	providers.Add(
		ProviderConfig{
			Name: "tinkerbell",
			Type: ProviderTypeInfrastructure,
			ResourceConfig: map[string]any{
				"templateInline": "version: \"0.1\"\nname: talos\n",
			},
		},
	)
	providers.Add(
		ProviderConfig{
			Name: "talos",
			Type: ProviderTypeControlPlane,
			ResourceConfig: map[string]any{
				"controlPlaneConfig": map[string]any{
					"controlplane": map[string]any{
						"talosVersion":     "v1.13",
						"strategicPatches": []any{"machine:\n  install:\n    disk: /dev/sda\n"},
					},
				},
				"rolloutStrategy": map[string]any{"type": "OnDelete"},
			},
		},
	)
	providers.Add(
		ProviderConfig{Name: "talos", Type: ProviderTypeBootstrap, ResourceConfig: map[string]any{
			"talosVersion": "v1.13",
		}},
	)
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
		Infrastructure: InfrastructureSpec{
			Provider: "tinkerbell",
			Spec: map[string]any{
				"imageLookupFormat": "{{.BaseRegistry}}/talos:{{.KubernetesVersion}}",
			},
		},
		ControlPlane: ControlPlaneSpec{
			Provider: "talos",
			Replicas: ptrInt64(1),
			Template: &TemplateRef{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "TinkerbellMachineTemplate",
				Name:       "cp-1a2b3c4d",
			},
			Spec: map[string]any{
				"controlPlaneConfig": map[string]any{
					"controlplane": map[string]any{
						"strategicPatches": []any{"machine:\n  network:\n    hostname: cp\n"},
					},
				},
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
	objs, err := parseMultiDocYAML(got)
	if err != nil {
		t.Fatal(err)
	}
	// Only the cluster's own objects: templates and node groups are other resources.
	var kinds []string
	for _, o := range objs {
		kinds = append(kinds, o.GetKind())
	}
	if strings.Join(kinds, ",") != "Cluster,TinkerbellCluster,TalosControlPlane" {
		t.Errorf("kinds = %v", kinds)
	}
	cp := findObject(t, got, "TalosControlPlane", "acc-control-plane")
	ref, _, _ := unstructured.NestedMap(
		cp.Object,
		"spec",
		"machineTemplate",
		"spec",
		"infrastructureRef",
	)
	if ref["name"] != "cp-1a2b3c4d" || ref["apiGroup"] != "infrastructure.cluster.x-k8s.io" ||
		ref["kind"] != "TinkerbellMachineTemplate" {
		t.Errorf("machine template ref = %v", ref)
	}
	// The cluster's own spec layers over the provider entry's resource_config and the defaults.
	patches, _, _ := unstructured.NestedSlice(
		cp.Object,
		"spec",
		"controlPlaneConfig",
		"controlplane",
		"strategicPatches",
	)
	if len(patches) != 1 || patches[0] != "machine:\n  network:\n    hostname: cp\n" {
		t.Errorf("control_plane.spec must replace the provider entry's list: %v", patches)
	}
	if v, _, _ := unstructured.NestedString(
		cp.Object,
		"spec",
		"controlPlaneConfig",
		"controlplane",
		"talosVersion",
	); v != "v1.13" {
		t.Errorf("the provider entry's resource_config must survive the merge: %q", v)
	}
	if v, _, _ := unstructured.NestedString(
		cp.Object,
		"spec",
		"controlPlaneConfig",
		"controlplane",
		"generateType",
	); v != "controlplane" {
		t.Errorf("provider defaults must survive: %q", v)
	}
	infra := findObject(t, got, "TinkerbellCluster", "acc")
	if v, _, _ := unstructured.NestedString(
		infra.Object,
		"spec",
		"templateInline",
	); !strings.HasPrefix(
		v,
		"version:",
	) {
		t.Errorf("infrastructure resource_config missing: %v", infra.Object["spec"])
	}
	if v, _, _ := unstructured.NestedString(infra.Object, "spec", "imageLookupFormat"); v == "" {
		t.Errorf("infrastructure.spec missing: %v", infra.Object["spec"])
	}
}

// The minimal `infrastructure = { provider = "docker" }` configuration on a
// management cluster whose providers are managed elsewhere: no provider set,
// the control plane defaults to kubeadm.
func TestGenerate_Direct_DockerKubeadmMinimal(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "docker-kubeadm.yaml")
	got, err := gen.Generate(context.Background(), &Cluster{Name: "mgmt"}, GenerateOptions{
		ClusterName:       "dev",
		Namespace:         "default",
		KubernetesVersion: "v1.31.0",
		Infrastructure:    InfrastructureSpec{Provider: "docker"},
		ControlPlane: ControlPlaneSpec{
			Template: &TemplateRef{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "DockerMachineTemplate",
				Name:       "dev-cp-aaaaaaaa",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertManifestMatchesGolden(t, "direct-docker-kubeadm", got)
}

// The infrastructure provider comes from the provider set when the cluster
// names none (a self-managed cluster's management.providers).
func TestGenerate_Direct_InfrastructureFromProviderSet(t *testing.T) {
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "docker", Type: ProviderTypeInfrastructure})
	gen := generatorFor(t, "core.yaml", "docker-kubeadm.yaml")
	got, err := gen.Generate(context.Background(), &Cluster{}, GenerateOptions{
		ClusterName:       "dev",
		Namespace:         "default",
		KubernetesVersion: "v1.31.0",
		Providers:         providers,
		ControlPlane: ControlPlaneSpec{
			Template: &TemplateRef{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "DockerMachineTemplate",
				Name:       "dev-cp-aaaaaaaa",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if findObject(t, got, "DockerCluster", "dev") == nil {
		t.Errorf("expected a DockerCluster:\n%s", got)
	}
}

func TestGenerate_Direct_LegacyV1beta1Contract(t *testing.T) {
	gen := generatorFor(t, "legacy-v1beta1.yaml")
	got, err := gen.Generate(context.Background(), &Cluster{Name: "mgmt"}, GenerateOptions{
		ClusterName:       "old",
		Namespace:         "default",
		KubernetesVersion: "v1.29.0",
		Network:           NetworkConfig{ControlPlaneEndpoint: &APIEndpoint{Host: "192.168.1.100"}},
		Infrastructure:    InfrastructureSpec{Provider: "docker"},
		ControlPlane: ControlPlaneSpec{
			Provider: "kubeadm",
			Replicas: ptrInt64(1),
			Template: &TemplateRef{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "DockerMachineTemplate",
				Name:       "old-cp-bbbbbbbb",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertManifestMatchesGolden(t, "direct-legacy-v1beta1", got)
	cp := findObject(t, got, "KubeadmControlPlane", "old-control-plane")
	ref, _, _ := unstructured.NestedMap(cp.Object, "spec", "machineTemplate", "infrastructureRef")
	if ref["name"] != "old-cp-bbbbbbbb" ||
		ref["apiVersion"] != "infrastructure.cluster.x-k8s.io/v1beta1" ||
		ref["namespace"] != "default" {
		t.Errorf("legacy machine template ref = %v", ref)
	}
}

// A self-managed cluster renders its own control-plane machine template
// from control_plane.machine_spec, hash-named so a spec change rolls the
// control plane, as the generator did before templates became resources.
func TestGenerate_Direct_OwnControlPlaneTemplate(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml")
	opts := tinkerbellTalosOptions()
	opts.ControlPlane.Template = nil
	opts.ControlPlane.TemplateSpec = map[string]any{
		"hardwareAffinity": map[string]any{
			"required": []any{
				map[string]any{
					"labelSelector": map[string]any{"matchLabels": map[string]any{"type": "cp"}},
				},
			},
		},
	}
	got, err := gen.Generate(context.Background(), &Cluster{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertManifestMatchesGolden(t, "direct-own-cp-template", got)
	cp := findObject(t, got, "TalosControlPlane", "acc-control-plane")
	ref, _, _ := unstructured.NestedString(
		cp.Object,
		"spec",
		"machineTemplate",
		"spec",
		"infrastructureRef",
		"name",
	)
	if !strings.HasPrefix(ref, "acc-control-plane-") || len(ref) != len("acc-control-plane-")+8 {
		t.Errorf("own template ref = %q", ref)
	}
	if findObject(t, got, "TinkerbellMachineTemplate", ref) == nil {
		t.Errorf("manifest lacks the machine template %q the control plane references", ref)
	}
	opts.ControlPlane.TemplateSpec = map[string]any{"hardwareName": "cp-1"}
	rotated, _ := gen.Generate(context.Background(), &Cluster{}, opts)
	after := findObject(t, rotated, "TalosControlPlane", "acc-control-plane")
	refAfter, _, _ := unstructured.NestedString(
		after.Object,
		"spec",
		"machineTemplate",
		"spec",
		"infrastructureRef",
		"name",
	)
	if refAfter == ref {
		t.Error("a spec change must rotate the template")
	}
}

func TestGenerate_Direct_RequiresControlPlaneTemplate(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "docker-kubeadm.yaml")
	_, err := gen.Generate(context.Background(), &Cluster{}, GenerateOptions{
		ClusterName:    "dev",
		Namespace:      "default",
		Infrastructure: InfrastructureSpec{Provider: "docker"},
	})
	if err == nil || !strings.Contains(err.Error(), "control_plane.infrastructure") {
		t.Fatalf("expected an error naming control_plane.infrastructure, got %v", err)
	}
	_, err = gen.Generate(
		context.Background(),
		&Cluster{},
		GenerateOptions{ClusterName: "dev", Namespace: "default"},
	)
	if err == nil || !strings.Contains(err.Error(), "no infrastructure provider") {
		t.Fatalf("expected an error about the infrastructure provider, got %v", err)
	}
}

func TestGenerate_Direct_MissingKindWrapsErrKindNotFound(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "cacppt.yaml") // no docker CRDs installed
	_, err := gen.Generate(context.Background(), &Cluster{}, GenerateOptions{
		ClusterName:    "dev",
		Namespace:      "default",
		Infrastructure: InfrastructureSpec{Provider: "docker"},
		ControlPlane: ControlPlaneSpec{
			Provider: "talos",
			Template: &TemplateRef{
				APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				Kind:       "DockerMachineTemplate",
				Name:       "x",
			},
		},
	})
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

func TestGenerate_Topology_ClassRef(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml")
	opts := tinkerbellTalosOptions()
	opts.Class = &ClassRef{Name: "prod", Namespace: "capi"}
	got, err := gen.Generate(context.Background(), &Cluster{Name: "mgmt"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertManifestMatchesGolden(t, "topology-classref", got)
	objs, err := parseMultiDocYAML(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].GetKind() != "Cluster" {
		t.Fatalf("a class cluster is one object: %v", objs)
	}
	topology, _, _ := unstructured.NestedMap(objs[0].Object, "spec", "topology")
	if ref, _, _ := unstructured.NestedMap(
		topology,
		"classRef",
	); ref["name"] != "prod" ||
		ref["namespace"] != nil {
		t.Errorf("classRef = %v (the class namespace equals the cluster's, so it is omitted)", ref)
	}
	if topology["version"] != "v1.34.0" {
		t.Errorf("version = %v", topology["version"])
	}
	if r, _, _ := unstructured.NestedInt64(topology, "controlPlane", "replicas"); r != 1 {
		t.Errorf("replicas = %d", r)
	}
	if _, ok := objs[0].Object["spec"].(map[string]any)["infrastructureRef"]; ok {
		t.Error("a topology cluster has no infrastructureRef")
	}

	// A class in another namespace is named.
	opts.Class = &ClassRef{Name: "prod", Namespace: "classes"}
	got, _ = gen.Generate(context.Background(), &Cluster{}, opts)
	cluster := findObject(t, got, "Cluster", "acc")
	if ns, _, _ := unstructured.NestedString(
		cluster.Object,
		"spec",
		"topology",
		"classRef",
		"namespace",
	); ns != "classes" {
		t.Errorf("classRef.namespace = %q", ns)
	}
}

func TestGenerate_Topology_LegacyClass(t *testing.T) {
	gen := generatorFor(t, "legacy-v1beta1.yaml")
	got, err := gen.Generate(context.Background(), &Cluster{}, GenerateOptions{
		ClusterName:       "old",
		Namespace:         "default",
		KubernetesVersion: "v1.29.0",
		Class: &ClassRef{
			Name:      "main",
			Namespace: "classes",
		},
		ControlPlane: ControlPlaneSpec{Replicas: ptrInt64(3)},
	})
	if err != nil {
		t.Fatal(err)
	}
	cluster := findObject(t, got, "Cluster", "old")
	topology, _, _ := unstructured.NestedMap(cluster.Object, "spec", "topology")
	if topology["class"] != "main" || topology["classNamespace"] != "classes" ||
		topology["version"] != "v1.29.0" {
		t.Errorf("legacy topology = %v", topology)
	}
	if _, ok := topology["classRef"]; ok {
		t.Error("legacy topology uses class, not classRef")
	}
}

func TestGenerate_Topology_RequiresKubernetesVersion(t *testing.T) {
	gen := generatorFor(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml")
	opts := tinkerbellTalosOptions()
	opts.Class = &ClassRef{Name: "prod"}
	opts.KubernetesVersion = ""
	_, err := gen.Generate(context.Background(), &Cluster{}, opts)
	if err == nil || !strings.Contains(err.Error(), "kubernetes_version") {
		t.Fatalf("expected an error about kubernetes_version, got %v", err)
	}
}
