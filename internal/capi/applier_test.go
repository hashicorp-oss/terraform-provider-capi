// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"os/exec"
	"testing"
)

func TestKubectlApplier_CheckAvailability(t *testing.T) {
	a := NewKubectlApplier()
	_, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl not available in PATH, skipping")
	}

	err = a.checkKubectlAvailable()
	if err != nil {
		t.Errorf("expected kubectl to be available: %v", err)
	}
}

func TestKubectlApplier_CheckAvailability_MissingBinary(t *testing.T) {
	a := NewKubectlApplierWithBinary("/nonexistent/kubectl")
	err := a.checkKubectlAvailable()
	if err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestKubectlWaiter_AreMachineDeploymentsReady_NoBinary(t *testing.T) {
	w := &KubectlWaiter{kubectlBinary: "/nonexistent/kubectl"}
	_, err := w.areMachineDeploymentsReady(context.Background(), &Cluster{
		Name:           "test",
		KubeconfigPath: "/tmp/fake",
	}, "test", "default")
	if err == nil {
		t.Error("expected error for missing kubectl")
	}
}
