// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"sigs.k8s.io/yaml"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/talos"
)

func TestCiliumValues_Defaults(t *testing.T) {
	out, err := ciliumValues("", false)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["kubeProxyReplacement"] != true {
		t.Errorf("kubeProxyReplacement = %v, want true", m["kubeProxyReplacement"])
	}
	if m["k8sServiceHost"] != "localhost" {
		t.Errorf("k8sServiceHost = %v, want localhost", m["k8sServiceHost"])
	}
	// YAML numbers decode as float64 through the JSON bridge.
	if p, _ := m["k8sServicePort"].(float64); p != 7445 {
		t.Errorf("k8sServicePort = %v, want 7445", m["k8sServicePort"])
	}
	if ipam, _ := m["ipam"].(map[string]any); ipam["mode"] != "kubernetes" {
		t.Errorf("ipam = %v, want mode kubernetes", m["ipam"])
	}
	// Talos requires Cilium not to auto-mount cgroups (Talos already mounts cgroup2).
	cgroup, _ := m["cgroup"].(map[string]any)
	if am, _ := cgroup["autoMount"].(map[string]any); am["enabled"] != false {
		t.Errorf("cgroup.autoMount.enabled = %v, want false (Talos requirement)", cgroup["autoMount"])
	}
	if cgroup["hostRoot"] != "/sys/fs/cgroup" {
		t.Errorf("cgroup.hostRoot = %v, want /sys/fs/cgroup", cgroup["hostRoot"])
	}
	// Talos rejects the default capability set; Cilium needs the restricted lists.
	sc, _ := m["securityContext"].(map[string]any)
	caps, _ := sc["capabilities"].(map[string]any)
	if _, ok := caps["ciliumAgent"].([]any); !ok {
		t.Errorf("securityContext.capabilities.ciliumAgent missing (Talos requirement): %v", sc)
	}
	if _, ok := caps["cleanCiliumState"].([]any); !ok {
		t.Errorf("securityContext.capabilities.cleanCiliumState missing (Talos requirement): %v", sc)
	}
}

func TestCiliumValues_UserMergeOverridesAndAdds(t *testing.T) {
	user := "kubeProxyReplacement: false\nhubble:\n  relay:\n    enabled: true\n"
	out, err := ciliumValues(user, false)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["kubeProxyReplacement"] != false {
		t.Errorf("user value must override default: %v", m["kubeProxyReplacement"])
	}
	if m["k8sServiceHost"] != "localhost" {
		t.Errorf("untouched default must survive the merge: %v", m["k8sServiceHost"])
	}
	relay, _ := m["hubble"].(map[string]any)["relay"].(map[string]any)
	if relay["enabled"] != true {
		t.Errorf("user-only key must be added: %v", m["hubble"])
	}
}

func TestApplyCNI_CiliumPrependsPatchAndAppendsRelease(t *testing.T) {
	cfg := &talos.Config{Talos: talos.TalosConfig{ConfigPatches: []string{"machine: {}"}}}
	if err := applyCNI(cfg, cniOptions{provider: "cilium", version: "1.18.0"}); err != nil {
		t.Fatal(err)
	}

	if len(cfg.Talos.ConfigPatches) != 2 {
		t.Fatalf("config patches = %v, want the CNI patch prepended", cfg.Talos.ConfigPatches)
	}
	patch := cfg.Talos.ConfigPatches[0]
	for _, want := range []string{"name: none", "disabled: true", "kubePrism"} {
		if !strings.Contains(patch, want) {
			t.Errorf("CNI patch missing %q:\n%s", want, patch)
		}
	}
	if cfg.Talos.ConfigPatches[1] != "machine: {}" {
		t.Errorf("existing patch must remain after the CNI patch: %v", cfg.Talos.ConfigPatches)
	}

	if len(cfg.Addons.CNI) != 1 || len(cfg.Addons.Helm) != 0 {
		t.Fatalf("cni releases = %v, helm = %v; cilium must be a CNI release", cfg.Addons.CNI, cfg.Addons.Helm)
	}
	rel := cfg.Addons.CNI[0]
	if rel.Name != "cilium" || rel.Namespace != "kube-system" || rel.Version != "1.18.0" {
		t.Errorf("cilium release = %+v", rel)
	}
	if !strings.Contains(rel.Chart, "cilium") {
		t.Errorf("chart = %q", rel.Chart)
	}
	if !strings.Contains(rel.Values, "kubeProxyReplacement") {
		t.Errorf("values must carry the cilium defaults: %q", rel.Values)
	}
	// Wait=true with a zero timeout gives Helm an already-expired deadline.
	if rel.Timeout <= 0 {
		t.Errorf("cilium release must have a non-zero install timeout, got %v", rel.Timeout)
	}
}

