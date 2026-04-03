// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import "context"

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

// MockTemplateGenerator is a test double for the TemplateGenerator interface.
type MockTemplateGenerator struct {
	GenerateFunc  func(ctx context.Context, cluster *Cluster, opts TemplateOptions) ([]byte, error)
	GenerateCalls []struct {
		Cluster *Cluster
		Opts    TemplateOptions
	}
}

func (m *MockTemplateGenerator) Generate(ctx context.Context, cluster *Cluster, opts TemplateOptions) ([]byte, error) {
	m.GenerateCalls = append(m.GenerateCalls, struct {
		Cluster *Cluster
		Opts    TemplateOptions
	}{cluster, opts})
	if m.GenerateFunc != nil {
		return m.GenerateFunc(ctx, cluster, opts)
	}
	return []byte("apiVersion: cluster.x-k8s.io/v1beta1\nkind: Cluster\nmetadata:\n  name: test\n"), nil
}

// MockApplier is a test double for the Applier interface.
type MockApplier struct {
	ApplyFunc  func(ctx context.Context, cluster *Cluster, manifest []byte) error
	DeleteFunc func(ctx context.Context, cluster *Cluster, clusterName, namespace string) error
	ApplyCalls []struct {
		Cluster  *Cluster
		Manifest []byte
	}
	DeleteCalls []struct {
		Cluster     *Cluster
		ClusterName string
		Namespace   string
	}
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

func (m *MockApplier) Delete(ctx context.Context, cluster *Cluster, clusterName, namespace string) error {
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
	WaitForControlPlaneFunc func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
	WaitForWorkersFunc      func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
	WaitForClusterReadyFunc func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error
}

func (m *MockWaiter) WaitForControlPlane(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	if m.WaitForControlPlaneFunc != nil {
		return m.WaitForControlPlaneFunc(ctx, mgmtCluster, clusterName, namespace, opts)
	}
	return nil
}

func (m *MockWaiter) WaitForWorkers(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	if m.WaitForWorkersFunc != nil {
		return m.WaitForWorkersFunc(ctx, mgmtCluster, clusterName, namespace, opts)
	}
	return nil
}

func (m *MockWaiter) WaitForClusterReady(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string, opts WaitOptions) error {
	if m.WaitForClusterReadyFunc != nil {
		return m.WaitForClusterReadyFunc(ctx, mgmtCluster, clusterName, namespace, opts)
	}
	return nil
}

// MockKubeconfigRetriever is a test double for KubeconfigRetriever.
type MockKubeconfigRetriever struct {
	GetKubeconfigFunc func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error)
}

func (m *MockKubeconfigRetriever) GetKubeconfig(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error) {
	if m.GetKubeconfigFunc != nil {
		return m.GetKubeconfigFunc(ctx, mgmtCluster, clusterName, namespace)
	}
	return "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: test\n", nil
}

// MockClusterDescriber is a test double for ClusterDescriber.
type MockClusterDescriber struct {
	DescribeFunc func(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error)
}

func (m *MockClusterDescriber) Describe(ctx context.Context, mgmtCluster *Cluster, clusterName, namespace string) (string, error) {
	if m.DescribeFunc != nil {
		return m.DescribeFunc(ctx, mgmtCluster, clusterName, namespace)
	}
	return "NAME: test\nNAMESPACE: default\nKIND: Cluster", nil
}
