// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/bmc"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/kairos"
)

// buildKairosBootstrapConfig assembles the Kairos bootstrapper configuration
// from management.bootstrap and the referenced inventory machine. It returns
// nil when the bootstrap type is not "kairos". Field validity is checked by
// validateManagementBootstrap, which runs first.
func buildKairosBootstrapConfig(ctx context.Context, data *ClusterResourceModel) (*kairos.Config, diag.Diagnostics) {
	const summary = "Invalid bootstrap configuration"
	var diags diag.Diagnostics

	mgmt, d := extractManagement(ctx, data)
	diags.Append(d...)
	bs, d := extractManagementBootstrap(ctx, mgmt)
	diags.Append(d...)
	if bs == nil || bs.Type.IsNull() || bs.Type.IsUnknown() || bs.Type.ValueString() != "kairos" {
		return nil, diags
	}

	m, d := findInventoryMachine(ctx, data, bs.Machine.ValueString())
	diags.Append(d...)
	if m == nil {
		diags.AddError(summary, fmt.Sprintf("management.bootstrap.machine %q not found in inventory.", bs.Machine.ValueString()))
		return nil, diags
	}
	var network NetworkModel
	var controller BMCModel
	lenient := basetypes.ObjectAsOptions{UnhandledNullAsEmpty: true, UnhandledUnknownAsEmpty: true}
	diags.Append(m.Network.As(ctx, &network, lenient)...)
	diags.Append(m.BMC.As(ctx, &controller, lenient)...)

	k, d := extractKairos(ctx, bs)
	diags.Append(d...)
	if k == nil {
		diags.AddError(summary, "management.bootstrap.kairos is required when type = \"kairos\".")
		return nil, diags
	}
	s, d := extractKairosSSH(ctx, k)
	diags.Append(d...)
	if s == nil {
		diags.AddError(summary, "management.bootstrap.kairos.ssh is required.")
		return nil, diags
	}
	if diags.HasError() {
		return nil, diags
	}

	cfg := &kairos.Config{
		Machine: kairos.Machine{
			Hostname: m.Hostname.ValueString(),
			Address:  network.IPAddress.ValueString(),
			BMC: bmc.Credentials{
				Address:  controller.Address.ValueString(),
				Username: controller.Username.ValueString(),
				Password: controller.Password.ValueString(),
			},
		},
		ISO: k.ISO.ValueString(),
		SSH: kairos.SSHConfig{
			User:       s.User.ValueString(),
			Port:       int(s.Port.ValueInt64()),
			PrivateKey: s.PrivateKey.ValueString(),
			HostKey:    s.HostKey.ValueString(),
		},
	}
	if a := k.Address.ValueString(); a != "" {
		cfg.Machine.Address = a
	}
	boot, d := bootConfigFromModel(ctx, bs)
	diags.Append(d...)
	cfg.Boot = bmc.BootConfig{Method: bmc.BootMethod(boot.Method), Timeout: boot.Timeout, Attempts: boot.Attempts, Device: bmc.BootDevice(boot.Device)}

	ad, d := extractBootstrapAddons(ctx, bs)
	diags.Append(d...)
	cfg.Addons, d = addonSetFromModel(ctx, ad)
	diags.Append(d...)
	return cfg, diags
}
