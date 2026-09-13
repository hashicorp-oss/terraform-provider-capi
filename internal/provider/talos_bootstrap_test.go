// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/talos"
)

func TestBuildTalosBootstrapConfig_NilForKind(t *testing.T) {
	ctx := context.Background()
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "kind", "", nil, nil, nil), nil)
	cfg, diags := buildTalosBootstrapConfig(ctx, data)
	if cfg != nil || diags.HasError() {
		t.Fatalf("expected nil config for kind, got %+v, %v", cfg, diags)
	}
	data = talosTestData(ctx, t, types.ObjectNull(managementBootstrapAttrTypes()), nil)
	if cfg, _ := buildTalosBootstrapConfig(ctx, data); cfg != nil {
		t.Fatal("expected nil config for null bootstrap")
	}
}

func TestBuildTalosBootstrapConfig_Full(t *testing.T) {
	ctx := context.Background()
	boot := &BootModel{Method: types.StringValue("http"), Timeout: types.StringValue("20m"), Attempts: types.Int64Value(5)}
	img := testImageModel(ctx, t, "", "", "", []string{"iscsi-tools", "nvme-cli"})
	tm := testTalosModel(ctx, t, img, []string{"a: 1\n", "b: 2\n"})
	tm.Architecture = types.StringValue("arm64")
	tm.Endpoint = types.StringValue("https://10.1.1.100:6443")
	manifests, _ := types.ListValueFrom(ctx, types.StringType, []string{"kind: Namespace\n"})
	addons := &BootstrapAddonsModel{
		Helm:      testHelmList(ctx, t, []HelmReleaseModel{helmRel("cilium", "kube-system", "oci://quay.io/cilium/charts/cilium", "12m", "k: v\n")}),
		Manifests: manifests,
	}
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", boot, tm, addons), goodMachine)

	cfg, diags := buildTalosBootstrapConfig(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if cfg == nil {
		t.Fatal("expected config")
	}
	if cfg.Machine.Hostname != "cp-1" || cfg.Machine.IP != "10.0.0.5" || cfg.Machine.Disk != "/dev/sda" {
		t.Errorf("machine = %+v", cfg.Machine)
	}
	if cfg.Machine.BMC != (talos.BMCCredentials{Address: "10.0.1.5", Username: "admin", Password: "secret"}) {
		t.Errorf("bmc = %+v", cfg.Machine.BMC)
	}
	if cfg.Boot.Method != talos.BootMethodHTTP || cfg.Boot.Timeout != 20*time.Minute || cfg.Boot.Attempts != 5 {
		t.Errorf("boot = %+v", cfg.Boot)
	}
	if cfg.Talos.Version != "v1.13.6" || cfg.Talos.Architecture != "arm64" || cfg.Talos.Endpoint != "https://10.1.1.100:6443" {
		t.Errorf("talos = %+v", cfg.Talos)
	}
	if len(cfg.Talos.Image.Extensions) != 2 || cfg.Talos.Image.Factory != "https://factory.talos.dev" {
		t.Errorf("image = %+v", cfg.Talos.Image)
	}
	if len(cfg.Talos.ConfigPatches) != 2 || cfg.Talos.ConfigPatches[1] != "b: 2\n" {
		t.Errorf("patches = %v", cfg.Talos.ConfigPatches)
	}
	if len(cfg.Addons.Helm) != 1 || cfg.Addons.Helm[0].Timeout != 12*time.Minute || cfg.Addons.Helm[0].Values != "k: v\n" {
		t.Errorf("helm = %+v", cfg.Addons.Helm)
	}
	if len(cfg.Addons.Manifests) != 1 {
		t.Errorf("manifests = %v", cfg.Addons.Manifests)
	}
}

func TestManagerFor(t *testing.T) {
	ctx := context.Background()
	def := capi.NewManager()
	r := &ClusterResource{manager: def}

	kind := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "kind", "", nil, nil, nil), nil)
	mgr, diags := r.managerFor(ctx, kind)
	if diags.HasError() || mgr != def {
		t.Fatalf("kind should return the default manager, got %p vs %p, %v", mgr, def, diags)
	}

	tal := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", nil, testTalosModel(ctx, t, nil, nil), nil), goodMachine)
	mgr, diags = r.managerFor(ctx, tal)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if mgr == nil || mgr == def {
		t.Fatal("talos should return a new manager")
	}
}

func TestBuildTalosBootstrapConfig_CNICilium(t *testing.T) {
	ctx := context.Background()
	data := talosTestData(ctx, t, buildTestBootstrap(ctx, t, "talos", "cp-1", nil, testTalosModel(ctx, t, nil, []string{"machine: {}\n"}), nil), goodMachine)
	cniVal, d := types.ObjectValueFrom(ctx, cniAttrTypes(), CNIModel{
		Provider: types.StringValue("cilium"),
		Version:  types.StringValue("1.18.0"),
		Values:   types.StringValue("kubeProxyReplacement: false\n"),
	})
	if d.HasError() {
		t.Fatal(d)
	}
	data.CNI = cniVal

	cfg, diags := buildTalosBootstrapConfig(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if len(cfg.Talos.ConfigPatches) != 2 || !strings.Contains(cfg.Talos.ConfigPatches[0], "name: none") {
		t.Errorf("expected the cilium CNI patch prepended, got %v", cfg.Talos.ConfigPatches)
	}
	var cilium *talos.HelmRelease
	for i := range cfg.Addons.Helm {
		if cfg.Addons.Helm[i].Name == "cilium" {
			cilium = &cfg.Addons.Helm[i]
		}
	}
	if cilium == nil {
		t.Fatalf("expected a cilium helm release, got %+v", cfg.Addons.Helm)
	}
	if cilium.Version != "1.18.0" || !strings.Contains(cilium.Values, "kubeProxyReplacement: false") {
		t.Errorf("cilium release = %+v", cilium)
	}
}
