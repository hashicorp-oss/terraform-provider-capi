// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"errors"
	"fmt"
)

var (
	// ErrBootstrapClusterCreate is returned when bootstrap cluster creation fails.
	ErrBootstrapClusterCreate = errors.New("failed to create bootstrap cluster")

	// ErrBootstrapClusterDelete is returned when bootstrap cluster deletion fails.
	ErrBootstrapClusterDelete = errors.New("failed to delete bootstrap cluster")

	// ErrCAPIInit is returned when CAPI initialization fails.
	ErrCAPIInit = errors.New("failed to initialize CAPI components")

	// ErrTemplateGenerate is returned when template generation fails.
	ErrTemplateGenerate = errors.New("failed to generate cluster template")

	// ErrManifestApply is returned when manifest application fails.
	ErrManifestApply = errors.New("failed to apply cluster manifest")

	// ErrManifestDelete is returned when manifest deletion fails.
	ErrManifestDelete = errors.New("failed to delete cluster manifest")

	// ErrClusterNotReady is returned when a cluster does not become ready in time.
	ErrClusterNotReady = errors.New("cluster did not become ready within timeout")

	// ErrCAPIMove is returned when CAPI management move fails.
	ErrCAPIMove = errors.New("failed to move CAPI management")

	// ErrKubeconfig is returned when kubeconfig retrieval fails.
	ErrKubeconfig = errors.New("failed to retrieve kubeconfig")
)

// BootstrapError wraps bootstrap-related errors with context.
type BootstrapError struct {
	ClusterName string
	Operation   string
	Err         error
}

func (e *BootstrapError) Error() string {
	return fmt.Sprintf("bootstrap cluster %q %s: %v", e.ClusterName, e.Operation, e.Err)
}

func (e *BootstrapError) Unwrap() error {
	return e.Err
}

// CAPIError wraps CAPI operation errors with context.
type CAPIError struct {
	Operation string
	Cluster   string
	Err       error
}

func (e *CAPIError) Error() string {
	return fmt.Sprintf("CAPI %s on cluster %q: %v", e.Operation, e.Cluster, e.Err)
}

func (e *CAPIError) Unwrap() error {
	return e.Err
}
