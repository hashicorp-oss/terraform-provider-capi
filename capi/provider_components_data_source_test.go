// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

func TestProviderTypeFromSchema(t *testing.T) {
	for in, want := range map[string]capi.ProviderType{"core": capi.ProviderTypeCore, "control_plane": capi.ProviderTypeControlPlane, "control-plane": capi.ProviderTypeControlPlane, "addon": capi.ProviderTypeAddon} {
		if got, ok := providerTypeFromSchema(in); !ok || got != want {
			t.Errorf("%s = %v %v", in, got, ok)
		}
	}
	if _, ok := providerTypeFromSchema("runtime"); ok {
		t.Error("unknown type")
	}
	_ = types.StringNull()
}
