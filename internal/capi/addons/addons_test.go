// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package addons

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

func (f *fakeHelm) key(rel capi.HelmRelease) string { return rel.Namespace + "/" + rel.Name }

func (f *fakeHelm) Exists(_ context.Context, _ string, rel capi.HelmRelease) (bool, error) {
	return f.existing[f.key(rel)], nil
}

func (f *fakeHelm) Install(_ context.Context, _ string, rel capi.HelmRelease) error {
	f.calls = append(f.calls, "install:"+f.key(rel))
	if f.order != nil {
		*f.order = append(*f.order, "install:"+f.key(rel))
	}
	if f.failOn == rel.Name {
		return errors.New("chart pull failed")
	}
	return nil
}

func (f *fakeHelm) Upgrade(_ context.Context, _ string, rel capi.HelmRelease) error {
	f.calls = append(f.calls, "upgrade:"+f.key(rel))
	return nil
}

func (f *fakeHelm) Uninstall(_ context.Context, _ string, rel capi.HelmRelease) error {
	f.calls = append(f.calls, "uninstall:"+f.key(rel))
	if f.failOn == rel.Name {
		return errors.New("release stuck")
	}
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

var testCluster = &capi.Cluster{Name: "bootstrap", KubeconfigPath: "/tmp/kc"}

func TestInstaller_OrderAndInstallVsUpgrade(t *testing.T) {
	helm := &fakeHelm{existing: map[string]bool{"kube-system/cilium": true}}
	applier := &fakeApplier{}
	inst := New(helm, applier, nil)

	err := inst.Install(context.Background(), testCluster, capi.AddonSet{
		Helm: []capi.HelmRelease{
			{Name: "cilium", Namespace: "kube-system", Chart: "oci://quay.io/cilium/charts/cilium"},
			{
				Name:       "metrics",
				Namespace:  "monitoring",
				Chart:      "metrics-server",
				Repository: "https://charts.example",
			},
		},
		Manifests: []string{
			"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n",
			"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: b\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(
		helm.calls,
		",",
	); got != "upgrade:kube-system/cilium,install:monitoring/metrics" {
		t.Fatalf("helm calls = %q", got)
	}
	if len(applier.manifests) != 2 || !strings.Contains(applier.manifests[1], "name: b") {
		t.Fatalf("manifests = %v", applier.manifests)
	}
}

func TestInstaller_HelmFailureWrapsErrInstall(t *testing.T) {
	helm := &fakeHelm{existing: map[string]bool{}, failOn: "cilium"}
	inst := New(helm, &fakeApplier{}, nil)
	err := inst.Install(
		context.Background(),
		testCluster,
		capi.AddonSet{
			Helm: []capi.HelmRelease{{Name: "cilium", Namespace: "kube-system", Chart: "x"}},
		},
	)
	if !errors.Is(err, ErrInstall) {
		t.Fatalf("error = %v, want ErrInstall", err)
	}
	if !strings.Contains(err.Error(), "cilium") {
		t.Fatalf("error %q should name the release", err)
	}
}

func TestInstaller_ManifestFailureWrapsErrInstall(t *testing.T) {
	inst := New(
		&fakeHelm{existing: map[string]bool{}},
		&fakeApplier{err: errors.New("apply failed")},
		nil,
	)
	err := inst.Install(
		context.Background(),
		testCluster,
		capi.AddonSet{Manifests: []string{"kind: X"}},
	)
	if !errors.Is(err, ErrInstall) {
		t.Fatalf("error = %v, want ErrInstall", err)
	}
}

func TestInstaller_PreHelmManifestsAppliedBeforeHelm(t *testing.T) {
	order := []string{}
	helm := &fakeHelm{existing: map[string]bool{}, order: &order}
	applier := &fakeApplier{order: &order}
	inst := New(helm, applier, nil)
	fetched := ""
	inst.httpGet = func(_ context.Context, url string) ([]byte, error) {
		fetched = url
		return []byte(
			"apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: gatewayclasses.gateway.networking.k8s.io\n",
		), nil
	}

	err := inst.Install(context.Background(), testCluster, capi.AddonSet{
		PreHelmManifestURLs: []string{"https://example.test/standard-install.yaml"},
		Helm: []capi.HelmRelease{
			{Name: "cilium", Namespace: "kube-system", Chart: "oci://quay.io/cilium/charts/cilium"},
		},
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

func TestInstaller_PrerequisitesThenHelmThenManifests(t *testing.T) {
	order := []string{}
	helm := &fakeHelm{existing: map[string]bool{}, order: &order}
	applier := &fakeApplier{order: &order}
	inst := New(helm, applier, nil)

	err := inst.Install(context.Background(), testCluster, capi.AddonSet{
		Prerequisites: []string{"kind: CiliumLoadBalancerIPPool"},
		Helm: []capi.HelmRelease{
			{
				Name:      "tinkerbell",
				Namespace: "tinkerbell",
				Chart:     "oci://ghcr.io/tinkerbell/charts/tinkerbell",
			},
		},
		Manifests: []string{"kind: Namespace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "apply:kind: CiliumLoadBalancerIPPool,install:tinkerbell/tinkerbell,apply:kind: Namespace"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order = %q\nwant    %q", got, want)
	}
}

func TestInstaller_PreHelmFetchErrorWrapsErrInstall(t *testing.T) {
	inst := New(&fakeHelm{existing: map[string]bool{}}, &fakeApplier{}, nil)
	inst.httpGet = func(_ context.Context, _ string) ([]byte, error) { return nil, errors.New("boom") }
	err := inst.Install(
		context.Background(),
		testCluster,
		capi.AddonSet{PreHelmManifestURLs: []string{"https://x/y.yaml"}},
	)
	if !errors.Is(err, ErrInstall) {
		t.Fatalf("error = %v, want ErrInstall", err)
	}
}

func TestInstaller_UninstallReversesReleasesAndLeavesManifests(t *testing.T) {
	helm := &fakeHelm{existing: map[string]bool{}}
	applier := &fakeApplier{}
	inst := New(helm, applier, nil)
	err := inst.Uninstall(context.Background(), testCluster, capi.AddonSet{
		Helm: []capi.HelmRelease{
			{Name: "rufio", Namespace: "tink"},
			{Name: "tinkerbell", Namespace: "tink"},
		},
		Manifests: []string{"kind: Namespace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(
		helm.calls,
		",",
	); got != "uninstall:tink/tinkerbell,uninstall:tink/rufio" {
		t.Fatalf("helm calls = %q", got)
	}
	if len(applier.manifests) != 0 {
		t.Fatal("uninstall must not touch raw manifests")
	}

	helm = &fakeHelm{failOn: "tinkerbell"}
	err = New(
		helm,
		applier,
		nil,
	).Uninstall(context.Background(), testCluster, capi.AddonSet{Helm: []capi.HelmRelease{{Name: "tinkerbell", Namespace: "tink"}}})
	if !errors.Is(err, ErrUninstall) || !strings.Contains(err.Error(), "tinkerbell") {
		t.Fatalf("error = %v, want ErrUninstall naming the release", err)
	}
}
