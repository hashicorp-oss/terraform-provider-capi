// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"testing"
)

func TestManager_CreateCluster_FullWorkflow(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	installer := &MockInstaller{}
	templateGen := &MockTemplateGenerator{}
	applier := &MockApplier{}
	waiter := &MockWaiter{}
	retriever := &MockKubeconfigRetriever{}
	describer := &MockClusterDescriber{}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(installer),
		WithTemplateGenerator(templateGen),
		WithApplier(applier),
		WithWaiter(waiter),
		WithInfoRetriever(retriever),
		WithDescriber(describer),
	)

	ctx := context.Background()
	cpCount := int64(1)
	workerCount := int64(2)

	result, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                     "test-cluster",
		Namespace:                "default",
		InfrastructureProvider:   "docker",
		BootstrapProvider:        "kubeadm",
		ControlPlaneProvider:     "kubeadm",
		KubernetesVersion:        "v1.31.0",
		ControlPlaneMachineCount: &cpCount,
		WorkerMachineCount:       &workerCount,
		WaitForReady:             true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify bootstrap cluster was created (no management_kubeconfig provided)
	if len(bootstrapper.CreateCalls) != 1 {
		t.Fatalf("expected 1 bootstrap create call, got %d", len(bootstrapper.CreateCalls))
	}
	if bootstrapper.CreateCalls[0].Name != "test-cluster-bootstrap" {
		t.Errorf("expected bootstrap name 'test-cluster-bootstrap', got %q", bootstrapper.CreateCalls[0].Name)
	}

	// Verify CAPI init was called
	if len(installer.InitCalls) != 1 {
		t.Fatalf("expected 1 init call, got %d", len(installer.InitCalls))
	}
	if installer.InitCalls[0].Opts.InfrastructureProviders[0] != "docker" {
		t.Errorf("expected infrastructure provider 'docker', got %q", installer.InitCalls[0].Opts.InfrastructureProviders[0])
	}

	// Verify template was generated
	if len(templateGen.GenerateCalls) != 1 {
		t.Fatalf("expected 1 template gen call, got %d", len(templateGen.GenerateCalls))
	}
	if templateGen.GenerateCalls[0].Opts.ClusterName != "test-cluster" {
		t.Errorf("expected cluster name 'test-cluster', got %q", templateGen.GenerateCalls[0].Opts.ClusterName)
	}

	// Verify template was applied
	if len(applier.ApplyCalls) != 1 {
		t.Fatalf("expected 1 apply call, got %d", len(applier.ApplyCalls))
	}

	// Verify result
	if result.Cluster.Name != "test-cluster" {
		t.Errorf("expected cluster name 'test-cluster', got %q", result.Cluster.Name)
	}
	if result.Kubeconfig == "" {
		t.Error("expected kubeconfig to be populated")
	}
	if result.ClusterDescription == "" {
		t.Error("expected cluster description to be populated")
	}
}

func TestManager_CreateCluster_WithExistingManagement(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	installer := &MockInstaller{}
	templateGen := &MockTemplateGenerator{}
	applier := &MockApplier{}
	waiter := &MockWaiter{}
	retriever := &MockKubeconfigRetriever{}
	describer := &MockClusterDescriber{}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(installer),
		WithTemplateGenerator(templateGen),
		WithApplier(applier),
		WithWaiter(waiter),
		WithInfoRetriever(retriever),
		WithDescriber(describer),
	)

	ctx := context.Background()
	result, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                   "test-cluster",
		ManagementKubeconfig:   "/tmp/mgmt-kubeconfig",
		InfrastructureProvider: "docker",
		WaitForReady:           false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify NO bootstrap cluster was created
	if len(bootstrapper.CreateCalls) != 0 {
		t.Fatalf("expected 0 bootstrap create calls, got %d", len(bootstrapper.CreateCalls))
	}

	// Verify CAPI init was called with the management kubeconfig
	if len(installer.InitCalls) != 1 {
		t.Fatalf("expected 1 init call, got %d", len(installer.InitCalls))
	}
	if installer.InitCalls[0].Cluster.KubeconfigPath != "/tmp/mgmt-kubeconfig" {
		t.Errorf("expected kubeconfig '/tmp/mgmt-kubeconfig', got %q", installer.InitCalls[0].Cluster.KubeconfigPath)
	}

	if result.BootstrapCluster != nil {
		t.Error("expected no bootstrap cluster in result")
	}
}

