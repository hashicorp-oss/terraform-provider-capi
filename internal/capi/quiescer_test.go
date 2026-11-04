// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func workflow(name, state string) *unstructured.Unstructured {
	wf := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tinkerbell.org/v1alpha1", "kind": "Workflow",
		"metadata": map[string]any{"name": name, "namespace": "tinkerbell"},
	}}
	if state != "" {
		wf.Object["status"] = map[string]any{"state": state}
	}
	return wf
}

func fakeDynamic(objs ...runtime.Object) dynamic.Interface {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{workflowsGVR: "WorkflowList"}, objs...)
}

func TestTinkerbellQuiescer_IdleWhenNoActiveWorkflows(t *testing.T) {
	q := &TinkerbellQuiescer{clientFor: func(string) (dynamic.Interface, error) {
		return fakeDynamic(workflow("done", "SUCCESS"), workflow("failed", "STATE_FAILED"), workflow("new", "")), nil
	}}
	err := q.WaitForQuiescence(context.Background(), &Cluster{Name: "b", KubeconfigPath: "/x"}, "tinkerbell", WaitOptions{Timeout: time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("finished workflows must count as idle: %v", err)
	}
}

func TestTinkerbellQuiescer_WaitsForRunningWorkflows(t *testing.T) {
	q := &TinkerbellQuiescer{clientFor: func(string) (dynamic.Interface, error) {
		return fakeDynamic(workflow("imaging", "running"), workflow("queued", "STATE_PENDING")), nil
	}}
	err := q.WaitForQuiescence(context.Background(), &Cluster{Name: "b", KubeconfigPath: "/x"}, "tinkerbell", WaitOptions{Timeout: 20 * time.Millisecond, PollInterval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "imaging") || !strings.Contains(err.Error(), "queued") {
		t.Fatalf("running workflows must block and be named, got %v", err)
	}
}

func TestTinkerbellQuiescer_NoWorkflowCRDIsIdle(t *testing.T) {
	q := &TinkerbellQuiescer{clientFor: func(string) (dynamic.Interface, error) {
		// A cluster without Tinkerbell answers the list with NotFound.
		c := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{workflowsGVR: "WorkflowList"})
		c.PrependReactor("list", "workflows", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "tinkerbell.org", Resource: "workflows"}, "")
		})
		return c, nil
	}}
	err := q.WaitForQuiescence(context.Background(), &Cluster{Name: "b", KubeconfigPath: "/x"}, "tinkerbell", WaitOptions{Timeout: time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatalf("a cluster without the Workflow CRD is idle: %v", err)
	}
}
