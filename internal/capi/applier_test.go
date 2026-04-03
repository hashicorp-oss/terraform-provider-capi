// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"testing"
)

func TestDynamicApplier_NewDefaults(t *testing.T) {
	a := NewDynamicApplier()
	if a.fieldManager != "terraform-provider-capi" {
		t.Errorf("expected field manager 'terraform-provider-capi', got %q", a.fieldManager)
	}
}

func TestDynamicApplier_CustomFieldManager(t *testing.T) {
	a := NewDynamicApplierWithFieldManager("custom-manager")
	if a.fieldManager != "custom-manager" {
		t.Errorf("expected field manager 'custom-manager', got %q", a.fieldManager)
	}
}
