// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"errors"
	"testing"
)

func TestBootstrapError(t *testing.T) {
	err := &BootstrapError{
		ClusterName: "test-cluster",
		Operation:   "create",
		Err:         ErrBootstrapClusterCreate,
	}

	if err.Error() != `bootstrap cluster "test-cluster" create: failed to create bootstrap cluster` {
		t.Errorf("unexpected error message: %s", err.Error())
	}

	if !errors.Is(err, ErrBootstrapClusterCreate) {
		t.Error("expected error to unwrap to ErrBootstrapClusterCreate")
	}
}

func TestCAPIError(t *testing.T) {
	err := &CAPIError{
		Operation: "init",
		Cluster:   "test-cluster",
		Err:       ErrCAPIInit,
	}

	if err.Error() != `CAPI init on cluster "test-cluster": failed to initialize CAPI components` {
		t.Errorf("unexpected error message: %s", err.Error())
	}

	if !errors.Is(err, ErrCAPIInit) {
		t.Error("expected error to unwrap to ErrCAPIInit")
	}
}

func TestBootstrapError_Unwrap(t *testing.T) {
	inner := errors.New("connection refused")
	err := &BootstrapError{
		ClusterName: "test",
		Operation:   "create",
		Err:         inner,
	}
	if !errors.Is(err, inner) {
		t.Error("Unwrap should return inner error")
	}
}

func TestCAPIError_Unwrap(t *testing.T) {
	inner := errors.New("timeout")
	err := &CAPIError{
		Operation: "move",
		Cluster:   "test",
		Err:       inner,
	}
	if !errors.Is(err, inner) {
		t.Error("Unwrap should return inner error")
	}
}
