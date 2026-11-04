// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"sigs.k8s.io/yaml"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/talos"
)

// ciliumChart is the OCI chart reference installed for the Cilium CNI.
const ciliumChart = "oci://quay.io/cilium/charts/cilium"

// ciliumNamespace is where the Cilium release is installed.
const ciliumNamespace = "kube-system"

// kubePrismPort is the Talos KubePrism API load-balancer port. Cilium's
// kube-proxy replacement talks to the API server through it.
const kubePrismPort = 7445

// defaultGatewayAPIVersion is the Gateway API release whose standard-install.yaml
// CRDs are applied before Cilium when cni.gateway_api.enabled and no version is set.
const defaultGatewayAPIVersion = "v1.3.0"

// ciliumInstallTimeout bounds the Helm install wait. Helm's install action
// treats a zero timeout as an already-expired deadline when Wait is set, so
// this must be non-zero. It allows for image pulls on a fresh node.
const ciliumInstallTimeout = 15 * time.Minute

// ciliumCNIPatch is the Talos machine-config patch that turns Talos's built-in
// flannel and kube-proxy off and enables KubePrism, so Cilium owns networking.
const ciliumCNIPatch = `cluster:
  network:
    cni:
      name: none
  proxy:
    disabled: true
machine:
  features:
    kubePrism:
      enabled: true
      port: 7445
`

// cniOptions configures the CNI injected into a Talos bootstrap config.
type cniOptions struct {
	provider string
	version  string
	values   string
	// gatewayAPIEnabled installs the Gateway API CRDs before Cilium and turns
	// on gatewayAPI in the Cilium values.
	gatewayAPIEnabled bool
	// gatewayAPIVersion is the Gateway API release for the CRDs; empty uses
	// defaultGatewayAPIVersion.
	gatewayAPIVersion string
}

// cniOptionsFor reads the cni block; ok is false when it is unset.
func cniOptionsFor(ctx context.Context, data *ClusterResourceModel) (o cniOptions, ok bool, diags diag.Diagnostics) {
	cni, d := extractCNI(ctx, data)
	diags.Append(d...)
	if cni == nil || cni.Provider.IsNull() || cni.Provider.ValueString() == "" {
		return cniOptions{}, false, diags
	}
	o = cniOptions{
		provider: cni.Provider.ValueString(),
		version:  cni.Version.ValueString(),
		values:   cni.Values.ValueString(),
	}
	if gw, gd := extractGatewayAPI(ctx, cni); gw != nil {
		diags.Append(gd...)
		o.gatewayAPIEnabled = gw.Enabled.ValueBool()
		o.gatewayAPIVersion = gw.Version.ValueString()
	}
	return o, true, diags
}

// applyCNI injects the chosen CNI into a Talos bootstrap config: a machine
// config patch disabling the built-in CNI and a Helm release installing the
// CNI. Only "cilium" is supported today. When gateway_api is enabled, the
// Gateway API CRDs are fetched and applied before Cilium (a pre-helm manifest)
// so the chart's GatewayClass resolves.
func applyCNI(cfg *talos.Config, o cniOptions) error {
	rel, urls, err := cniRelease(o)
	if err != nil {
		return err
	}
	cfg.Talos.ConfigPatches = append([]string{ciliumCNIPatch}, cfg.Talos.ConfigPatches...)
	// The CNI installs before every other release: nothing schedules without it.
	cfg.Addons.CNI = append(cfg.Addons.CNI, rel)
	cfg.Addons.PreHelmManifestURLs = append(cfg.Addons.PreHelmManifestURLs, urls...)
	return nil
}

// cniRelease builds the CNI Helm release and the URLs of the manifests that
// must be applied before it (the Gateway API CRDs).
func cniRelease(o cniOptions) (talos.HelmRelease, []string, error) {
	if o.provider != "cilium" {
		return talos.HelmRelease{}, nil, fmt.Errorf("unsupported cni.provider %q: only \"cilium\" is supported", o.provider)
	}
	vals, err := ciliumValues(o.values, o.gatewayAPIEnabled)
	if err != nil {
		return talos.HelmRelease{}, nil, err
	}
	rel := talos.HelmRelease{
		Name:      "cilium",
		Namespace: ciliumNamespace,
		Chart:     ciliumChart,
		Version:   o.version,
		Values:    vals,
		Timeout:   ciliumInstallTimeout,
	}
	var urls []string
	if o.gatewayAPIEnabled {
		v := o.gatewayAPIVersion
		if v == "" {
			v = defaultGatewayAPIVersion
		}
		urls = append(urls, gatewayAPICRDURL(v))
	}
	return rel, urls, nil
}

// gatewayAPICRDURL is the upstream standard-install.yaml for a Gateway API release.
func gatewayAPICRDURL(version string) string {
	return fmt.Sprintf("https://github.com/kubernetes-sigs/gateway-api/releases/download/%s/standard-install.yaml", version)
}

