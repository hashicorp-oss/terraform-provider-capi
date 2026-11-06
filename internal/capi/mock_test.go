// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// MockBootstrapper is a test double for the Bootstrapper interface.
type MockBootstrapper struct {
	CreateFunc  func(ctx context.Context, opts BootstrapOptions) (*Cluster, error)
	DeleteFunc  func(ctx context.Context, cluster *Cluster) error
	ExistsFunc  func(ctx context.Context, name string) (bool, error)
	CreateCalls []BootstrapOptions
	DeleteCalls []*Cluster
}

func (m *MockBootstrapper) Create(ctx context.Context, opts BootstrapOptions) (*Cluster, error) {
	m.CreateCalls = append(m.CreateCalls, opts)
	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, opts)
	}
	return &Cluster{Name: opts.Name, KubeconfigPath: "/tmp/mock-kubeconfig"}, nil
}

func (m *MockBootstrapper) Delete(ctx context.Context, cluster *Cluster) error {
	m.DeleteCalls = append(m.DeleteCalls, cluster)
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, cluster)
	}
	return nil
}

func (m *MockBootstrapper) Exists(ctx context.Context, name string) (bool, error) {
	if m.ExistsFunc != nil {
		return m.ExistsFunc(ctx, name)
	}
	return false, nil
}

// MockInstaller is a test double for the Installer interface.
type MockInstaller struct {
	InitFunc  func(ctx context.Context, cluster *Cluster, opts InitOptions) error
	InitCalls []struct {
		Cluster *Cluster
		Opts    InitOptions
	}
	UpgradeFunc  func(ctx context.Context, cluster *Cluster, opts UpgradeOptions) error
	UpgradeCalls []struct {
		Cluster *Cluster
		Opts    UpgradeOptions
	}
	UninstallFunc  func(ctx context.Context, cluster *Cluster, opts UninstallOptions) error
	UninstallCalls []struct {
		Cluster *Cluster
		Opts    UninstallOptions
	}
}

func (m *MockInstaller) Upgrade(ctx context.Context, cluster *Cluster, opts UpgradeOptions) error {
	m.UpgradeCalls = append(m.UpgradeCalls, struct {
		Cluster *Cluster
		Opts    UpgradeOptions
	}{cluster, opts})
	if m.UpgradeFunc != nil {
		return m.UpgradeFunc(ctx, cluster, opts)
	}
	return nil
}

func (m *MockInstaller) Uninstall(
	ctx context.Context,
	cluster *Cluster,
	opts UninstallOptions,
) error {
	m.UninstallCalls = append(m.UninstallCalls, struct {
		Cluster *Cluster
		Opts    UninstallOptions
	}{cluster, opts})
	if m.UninstallFunc != nil {
		return m.UninstallFunc(ctx, cluster, opts)
	}
	return nil
}

func (m *MockInstaller) Init(ctx context.Context, cluster *Cluster, opts InitOptions) error {
	m.InitCalls = append(m.InitCalls, struct {
		Cluster *Cluster
		Opts    InitOptions
	}{cluster, opts})
	if m.InitFunc != nil {
		return m.InitFunc(ctx, cluster, opts)
	}
	return nil
}

// MockManifestGenerator is a test double for the ManifestGenerator interface.
type MockManifestGenerator struct {
	GenerateFunc  func(ctx context.Context, cluster *Cluster, opts GenerateOptions) ([]byte, error)
	GenerateCalls []struct {
		Cluster *Cluster
		Opts    GenerateOptions
	}
}

func (m *MockManifestGenerator) Generate(
	ctx context.Context,
	cluster *Cluster,
	opts GenerateOptions,
) ([]byte, error) {
	m.GenerateCalls = append(m.GenerateCalls, struct {
		Cluster *Cluster
		Opts    GenerateOptions
	}{cluster, opts})
	if m.GenerateFunc != nil {
		return m.GenerateFunc(ctx, cluster, opts)
	}
	return []byte(
		"apiVersion: cluster.x-k8s.io/v1beta1\nkind: Cluster\nmetadata:\n  name: test\n",
	), nil
}

