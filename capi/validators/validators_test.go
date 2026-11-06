// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package validators

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestValidators(t *testing.T) {
	for _, tc := range []struct {
		name  string
		v     validator.String
		value types.String
		valid bool
	}{
		{"cidr v4", CIDRValidator(), types.StringValue("10.42.0.0/16"), true},
		{"cidr v6", CIDRValidator(), types.StringValue("fda1:b2c3:d4e5:1::/127"), true},
		{"cidr bare address", CIDRValidator(), types.StringValue("10.42.0.1"), false},
		{"ip v4", IPAddressValidator(), types.StringValue("10.1.40.100"), true},
		{"ip v6", IPAddressValidator(), types.StringValue("fda1:b2c3:d4e5:1::1"), true},
		{"ip hostname", IPAddressValidator(), types.StringValue("talos.local"), false},
		{"mac", MACAddressValidator(), types.StringValue("88:a2:9e:87:76:6c"), true},
		{"mac short", MACAddressValidator(), types.StringValue("88:a2:9e"), false},
		{"duration", DurationValidator(), types.StringValue("1h30m"), true},
		{"duration unitless", DurationValidator(), types.StringValue("90"), false},
		{"duration zero", DurationValidator(), types.StringValue("0s"), false},
		{"null is skipped", DurationValidator(), types.StringNull(), true},
		{"unknown is skipped", MACAddressValidator(), types.StringUnknown(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &validator.StringResponse{}
			tc.v.ValidateString(
				context.Background(),
				validator.StringRequest{Path: path.Root("x"), ConfigValue: tc.value},
				resp,
			)
			if got := !resp.Diagnostics.HasError(); got != tc.valid {
				t.Errorf("valid = %v, want %v: %v", got, tc.valid, resp.Diagnostics)
			}
			if tc.v.Description(context.Background()) == "" {
				t.Error("every validator describes itself")
			}
		})
	}
}
