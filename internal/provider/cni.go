// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"maps"

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

// applyCNI injects the chosen CNI into a Talos bootstrap config: a machine
// config patch disabling the built-in CNI and a Helm release installing the
// CNI. Only "cilium" is supported today.
func applyCNI(cfg *talos.Config, provider, version, values string) error {
	if provider != "cilium" {
		return fmt.Errorf("unsupported cni.provider %q: only \"cilium\" is supported", provider)
	}
	vals, err := ciliumValues(values)
	if err != nil {
		return err
	}
	cfg.Talos.ConfigPatches = append([]string{ciliumCNIPatch}, cfg.Talos.ConfigPatches...)
	cfg.Addons.Helm = append(cfg.Addons.Helm, talos.HelmRelease{
		Name:      "cilium",
		Namespace: ciliumNamespace,
		Chart:     ciliumChart,
		Version:   version,
		Values:    vals,
	})
	return nil
}

// ciliumValues deep-merges the user's Helm values (YAML) over the Cilium
// defaults that pair with the Talos CNI patch (kube-proxy replacement via
// KubePrism, Kubernetes IPAM). User keys win; unset defaults survive.
func ciliumValues(userValues string) (string, error) {
	merged := map[string]any{
		"kubeProxyReplacement": true,
		"k8sServiceHost":       "localhost",
		"k8sServicePort":       kubePrismPort,
		"ipam":                 map[string]any{"mode": "kubernetes"},
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