// MockApplier is a test double for the Applier interface.
type MockApplier struct {
	ApplyFunc  func(ctx context.Context, cluster *Cluster, manifest []byte) error
	DeleteFunc func(ctx context.Context, cluster *Cluster, clusterName, namespace string) error
	GetFunc    func(ctx context.Context, cluster *Cluster, clusterName, namespace string) (*unstructured.Unstructured, error)
	PauseFunc  func(ctx context.Context, cluster *Cluster, clusterName, namespace string) error
	PauseCalls []struct {
		Cluster     *Cluster
		ClusterName string
	}
	ApplyCalls []struct {
		Cluster  *Cluster
		Manifest []byte
	}
	DeleteCalls []struct {
		Cluster     *Cluster
		ClusterName string
		Namespace   string
	}
	GetCalls []struct {
		Cluster     *Cluster
		ClusterName string
		Namespace   string
	}
}

func (m *MockApplier) Pause(
	ctx context.Context,
	cluster *Cluster,
	clusterName, namespace string,
) error {
	m.PauseCalls = append(m.PauseCalls, struct {
		Cluster     *Cluster
		ClusterName string
	}{cluster, clusterName})
	if m.PauseFunc != nil {
		return m.PauseFunc(ctx, cluster, clusterName, namespace)
	}
	return nil
}

// Get returns a minimal Cluster object unless GetFunc is set.
func (m *MockApplier) Get(
	ctx context.Context,
	cluster *Cluster,
	clusterName, namespace string,
) (*unstructured.Unstructured, error) {
	m.GetCalls = append(m.GetCalls, struct {
		Cluster     *Cluster
		ClusterName string
		Namespace   string
	}{cluster, clusterName, namespace})
	if m.GetFunc != nil {
		return m.GetFunc(ctx, cluster, clusterName, namespace)
	}
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("cluster.x-k8s.io/v1beta2")
	obj.SetKind("Cluster")
	obj.SetName(clusterName)
	obj.SetNamespace(namespace)
	return obj, nil
}

func (m *MockApplier) Apply(ctx context.Context, cluster *Cluster, manifest []byte) error {
	m.ApplyCalls = append(m.ApplyCalls, struct {
		Cluster  *Cluster
		Manifest []byte
	}{cluster, manifest})
	if m.ApplyFunc != nil {
		return m.ApplyFunc(ctx, cluster, manifest)
	}
	return nil
}

func (m *MockApplier) Delete(
	ctx context.Context,
	cluster *Cluster,
	clusterName, namespace string,
) error {
	m.DeleteCalls = append(m.DeleteCalls, struct {
		Cluster     *Cluster
		ClusterName string
		Namespace   string
	}{cluster, clusterName, namespace})
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, cluster, clusterName, namespace)
	}
	return nil
}

// MockMover is a test double for the Mover interface.
type MockMover struct {
	MoveFunc  func(ctx context.Context, from, to *Cluster, opts MoveOptions) error
	MoveCalls []struct {
		From *Cluster
		To   *Cluster
		Opts MoveOptions
	}
}

func (m *MockMover) Move(ctx context.Context, from, to *Cluster, opts MoveOptions) error {
	m.MoveCalls = append(m.MoveCalls, struct {
		From *Cluster
		To   *Cluster
		Opts MoveOptions
	}{from, to, opts})
	if m.MoveFunc != nil {
		return m.MoveFunc(ctx, from, to, opts)
	}
	return nil
}

// MockWaiter is a test double for the Waiter interface.
type MockWaiter struct {
	WaitForControlPlaneFunc   func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
	WaitForWorkersFunc        func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
	WaitForClusterReadyFunc   func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
	WaitForClusterDeletedFunc func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
	WaitForAddonsFunc         func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
	// ControlPlaneCalls, ReadyCalls, DeletedCalls and AddonsCalls record which cluster each wait ran against.
	ControlPlaneCalls []*Cluster
	ReadyCalls        []*Cluster
	DeletedCalls      []*Cluster
	AddonsCalls       []*Cluster
}

func (m *MockWaiter) WaitForAddons(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	m.AddonsCalls = append(m.AddonsCalls, mgmtCluster)
	if m.WaitForAddonsFunc != nil {
		return m.WaitForAddonsFunc(ctx, mgmtCluster, clusterName, namespace, opts)
	}
	return nil
}

func (m *MockWaiter) WaitForClusterDeleted(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	m.DeletedCalls = append(m.DeletedCalls, mgmtCluster)
	if m.WaitForClusterDeletedFunc != nil {
		return m.WaitForClusterDeletedFunc(ctx, mgmtCluster, clusterName, namespace, opts)
	}
	return nil
}