// ciliumValues deep-merges the user's Helm values (YAML) over the Cilium
// defaults that pair with the Talos CNI patch (kube-proxy replacement via
// KubePrism, Kubernetes IPAM). User keys win; unset defaults survive.
func ciliumValues(userValues string, gatewayAPI bool) (string, error) {
	// Defaults required to run Cilium on Talos, per the official Talos guide:
	// KubePrism kube-proxy replacement, Kubernetes IPAM, no cgroup auto-mount
	// (Talos already mounts cgroup2), and the restricted capability sets Talos
	// permits. Anything beyond this (BGP, native routing, Gateway API, Hubble,
	// resource limits) is environment-specific and belongs in cni.values.
	merged := map[string]any{
		"kubeProxyReplacement": true,
		"k8sServiceHost":       "localhost",
		"k8sServicePort":       kubePrismPort,
		"ipam":                 map[string]any{"mode": "kubernetes"},
		"cgroup": map[string]any{
			"autoMount": map[string]any{"enabled": false},
			"hostRoot":  "/sys/fs/cgroup",
		},
		"securityContext": map[string]any{
			"capabilities": map[string]any{
				"ciliumAgent":      []any{"BPF", "CHOWN", "DAC_OVERRIDE", "FOWNER", "IPC_LOCK", "KILL", "NET_ADMIN", "NET_RAW", "PERFMON", "SETGID", "SETUID", "SYS_ADMIN", "SYS_RESOURCE"},
				"cleanCiliumState": []any{"NET_ADMIN", "SYS_ADMIN", "SYS_RESOURCE"},
			},
		},
	}
	if gatewayAPI {
		// The provider owns gatewayAPI.enabled (paired with the CRD install);
		// user values may still add gatewayAPI.gatewayClass, enableAlpn, etc.
		merged["gatewayAPI"] = map[string]any{"enabled": true}
	}
	if userValues != "" {
		var user map[string]any
		if err := yaml.Unmarshal([]byte(userValues), &user); err != nil {
			return "", fmt.Errorf("parsing cni.values: %w", err)
		}
		merged = mergeMaps(merged, user)
	}
	out, err := yaml.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("rendering cni.values: %w", err)
	}
	return string(out), nil
}

// mergeMaps deep-merges override into base, returning a new map. Nested maps
// are merged recursively; every other value in override replaces base.
func mergeMaps(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	maps.Copy(out, base)
	for k, ov := range override {
		if bv, ok := out[k]; ok {
			if bm, ok1 := bv.(map[string]any); ok1 {
				if om, ok2 := ov.(map[string]any); ok2 {
					out[k] = mergeMaps(bm, om)
					continue
				}
			}
		}
		out[k] = ov
	}
	return out
}

// injectCNIPatch puts the Talos CNI patch in front of every strategic patch
// list CAPI renders for the cluster's Talos machines: the control plane's
// controlPlaneConfig.controlplane.strategicPatches, the bootstrap provider's
// default TalosConfigTemplate strategicPatches, and each machine deployment's
// own bootstrap spec. The bootstrap node gets the same patch through its
// config_patches, so the node and the CAPI render agree. Idempotent.
func injectCNIPatch(opts *capi.CreateClusterOptions) {
	for i, p := range opts.Providers[capi.ProviderTypeControlPlane] {
		if strings.EqualFold(p.Name, "talos") {
			p.ResourceConfig = prependStrategicPatch(p.ResourceConfig, "controlPlaneConfig", "controlplane")
			opts.Providers[capi.ProviderTypeControlPlane][i] = p
		}
	}
	for i, p := range opts.Providers[capi.ProviderTypeBootstrap] {
		if strings.EqualFold(p.Name, "talos") {
			p.ResourceConfig = prependStrategicPatch(p.ResourceConfig)
			opts.Providers[capi.ProviderTypeBootstrap][i] = p
		}
	}
	for i, md := range opts.MachineDeployments {
		if md.Bootstrap != nil {
			opts.MachineDeployments[i].Bootstrap = prependStrategicPatch(md.Bootstrap)
		}
	}
}

// prependStrategicPatch returns spec with ciliumCNIPatch first in the
// strategicPatches list found under path (created when missing).
func prependStrategicPatch(spec map[string]any, path ...string) map[string]any {
	if spec == nil {
		spec = map[string]any{}
	}
	node := spec
	for _, key := range path {
		next, ok := node[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[key] = next
		}
		node = next
	}
	existing, _ := node["strategicPatches"].([]any)
	for _, p := range existing {
		if s, ok := p.(string); ok && s == ciliumCNIPatch {
			return spec
		}
	}
	node["strategicPatches"] = append([]any{ciliumCNIPatch}, existing...)
	return spec
}
