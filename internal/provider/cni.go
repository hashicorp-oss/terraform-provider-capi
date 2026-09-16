// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"maps"
	"time"

	"sigs.k8s.io/yaml"

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

// applyCNI injects the chosen CNI into a Talos bootstrap config: a machine
// config patch disabling the built-in CNI and a Helm release installing the
// CNI. Only "cilium" is supported today. When gateway_api is enabled, the
// Gateway API CRDs are fetched and applied before Cilium (a pre-helm manifest)
// so the chart's GatewayClass resolves.
func applyCNI(cfg *talos.Config, o cniOptions) error {
	if o.provider != "cilium" {
		return fmt.Errorf("unsupported cni.provider %q: only \"cilium\" is supported", o.provider)
	}
	vals, err := ciliumValues(o.values, o.gatewayAPIEnabled)
	if err != nil {
		return err
	}
	cfg.Talos.ConfigPatches = append([]string{ciliumCNIPatch}, cfg.Talos.ConfigPatches...)
	cfg.Addons.Helm = append(cfg.Addons.Helm, talos.HelmRelease{
		Name:      "cilium",
		Namespace: ciliumNamespace,
		Chart:     ciliumChart,
		Version:   o.version,
		Values:    vals,
		Timeout:   ciliumInstallTimeout,
	})
	if o.gatewayAPIEnabled {
		v := o.gatewayAPIVersion
		if v == "" {
			v = defaultGatewayAPIVersion
		}
		cfg.Addons.PreHelmManifestURLs = append(cfg.Addons.PreHelmManifestURLs, gatewayAPICRDURL(v))
	}
	return nil
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