func TestApplyCNI_UnknownProviderErrors(t *testing.T) {
	cfg := &talos.Config{}
	if err := applyCNI(cfg, cniOptions{provider: "calico"}); err == nil {
		t.Fatal("unknown CNI provider must error")
	}
}

func TestApplyCNI_GatewayAPIInstallsCRDsBeforeCiliumAndEnablesInValues(t *testing.T) {
	cfg := &talos.Config{}
	if err := applyCNI(cfg, cniOptions{provider: "cilium", version: "1.20.1", gatewayAPIEnabled: true, gatewayAPIVersion: "v1.3.0"}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Addons.PreHelmManifestURLs) != 1 ||
		cfg.Addons.PreHelmManifestURLs[0] != "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.3.0/standard-install.yaml" {
		t.Fatalf("gateway API CRD URL = %v", cfg.Addons.PreHelmManifestURLs)
	}
	var vals map[string]any
	if err := yaml.Unmarshal([]byte(cfg.Addons.CNI[0].Values), &vals); err != nil {
		t.Fatal(err)
	}
	if ga, _ := vals["gatewayAPI"].(map[string]any); ga["enabled"] != true {
		t.Errorf("cilium values must set gatewayAPI.enabled=true, got %v", vals["gatewayAPI"])
	}
}

func TestApplyCNI_GatewayAPIDefaultVersion(t *testing.T) {
	cfg := &talos.Config{}
	if err := applyCNI(cfg, cniOptions{provider: "cilium", gatewayAPIEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Addons.PreHelmManifestURLs) != 1 || !strings.Contains(cfg.Addons.PreHelmManifestURLs[0], defaultGatewayAPIVersion) {
		t.Errorf("default gateway API version not used: %v", cfg.Addons.PreHelmManifestURLs)
	}
}

func TestApplyCNI_NoGatewayAPIByDefault(t *testing.T) {
	cfg := &talos.Config{}
	if err := applyCNI(cfg, cniOptions{provider: "cilium"}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Addons.PreHelmManifestURLs) != 0 {
		t.Errorf("no CRDs should be fetched when gateway_api is off: %v", cfg.Addons.PreHelmManifestURLs)
	}
}

func TestSchema_CNIBlock(t *testing.T) {
	ctx := context.Background()
	resp := &resource.SchemaResponse{}
	NewClusterResource().Schema(ctx, resource.SchemaRequest{}, resp)
	cni, ok := resp.Schema.Attributes["cni"].(schema.SingleNestedAttribute)
	if !ok {
		t.Fatalf("cni must be a SingleNestedAttribute, got %T", resp.Schema.Attributes["cni"])
	}
	for _, name := range []string{"provider", "version", "values"} {
		if _, ok := cni.Attributes[name]; !ok {
			t.Errorf("cni missing %s", name)
		}
	}
	if p, ok := cni.Attributes["provider"].(schema.StringAttribute); !ok || !p.Required {
		t.Error("cni.provider must be a required string within the block")
	}
}

func TestValidateLifecycleConfig_CNIRequiresTalosBootstrap(t *testing.T) {
	ctx := context.Background()
	cniVal := func(t *testing.T) types.Object {
		v, d := types.ObjectValueFrom(ctx, cniAttrTypes(), CNIModel{
			Provider: types.StringValue("cilium"), Version: types.StringNull(), Values: types.StringNull(),
			GatewayAPI: types.ObjectNull(gatewayAPIAttrTypes()),
		})
		if d.HasError() {
			t.Fatal(d)
		}
		return v
	}
	// Docker cluster (no talos bootstrap) with cni set must fail.
	diags := validateWith(t, ctx, func(d *ClusterResourceModel) {
		d.CNI = cniVal(t)
	})
	if !hasErrorContaining(diags, "cni") {
		t.Errorf("cni without a talos bootstrap must be rejected: %v", diags)
	}
}
