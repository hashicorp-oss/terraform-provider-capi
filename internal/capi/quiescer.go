// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// workflowsGVR is the Tinkerbell Workflow resource tink-server executes.
var workflowsGVR = schema.GroupVersionResource{Group: "tinkerbell.org", Version: "v1alpha1", Resource: "workflows"}

// activeWorkflowStates are the Workflow states in which an agent is still
// talking to the tink-server that booted it (upstream and legacy spellings).
var activeWorkflowStates = map[string]bool{
	"PREPARING": true, "PENDING": true, "RUNNING": true, "POST": true,
	"STATE_PENDING": true, "STATE_RUNNING": true,
}

// TinkerbellQuiescer implements Quiescer by polling Tinkerbell Workflows in
// the cluster namespace until none is active. A cluster without the Workflow
// CRD (no Tinkerbell) is quiescent.
type TinkerbellQuiescer struct {
	clientFor func(kubeconfigPath string) (dynamic.Interface, error)
}

// NewTinkerbellQuiescer returns a Quiescer backed by the dynamic client.
func NewTinkerbellQuiescer() *TinkerbellQuiescer {
	return &TinkerbellQuiescer{clientFor: buildDynamicClient}
}

// WaitForQuiescence implements Quiescer.
func (q *TinkerbellQuiescer) WaitForQuiescence(ctx context.Context, mgmtCluster *Cluster, namespace string, opts WaitOptions) error {
	dynClient, err := q.clientFor(mgmtCluster.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", mgmtCluster.Name, err)
	}
	namespace, timeout, pollInterval := waitDefaults(namespace, opts)
	deadline := time.Now().Add(timeout)
	for {
		active, err := activeWorkflows(ctx, dynClient, namespace)
		if err != nil {
			return err
		}
		if len(active) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for Tinkerbell workflows to finish in %s: %s", namespace, strings.Join(active, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// activeWorkflows lists the names of Workflows still running. A missing
// Workflow CRD means there is no Tinkerbell here and nothing can be running.
func activeWorkflows(ctx context.Context, dynClient dynamic.Interface, namespace string) ([]string, error) {
	list, err := dynClient.Resource(workflowsGVR).Namespace(namespace).List(ctx, meta_v1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing Tinkerbell workflows in %s: %w", namespace, err)
	}
	var active []string
	for _, wf := range list.Items {
		if isActiveWorkflow(&wf) {
			active = append(active, wf.GetName())
		}
	}
	return active, nil
}

func isActiveWorkflow(wf *unstructured.Unstructured) bool {
	state, _, _ := unstructured.NestedString(wf.Object, "status", "state")
	return activeWorkflowStates[strings.ToUpper(state)]
}
