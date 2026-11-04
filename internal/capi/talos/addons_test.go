// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

type fakeHelm struct {
	existing map[string]bool
	calls    []string
	failOn   string
	order    *[]string
}

func (f *fakeHelm) key(rel HelmRelease) string { return rel.Namespace + "/" + rel.Name }

func (f *fakeHelm) Exists(_ context.Context, _ string, rel HelmRelease) (bool, error) {
	return f.existing[f.key(rel)], nil
}

func (f *fakeHelm) Install(_ context.Context, _ string, rel HelmRelease) error {
	f.calls = append(f.calls, "install:"+f.key(rel))
	if f.order != nil {
		*f.order = append(*f.order, "install:"+f.key(rel))
	}
	if f.failOn == rel.Name {
		return errors.New("chart pull failed")
	}
	return nil
}

func (f *fakeHelm) Upgrade(_ context.Context, _ string, rel HelmRelease) error {
	f.calls = append(f.calls, "upgrade:"+f.key(rel))
	return nil
}

type fakeApplier struct {
	manifests []string
	err       error
	order     *[]string
}

func (f *fakeApplier) Apply(_ context.Context, _ *capi.Cluster, manifest []byte) error {
	f.manifests = append(f.manifests, string(manifest))
	if f.order != nil {
		*f.order = append(*f.order, "apply:"+string(manifest))
	}
	return f.err
}

func (f *fakeApplier) Delete(context.Context, *capi.Cluster, string, string) error { return nil }

func TestAddonInstaller_OrderAndInstallVsUpgrade(t *testing.T) {
	helm := &fakeHelm{existing: map[string]bool{"kube-system/cilium": true}}
	applier := &fakeApplier{}
	inst := NewAddonInstaller(helm, applier, nil)

	err := inst.Install(context.Background(), "/tmp/kc", Addons{
		Helm: []HelmRelease{
			{Name: "cilium", Namespace: "kube-system", Chart: "oci://quay.io/cilium/charts/cilium"},
			{Name: "metrics", Namespace: "monitoring", Chart: "metrics-server", Repository: "https://charts.example"},
		},
		Manifests: []string{"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: b\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(helm.calls, ","); got != "upgrade:kube-system/cilium,install:monitoring/metrics" {
		t.Fatalf("helm calls = %q", got)
	}
	if len(applier.manifests) != 2 || !strings.Contains(applier.manifests[1], "name: b") {
		t.Fatalf("manifests = %v", applier.manifests)
	}
}

func TestAddonInstaller_HelmFailureWrapsErrAddons(t *testing.T) {
	helm := &fakeHelm{existing: map[string]bool{}, failOn: "cilium"}
	inst := NewAddonInstaller(helm, &fakeApplier{}, nil)
	err := inst.Install(context.Background(), "/tmp/kc", Addons{Helm: []HelmRelease{{Name: "cilium", Namespace: "kube-system", Chart: "x"}}})
	if !errors.Is(err, ErrAddons) {
		t.Fatalf("error = %v, want ErrAddons", err)
	}
	if !strings.Contains(err.Error(), "cilium") {
		t.Fatalf("error %q should name the release", err)
	}
}

func TestAddonInstaller_ManifestFailureWrapsErrAddons(t *testing.T) {
	inst := NewAddonInstaller(&fakeHelm{existing: map[string]bool{}}, &fakeApplier{err: errors.New("apply failed")}, nil)
	err := inst.Install(context.Background(), "/tmp/kc", Addons{Manifests: []string{"kind: X"}})
	if !errors.Is(err, ErrAddons) {
		t.Fatalf("error = %v, want ErrAddons", err)
	}
}

func TestAddons_Empty(t *testing.T) {
	if !(Addons{}).Empty() {
		t.Fatal("zero Addons should be empty")
	}
	if (Addons{Manifests: []string{"x"}}).Empty() {
		t.Fatal("Addons with manifests should not be empty")
	}
}

func TestAddonInstaller_PreHelmManifestsAppliedBeforeHelm(t *testing.T) {
	order := []string{}
	helm := &fakeHelm{existing: map[string]bool{}, order: &order}
	applier := &fakeApplier{order: &order}
	inst := NewAddonInstaller(helm, applier, nil)
	fetched := ""
	inst.httpGet = func(_ context.Context, url string) ([]byte, error) {
		fetched = url
		return []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: gatewayclasses.gateway.networking.k8s.io\n"), nil
	}

	err := inst.Install(context.Background(), "/tmp/kc", Addons{
		PreHelmManifestURLs: []string{"https://example.test/standard-install.yaml"},
		Helm:                []HelmRelease{{Name: "cilium", Namespace: "kube-system", Chart: "oci://quay.io/cilium/charts/cilium"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fetched != "https://example.test/standard-install.yaml" {
		t.Errorf("pre-helm URL not fetched: %q", fetched)
	}
	if len(order) != 2 || order[0][:6] != "apply:" || order[1] != "install:kube-system/cilium" {
		t.Fatalf("order = %v; CRDs must apply before the cilium helm install", order)
	}
}

func TestAddonInstaller_CNIThenPrerequisitesThenHelm(t *testing.T) {
	order := []string{}
	helm := &fakeHelm{existing: map[string]bool{}, order: &order}
	applier := &fakeApplier{order: &order}
	inst := NewAddonInstaller(helm, applier, nil)

	err := inst.Install(context.Background(), "/tmp/kc", Addons{
		CNI:           []HelmRelease{{Name: "cilium", Namespace: "kube-system", Chart: "oci://quay.io/cilium/charts/cilium"}},
		Prerequisites: []string{"kind: CiliumLoadBalancerIPPool"},
		Helm:          []HelmRelease{{Name: "tinkerbell", Namespace: "tinkerbell", Chart: "oci://ghcr.io/tinkerbell/charts/tinkerbell"}},
		Manifests:     []string{"kind: Namespace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "install:kube-system/cilium,apply:kind: CiliumLoadBalancerIPPool,install:tinkerbell/tinkerbell,apply:kind: Namespace"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order = %q\nwant    %q", got, want)
	}
	if (Addons{CNI: []HelmRelease{{Name: "cilium"}}}).Empty() || (Addons{Prerequisites: []string{"x"}}).Empty() {
		t.Fatal("CNI releases and prerequisites count as addons")
	}
}

func TestAddonInstaller_PreHelmFetchErrorWrapsErrAddons(t *testing.T) {
	inst := NewAddonInstaller(&fakeHelm{existing: map[string]bool{}}, &fakeApplier{}, nil)
	inst.httpGet = func(_ context.Context, _ string) ([]byte, error) { return nil, errors.New("boom") }
	err := inst.Install(context.Background(), "/tmp/kc", Addons{PreHelmManifestURLs: []string{"https://x/y.yaml"}})
	if !errors.Is(err, ErrAddons) {
		t.Fatalf("error = %v, want ErrAddons", err)
	}
}
