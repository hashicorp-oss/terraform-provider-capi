// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

// Package validators holds the provider's attribute validators beyond the
// ones terraform-plugin-framework-validators ships.
package validators

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// CIDRValidator validates that a string is a CIDR block (`10.42.0.0/16`).
func CIDRValidator() validator.String {
	return &cidrValidator{}
}

type cidrValidator struct{}

func (v cidrValidator) Description(ctx context.Context) string {
	return "value must be a CIDR block"
}

func (v cidrValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v cidrValidator) ValidateString(
	ctx context.Context,
	req validator.StringRequest,
	resp *validator.StringResponse,
) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	if _, err := netip.ParsePrefix(value); err != nil {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid CIDR Block",
			fmt.Sprintf("Value %q is not a CIDR block: %s.", value, err),
		)
	}
}

// IPAddressValidator validates that a string is an IPv4 or IPv6 address.
func IPAddressValidator() validator.String {
	return &ipAddressValidator{}
}

type ipAddressValidator struct{}

func (v ipAddressValidator) Description(ctx context.Context) string {
	return "value must be an IPv4 or IPv6 address"
}

func (v ipAddressValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v ipAddressValidator) ValidateString(
	ctx context.Context,
	req validator.StringRequest,
	resp *validator.StringResponse,
) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	if _, err := netip.ParseAddr(value); err != nil {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid IP Address",
			fmt.Sprintf("Value %q is not an IP address: %s.", value, err),
		)
	}
}

// MACAddressValidator validates that a string is a MAC address
// (`aa:bb:cc:dd:ee:01`).
func MACAddressValidator() validator.String {
	return &macAddressValidator{}
}

type macAddressValidator struct{}

func (v macAddressValidator) Description(ctx context.Context) string {
	return "value must be a MAC address"
}

func (v macAddressValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v macAddressValidator) ValidateString(
	ctx context.Context,
	req validator.StringRequest,
	resp *validator.StringResponse,
) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	if _, err := net.ParseMAC(value); err != nil {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid MAC Address",
			fmt.Sprintf("Value %q is not a MAC address: %s.", value, err),
		)
	}
}