func TestManager_CreateCluster_SkipInit(t *testing.T) {
	installer := &MockInstaller{}
	templateGen := &MockTemplateGenerator{}
	applier := &MockApplier{}
	waiter := &MockWaiter{}
	retriever := &MockKubeconfigRetriever{}
	describer := &MockClusterDescriber{}

	mgr := NewManager(
		WithBootstrapper(&MockBootstrapper{}),
		WithInstaller(installer),
		WithTemplateGenerator(templateGen),
		WithApplier(applier),
		WithWaiter(waiter),
		WithInfoRetriever(retriever),
		WithDescriber(describer),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                   "test-cluster",
		ManagementKubeconfig:   "/tmp/mgmt-kubeconfig",
		InfrastructureProvider: "docker",
		SkipInit:               true,
		WaitForReady:           false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify CAPI init was NOT called
	if len(installer.InitCalls) != 0 {
		t.Fatalf("expected 0 init calls when skip_init=true, got %d", len(installer.InitCalls))
	}

	// Verify template was still generated and applied
	if len(templateGen.GenerateCalls) != 1 {
		t.Fatalf("expected 1 template gen call, got %d", len(templateGen.GenerateCalls))
	}
	if len(applier.ApplyCalls) != 1 {
		t.Fatalf("expected 1 apply call, got %d", len(applier.ApplyCalls))
	}
}

func TestManager_CreateCluster_InitFailure_CleansUpBootstrap(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	installer := &MockInstaller{
		InitFunc: func(ctx context.Context, cluster *Cluster, opts InitOptions) error {
			return fmt.Errorf("init failed")
		},
	}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(installer),
		WithTemplateGenerator(&MockTemplateGenerator{}),
		WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                   "test-cluster",
		InfrastructureProvider: "docker",
	})
	if err == nil {
		t.Fatal("expected error")
	}

	// Verify bootstrap was created then deleted on cleanup
	if len(bootstrapper.CreateCalls) != 1 {
		t.Fatalf("expected 1 bootstrap create call, got %d", len(bootstrapper.CreateCalls))
	}
	if len(bootstrapper.DeleteCalls) != 1 {
		t.Fatalf("expected 1 bootstrap delete call (cleanup), got %d", len(bootstrapper.DeleteCalls))
	}
}

func TestManager_CreateCluster_TemplateFailure_CleansUpBootstrap(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	templateGen := &MockTemplateGenerator{
		GenerateFunc: func(ctx context.Context, cluster *Cluster, opts TemplateOptions) ([]byte, error) {
			return nil, fmt.Errorf("template generation failed")
		},
	}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(&MockInstaller{}),
		WithTemplateGenerator(templateGen),
		WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                   "test-cluster",
		InfrastructureProvider: "docker",
	})
	if err == nil {
		t.Fatal("expected error")
	}

	// Verify cleanup
	if len(bootstrapper.DeleteCalls) != 1 {
		t.Fatalf("expected 1 bootstrap delete call (cleanup), got %d", len(bootstrapper.DeleteCalls))
	}
}

func TestManager_CreateCluster_ApplyFailure_CleansUpBootstrap(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	applier := &MockApplier{
		ApplyFunc: func(ctx context.Context, cluster *Cluster, manifest []byte) error {
			return fmt.Errorf("apply failed")
		},
	}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(&MockInstaller{}),
		WithTemplateGenerator(&MockTemplateGenerator{}),
		WithApplier(applier),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                   "test-cluster",
		InfrastructureProvider: "docker",
	})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(bootstrapper.DeleteCalls) != 1 {
		t.Fatalf("expected 1 bootstrap delete call (cleanup), got %d", len(bootstrapper.DeleteCalls))
	}
}

func TestManager_CreateCluster_WaitFailure_CleansUpBootstrap(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	waiter := &MockWaiter{
		WaitForClusterReadyFunc: func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
			return fmt.Errorf("timeout waiting for cluster")
		},
	}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(&MockInstaller{}),
		WithTemplateGenerator(&MockTemplateGenerator{}),
		WithApplier(&MockApplier{}),
		WithWaiter(waiter),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                   "test-cluster",
		InfrastructureProvider: "docker",
		WaitForReady:           true,
	})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(bootstrapper.DeleteCalls) != 1 {
		t.Fatalf("expected 1 bootstrap delete call (cleanup), got %d", len(bootstrapper.DeleteCalls))
	}
}