func (m *MockWaiter) WaitForControlPlane(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	m.ControlPlaneCalls = append(m.ControlPlaneCalls, mgmtCluster)
	if m.WaitForControlPlaneFunc != nil {
		return m.WaitForControlPlaneFunc(ctx, mgmtCluster, clusterName, namespace, opts)
	}
	return nil
}

func (m *MockWaiter) WaitForWorkers(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	if m.WaitForWorkersFunc != nil {
		return m.WaitForWorkersFunc(ctx, mgmtCluster, clusterName, namespace, opts)
	}
	return nil
}

func (m *MockWaiter) WaitForClusterReady(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
	opts WaitOptions,
) error {
	m.ReadyCalls = append(m.ReadyCalls, mgmtCluster)
	if m.WaitForClusterReadyFunc != nil {
		return m.WaitForClusterReadyFunc(ctx, mgmtCluster, clusterName, namespace, opts)
	}
	return nil
}

// MockKubeconfigRetriever is a test double for KubeconfigRetriever.
type MockKubeconfigRetriever struct {
	GetKubeconfigFunc func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error)
}

func (m *MockKubeconfigRetriever) GetKubeconfig(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
) (string, error) {
	if m.GetKubeconfigFunc != nil {
		return m.GetKubeconfigFunc(ctx, mgmtCluster, clusterName, namespace)
	}
	return "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: test\n", nil
}

// MockClusterDescriber is a test double for ClusterDescriber.
type MockClusterDescriber struct {
	DescribeFunc func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error)
}

func (m *MockClusterDescriber) Describe(
	ctx context.Context,
	mgmtCluster *Cluster,
	clusterName, namespace string,
) (string, error) {
	if m.DescribeFunc != nil {
		return m.DescribeFunc(ctx, mgmtCluster, clusterName, namespace)
	}
	return "NAME: test\nNAMESPACE: default\nKIND: Cluster", nil
}

// MockAddonInstaller is a test double for AddonInstaller.
type MockAddonInstaller struct {
	InstallFunc   func(ctx context.Context, cluster *Cluster, addons AddonSet) error
	UninstallFunc func(ctx context.Context, cluster *Cluster, addons AddonSet) error
	Calls         []struct {
		Cluster *Cluster
		Addons  AddonSet
	}
	UninstallCalls []struct {
		Cluster *Cluster
		Addons  AddonSet
	}
}

func (m *MockAddonInstaller) Uninstall(
	ctx context.Context,
	cluster *Cluster,
	addons AddonSet,
) error {
	m.UninstallCalls = append(m.UninstallCalls, struct {
		Cluster *Cluster
		Addons  AddonSet
	}{cluster, addons})
	if m.UninstallFunc != nil {
		return m.UninstallFunc(ctx, cluster, addons)
	}
	return nil
}

// MockSecretReader is a test double for SecretReader: Secrets maps
// "namespace/name" to decoded data; anything else is not found.
type MockSecretReader struct {
	Secrets map[string]map[string]string
	Calls   []string
}

func (m *MockSecretReader) ReadSecret(
	_ context.Context,
	cluster *Cluster,
	namespace, name string,
) (map[string]string, error) {
	m.Calls = append(m.Calls, cluster.Name+":"+namespace+"/"+name)
	if data, ok := m.Secrets[namespace+"/"+name]; ok {
		return data, nil
	}
	return nil, &CAPIError{
		Operation: "read-secret",
		Cluster:   cluster.Name,
		Err:       fmt.Errorf("%w: secret %s/%s", ErrClusterNotFound, namespace, name),
	}
}

func (m *MockAddonInstaller) Install(ctx context.Context, cluster *Cluster, addons AddonSet) error {
	m.Calls = append(m.Calls, struct {
		Cluster *Cluster
		Addons  AddonSet
	}{cluster, addons})
	if m.InstallFunc != nil {
		return m.InstallFunc(ctx, cluster, addons)
	}
	return nil
}

// MockQuiescer is a test double for Quiescer.
type MockQuiescer struct {
	WaitFunc func(ctx context.Context, mgmtCluster *Cluster, namespace string, opts WaitOptions) error
	Calls    []*Cluster
}

func (m *MockQuiescer) WaitForQuiescence(
	ctx context.Context,
	mgmtCluster *Cluster,
	namespace string,
	opts WaitOptions,
) error {
	m.Calls = append(m.Calls, mgmtCluster)
	if m.WaitFunc != nil {
		return m.WaitFunc(ctx, mgmtCluster, namespace, opts)
	}
	return nil
}
