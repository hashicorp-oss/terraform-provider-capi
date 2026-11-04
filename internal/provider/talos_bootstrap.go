// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/kairos"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/talos"
)

const defaultHelmTimeout = 10 * time.Minute

// buildTalosBootstrapConfig assembles the Talos bootstrapper configuration
// from management.bootstrap and the referenced inventory machine. It returns
// nil when the bootstrap type is not "talos". Field validity is checked by
// validateManagementBootstrap, which runs first.
func buildTalosBootstrapConfig(ctx context.Context, data *ClusterResourceModel) (*talos.Config, diag.Diagnostics) {
	var diags diag.Diagnostics

	mgmt, d := extractManagement(ctx, data)
	diags.Append(d...)
	bs, d := extractManagementBootstrap(ctx, mgmt)
	diags.Append(d...)
	if bs == nil || bs.Type.IsNull() || bs.Type.IsUnknown() || bs.Type.ValueString() != "talos" {
		return nil, diags
	}

	m, d := findInventoryMachine(ctx, data, bs.Machine.ValueString())
	diags.Append(d...)
	if m == nil {
		diags.AddError("Invalid bootstrap configuration", fmt.Sprintf("management.bootstrap.machine %q not found in inventory.", bs.Machine.ValueString()))
		return nil, diags
	}
	var network NetworkModel
	var disk DiskModel
	var bmc BMCModel
	diags.Append(m.Network.As(ctx, &network, basetypes.ObjectAsOptions{})...)
	diags.Append(m.Disk.As(ctx, &disk, basetypes.ObjectAsOptions{})...)
	diags.Append(m.BMC.As(ctx, &bmc, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nil, diags
	}

	nameservers, d := stringList(ctx, network.Nameservers)
	diags.Append(d...)

	cfg := &talos.Config{
		Machine: talos.MachineConfig{
			Hostname:    m.Hostname.ValueString(),
			IP:          network.IPAddress.ValueString(),
			Netmask:     network.Netmask.ValueString(),
			Gateway:     network.Gateway.ValueString(),
			MAC:         network.MACAddress.ValueString(),
			Nameservers: nameservers,
			Disk:        disk.Device.ValueString(),
			BMC: talos.BMCCredentials{
				Address:  bmc.Address.ValueString(),
				Username: bmc.Username.ValueString(),
				Password: bmc.Password.ValueString(),
			},
		},
	}
	if !bs.StateDir.IsNull() {
		cfg.StateDir = bs.StateDir.ValueString()
	}

	cfg.Boot, d = bootConfigFromModel(ctx, bs)
	diags.Append(d...)

	tm, d := extractTalos(ctx, bs)
	diags.Append(d...)
	if tm != nil {
		cfg.Talos.Version = tm.Version.ValueString()
		cfg.Talos.Architecture = tm.Architecture.ValueString()
		cfg.Talos.Endpoint = tm.Endpoint.ValueString()
		patches, d := stringList(ctx, tm.ConfigPatches)
		diags.Append(d...)
		cfg.Talos.ConfigPatches = patches

		img, d := extractTalosImage(ctx, tm)
		diags.Append(d...)
		if img != nil {
			cfg.Talos.Image = talos.ImageSpec{
				Factory:   img.Factory.ValueString(),
				Schematic: img.Schematic.ValueString(),
				ISO:       img.ISO.ValueString(),
				Installer: img.Installer.ValueString(),
			}
			cfg.Talos.Image.Extensions, d = stringList(ctx, img.Extensions)
			diags.Append(d...)
			cfg.Talos.Image.KernelArgs, d = stringList(ctx, img.KernelArgs)
			diags.Append(d...)
		}
	}

	ad, d := extractBootstrapAddons(ctx, bs)
	diags.Append(d...)
	set, d := addonSetFromModel(ctx, ad)
	diags.Append(d...)
	cfg.Addons.Prerequisites = set.Prerequisites
	cfg.Addons.Helm = set.Helm
	cfg.Addons.Manifests = set.Manifests

	applyInPlaceContract(ctx, data, bs, cfg, &diags)

	o, ok, d := cniOptionsFor(ctx, data)
	diags.Append(d...)
	if ok {
		if err := applyCNI(cfg, o); err != nil {
			diags.AddError("Invalid cni configuration", err.Error())
		}
	}

	return cfg, diags
}

// bootConfigFromModel reads management.bootstrap.boot; zero fields take the
// bootstrapper's defaults.
func bootConfigFromModel(ctx context.Context, bs *ManagementBootstrapModel) (talos.BootConfig, diag.Diagnostics) {
	var cfg talos.BootConfig
	boot, diags := extractBoot(ctx, bs)
	if boot == nil {
		return cfg, diags
	}
	cfg.Method = talos.BootMethod(boot.Method.ValueString())
	if s := boot.Timeout.ValueString(); s != "" {
		if dur, err := time.ParseDuration(s); err == nil {
			cfg.Timeout = dur
		}
	}
	if !boot.Attempts.IsNull() && !boot.Attempts.IsUnknown() {
		cfg.Attempts = int(boot.Attempts.ValueInt64())
	}
	if d := boot.Device.ValueString(); d != "" {
		cfg.Device = talos.BootDevice(d)
	}
	return cfg, diags
}

// managerFor returns the resource's default manager, or one wired with the
// bootstrapper management.bootstrap.type names: the Talos BMC bootstrapper
// for "talos", the disposable Kairos live node for "kairos".
func (r *ClusterResource) managerFor(ctx context.Context, data *ClusterResourceModel) (*capi.Manager, diag.Diagnostics) {
	var diags diag.Diagnostics
	logger := log.New(os.Stderr, "[capi-tf] ", log.LstdFlags)
	var bootstrapper capi.Bootstrapper
	switch bootstrapType(ctx, data) {
	case "talos":
		cfg, d := buildTalosBootstrapConfig(ctx, data)
		diags.Append(d...)
		if diags.HasError() || cfg == nil {
			return r.manager, diags
		}
		bootstrapper = talos.New(*cfg, talos.WithLogger(logger))
	case "kairos":
		cfg, d := buildKairosBootstrapConfig(ctx, data)
		diags.Append(d...)
		if diags.HasError() || cfg == nil {
			return r.manager, diags
		}
		bootstrapper = kairos.New(*cfg, kairos.WithLogger(logger))
	default:
		return r.manager, nil
	}
	return capi.NewManager(
		capi.WithLogger(logger),
		capi.WithBootstrapper(bootstrapper),
		capi.WithAddonInstaller(talos.NewManagementAddonInstaller(logger)),
	), diags
}

// workloadAddonsFor returns the addons the manager installs on the workload
// cluster during a pivot: the cni release and its Gateway API CRDs, which
// otherwise only a Talos bootstrap node gets. In place the node is the
// cluster, so there is nothing to add; a kind bootstrap has no cni.
func workloadAddonsFor(ctx context.Context, data *ClusterResourceModel) capi.AddonSet {
	if !talosClusterBootstrap(ctx, data) {
		return capi.AddonSet{}
	}
	mgmt, _ := extractManagement(ctx, data)
	bs, _ := extractManagementBootstrap(ctx, mgmt)
	if bs != nil && bs.Mode.ValueString() == "in_place" {
		return capi.AddonSet{}
	}
	o, ok, _ := cniOptionsFor(ctx, data)
	if !ok {
		return capi.AddonSet{}
	}
	rel, urls, err := cniRelease(o)
	if err != nil {
		return capi.AddonSet{} // reported by validation
	}
	return capi.AddonSet{PreHelmManifestURLs: urls, Helm: []capi.HelmRelease{rel}}
}

// kubeletProviderIDPatch pins the kubelet provider ID so the adopted node's
// Node object matches the CAPT-assigned Machine provider ID.
func kubeletProviderIDPatch(providerID string) string {
	return "machine:\n  kubelet:\n    extraArgs:\n      provider-id: " + providerID + "\n"
}

// controlPlaneStrategicPatches returns control_plane["talos"].resource_config
// controlPlaneConfig.controlplane.strategicPatches, in order. Missing pieces
// yield nil; anything other than a list of strings is reported.
func controlPlaneStrategicPatches(ctx context.Context, data *ClusterResourceModel, diags *diag.Diagnostics) ([]string, bool) {
	entries, d := extractProviders(ctx, data.ControlPlane)
	diags.Append(d...)
	for name, pm := range entries {
		if strings.ToLower(name) != "talos" {
			continue
		}
		rc, d := yamlObject(pm.ResourceConfig, fmt.Sprintf("control_plane[%q].resource_config", name))
		diags.Append(d...)
		cpc, _ := rc["controlPlaneConfig"].(map[string]any)
		cp, _ := cpc["controlplane"].(map[string]any)
		raw, _ := cp["strategicPatches"].([]any)
		var patches []string
		for i, p := range raw {
			s, ok := p.(string)
			if !ok {
				diags.AddError("Invalid control_plane resource_config",
					fmt.Sprintf("control_plane[%q].resource_config controlPlaneConfig.controlplane.strategicPatches[%d] must be a YAML string (yamlencode({...})).", name, i))
				continue
			}
			patches = append(patches, s)
		}
		return patches, true
	}
	return nil, false
}

// applyInPlaceContract makes the bootstrap node's machine config what CAPI
// would render for it as a control-plane Machine (the in-place contract):
//
//  1. the control-plane provider's strategic patches, with PROVIDER_ID
//     substituted the way CAPT substitutes it in Hardware userData, come
//     before the user's config_patches;
//  2. a kubelet provider-id patch is added when none of them sets one;
//  3. the cluster endpoint defaults to network.control_plane_endpoint, and
//     the two may not disagree.
//
// Scheduling on control planes is warned about rather than forced: the node
// allows it, but the first in-place update applies the CAPI render, which
// taints the node unless the patches say otherwise.
func applyInPlaceContract(ctx context.Context, data *ClusterResourceModel, bs *ManagementBootstrapModel, cfg *talos.Config, diags *diag.Diagnostics) {
	if bs == nil || bs.Mode.IsNull() || bs.Mode.IsUnknown() || bs.Mode.ValueString() != "in_place" {
		return
	}
	mgmt, d := extractManagement(ctx, data)
	diags.Append(d...)
	namespace := "default"
	if mgmt != nil && !mgmt.Namespace.IsNull() && !mgmt.Namespace.IsUnknown() && mgmt.Namespace.ValueString() != "" {
		namespace = mgmt.Namespace.ValueString()
	}
	providerID := "tinkerbell://" + namespace + "/" + cfg.Machine.Hostname

	patches, found := controlPlaneStrategicPatches(ctx, data, diags)
	if !found {
		diags.AddError("Invalid bootstrap configuration",
			"management.bootstrap.mode = \"in_place\" needs a control_plane[\"talos\"] entry: the node is adopted by the Talos control-plane provider, whose resource_config strategic patches are what the node runs.")
		return
	}
	composed := make([]string, 0, len(patches)+1)
	hasProviderID, allowsScheduling := false, false
	for _, p := range patches {
		if strings.Contains(p, "provider-id") {
			hasProviderID = true
		}
		if strings.Contains(p, "allowSchedulingOnControlPlanes") {
			allowsScheduling = true
		}
		composed = append(composed, strings.ReplaceAll(p, "PROVIDER_ID", providerID))
	}
	if !hasProviderID {
		composed = append(composed, kubeletProviderIDPatch(providerID))
	}
	if !allowsScheduling {
		diags.AddWarning("Control plane scheduling not in control_plane patches",
			"The bootstrap node allows scheduling on control planes so the platform pods can run on a single node, but control_plane[\"talos\"].resource_config strategic patches do not set cluster.allowSchedulingOnControlPlanes. The first in-place update applies the CAPI-rendered config and would taint the node; add the setting to the patches so both agree.")
	}
	cfg.Talos.ConfigPatches = append(composed, cfg.Talos.ConfigPatches...)

	network, d := extractNetwork(ctx, data)
	diags.Append(d...)
	if ep := network.ControlPlaneEndpoint; ep != nil && ep.Host != "" {
		port := ep.Port
		if port == 0 {
			port = 6443
		}
		want := fmt.Sprintf("https://%s", net.JoinHostPort(ep.Host, strconv.Itoa(int(port))))
		switch {
		case cfg.Talos.Endpoint == "":
			cfg.Talos.Endpoint = want
		case endpointHost(cfg.Talos.Endpoint) != ep.Host:
			diags.AddError("Bootstrap endpoint does not match the cluster endpoint",
				fmt.Sprintf("management.bootstrap.talos.endpoint %q and network.control_plane_endpoint.host %q name different hosts. In place the bootstrap node is the cluster, so its endpoint, certificate SANs and kubeconfig must use the cluster endpoint; set one of them.", cfg.Talos.Endpoint, ep.Host))
		}
	}
}

// endpointHost returns the host of a URL or host:port string, else the input.
func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Hostname()
	}
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		return h
	}
	return endpoint
}
