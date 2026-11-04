// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"context"
	"io"
	"log"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

type noopInstaller struct{ inits []string }

func (n *noopInstaller) Init(_ context.Context, c *capi.Cluster, _ capi.InitOptions) error {
	n.inits = append(n.inits, c.Name)
	return nil
}
func (n *noopInstaller) Upgrade(context.Context, *capi.Cluster, capi.UpgradeOptions) error {
	return nil
}
func (n *noopInstaller) Uninstall(context.Context, *capi.Cluster, capi.UninstallOptions) error {
	return nil
}

type noopGenerator struct{}

func (noopGenerator) Generate(context.Context, *capi.Cluster, capi.GenerateOptions) ([]byte, error) {
	return []byte("kind: Cluster\n"), nil
}

type noopApplier struct{}

func (noopApplier) Apply(context.Context, *capi.Cluster, []byte) error          { return nil }
func (noopApplier) Delete(context.Context, *capi.Cluster, string, string) error { return nil }
func (noopApplier) Get(context.Context, *capi.Cluster, string, string) (*unstructured.Unstructured, error) {
	return &unstructured.Unstructured{}, nil
}

type noopMover struct{ moves int }

func (m *noopMover) Move(context.Context, *capi.Cluster, *capi.Cluster, capi.MoveOptions) error {
	m.moves++
	return nil
}

type noopQuiescer struct{}

func (noopQuiescer) WaitForQuiescence(context.Context, *capi.Cluster, string, capi.WaitOptions) error {
	return nil
}

type noopWaiter struct{}

func (noopWaiter) WaitForControlPlane(context.Context, *capi.Cluster, string, string, capi.WaitOptions) error {
	return nil
}
func (noopWaiter) WaitForWorkers(context.Context, *capi.Cluster, string, string, capi.WaitOptions) error {
	return nil
}
func (noopWaiter) WaitForClusterReady(context.Context, *capi.Cluster, string, string, capi.WaitOptions) error {
	return nil
}
func (noopWaiter) WaitForClusterDeleted(context.Context, *capi.Cluster, string, string, capi.WaitOptions) error {
	return nil
}

type noopInfo struct{}

func (noopInfo) GetKubeconfig(context.Context, *capi.Cluster, string, string) (string, error) {
	return "apiVersion: v1\nkind: Config\n", nil
}
func (noopInfo) Describe(context.Context, *capi.Cluster, string, string) (string, error) {
	return "ok", nil
}

func TestManager_UsesTalosBootstrapperForSelfManagedCreate(t *testing.T) {
	sim := newSim()
	b, _ := newTestBootstrapper(t, sim, nil)
	installer := &noopInstaller{}
	mover := &noopMover{}

	mgr := capi.NewManager(
		capi.WithBootstrapper(b),
		capi.WithInstaller(installer),
		capi.WithManifestGenerator(noopGenerator{}),
		capi.WithApplier(noopApplier{}),
		capi.WithMover(mover),
		capi.WithWaiter(noopWaiter{}),
		capi.WithInfoRetriever(noopInfo{}),
		capi.WithDescriber(noopInfo{}),
		capi.WithQuiescer(noopQuiescer{}),
		capi.WithLogger(log.New(io.Discard, "", 0)),
	)

	result, err := mgr.CreateCluster(context.Background(), capi.CreateClusterOptions{
		Name: "prod", Namespace: "default",
		Providers:         capi.ProviderSet{capi.ProviderTypeInfrastructure: {{Type: capi.ProviderTypeInfrastructure, Name: "tinkerbell"}}},
		KubernetesVersion: "v1.34.0", SelfManaged: true, WaitForReady: true,
	})
	if err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if result.BootstrapCluster != nil {
		t.Error("bootstrap cluster should be deleted after the pivot")
	}
	if len(installer.inits) != 2 || installer.inits[0] != "prod-bootstrap" {
		t.Errorf("clusterctl init calls = %v, want [prod-bootstrap prod]", installer.inits)
	}
	if mover.moves != 1 {
		t.Errorf("moves = %d, want 1", mover.moves)
	}
	if sim.count("reset") != 1 || sim.diskInstalled {
		t.Error("bootstrap node should be reset and wiped after the pivot")
	}
	if sim.count("power-off") == 0 {
		t.Error("bootstrap node should be powered off after the pivot")
	}
}
