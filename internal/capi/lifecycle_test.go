// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/tree"
)

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: dev
clusters:
- name: dev
  cluster:
    server: https://10.0.0.160:6443
    certificate-authority-data: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCmZha2UKLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo=
contexts:
- name: dev
  context:
    cluster: dev
    user: dev
users:
- name: dev
  user:
    token: x
`

func notFound(name string) error {
	return &CAPIError{Operation: "get-cluster", Cluster: "management", Err: fmt.Errorf("%w: default/%s", ErrClusterNotFound, name)}
}

func TestManager_CreateCluster_UpgradesAndRemovesProvidersAfterInit(t *testing.T) {
	installer := &MockInstaller{}
	mgr := NewManager(
		WithInstaller(installer), WithManifestGenerator(&MockManifestGenerator{}), WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}), WithInfoRetriever(&MockKubeconfigRetriever{}), WithDescriber(&MockClusterDescriber{}),
	)
	removed := ProviderSet{}
	removed.Add(ProviderConfig{Name: "helm", Type: ProviderTypeAddon})
	opts := CreateClusterOptions{
		Name: "test", ManagementKubeconfig: "/tmp/mgmt", Providers: dockerProviders(),
		UpgradeProviders: []ProviderKey{{Type: ProviderTypeInfrastructure, Name: "docker"}},
		RemovedProviders: removed,
		Variables:        map[string]string{"A": "b"},
	}
	if _, err := mgr.CreateCluster(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(installer.InitCalls) != 1 || len(installer.UpgradeCalls) != 1 || len(installer.UninstallCalls) != 1 {
		t.Fatalf("init/upgrade/uninstall calls = %d/%d/%d", len(installer.InitCalls), len(installer.UpgradeCalls), len(installer.UninstallCalls))
	}
	up := installer.UpgradeCalls[0].Opts
	if len(up.Targets) != 1 || up.Targets[0].Name != "docker" || up.Variables["A"] != "b" || len(up.Providers.All()) != len(dockerProviders().All()) {
		t.Errorf("upgrade options = %+v", up)
	}
	if un := installer.UninstallCalls[0].Opts; un.All || len(un.Providers[ProviderTypeAddon]) != 1 {
		t.Errorf("uninstall options = %+v", un)
	}

	// With SkipInit the providers are not touched at all.
	installer = &MockInstaller{}
	mgr = NewManager(
		WithInstaller(installer), WithManifestGenerator(&MockManifestGenerator{}), WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}), WithInfoRetriever(&MockKubeconfigRetriever{}), WithDescriber(&MockClusterDescriber{}),
	)
	opts.SkipInit = true
	if _, err := mgr.CreateCluster(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(installer.InitCalls)+len(installer.UpgradeCalls)+len(installer.UninstallCalls) != 0 {
		t.Error("skip_init must leave the providers alone")
	}
}

func TestManager_CreateCluster_PopulatesEndpointFromKubeconfig(t *testing.T) {
	mgr := NewManager(
		WithInstaller(&MockInstaller{}), WithManifestGenerator(&MockManifestGenerator{}), WithApplier(&MockApplier{}),
		WithWaiter(&MockWaiter{}), WithDescriber(&MockClusterDescriber{}),
		WithInfoRetriever(&MockKubeconfigRetriever{GetKubeconfigFunc: func(context.Context, *Cluster, string, string) (string, error) {
			return testKubeconfig, nil
		}}),
	)
	result, err := mgr.CreateCluster(context.Background(), CreateClusterOptions{Name: "test", ManagementKubeconfig: "/tmp/mgmt", Providers: dockerProviders(), SkipInit: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Endpoint != "https://10.0.0.160:6443" || !strings.HasPrefix(result.CACertificate, "-----BEGIN CERTIFICATE-----") {
		t.Errorf("endpoint %q ca %q", result.Endpoint, result.CACertificate)
	}
}

func TestManager_DeleteCluster_ToleratesAnAlreadyDeletedCluster(t *testing.T) {
	applier := &MockApplier{DeleteFunc: func(_ context.Context, _ *Cluster, name, _ string) error { return notFound(name) }}
	waiter := &MockWaiter{}
	bootstrapper := &MockBootstrapper{}
	mgr := NewManager(WithApplier(applier), WithWaiter(waiter), WithBootstrapper(bootstrapper))
	err := mgr.DeleteCluster(context.Background(), DeleteClusterOptions{
		Name: "test", ManagementKubeconfig: "/tmp/mgmt", DeleteBootstrap: true, BootstrapName: "test-bootstrap",
	})
	if err != nil {
		t.Fatalf("a cluster that is already gone is not an error: %v", err)
	}
	if len(waiter.DeletedCalls) != 0 {
		t.Error("nothing to wait for")
	}
	if len(bootstrapper.DeleteCalls) != 1 {
		t.Error("the bootstrap cluster is still deleted")
	}
}

func TestManager_DeleteCluster_FailsWhenTheClusterDoesNotDisappear(t *testing.T) {
	waiter := &MockWaiter{WaitForClusterDeletedFunc: func(context.Context, *Cluster, string, string, WaitOptions) error {
		return fmt.Errorf("%w: still there", ErrClusterNotDeleted)
	}}
	bootstrapper := &MockBootstrapper{}
	mgr := NewManager(WithApplier(&MockApplier{}), WithWaiter(waiter), WithBootstrapper(bootstrapper))
	err := mgr.DeleteCluster(context.Background(), DeleteClusterOptions{
		Name: "test", ManagementKubeconfig: "/tmp/mgmt", DeleteBootstrap: true, BootstrapName: "test-bootstrap",
		Wait: WaitOptions{Timeout: time.Minute},
	})
	if !errors.Is(err, ErrClusterNotDeleted) {
		t.Fatalf("err = %v", err)
	}
	if len(bootstrapper.DeleteCalls) != 0 {
		t.Error("the bootstrap cluster must outlive a cluster that is still being deleted")
	}
}

func TestManager_DeleteCluster_UninstallsProviders(t *testing.T) {
	installer := &MockInstaller{}
	mgr := NewManager(WithApplier(&MockApplier{}), WithWaiter(&MockWaiter{}), WithInstaller(installer))
	err := mgr.DeleteCluster(context.Background(), DeleteClusterOptions{Name: "test", ManagementKubeconfig: "/tmp/mgmt", UninstallProviders: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(installer.UninstallCalls) != 1 || !installer.UninstallCalls[0].Opts.All {
		t.Errorf("uninstall calls = %+v", installer.UninstallCalls)
	}
}

func TestManager_DeleteCluster_SelfManagedReversesThePivot(t *testing.T) {
	var sequence []string
	bootstrapper := &MockBootstrapper{
		CreateFunc: func(_ context.Context, opts BootstrapOptions) (*Cluster, error) {
			sequence = append(sequence, "bootstrap-create")
			return &Cluster{Name: opts.Name, KubeconfigPath: "/tmp/bootstrap"}, nil
		},
		DeleteFunc: func(context.Context, *Cluster) error { sequence = append(sequence, "bootstrap-delete"); return nil },
	}
	installer := &MockInstaller{InitFunc: func(_ context.Context, c *Cluster, _ InitOptions) error {
		sequence = append(sequence, "init:"+c.KubeconfigPath)
		return nil
	}}
	addons := &MockAddonInstaller{InstallFunc: func(_ context.Context, c *Cluster, set AddonSet) error {
		sequence = append(sequence, fmt.Sprintf("addons:%s:prereqs=%d", c.KubeconfigPath, len(set.Prerequisites)))
		return nil
	}}
	applier := &MockApplier{
		ApplyFunc: func(_ context.Context, c *Cluster, manifest []byte) error {
			kind := "manifest"
			if strings.Contains(string(manifest), "kind: Hardware") {
				kind = "inventory"
			}
			sequence = append(sequence, kind+":"+c.KubeconfigPath)
			return nil
		},
		DeleteFunc: func(_ context.Context, c *Cluster, _, _ string) error {
			sequence = append(sequence, "delete:"+c.KubeconfigPath)
			return nil
		},
		GetFunc: func(_ context.Context, c *Cluster, name, ns string) (*unstructured.Unstructured, error) {
			sequence = append(sequence, "get:"+c.KubeconfigPath)
			return (&MockApplier{}).Get(context.Background(), c, name, ns)
		},
	}
	applier.PauseFunc = func(_ context.Context, c *Cluster, _, _ string) error {
		sequence = append(sequence, "pause:"+c.KubeconfigPath)
		return nil
	}
	quiescer := &MockQuiescer{WaitFunc: func(_ context.Context, c *Cluster, _ string, _ WaitOptions) error {
		sequence = append(sequence, "quiesce:"+c.KubeconfigPath)
		return nil
	}}
	mover := &MockMover{MoveFunc: func(_ context.Context, from, to *Cluster, _ MoveOptions) error {
		sequence = append(sequence, "move:"+from.KubeconfigPath+"->"+to.KubeconfigPath)
		return nil
	}}
	waiter := &MockWaiter{WaitForClusterDeletedFunc: func(_ context.Context, c *Cluster, _, _ string, _ WaitOptions) error {
		sequence = append(sequence, "wait-deleted:"+c.KubeconfigPath)
		return nil
	}}
	mgr := NewManager(
		WithBootstrapper(bootstrapper), WithInstaller(installer), WithAddonInstaller(addons), WithApplier(applier),
		WithQuiescer(quiescer), WithMover(mover), WithWaiter(waiter),
	)

	spec := CreateClusterOptions{
		Providers: tinkerbellProviders(), Class: &ClassRef{Name: "test"},
		BootstrapAddons:     AddonSet{Helm: []HelmRelease{{Name: "tinkerbell"}}},
		ManagementManifests: [][]byte{[]byte("kind: ConfigMap\n")},
		ManageInventory:     true, Inventory: inventoryTwoMachines(), BootstrapMachine: "cp-1",
	}
	err := mgr.DeleteCluster(context.Background(), DeleteClusterOptions{
		Name: "test", Namespace: "capi", ManagementKubeconfig: "/tmp/workload", SelfManaged: true, Spec: spec,
		ProviderSecrets: map[string]string{"talos": "bundle"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"get:/tmp/workload",
		"bootstrap-create",
		"init:/tmp/bootstrap",
		"addons:/tmp/bootstrap:prereqs=0",
		"inventory:/tmp/bootstrap",
		"manifest:/tmp/bootstrap",
		"pause:/tmp/workload",
		"quiesce:/tmp/workload",
		"move:/tmp/workload->/tmp/bootstrap",
		"delete:/tmp/bootstrap",
		"wait-deleted:/tmp/bootstrap",
		"bootstrap-delete",
	}
	if strings.Join(sequence, "\n") != strings.Join(want, "\n") {
		t.Errorf("sequence:\n%s\nwant:\n%s", strings.Join(sequence, "\n"), strings.Join(want, "\n"))
	}
	if got := installer.InitCalls[0].Opts.Variables["CLUSTER_TOPOLOGY"]; got != "true" {
		t.Errorf("the bootstrap cluster is initialised like at create time, CLUSTER_TOPOLOGY = %q", got)
	}
	if bootstrapper.CreateCalls[0].ProviderSecrets["talos"] != "bundle" {
		t.Error("provider secrets must reach the bootstrapper")
	}
	// Every inventory machine is offered, the former bootstrap machine included.
	for _, call := range applier.ApplyCalls {
		if strings.Contains(string(call.Manifest), "kind: Hardware") && !strings.Contains(string(call.Manifest), "name: cp-1") {
			t.Error("the inventory applied for deletion must include the pivoted bootstrap machine")
		}
	}
}

func TestManager_DeleteCluster_SelfManagedGoneClusterNeedsNoBootstrap(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	applier := &MockApplier{GetFunc: func(_ context.Context, _ *Cluster, name, _ string) (*unstructured.Unstructured, error) {
		return nil, notFound(name)
	}}
	mgr := NewManager(WithBootstrapper(bootstrapper), WithApplier(applier), WithWaiter(&MockWaiter{}))
	err := mgr.DeleteCluster(context.Background(), DeleteClusterOptions{Name: "test", ManagementKubeconfig: "/tmp/workload", SelfManaged: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(bootstrapper.CreateCalls) != 0 {
		t.Error("no cluster, no reverse pivot")
	}
}

func TestManager_DeleteCluster_SelfManagedResumesFromABootstrapClusterLeftBehind(t *testing.T) {
	bootstrapper := &MockBootstrapper{ExistsFunc: func(_ context.Context, name string) (bool, error) { return name == "test-bootstrap", nil }}
	applier := &MockApplier{GetFunc: func(_ context.Context, _ *Cluster, name, _ string) (*unstructured.Unstructured, error) {
		return nil, notFound(name)
	}}
	mover := &MockMover{}
	waiter := &MockWaiter{}
	mgr := NewManager(WithBootstrapper(bootstrapper), WithApplier(applier), WithWaiter(waiter), WithMover(mover), WithInstaller(&MockInstaller{}))
	err := mgr.DeleteCluster(context.Background(), DeleteClusterOptions{Name: "test", ManagementKubeconfig: "/tmp/workload", SelfManaged: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(mover.MoveCalls) != 0 {
		t.Error("the objects already moved; nothing to move again")
	}
	if len(bootstrapper.CreateCalls) != 1 || bootstrapper.CreateCalls[0].Name != "test-bootstrap" {
		t.Errorf("the existing bootstrap cluster is reconnected, create calls = %+v", bootstrapper.CreateCalls)
	}
	if len(applier.DeleteCalls) != 1 || applier.DeleteCalls[0].Cluster.Name != "test-bootstrap" || len(waiter.DeletedCalls) != 1 {
		t.Errorf("the Cluster is deleted and waited for on the bootstrap cluster: %+v %d", applier.DeleteCalls, len(waiter.DeletedCalls))
	}
	if len(bootstrapper.DeleteCalls) != 1 {
		t.Error("the bootstrap cluster is deleted once the cluster is gone")
	}
}

func TestManager_DeleteCluster_SelfManagedLeavesBootstrapAfterFailedMove(t *testing.T) {
	bootstrapper := &MockBootstrapper{}
	mover := &MockMover{MoveFunc: func(context.Context, *Cluster, *Cluster, MoveOptions) error { return errors.New("boom") }}
	mgr := NewManager(
		WithBootstrapper(bootstrapper), WithInstaller(&MockInstaller{}), WithApplier(&MockApplier{}),
		WithQuiescer(&MockQuiescer{}), WithMover(mover), WithWaiter(&MockWaiter{}),
	)
	err := mgr.DeleteCluster(context.Background(), DeleteClusterOptions{Name: "test", ManagementKubeconfig: "/tmp/workload", SelfManaged: true, Spec: CreateClusterOptions{Providers: dockerProviders()}})
	if err == nil || !strings.Contains(err.Error(), "left in place") {
		t.Fatalf("err = %v", err)
	}
	if len(bootstrapper.DeleteCalls) != 0 {
		t.Error("a failed move must leave the bootstrap cluster for recovery")
	}

	// A failure before the move does clean the bootstrap cluster up.
	bootstrapper = &MockBootstrapper{}
	mgr = NewManager(
		WithBootstrapper(bootstrapper), WithApplier(&MockApplier{}), WithWaiter(&MockWaiter{}),
		WithInstaller(&MockInstaller{InitFunc: func(context.Context, *Cluster, InitOptions) error { return errors.New("init boom") }}),
	)
	err = mgr.DeleteCluster(context.Background(), DeleteClusterOptions{Name: "test", ManagementKubeconfig: "/tmp/workload", SelfManaged: true, Spec: CreateClusterOptions{Providers: dockerProviders()}})
	if err == nil || len(bootstrapper.DeleteCalls) != 1 {
		t.Errorf("err = %v, bootstrap deletes = %d", err, len(bootstrapper.DeleteCalls))
	}
}

func TestManager_GetClusterInfo_ReportsAMissingCluster(t *testing.T) {
	applier := &MockApplier{GetFunc: func(_ context.Context, _ *Cluster, name, _ string) (*unstructured.Unstructured, error) {
		return nil, notFound(name)
	}}
	mgr := NewManager(WithApplier(applier), WithInfoRetriever(&MockKubeconfigRetriever{}), WithDescriber(&MockClusterDescriber{}))
	_, err := mgr.GetClusterInfo(context.Background(), "/tmp/mgmt", "test", "default")
	if !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("err = %v", err)
	}

	// Any other failure is not a missing cluster.
	applier.GetFunc = func(context.Context, *Cluster, string, string) (*unstructured.Unstructured, error) {
		return nil, errors.New("connection refused")
	}
	_, err = mgr.GetClusterInfo(context.Background(), "/tmp/mgmt", "test", "default")
	if err == nil || errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestManager_GetClusterInfo_Endpoint(t *testing.T) {
	retriever := &MockKubeconfigRetriever{GetKubeconfigFunc: func(context.Context, *Cluster, string, string) (string, error) {
		return testKubeconfig, nil
	}}
	mgr := NewManager(WithApplier(&MockApplier{}), WithInfoRetriever(retriever), WithDescriber(&MockClusterDescriber{}))
	result, err := mgr.GetClusterInfo(context.Background(), "/tmp/mgmt", "test", "default")
	if err != nil {
		t.Fatal(err)
	}
	if result.Endpoint != "https://10.0.0.160:6443" || !strings.Contains(result.CACertificate, "fake") {
		t.Errorf("endpoint %q ca %q", result.Endpoint, result.CACertificate)
	}

	// Without a kubeconfig the endpoint comes from the Cluster object.
	retriever.GetKubeconfigFunc = func(context.Context, *Cluster, string, string) (string, error) {
		return "", errors.New("no secret yet")
	}
	applier := &MockApplier{GetFunc: func(_ context.Context, c *Cluster, name, ns string) (*unstructured.Unstructured, error) {
		obj, _ := (&MockApplier{}).Get(context.Background(), c, name, ns)
		_ = unstructured.SetNestedField(obj.Object, "192.168.1.100", "spec", "controlPlaneEndpoint", "host")
		_ = unstructured.SetNestedField(obj.Object, int64(443), "spec", "controlPlaneEndpoint", "port")
		return obj, nil
	}}
	mgr = NewManager(WithApplier(applier), WithInfoRetriever(retriever), WithDescriber(&MockClusterDescriber{}))
	result, err = mgr.GetClusterInfo(context.Background(), "/tmp/mgmt", "test", "default")
	if err != nil {
		t.Fatal(err)
	}
	if result.Endpoint != "https://192.168.1.100:443" || result.CACertificate != "" {
		t.Errorf("endpoint %q ca %q", result.Endpoint, result.CACertificate)
	}
}

func TestKubeconfigEndpoint_Garbage(t *testing.T) {
	if ep, ca := kubeconfigEndpoint("not: [yaml"); ep != "" || ca != "" {
		t.Errorf("%q %q", ep, ca)
	}
	if ep, ca := kubeconfigEndpoint(""); ep != "" || ca != "" {
		t.Errorf("%q %q", ep, ca)
	}
}

func installedList(entries ...clusterctlv1.Provider) *clusterctlv1.ProviderList {
	return &clusterctlv1.ProviderList{Items: entries}
}

func installed(ns, name string, typ clusterctlv1.ProviderType, version string) clusterctlv1.Provider {
	return clusterctlv1.Provider{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: string(typ) + "-" + name}, ProviderName: name, Type: string(typ), Version: version}
}

func TestVersionMismatches(t *testing.T) {
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "cluster-api", Type: ProviderTypeCore, Version: "v1.14.0"})
	providers.Add(ProviderConfig{Name: "tinkerbell", Type: ProviderTypeInfrastructure, Version: "v0.7.9"})
	providers.Add(ProviderConfig{Name: "talos", Type: ProviderTypeBootstrap})
	providers.Add(ProviderConfig{Name: "helm", Type: ProviderTypeAddon, Version: "v0.3.0"})
	list := installedList(
		installed("capi-system", "cluster-api", clusterctlv1.CoreProviderType, "v1.13.2"),
		installed("capt-system", "tinkerbell", clusterctlv1.InfrastructureProviderType, "0.7.9"),
		installed("cabpt-system", "talos", clusterctlv1.BootstrapProviderType, "v0.6.0"),
	)
	got := versionMismatches(providers, list)
	if len(got) != 1 || got[0] != (ProviderKey{Type: ProviderTypeCore, Name: "cluster-api"}) {
		t.Errorf("mismatches = %v (a missing v prefix is not a mismatch, an unpinned entry never is, a missing provider is for init)", got)
	}
	if got := versionMismatches(providers, nil); got != nil {
		t.Errorf("no inventory, no mismatches: %v", got)
	}
}

func TestUpgradeRefs(t *testing.T) {
	providers := ProviderSet{}
	providers.Add(ProviderConfig{Name: "cluster-api", Type: ProviderTypeCore, Version: "v1.14.0"})
	providers.Add(ProviderConfig{Name: "talos", Type: ProviderTypeBootstrap, Manager: &ManagerConfig{Verbosity: ptrInt64(5)}})
	providers.Add(ProviderConfig{Name: "talos", Type: ProviderTypeControlPlane, Version: "v0.7.1"})
	providers.Add(ProviderConfig{Name: "unifi", Type: ProviderTypeIPAM, Version: "v0.4.1"})
	list := installedList(
		installed("capi-system", "cluster-api", clusterctlv1.CoreProviderType, "v1.13.2"),
		installed("cabpt-system", "talos", clusterctlv1.BootstrapProviderType, "v0.6.0"),
		installed("cacppt-system", "talos", clusterctlv1.ControlPlaneProviderType, "v0.7.1"),
	)
	targets := []ProviderKey{
		{Type: ProviderTypeCore, Name: "cluster-api"},
		{Type: ProviderTypeBootstrap, Name: "talos"},
		{Type: ProviderTypeControlPlane, Name: "talos"},
		{Type: ProviderTypeIPAM, Name: "unifi"},
		{Type: ProviderTypeAddon, Name: "nope"},
	}
	refs, missing := upgradeRefs(providers, targets, list)
	if refs[ProviderTypeCore][0] != "capi-system/cluster-api:v1.14.0" {
		t.Errorf("a pinned target upgrades to its version: %v", refs)
	}
	if refs[ProviderTypeBootstrap][0] != "cabpt-system/talos:v0.6.0" {
		t.Errorf("an unpinned target is re-rendered at the installed version: %v", refs)
	}
	if refs[ProviderTypeControlPlane][0] != "cacppt-system/talos:v0.7.1" {
		t.Errorf("a target at its version is reinstalled, which re-applies its customizations: %v", refs)
	}
	if len(missing) != 1 || missing[0].Name != "unifi" {
		t.Errorf("a target that is not installed is installed instead; an unknown key is ignored: %v", missing)
	}
}

func TestRenderObjectTree(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	cond := func(typ, status, reason, msg string, age time.Duration) map[string]any {
		return map[string]any{"type": typ, "status": status, "reason": reason, "message": msg, "lastTransitionTime": now.Add(-age).Format(time.RFC3339)}
	}
	obj := func(kind, name string, uid string, conditions ...map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("cluster.x-k8s.io/v1beta2")
		u.SetKind(kind)
		u.SetName(name)
		u.SetNamespace("default")
		u.SetUID(types.UID(uid))
		var list []any
		for _, c := range conditions {
			list = append(list, c)
		}
		if list != nil {
			_ = unstructured.SetNestedSlice(u.Object, list, "status", "conditions")
		}
		return u
	}
	root := obj("Cluster", "dev", "1", cond("Available", "True", "Available", "", 90*time.Minute))
	tr := tree.NewObjectTree(root, tree.ObjectTreeOptions{})
	infra := obj("DockerCluster", "dev", "2", cond("Ready", "True", "Ready", "", time.Hour))
	tr.Add(root, infra, tree.ObjectMetaName("ClusterInfrastructure"))
	cp := obj("KubeadmControlPlane", "dev-control-plane", "3", cond("Available", "False", "NotAvailable", "waiting for etcd\nmember 2 is down", 5*time.Minute))
	tr.Add(root, cp, tree.ObjectMetaName("ControlPlane"))
	machine := obj("Machine", "dev-cp-x", "4", cond("Ready", "Unknown", "", "", 0))
	tr.Add(cp, machine)
	workers := tree.VirtualObject("default", "WorkerGroup", "Workers")
	tr.Add(root, workers)
	md := obj("MachineDeployment", "dev-md-0", "6")
	tr.Add(workers, md)
	if !strings.Contains(treeRowName(md), "MachineDeployment/dev-md-0") {
		t.Fatalf("row name = %q", treeRowName(md))
	}

	got := RenderObjectTree(tr, now)
	want := []string{
		"NAME", "STATUS", "REASON", "SINCE", "MESSAGE",
		"Cluster/dev", "True", "Available", "90m",
		"ClusterInfrastructure - DockerCluster/dev", "60m",
		"ControlPlane - KubeadmControlPlane/dev-control-plane", "False", "NotAvailable", "5m", "waiting for etcd",
		"member 2 is down",
		"Machine/dev-cp-x", "Unknown",
		"Workers",
		"MachineDeployment/dev-md-0",
		treeFirstElem, treeLastElem, treePipe,
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("rendering lacks %q:\n%s", w, got)
		}
	}
	if RenderObjectTree(nil, now) != "" {
		t.Error("nil tree renders empty")
	}
}