func TestManager_CreateCluster_DefaultNamespace(t *testing.T) {
	templateGen := &MockTemplateGenerator{}

	mgr := NewManager(
		WithBootstrapper(&MockBootstrapper{}),
		WithInstaller(&MockInstaller{}),
		WithTemplateGenerator(templateGen),
		WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	result, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                   "test-cluster",
		ManagementKubeconfig:   "/tmp/mgmt-kubeconfig",
		InfrastructureProvider: "docker",
		WaitForReady:           false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should default to "default" namespace
	if templateGen.GenerateCalls[0].Opts.Namespace != "default" {
		t.Errorf("expected default namespace, got %q", templateGen.GenerateCalls[0].Opts.Namespace)
	}
	if result.Cluster.Namespace != "default" {
		t.Errorf("expected default namespace in result, got %q", result.Cluster.Namespace)
	}
}

func TestManager_DeleteCluster(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	applier := &MockApplier{}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithApplier(applier),
	)

	ctx := context.Background()
	err := mgr.DeleteCluster(ctx, DeleteClusterOptions{
		Name:                 "test-cluster",
		Namespace:            "default",
		ManagementKubeconfig: "/tmp/mgmt-kubeconfig",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(applier.DeleteCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(applier.DeleteCalls))
	}
	if applier.DeleteCalls[0].ClusterName != "test-cluster" {
		t.Errorf("expected cluster name 'test-cluster', got %q", applier.DeleteCalls[0].ClusterName)
	}
}

func TestManager_DeleteCluster_WithBootstrap(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	applier := &MockApplier{}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithApplier(applier),
	)

	ctx := context.Background()
	err := mgr.DeleteCluster(ctx, DeleteClusterOptions{
		Name:                 "test-cluster",
		Namespace:            "default",
		ManagementKubeconfig: "/tmp/mgmt-kubeconfig",
		DeleteBootstrap:      true,
		BootstrapName:        "test-cluster-bootstrap",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify both cluster and bootstrap were deleted
	if len(applier.DeleteCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(applier.DeleteCalls))
	}
	if len(bootstrapper.DeleteCalls) != 1 {
		t.Fatalf("expected 1 bootstrap delete call, got %d", len(bootstrapper.DeleteCalls))
	}
	if bootstrapper.DeleteCalls[0].Name != "test-cluster-bootstrap" {
		t.Errorf("expected bootstrap name 'test-cluster-bootstrap', got %q", bootstrapper.DeleteCalls[0].Name)
	}
}

func TestManager_DeleteCluster_NoManagementKubeconfig(t *testing.T) {
	mgr := NewManager()

	ctx := context.Background()
	err := mgr.DeleteCluster(ctx, DeleteClusterOptions{
		Name: "test-cluster",
	})
	if err == nil {
		t.Fatal("expected error when no management kubeconfig provided")
	}
}

func TestManager_GetClusterInfo(t *testing.T) {
	retriever := &MockKubeconfigRetriever{
		GetKubeconfigFunc: func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error) {
			return "mock-kubeconfig-content", nil
		},
	}
	describer := &MockClusterDescriber{
		DescribeFunc: func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error) {
			return "NAME: test\nSTATUS: Ready", nil
		},
	}

	mgr := NewManager(
		WithInfoRetriever(retriever),
		WithDescriber(describer),
	)

	ctx := context.Background()
	result, err := mgr.GetClusterInfo(ctx, "/tmp/mgmt-kubeconfig", "test-cluster", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Kubeconfig != "mock-kubeconfig-content" {
		t.Errorf("expected mock kubeconfig, got %q", result.Kubeconfig)
	}
	if result.ClusterDescription != "NAME: test\nSTATUS: Ready" {
		t.Errorf("expected mock description, got %q", result.ClusterDescription)
	}
}

func TestManager_CreateCluster_SelfManaged(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	installer := &MockInstaller{}
	mover := &MockMover{}
	retriever := &MockKubeconfigRetriever{}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(installer),
		WithTemplateGenerator(&MockTemplateGenerator{}),
		WithApplier(&MockApplier{}),
		WithMover(mover),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(retriever),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	result, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                   "self-managed",
		InfrastructureProvider: "docker",
		SelfManaged:            true,
		WaitForReady:           true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should create bootstrap, init, apply, wait, init on workload, move, delete bootstrap
	if len(bootstrapper.CreateCalls) != 1 {
		t.Fatalf("expected 1 bootstrap create, got %d", len(bootstrapper.CreateCalls))
	}

	// CAPI init called twice: once on bootstrap, once on workload for pivot
	if len(installer.InitCalls) != 2 {
		t.Fatalf("expected 2 init calls (bootstrap + workload), got %d", len(installer.InitCalls))
	}

	// Move should be called once
	if len(mover.MoveCalls) != 1 {
		t.Fatalf("expected 1 move call, got %d", len(mover.MoveCalls))
	}

	// Bootstrap should be deleted after successful pivot
	if len(bootstrapper.DeleteCalls) != 1 {
		t.Fatalf("expected 1 bootstrap delete call (post-pivot), got %d", len(bootstrapper.DeleteCalls))
	}

	// Result should have no bootstrap cluster (it was cleaned up)
	if result.BootstrapCluster != nil {
		t.Error("expected nil bootstrap cluster after successful pivot")
	}
}
