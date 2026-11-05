// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"strings"
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
	var phases []Phase

	result, err := mgr.CreateCluster(ctx, CreateClusterOptions{
		Name:              "test-cluster",
		Namespace:         "default",
		Providers:         dockerProviders(),
		Variables:         map[string]string{"EXP_MACHINE_POOL": "true"},
		KubernetesVersion: "v1.31.0",
		Network:           NetworkConfig{ControlPlaneEndpoint: &APIEndpoint{Host: "10.0.0.160", Port: 6443}},
		ControlPlane:      ControlPlaneSpec{Replicas: &cpCount, Template: &TemplateRef{Kind: "DockerMachineTemplate", Name: "cp"}},
		WaitForReady:      true,
		OnPhase:           func(p Phase) { phases = append(phases, p) },
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
	if gen.Opts.ControlPlane.Replicas == nil || *gen.Opts.ControlPlane.Replicas != 1 || gen.Opts.ControlPlane.Template == nil || gen.Opts.ControlPlane.Template.Name != "cp" {
		t.Errorf("control plane not passed through: %+v", gen.Opts.ControlPlane)
	}
	if gen.Opts.Class != nil {
		t.Error("Class must be unset by default")
	}
	if len(phases) < 3 || phases[0] != PhaseBootstrapping || phases[1] != PhaseInitialized || phases[len(phases)-1] != PhaseReady || result.Phase != PhaseReady {
		t.Errorf("phases = %v, result phase = %s", phases, result.Phase)
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
	waiter := &MockWaiter{}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithApplier(applier),
		WithWaiter(waiter),
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
	if len(waiter.DeletedCalls) != 1 {
		t.Errorf("the delete must wait for the Cluster object to disappear, got %d waits", len(waiter.DeletedCalls))
	}
}

func TestManager_DeleteCluster_WithBootstrap(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	applier := &MockApplier{}

	mgr := NewManager(
		WithBootstrapper(bootstrapper),
		WithApplier(applier),
		WithWaiter(&MockWaiter{}),
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
		WithApplier(&MockApplier{}),
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
		WithQuiescer(&MockQuiescer{}),
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
		WithQuiescer(&MockQuiescer{}),
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
		Variables: map[string]string{"OTHER": "x"}, Class: &ClassRef{Name: "cc"},
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
	if len(generator.GenerateCalls) != 1 || generator.GenerateCalls[0].Opts.Class == nil || generator.GenerateCalls[0].Opts.Class.Name != "cc" {
		t.Error("generator must be asked for a topology on the class")
	}
}

func dockerProviders() ProviderSet {
	return ProviderSet{
		ProviderTypeInfrastructure: {{Type: ProviderTypeInfrastructure, Name: "docker"}},
		ProviderTypeBootstrap:      {{Type: ProviderTypeBootstrap, Name: "kubeadm"}},
		ProviderTypeControlPlane:   {{Type: ProviderTypeControlPlane, Name: "kubeadm"}},
	}
}

func tinkerbellProviders() ProviderSet {
	return ProviderSet{
		ProviderTypeInfrastructure: {{Type: ProviderTypeInfrastructure, Name: "tinkerbell"}},
		ProviderTypeBootstrap:      {{Type: ProviderTypeBootstrap, Name: "talos"}},
		ProviderTypeControlPlane:   {{Type: ProviderTypeControlPlane, Name: "talos"}},
	}
}

func inventoryTwoMachines() []InventoryMachine {
	return []InventoryMachine{
		{Hostname: "cp-1", IP: "10.0.0.5", MAC: "aa:bb:cc:dd:ee:01", Labels: map[string]string{"type": "cp"}},
		{Hostname: "worker-1", IP: "10.0.0.6", MAC: "aa:bb:cc:dd:ee:02", Labels: map[string]string{"type": "worker"}},
	}
}

func TestManager_CreateCluster_PivotLeavesBootstrapMachineOutOfInventory(t *testing.T) {
	applier := &MockApplier{}
	mgr := NewManager(
		WithBootstrapper(&MockBootstrapper{}), WithInstaller(&MockInstaller{}), WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(applier), WithWaiter(&MockWaiter{}), WithInfoRetriever(&MockKubeconfigRetriever{}), WithDescriber(&MockClusterDescriber{}),
		WithMover(&MockMover{}),
		WithQuiescer(&MockQuiescer{}),
	)
	_, err := mgr.CreateCluster(context.Background(), CreateClusterOptions{
		Name: "bm", Namespace: "tinkerbell", Providers: tinkerbellProviders(),
		SelfManaged: true, ManageInventory: true, Inventory: inventoryTwoMachines(), BootstrapMachine: "cp-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inventory := string(applier.ApplyCalls[0].Manifest)
	if strings.Contains(inventory, "name: cp-1") || strings.Contains(inventory, "provisioned") {
		t.Errorf("pivot: the bootstrap machine still hosts the management cluster and must not be offered to CAPT:\n%s", inventory)
	}
	if !strings.Contains(inventory, "name: worker-1") {
		t.Errorf("the other machines must be registered:\n%s", inventory)
	}
}

func TestManager_CreateCluster_InventoryOnlyForTinkerbell(t *testing.T) {
	applier := &MockApplier{}
	mgr := NewManager(
		WithBootstrapper(&MockBootstrapper{}), WithInstaller(&MockInstaller{}), WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(applier), WithWaiter(&MockWaiter{}), WithInfoRetriever(&MockKubeconfigRetriever{}), WithDescriber(&MockClusterDescriber{}),
	)
	_, err := mgr.CreateCluster(context.Background(), CreateClusterOptions{
		Name: "dev", Providers: dockerProviders(), ManageInventory: true, Inventory: inventoryTwoMachines(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applier.ApplyCalls) != 1 {
		t.Fatalf("docker clusters have no Tinkerbell inventory; got %d applies", len(applier.ApplyCalls))
	}
}

// pivotManager wires every collaborator with recording mocks for pivot tests.
type pivotMocks struct {
	bootstrapper *MockBootstrapper
	installer    *MockInstaller
	applier      *MockApplier
	mover        *MockMover
	waiter       *MockWaiter
	addons       *MockAddonInstaller
	quiescer     *MockQuiescer
	secrets      *MockSecretReader
	events       []string
}

func newPivotMocks() (*Manager, *pivotMocks) {
	pm := &pivotMocks{bootstrapper: &MockBootstrapper{}, installer: &MockInstaller{}, applier: &MockApplier{}, mover: &MockMover{}, waiter: &MockWaiter{}, addons: &MockAddonInstaller{}, quiescer: &MockQuiescer{},
		secrets: &MockSecretReader{Secrets: map[string]map[string]string{"tinkerbell/bm-talos": {"bundle": "secrets-yaml"}}}}
	record := func(e string) { pm.events = append(pm.events, e) }
	pm.installer.InitFunc = func(_ context.Context, c *Cluster, _ InitOptions) error { record("init:" + c.Name); return nil }
	pm.addons.InstallFunc = func(_ context.Context, c *Cluster, set AddonSet) error {
		names := []string{}
		for _, h := range set.Helm {
			names = append(names, h.Name)
		}
		record(fmt.Sprintf("addons:%s:%s:prereqs=%d", c.Name, strings.Join(names, "+"), len(set.Prerequisites)))
		return nil
	}
	pm.addons.UninstallFunc = func(_ context.Context, c *Cluster, set AddonSet) error {
		names := []string{}
		for _, h := range set.Helm {
			names = append(names, h.Name)
		}
		record("uninstall:" + c.Name + ":" + strings.Join(names, "+"))
		return nil
	}
	pm.quiescer.WaitFunc = func(_ context.Context, c *Cluster, _ string, _ WaitOptions) error {
		record("quiesce:" + c.Name)
		return nil
	}
	pm.applier.PauseFunc = func(_ context.Context, c *Cluster, name, _ string) error {
		record("pause:" + c.Name + ":" + name)
		return nil
	}
	pm.waiter.WaitForAddonsFunc = func(_ context.Context, c *Cluster, name, _ string, _ WaitOptions) error {
		record("wait-addons:" + c.Name + ":" + name)
		return nil
	}
	pm.mover.MoveFunc = func(_ context.Context, from, to *Cluster, _ MoveOptions) error {
		record("move:" + from.Name + ">" + to.Name)
		return nil
	}
	pm.applier.ApplyFunc = func(_ context.Context, c *Cluster, m []byte) error {
		kind := "cluster"
		switch {
		case strings.Contains(string(m), "kind: Hardware"):
			kind = "inventory"
		case strings.Contains(string(m), "unifi"):
			kind = "management-manifests"
		}
		record("apply:" + c.Name + ":" + kind)
		return nil
	}
	pm.waiter.WaitForControlPlaneFunc = func(_ context.Context, c *Cluster, _, _ string, _ WaitOptions) error {
		record("wait-cp:" + c.Name)
		return nil
	}
	pm.waiter.WaitForClusterReadyFunc = func(_ context.Context, c *Cluster, _, _ string, _ WaitOptions) error {
		record("wait-ready:" + c.Name)
		return nil
	}
	pm.bootstrapper.DeleteFunc = func(_ context.Context, c *Cluster) error { record("delete-bootstrap:" + c.Name); return nil }
	mgr := NewManager(
		WithBootstrapper(pm.bootstrapper), WithInstaller(pm.installer), WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(pm.applier), WithMover(pm.mover), WithWaiter(pm.waiter), WithInfoRetriever(&MockKubeconfigRetriever{}),
		WithDescriber(&MockClusterDescriber{}), WithAddonInstaller(pm.addons), WithQuiescer(pm.quiescer),
		WithSecretReader(pm.secrets),
	)
	return mgr, pm
}

func pivotOptions() CreateClusterOptions {
	return CreateClusterOptions{
		Name: "bm", Namespace: "tinkerbell", Providers: tinkerbellProviders(),
		SelfManaged: true, WaitForReady: true,
		ManageInventory: true, Inventory: inventoryTwoMachines(), BootstrapMachine: "cp-1",
		ManagementManifests: [][]byte{[]byte("kind: Secret\nmetadata:\n  name: unifi-credentials\n")},
		BootstrapAddons:     AddonSet{Helm: []HelmRelease{{Name: "tinkerbell"}}},
	}
}

func TestManager_CreateCluster_Pivot_Sequence(t *testing.T) {
	mgr, pm := newPivotMocks()
	result, err := mgr.CreateCluster(context.Background(), pivotOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		"init:bm-bootstrap",
		"addons:bm-bootstrap:tinkerbell:prereqs=0", // the bootstrap node's own Tinkerbell
		"apply:bm-bootstrap:inventory",             // without the bootstrap machine
		"apply:bm-bootstrap:management-manifests",
		"apply:bm-bootstrap:cluster",
		"wait-cp:bm-bootstrap",              // phase one: the first control plane machine
		"pause:bm-bootstrap:bm",             // no further Machines or Workflows
		"quiesce:bm-bootstrap",              // in-flight Workflows finish
		"uninstall:bm-bootstrap:tinkerbell", // the bootstrap responder goes away
		"wait-addons:bm-bootstrap:bm",       // the workload's own stack (CAAPH) is up
		"init:bm",
		"move:bm-bootstrap>bm",
		"apply:bm:inventory", // now with the bootstrap machine as a netboot target
		"apply:bm:management-manifests",
		"delete-bootstrap:bm-bootstrap",
		"wait-ready:bm", // phase two on the new management cluster
	}
	if got := strings.Join(pm.events, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("pivot sequence:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if result.BootstrapCluster != nil || result.Cluster.KubeconfigPath == "" {
		t.Errorf("result must point at the workload cluster with no bootstrap left: %+v", result)
	}
	if result.ProviderSecrets["bm-talos/bundle"] != "secrets-yaml" {
		t.Errorf("the root credential is captured from the workload cluster after the pivot: %v", result.ProviderSecrets)
	}
	if len(pm.secrets.Calls) != 1 || pm.secrets.Calls[0] != "bm:tinkerbell/bm-talos" {
		t.Errorf("secret reads = %v", pm.secrets.Calls)
	}
	first := string(pm.applier.ApplyCalls[0].Manifest)
	if strings.Contains(first, "name: cp-1") {
		t.Error("the bootstrap machine must not be offered to CAPT while it hosts the management cluster")
	}
	var afterMove string
	for _, c := range pm.applier.ApplyCalls {
		if c.Cluster.Name == "bm" && strings.Contains(string(c.Manifest), "kind: Hardware") {
			afterMove = string(c.Manifest)
		}
	}
	if !strings.Contains(afterMove, "name: cp-1") || strings.Contains(afterMove, "provisioned") || !strings.Contains(afterMove, "allowPXE: true") {
		t.Errorf("after the move the bootstrap machine is a plain netboot target:\n%s", afterMove)
	}
}

func TestManager_CreateCluster_Pivot_QuiescenceFailureCleansUpBootstrap(t *testing.T) {
	mgr, pm := newPivotMocks()
	pm.quiescer.WaitFunc = func(context.Context, *Cluster, string, WaitOptions) error {
		return fmt.Errorf("workflows still running")
	}
	_, err := mgr.CreateCluster(context.Background(), pivotOptions())
	if err == nil || !strings.Contains(err.Error(), "workflows still running") {
		t.Fatalf("expected the quiescence error, got %v", err)
	}
	if len(pm.mover.MoveCalls) != 0 || len(pm.addons.UninstallCalls) != 0 {
		t.Error("nothing may move and the bootstrap Tinkerbell must keep running while provisioning is in flight")
	}
	if len(pm.applier.PauseCalls) != 1 {
		t.Error("the Cluster is paused before the drain so no new Workflows start")
	}
	if len(pm.bootstrapper.DeleteCalls) != 1 {
		t.Error("the bootstrap cluster must be cleaned up on failure")
	}
}

func TestManager_CreateCluster_Pivot_AddonsNotReadyBlocksTheMove(t *testing.T) {
	mgr, pm := newPivotMocks()
	pm.waiter.WaitForAddonsFunc = func(context.Context, *Cluster, string, string, WaitOptions) error {
		return fmt.Errorf("tinkerbell: pending: chart not found")
	}
	_, err := mgr.CreateCluster(context.Background(), pivotOptions())
	if err == nil || !strings.Contains(err.Error(), "chart not found") {
		t.Fatalf("expected the addon error, got %v", err)
	}
	if len(pm.mover.MoveCalls) != 0 {
		t.Error("the move waits for the workload cluster's own provisioning stack")
	}
	if len(pm.addons.UninstallCalls) != 1 {
		t.Error("the bootstrap Tinkerbell is uninstalled before the workload's is waited for, so only one ever answers")
	}
}

func TestManager_CreateCluster_Pivot_NoRootSecretIsNotAnError(t *testing.T) {
	mgr, pm := newPivotMocks()
	pm.secrets.Secrets = nil
	result, err := mgr.CreateCluster(context.Background(), pivotOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ProviderSecrets) != 0 {
		t.Errorf("no secret, no root credential: %v", result.ProviderSecrets)
	}
	if len(pm.secrets.Calls) != 2 {
		t.Errorf("both candidate secrets are tried: %v", pm.secrets.Calls)
	}
}

func TestManager_CreateCluster_Pivot_MissingKubeconfigFails(t *testing.T) {
	mgr, pm := newPivotMocks()
	_ = pm
	retriever := &MockKubeconfigRetriever{GetKubeconfigFunc: func(context.Context, *Cluster, string, string) (string, error) { return "", fmt.Errorf("not yet") }}
	mgr2 := NewManager(WithBootstrapper(pm.bootstrapper), WithInstaller(pm.installer), WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(pm.applier), WithMover(pm.mover), WithWaiter(pm.waiter), WithInfoRetriever(retriever), WithDescriber(&MockClusterDescriber{}),
		WithAddonInstaller(pm.addons), WithQuiescer(pm.quiescer))
	_ = mgr
	_, err := mgr2.CreateCluster(context.Background(), pivotOptions())
	if err == nil || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Fatalf("a pivot without a workload kubeconfig must fail instead of deleting the bootstrap, got %v", err)
	}
	if len(pm.mover.MoveCalls) != 0 {
		t.Error("no move without a kubeconfig")
	}
}

func TestManager_CreateCluster_AddonsWithoutInstallerFail(t *testing.T) {
	mgr := NewManager(WithBootstrapper(&MockBootstrapper{}), WithInstaller(&MockInstaller{}), WithManifestGenerator(&MockManifestGenerator{}),
		WithApplier(&MockApplier{}), WithWaiter(&MockWaiter{}), WithInfoRetriever(&MockKubeconfigRetriever{}), WithDescriber(&MockClusterDescriber{}))
	_, err := mgr.CreateCluster(context.Background(), CreateClusterOptions{
		Name: "x", Providers: dockerProviders(), BootstrapAddons: AddonSet{Helm: []HelmRelease{{Name: "tinkerbell"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "no addon installer") {
		t.Fatalf("expected a missing-installer error, got %v", err)
	}
}
