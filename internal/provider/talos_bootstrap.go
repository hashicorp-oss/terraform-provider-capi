// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
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

	boot, d := extractBoot(ctx, bs)
	diags.Append(d...)
	if boot != nil {
		cfg.Boot.Method = talos.BootMethod(boot.Method.ValueString())
		if s := boot.Timeout.ValueString(); s != "" {
			if dur, err := time.ParseDuration(s); err == nil {
				cfg.Boot.Timeout = dur
			}
		}
		if !boot.Attempts.IsNull() && !boot.Attempts.IsUnknown() {
			cfg.Boot.Attempts = int(boot.Attempts.ValueInt64())
		}
		if d := boot.Device.ValueString(); d != "" {
			cfg.Boot.Device = talos.BootDevice(d)
		}
	}

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
	if ad != nil {
		var rels []HelmReleaseModel
		if !ad.Helm.IsNull() && !ad.Helm.IsUnknown() {
			diags.Append(ad.Helm.ElementsAs(ctx, &rels, false)...)
		}
		for _, rel := range rels {
			timeout := defaultHelmTimeout
			if s := rel.Timeout.ValueString(); s != "" {
				if dur, err := time.ParseDuration(s); err == nil {
					timeout = dur
				}
			}
			cfg.Addons.Helm = append(cfg.Addons.Helm, talos.HelmRelease{
				Name:       rel.Name.ValueString(),
				Namespace:  rel.Namespace.ValueString(),
				Chart:      rel.Chart.ValueString(),
				Repository: rel.Repository.ValueString(),
				Version:    rel.Version.ValueString(),
				Values:     rel.Values.ValueString(),
				Timeout:    timeout,
			})
		}
		cfg.Addons.Manifests, d = stringList(ctx, ad.Manifests)
		diags.Append(d...)
	}

	cni, d := extractCNI(ctx, data)
	diags.Append(d...)
	if cni != nil && !cni.Provider.IsNull() && cni.Provider.ValueString() != "" {
		o := cniOptions{
			provider: cni.Provider.ValueString(),
			version:  cni.Version.ValueString(),
			values:   cni.Values.ValueString(),
		}
		if gw, gd := extractGatewayAPI(ctx, cni); gw != nil {
			diags.Append(gd...)
			o.gatewayAPIEnabled = gw.Enabled.ValueBool()
			o.gatewayAPIVersion = gw.Version.ValueString()
		}
		if err := applyCNI(cfg, o); err != nil {
			diags.AddError("Invalid cni configuration", err.Error())
		}
	}

	return cfg, diags
}

// managerFor returns the resource's default manager, or one wired with the
// Talos BMC bootstrapper when management.bootstrap.type = "talos".
func (r *ClusterResource) managerFor(ctx context.Context, data *ClusterResourceModel) (*capi.Manager, diag.Diagnostics) {
	cfg, diags := buildTalosBootstrapConfig(ctx, data)
	if diags.HasError() || cfg == nil {
		return r.manager, diags
	}
	logger := log.New(os.Stderr, "[capi-tf] ", log.LstdFlags)
	return capi.NewManager(
		capi.WithLogger(logger),
		capi.WithBootstrapper(talos.New(*cfg, talos.WithLogger(logger))),
	), diags
}
