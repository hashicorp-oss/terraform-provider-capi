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
	templateGen := &MockManifestGenerator{}
	applier := &MockApplier{}
	waiter := &MockWaiter{}
	retriever := &MockKubeconfigRetriever{}
	describer := &MockClusterDescriber{}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(installer),
		WithManifestGenerator(templateGen),
		WithApplier(applier),
		WithWaiter(waiter),
		WithInfoRetriever(retriever),
		WithDescriber(describer),
	)

	ctx := context.Background()
	cpCount := int64(1)
	workerCount := int64(2)

	result, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:               "test-cluster",
		Namespace:          "default",
		Providers:          dockerProviders(),
		Variables:          map[string]string{"EXP_MACHINE_POOL": "true"},
		KubernetesVersion:  "v1.31.0",
		Network:            NetworkConfig{ControlPlaneEndpoint: &APIEndpoint{Host: "10.0.0.160", Port: 6443}},
		ControlPlane:       ControlPlaneTopology{Replicas: &cpCount},
		MachineDeployments: []MachineDeploymentTopology{{Name: "md-0", Replicas: &workerCount}},
		WaitForReady:       true,
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
	if got := installer.InitCalls[0].Opts.Providers.InitStrings(ProviderTypeInfrastructure); len(got) != 1 || got[0] != "docker" {
		t.Errorf("expected infrastructure provider 'docker', got %v", got)
	}

	// Verify the manifest was generated on the management cluster from the
	// same provider set init used, with the cluster settings passed through.
	if len(templateGen.GenerateCalls) != 1 {
		t.Fatalf("expected 1 generate call, got %d", len(templateGen.GenerateCalls))
	}
	gen := templateGen.GenerateCalls[0]
	if gen.Cluster == nil || gen.Cluster.Name != "test-cluster-bootstrap" {
		t.Errorf("generation must run against the management (bootstrap) cluster, got %+v", gen.Cluster)
	}
	if gen.Opts.ClusterName != "test-cluster" || gen.Opts.Namespace != "default" || gen.Opts.KubernetesVersion != "v1.31.0" {
		t.Errorf("cluster identity not passed through: %+v", gen.Opts)
	}
	if got := gen.Opts.Providers.InitStrings(ProviderTypeInfrastructure); len(got) != 1 || got[0] != "docker" {
		t.Errorf("generation must receive the provider set (got %v) so it resolves the same names as init", got)
	}
	if gen.Opts.Network.ControlPlaneEndpoint == nil || gen.Opts.Network.ControlPlaneEndpoint.Host != "10.0.0.160" {
		t.Errorf("network not passed through: %+v", gen.Opts.Network)
	}
	if gen.Opts.ControlPlane.Replicas == nil || *gen.Opts.ControlPlane.Replicas != 1 {
		t.Errorf("control plane topology not passed through: %+v", gen.Opts.ControlPlane)
	}
	if len(gen.Opts.MachineDeployments) != 1 || gen.Opts.MachineDeployments[0].Name != "md-0" {
		t.Errorf("machine deployments not passed through: %+v", gen.Opts.MachineDeployments)
	}
	if gen.Opts.ClusterClass {
		t.Error("ClusterClass must be off by default")
	}
	if installer.InitCalls[0].Opts.Variables["EXP_MACHINE_POOL"] != "true" {
		t.Error("init must receive the caller's variables")
	}
	if _, set := installer.InitCalls[0].Opts.Variables["CLUSTER_TOPOLOGY"]; set {
		t.Error("CLUSTER_TOPOLOGY must not be injected without ClusterClass")
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
	templateGen := &MockManifestGenerator{}
	applier := &MockApplier{}
	waiter := &MockWaiter{}
	retriever := &MockKubeconfigRetriever{}
	describer := &MockClusterDescriber{}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(installer),
		WithManifestGenerator(templateGen),
		WithApplier(applier),
		WithWaiter(waiter),
		WithInfoRetriever(retriever),
		WithDescriber(describer),
	)

	ctx := context.Background()
	result, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                 "test-cluster",
		ManagementKubeconfig: "/tmp/mgmt-kubeconfig",
		Providers:            dockerProviders(),
		WaitForReady:         false,
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
	templateGen := &MockManifestGenerator{}
	applier := &MockApplier{}
	waiter := &MockWaiter{}
	retriever := &MockKubeconfigRetriever{}
	describer := &MockClusterDescriber{}

	mgr := NewManager(
		WithBootstrapper(&MockBootstrapper{}),
		WithInstaller(installer),
		WithManifestGenerator(templateGen),
		WithApplier(applier),
		WithWaiter(waiter),
		WithInfoRetriever(retriever),
		WithDescriber(describer),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                 "test-cluster",
		ManagementKubeconfig: "/tmp/mgmt-kubeconfig",
		Providers:            dockerProviders(),
		SkipInit:             true,
		WaitForReady:         false,
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
		WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:      "test-cluster",
		Providers: dockerProviders(),
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
	templateGen := &MockManifestGenerator{
		GenerateFunc: func(ctx context.Context, cluster *Cluster, opts GenerateOptions) ([]byte, error) {
			return nil, fmt.Errorf("template generation failed")
		},
	}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(&MockInstaller{}),
		WithManifestGenerator(templateGen),
		WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:      "test-cluster",
		Providers: dockerProviders(),
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
		WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(applier),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:      "test-cluster",
		Providers: dockerProviders(),
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
		WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(&MockApplier{}),
		WithWaiter(waiter),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:         "test-cluster",
		Providers:    dockerProviders(),
		WaitForReady: true,
	})
	if err == nil {
		t.Fatal("expected error")
	}

	if len(bootstrapper.DeleteCalls) != 1 {
		t.Fatalf("expected 1 bootstrap delete call (cleanup), got %d", len(bootstrapper.DeleteCalls))
	}
}

