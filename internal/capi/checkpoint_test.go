// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// workloadKubeconfig is a kubeconfig with a current context, so its
// endpoint can be read.
const workloadKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://10.1.40.100:6443
  name: bm
contexts:
- context:
    cluster: bm
    user: admin
  name: admin@bm
current-context: admin@bm
users:
- name: admin
  user:
    token: t
`

func TestCheckpoint_MarkAndDone(t *testing.T) {
	var none *Checkpoint
	if none.Done(StepMoved) {
		t.Error("a nil checkpoint records nothing")
	}
	cp := &Checkpoint{}
	cp.Mark(StepMoved)
	cp.Mark(StepMoved)
	if !cp.Done(StepMoved) || len(cp.Steps) != 1 {
		t.Errorf("steps = %v", cp.Steps)
	}
	cp.Bootstrap = &BootstrapRecord{Type: "kind", Host: "runner-a"}
	cp.recordBootstrap("bm-bootstrap")
	if cp.Bootstrap.Name != "bm-bootstrap" || cp.Bootstrap.Host != "runner-a" {
		t.Errorf("recording the name keeps the caller's facts: %+v", cp.Bootstrap)
	}
}

func TestManager_CreateCluster_Pivot_RecordsTheCheckpoint(t *testing.T) {
	mgr, _ := newPivotMocks()
	opts := pivotOptions()
	cp := &Checkpoint{}
	opts.Checkpoint = cp
	if _, err := mgr.CreateCluster(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	for _, s := range []Step{StepBootstrapAddonsRemoved, StepMoved, StepBootstrapDeleted} {
		if !cp.Done(s) {
			t.Errorf("step %s not recorded: %v", s, cp.Steps)
		}
	}
	if cp.Bootstrap == nil || cp.Bootstrap.Name != "bm-bootstrap" || !strings.Contains(cp.WorkloadKubeconfig, "server:") {
		t.Errorf("checkpoint = %+v", cp)
	}
}

// A failure after the move is recorded at phase moved with what reaches
// the cluster, and the bootstrap cluster (which holds nothing) is kept.
func TestManager_CreateCluster_Pivot_FailureAfterTheMove(t *testing.T) {
	mgr, pm := newPivotMocks()
	pm.applier.ApplyFunc = func(_ context.Context, c *Cluster, _ []byte) error {
		if c.Name == "bm" {
			return errors.New("webhook not ready")
		}
		return nil
	}
	opts := pivotOptions()
	cp := &Checkpoint{}
	opts.Checkpoint = cp
	_, err := mgr.CreateCluster(context.Background(), opts)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != PhaseMoved {
		t.Fatalf("err = %v", err)
	}
	if pe.Result == nil || pe.Result.Kubeconfig == "" {
		t.Errorf("the partial result carries what reaches the cluster: %+v", pe.Result)
	}
	if pe.Bootstrap == nil || len(pm.bootstrapper.DeleteCalls) != 0 {
		t.Error("the bootstrap cluster stays for the resume")
	}
	if !cp.Done(StepMoved) || cp.Done(StepBootstrapDeleted) || cp.WorkloadKubeconfig == "" {
		t.Errorf("checkpoint = %+v", cp)
	}
}

// The resume after the move works on the cluster itself: nothing is created,
// installed or applied on the bootstrap cluster again.
func TestManager_CreateCluster_ResumesAfterTheMove(t *testing.T) {
	mgr, pm := newPivotMocks()
	opts := pivotOptions()
	cp := &Checkpoint{
		Steps:              []Step{StepBootstrapAddonsRemoved, StepMoved},
		Bootstrap:          &BootstrapRecord{Type: "kind", Name: "bm-bootstrap"},
		WorkloadKubeconfig: workloadKubeconfig,
	}
	opts.Checkpoint = cp
	var phases []Phase
	opts.OnPhase = func(p Phase) { phases = append(phases, p) }
	result, err := mgr.CreateCluster(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"apply:bm:inventory",
		"apply:bm:management-manifests",
		"delete-bootstrap:bm-bootstrap",
		"wait-ready:bm",
	}
	if got := strings.Join(pm.events, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("resume sequence:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if len(pm.bootstrapper.CreateCalls) != 0 {
		t.Error("the bootstrap cluster is not created or reused after the move")
	}
	if result.Endpoint != "https://10.1.40.100:6443" || result.ProviderSecrets["bm-talos/bundle"] != "secrets-yaml" || result.Phase != PhaseReady {
		t.Errorf("result = %+v", result)
	}
	if !cp.Done(StepBootstrapDeleted) {
		t.Error("the teardown is recorded")
	}
	if phases[0] != PhaseMoved || phases[len(phases)-1] != PhaseReady {
		t.Errorf("phases = %v", phases)
	}

	// Once torn down, the bootstrap is never touched again: a Kairos node is
	// a cluster member by then.
	mgr, pm = newPivotMocks()
	opts.Checkpoint = &Checkpoint{Steps: []Step{StepMoved, StepBootstrapDeleted}, Bootstrap: cp.Bootstrap, WorkloadKubeconfig: cp.WorkloadKubeconfig}
	if _, err := mgr.CreateCluster(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(pm.bootstrapper.DeleteCalls) != 0 {
		t.Error("a deleted bootstrap is not deleted again")
	}
}

func TestManager_CreateCluster_ResumeAfterTheMoveNeedsTheKubeconfig(t *testing.T) {
	mgr, _ := newPivotMocks()
	opts := pivotOptions()
	opts.Checkpoint = &Checkpoint{Steps: []Step{StepMoved}}
	_, err := mgr.CreateCluster(context.Background(), opts)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != PhaseMoved || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Fatalf("err = %v", err)
	}
}

// A resume after the handover removed the bootstrap Tinkerbell must not
// install it again: the workload's already answers DHCP.
func TestManager_CreateCluster_ResumeDoesNotReinstallRemovedBootstrapAddons(t *testing.T) {
	mgr, pm := newPivotMocks()
	opts := pivotOptions()
	opts.Checkpoint = &Checkpoint{Steps: []Step{StepBootstrapAddonsRemoved}}
	if _, err := mgr.CreateCluster(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	for _, e := range pm.events {
		if strings.HasPrefix(e, "addons:") || strings.HasPrefix(e, "uninstall:") {
			t.Errorf("the removed bootstrap addons are left alone, got %s", e)
		}
	}
}

// The readiness wait after the pivot keeps what the pivot captured, so the
// resume can reach the cluster through its root credential.
func TestManager_CreateCluster_ReadinessFailureAfterThePivotKeepsTheCredential(t *testing.T) {
	mgr, pm := newPivotMocks()
	pm.waiter.WaitForClusterReadyFunc = func(context.Context, *Cluster, string, string, WaitOptions) error {
		return fmt.Errorf("workers not ready")
	}
	_, err := mgr.CreateCluster(context.Background(), pivotOptions())
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != PhasePivoted {
		t.Fatalf("err = %v", err)
	}
	if pe.Result == nil || pe.Result.ProviderSecrets["bm-talos/bundle"] != "secrets-yaml" || pe.Bootstrap != nil {
		t.Errorf("the credential travels with the error and the bootstrap is gone: %+v %+v", pe.Result, pe.Bootstrap)
	}
}

func TestManager_DeleteCluster_SelfManagedRecordsTheMove(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	waiter := &MockWaiter{WaitForClusterDeletedFunc: func(context.Context, *Cluster, string, string, WaitOptions) error {
		return errors.New("machines still deleting")
	}}
	mgr := NewManager(
		WithBootstrapper(bootstrapper), WithInstaller(&MockInstaller{}), WithApplier(&MockApplier{}),
		WithQuiescer(&MockQuiescer{}), WithMover(&MockMover{}), WithWaiter(waiter),
	)
	cp := &DeleteCheckpoint{}
	err := mgr.DeleteCluster(context.Background(), DeleteClusterOptions{
		Name: "test", ManagementKubeconfig: "/tmp/workload", SelfManaged: true,
		Spec: CreateClusterOptions{Providers: dockerProviders()}, Checkpoint: cp,
	})
	if err == nil {
		t.Fatal("expected the wait error")
	}
	if cp.MovedTo == nil || cp.MovedTo.Name != "test-bootstrap" {
		t.Errorf("the move is recorded before the wait fails: %+v", cp.MovedTo)
	}
}

// With the objects recorded on a bootstrap cluster that no longer exists,
// the deletion fails instead of reporting a cluster that may still run as
// deleted.
func TestManager_DeleteCluster_SelfManagedLostBootstrapIsAnError(t *testing.T) {
	bootstrapper := &MockBootstrapper{ExistsFunc: func(context.Context, string) (bool, error) { return false, nil }}
	applier := &MockApplier{GetFunc: func(_ context.Context, _ *Cluster, name, _ string) (*unstructured.Unstructured, error) {
		return nil, notFound(name)
	}}
	mgr := NewManager(WithBootstrapper(bootstrapper), WithApplier(applier), WithWaiter(&MockWaiter{}))
	err := mgr.DeleteCluster(context.Background(), DeleteClusterOptions{
		Name: "test", ManagementKubeconfig: "/tmp/workload", SelfManaged: true,
		Checkpoint: &DeleteCheckpoint{MovedTo: &BootstrapRecord{Type: "kind", Name: "test-bootstrap", Host: "runner-a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("err = %v", err)
	}
}