func TestManager_CreateCluster_DefaultNamespace(t *testing.T) {
	templateGen := &MockManifestGenerator{}

	mgr := NewManager(
		WithBootstrapper(&MockBootstrapper{}),
		WithInstaller(&MockInstaller{}),
		WithManifestGenerator(templateGen),
		WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	result, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:                 "test-cluster",
		ManagementKubeconfig: "/tmp/mgmt-kubeconfig",
		Providers:            dockerProviders(),
		WaitForReady:         false,
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
		WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(&MockApplier{}),
		WithMover(mover),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(retriever),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	result, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:         "self-managed",
		Providers:    dockerProviders(),
		SelfManaged:  true,
		WaitForReady: true,
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

func TestManager_CreateCluster_SelfManaged_RetriesMoveTransientErrors(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	installer := &MockInstaller{}
	retriever := &MockKubeconfigRetriever{}

	moveCalls := 0
	mover := &MockMover{
		MoveFunc: func(ctx context.Context, from, to *Cluster, opts MoveOptions) error {
			moveCalls++
			if moveCalls < 3 {
				return fmt.Errorf("connection reset by peer")
			}
			return nil
		},
	}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithInstaller(installer),
		WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(&MockApplier{}),
		WithMover(mover),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(retriever),
		WithDescriber(&MockClusterDescriber{}),
	)

	ctx := context.Background()
	_, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:         "self-managed-retry",
		Providers:    dockerProviders(),
		SelfManaged:  true,
		WaitForReady: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if moveCalls != 3 {
		t.Fatalf("expected move to be retried 3 times, got %d", moveCalls)
	}
}

func TestManager_CreateCluster_ClusterClassEnablesTopologyGate(t *testing.T) {
	installer := &MockInstaller{}
	generator := &MockManifestGenerator{}
	mgr := NewManager(
		WithBootstrapper(&MockBootstrapper{}),
		WithInstaller(installer),
		WithManifestGenerator(generator),
		WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}),
		WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}),
	)

	_, err := mgr.CreateCluster(context.Background(), CreateClusterOptions{
		Name: "cc", Namespace: "default", Providers: dockerProviders(),
		Variables: map[string]string{"OTHER": "x"}, ClusterClass: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(installer.InitCalls) != 1 {
		t.Fatalf("expected 1 init call, got %d", len(installer.InitCalls))
	}
	vars := installer.InitCalls[0].Opts.Variables
	if vars["CLUSTER_TOPOLOGY"] != "true" || vars["OTHER"] != "x" {
		t.Errorf("init variables = %v, want CLUSTER_TOPOLOGY=true added to the caller's", vars)
	}
	if len(generator.GenerateCalls) != 1 || !generator.GenerateCalls[0].Opts.ClusterClass {
		t.Error("generator must be asked for ClusterClass output")
	}
}

func dockerProviders() ProviderSet {
	return ProviderSet{
		ProviderTypeInfrastructure: {{Type: ProviderTypeInfrastructure, Name: "docker"}},
		ProviderTypeBootstrap:      {{Type: ProviderTypeBootstrap, Name: "kubeadm"}},
		ProviderTypeControlPlane:   {{Type: ProviderTypeControlPlane, Name: "kubeadm"}},
	}
}
