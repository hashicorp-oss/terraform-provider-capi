// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

// Package capi implements the Cluster API (CAPI) management workflow
// modeled after the EKS Anywhere patterns for bootstrap cluster lifecycle
// management, CAPI component installation, workload cluster creation,
// and management pivot operations.
//
// The core workflow follows EKS Anywhere's management/create.go task sequence:
//
//  1. Create bootstrap cluster (kind)
//  2. Install CAPI providers on bootstrap (clusterctl init)
//  3. Generate the workload cluster manifest from the configured providers
//  4. Apply the manifest to the bootstrap cluster (server-side apply)
//  5. Wait for workload cluster readiness
//  6. Retrieve workload cluster kubeconfig
//  7. (Optional) Install CAPI on workload cluster + move management
//  8. Clean up bootstrap cluster
package capi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
)

// Manager orchestrates the full CAPI cluster lifecycle.
// It mirrors EKS Anywhere's Create workflow (management/create.go)
// by coordinating bootstrap creation, CAPI init, template apply,
// readiness waiting, management move, and bootstrap cleanup.
type Manager struct {
	bootstrapper  Bootstrapper
	installer     Installer
	generator     ManifestGenerator
	applier       Applier
	mover         Mover
	waiter        Waiter
	infoRetriever KubeconfigRetriever
	describer     ClusterDescriber
	addons        AddonInstaller
	quiescer      Quiescer
	logger        *log.Logger
}

// ManagerOption is a functional option for configuring a Manager.
type ManagerOption func(*Manager)

// WithBootstrapper sets the bootstrapper implementation.
func WithBootstrapper(b Bootstrapper) ManagerOption {
	return func(m *Manager) { m.bootstrapper = b }
}

// WithInstaller sets the installer implementation.
func WithInstaller(i Installer) ManagerOption {
	return func(m *Manager) { m.installer = i }
}

// WithManifestGenerator sets the manifest generator implementation.
func WithManifestGenerator(g ManifestGenerator) ManagerOption {
	return func(m *Manager) { m.generator = g }
}

// WithApplier sets the applier implementation.
func WithApplier(a Applier) ManagerOption {
	return func(m *Manager) { m.applier = a }
}

// WithMover sets the mover implementation.
func WithMover(mv Mover) ManagerOption {
	return func(m *Manager) { m.mover = mv }
}

// WithWaiter sets the waiter implementation.
func WithWaiter(w Waiter) ManagerOption {
	return func(m *Manager) { m.waiter = w }
}

// WithInfoRetriever sets the kubeconfig retriever implementation.
func WithInfoRetriever(r KubeconfigRetriever) ManagerOption {
	return func(m *Manager) { m.infoRetriever = r }
}

// WithDescriber sets the cluster describer implementation.
func WithDescriber(d ClusterDescriber) ManagerOption {
	return func(m *Manager) { m.describer = d }
}

// WithAddonInstaller sets the installer for ManagementAddons and WorkloadAddons.
func WithAddonInstaller(a AddonInstaller) ManagerOption {
	return func(m *Manager) { m.addons = a }
}

// WithQuiescer sets how the manager waits for provisioning to go idle before a move.
func WithQuiescer(q Quiescer) ManagerOption {
	return func(m *Manager) { m.quiescer = q }
}

// WithLogger sets the logger implementation.
func WithLogger(l *log.Logger) ManagerOption {
	return func(m *Manager) { m.logger = l }
}

// NewManager creates a new Manager with the given options.
// If not provided, default implementations are used.
func NewManager(opts ...ManagerOption) *Manager {
	// An empty configPath lets clusterctl fall back to its own default
	// config file resolution ($XDG_CONFIG_HOME/cluster-api or $HOME/.cluster-api).
	const configPath = ""

	infoRetriever := NewClusterctlInfoRetriever(configPath)

	m := &Manager{
		bootstrapper:  NewKindBootstrapper(),
		installer:     NewClusterctlInstaller(configPath),
		generator:     NewManifestGenerator(NewCRDKindResolver()),
		applier:       NewDynamicApplier(),
		mover:         NewClusterctlMover(configPath),
		waiter:        NewDynamicWaiter(),
		infoRetriever: infoRetriever,
		describer:     infoRetriever,
		quiescer:      NewTinkerbellQuiescer(),
		logger:        log.New(os.Stderr, "[capi] ", log.LstdFlags),
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// CreateCluster executes the full cluster creation workflow.
// This mirrors EKS Anywhere's management/create.go Run method:
//
//  1. setupAndValidateCreate
//  2. createBootStrapClusterTask
//  3. installCAPIComponentsTask
//  4. createWorkloadClusterTask (generate + apply manifest)
//  5. Wait for readiness
//  6. moveClusterManagementTask (optional, for self-managed clusters)
//  7. deleteBootstrapClusterTask
func (m *Manager) CreateCluster(ctx context.Context, opts CreateClusterOptions) (*ClusterResult, error) {
	m.logger.Printf("Starting cluster creation: %s", opts.Name)

	if _, ok := opts.Providers.Infrastructure(); !ok {
		return nil, fmt.Errorf("creating cluster %s: no infrastructure provider configured", opts.Name)
	}

	// Determine the management cluster to use
	var mgmtCluster *Cluster
	var bootstrapCluster *Cluster
	needsBootstrap := opts.ManagementKubeconfig == ""

	// Step 1: Create or use existing management cluster
	if needsBootstrap {
		m.logger.Printf("Creating bootstrap cluster for %s", opts.Name)
		var err error
		bootstrapCluster, err = m.bootstrapper.Create(ctx, BootstrapOptions{
			Name:              fmt.Sprintf("%s-bootstrap", opts.Name),
			KubernetesVersion: opts.KubernetesVersion,
			ProviderSecrets:   opts.ProviderSecrets,
		})
		if err != nil {
			return nil, fmt.Errorf("creating bootstrap cluster: %w", err)
		}
		mgmtCluster = bootstrapCluster
		m.logger.Printf("Bootstrap cluster created: %s (kubeconfig: %s)", bootstrapCluster.Name, bootstrapCluster.KubeconfigPath)
	} else {
		mgmtCluster = &Cluster{
			Name:           "management",
			KubeconfigPath: opts.ManagementKubeconfig,
		}
	}

	// Step 2: Install CAPI providers on management/bootstrap cluster.
	// All addon providers (including those with customizations) go through
	// clusterctl init. Customized addons have their component YAML modified
	// transparently via the injected RepositoryClientFactory in the installer.
	namespace := opts.Namespace
	if namespace == "" {
		namespace = "default"
	}
	if !opts.SkipInit {
		if err := m.reconcileProviders(ctx, mgmtCluster, opts); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, err
		}
	}

	pivot := opts.SelfManaged && !opts.InPlace && needsBootstrap

	// Step 2b: management-plane addons (the Tinkerbell stack) on the cluster
	// that manages now. With a pivot this cluster hands over later, so its
	// prerequisites are skipped and the set is installed again on the
	// workload cluster.
	if err := m.installAddons(ctx, mgmtCluster, opts.ManagementAddons, !pivot); err != nil {
		m.cleanupOnError(ctx, bootstrapCluster)
		return nil, fmt.Errorf("installing management addons: %w", err)
	}

	// Step 3: Generate the cluster manifest from the installed provider CRDs
	m.logger.Printf("Generating cluster manifest for %s", opts.Name)

	manifest, err := m.generator.Generate(ctx, mgmtCluster, GenerateOptions{
		ClusterName:        opts.Name,
		Namespace:          namespace,
		KubernetesVersion:  opts.KubernetesVersion,
		Providers:          opts.Providers,
		Network:            opts.Network,
		ControlPlane:       opts.ControlPlane,
		MachineDeployments: opts.MachineDeployments,
		ClusterClass:       opts.ClusterClass,
	})
	if err != nil {
		m.cleanupOnError(ctx, bootstrapCluster)
		return nil, fmt.Errorf("generating cluster manifest: %w", err)
	}

	// Step 4a: Apply any pre-template manifests (e.g. a pre-populated Talos
	// machine-secrets Secret and cluster kubeconfig Secret) so CAPI providers
	// adopt them instead of generating fresh ones. These must land before the
	// generated TalosControlPlane/TalosConfig objects reconcile. Manifests come
	// from the caller and, when a bootstrap cluster was created, from the
	// bootstrapper (built from the bootstrap node's secrets).
	preManifests := append([][]byte(nil), opts.PreTemplateManifests...)
	if needsBootstrap && bootstrapCluster != nil {
		if pm, ok := m.bootstrapper.(PreTemplateManifester); ok {
			extra, err := pm.PreTemplateManifests(ctx, opts.Name, namespace)
			if err != nil {
				m.cleanupOnError(ctx, bootstrapCluster)
				return nil, fmt.Errorf("building pre-template manifests: %w", err)
			}
			preManifests = append(preManifests, extra...)
		}
	}
	// Step 4b: the Tinkerbell inventory (Hardware, rufio Machine, BMC Secret)
	// and the caller's management manifests, so CAPT has hardware to claim
	// and IPAM pools exist before the first Machine is created.
	if inventory, err := inventoryManifest(opts, namespace, false); err != nil {
		m.cleanupOnError(ctx, bootstrapCluster)
		return nil, fmt.Errorf("rendering inventory: %w", err)
	} else if inventory != nil {
		preManifests = append(preManifests, inventory)
	}
	preManifests = append(preManifests, opts.ManagementManifests...)
	for i, pre := range preManifests {
		if len(pre) == 0 {
			continue
		}
		if err := m.applier.Apply(ctx, mgmtCluster, pre); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("applying pre-template manifest %d: %w", i, err)
		}
	}

	// Step 4: Apply the manifest to the management cluster
	m.logger.Printf("Applying cluster manifest for %s", opts.Name)
	if err := m.applier.Apply(ctx, mgmtCluster, manifest); err != nil {
		m.cleanupOnError(ctx, bootstrapCluster)
		return nil, fmt.Errorf("applying cluster manifest: %w", err)
	}

	result := &ClusterResult{
		Cluster: &Cluster{
			Name:      opts.Name,
			Namespace: namespace,
		},
		BootstrapCluster: bootstrapCluster,
	}
	// Surface any provider-specific secrets the bootstrapper generated (e.g. the
	// Talos machine-secrets bundle) so the caller can persist them in state.
	if bootstrapCluster != nil && len(bootstrapCluster.ProviderSecrets) > 0 {
		result.ProviderSecrets = bootstrapCluster.ProviderSecrets
	}

	waitOpts := opts.Wait
	if waitOpts.Timeout == 0 {
		waitOpts = DefaultWaitOptions()
	}

	// Step 5: Wait for cluster readiness. A pivot only waits for the control
	// plane to initialize here (phase one); full readiness is waited for on
	// the new management cluster after the move (phase two), because machines
	// that need the bootstrap node's hardware only complete then.
	switch {
	case pivot:
		m.logger.Printf("Waiting for cluster %s control plane to initialize", opts.Name)
		if err := m.waiter.WaitForControlPlane(ctx, mgmtCluster, opts.Name, namespace, waitOpts); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("waiting for control plane: %w", err)
		}
	case opts.WaitForReady:
		m.logger.Printf("Waiting for cluster %s to become ready", opts.Name)
		if err := m.waiter.WaitForClusterReady(ctx, mgmtCluster, opts.Name, namespace, waitOpts); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("waiting for cluster readiness: %w", err)
		}
		m.logger.Printf("Cluster %s is ready", opts.Name)
	}

	// Step 6: Retrieve workload cluster kubeconfig
	m.logger.Printf("Retrieving kubeconfig for %s", opts.Name)
	kubeconfig, err := m.infoRetriever.GetKubeconfig(ctx, mgmtCluster, opts.Name, namespace)
	if err != nil {
		m.logger.Printf("Warning: could not retrieve kubeconfig: %v", err)
	} else {
		result.Kubeconfig = kubeconfig
		result.Endpoint, result.CACertificate = kubeconfigEndpoint(kubeconfig)

		// Write kubeconfig to disk if requested
		if opts.KubeconfigOutputPath != "" {
			retriever, ok := m.infoRetriever.(*ClusterctlInfoRetriever)
			if ok {
				if wErr := retriever.WriteKubeconfig(ctx, mgmtCluster, opts.Name, namespace, opts.KubeconfigOutputPath); wErr != nil {
					m.logger.Printf("Warning: could not write kubeconfig to %s: %v", opts.KubeconfigOutputPath, wErr)
				}
			}
		}
	}

	// Step 7: Self-managed pivot. This mirrors EKS Anywhere's
	// moveClusterManagementTask. In-place skips it entirely: the bootstrap
	// cluster stays as the management cluster (for Talos, the bootstrap node
	// IS the cluster via adopted secrets).
	if pivot {
		if kubeconfig == "" {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("pivoting CAPI management: no kubeconfig for cluster %s", opts.Name)
		}
		m.logger.Printf("Pivoting CAPI management to workload cluster %s", opts.Name)

		workloadKubeconfigPath := filepath.Join(os.TempDir(), fmt.Sprintf("%s-workload-kubeconfig", opts.Name))
		if err := os.WriteFile(workloadKubeconfigPath, []byte(kubeconfig), 0600); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("writing workload kubeconfig: %w", err)
		}
		workloadCluster := &Cluster{
			Name:           opts.Name,
			KubeconfigPath: workloadKubeconfigPath,
			Namespace:      namespace,
		}

		// 7a: the workload cluster's own addons (its CNI), so its nodes can
		// become Ready at all.
		if err := m.installAddons(ctx, workloadCluster, opts.WorkloadAddons, true); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("installing workload addons: %w", err)
		}

		// 7b: nothing may be mid-provision when management moves.
		if err := m.quiescer.WaitForQuiescence(ctx, mgmtCluster, namespace, waitOpts); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("waiting for provisioning to go idle before the move: %w", err)
		}

		// 7c: CAPI providers and the management addons on the workload
		// cluster, this time with their prerequisites: it is the final
		// management cluster.
		initOpts := InitOptions{Providers: opts.Providers, Variables: initVariables(opts)}
		if err := m.installer.Init(ctx, workloadCluster, initOpts); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("installing CAPI on workload cluster for pivot: %w", err)
		}
		if err := m.installAddons(ctx, workloadCluster, opts.ManagementAddons, true); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("installing management addons on workload cluster: %w", err)
		}

		// 7d: move CAPI management from bootstrap to workload. EKS Anywhere
		// applies retry policy here to absorb transient network faults.
		if err := m.moveWithRetry(ctx, mgmtCluster, workloadCluster, MoveOptions{Namespace: namespace}); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("moving CAPI management: %w", err)
		}

		// 7e: what the move does not carry: the inventory (now including the
		// bootstrap machine as a normal netboot target; clusterctl moves
		// Hardware and rufio Machines but not their BMC Secrets) and the
		// caller's management manifests.
		var post [][]byte
		if inventory, err := inventoryManifest(opts, namespace, true); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("rendering inventory for the workload cluster: %w", err)
		} else if inventory != nil {
			post = append(post, inventory)
		}
		post = append(post, opts.ManagementManifests...)
		for i, manifest := range post {
			if err := m.applier.Apply(ctx, workloadCluster, manifest); err != nil {
				m.cleanupOnError(ctx, bootstrapCluster)
				return nil, fmt.Errorf("applying post-move manifest %d to the workload cluster: %w", i, err)
			}
		}

		// Update the result to reflect the new management cluster
		result.Cluster.KubeconfigPath = workloadKubeconfigPath
		mgmtCluster = workloadCluster
		m.logger.Printf("CAPI management pivoted to %s", opts.Name)

		// Step 8: Clean up the bootstrap cluster; its provisioning stack goes with it.
		if bootstrapCluster != nil {
			m.logger.Printf("Deleting bootstrap cluster %s", bootstrapCluster.Name)
			if err := m.bootstrapper.Delete(ctx, bootstrapCluster); err != nil {
				m.logger.Printf("Warning: failed to delete bootstrap cluster %s: %v", bootstrapCluster.Name, err)
				// Don't fail the operation - the workload cluster is running
			} else {
				result.BootstrapCluster = nil
			}
		}

		// Phase two: full readiness on the new management cluster.
		if opts.WaitForReady {
			m.logger.Printf("Waiting for cluster %s to become ready", opts.Name)
			if err := m.waiter.WaitForClusterReady(ctx, workloadCluster, opts.Name, namespace, waitOpts); err != nil {
				return nil, fmt.Errorf("waiting for cluster readiness after the pivot: %w", err)
			}
			m.logger.Printf("Cluster %s is ready", opts.Name)
		}
	}

	// Step 9: Get cluster description
	description, err := m.describer.Describe(ctx, mgmtCluster, opts.Name, namespace)
	if err != nil {
		m.logger.Printf("Warning: could not describe cluster: %v", err)
	} else {
		result.ClusterDescription = description
	}

	m.logger.Printf("Cluster %s creation completed successfully", opts.Name)
	return result, nil
}

// reconcileProviders converges a management cluster on the configured
// providers: clusterctl init for what is missing (and for pinned versions
// that differ from the installed ones), clusterctl upgrade apply for the
// entries whose settings changed, clusterctl delete for the entries dropped
// from the configuration.
func (m *Manager) reconcileProviders(ctx context.Context, mgmtCluster *Cluster, opts CreateClusterOptions) error {
	m.logger.Printf("Installing CAPI providers on %s", mgmtCluster.Name)
	variables := initVariables(opts)
	if err := m.installer.Init(ctx, mgmtCluster, InitOptions{Providers: opts.Providers, Variables: variables}); err != nil {
		return fmt.Errorf("installing CAPI providers: %w", err)
	}
	m.logger.Printf("CAPI providers installed on %s", mgmtCluster.Name)

	if len(opts.UpgradeProviders) > 0 {
		m.logger.Printf("Upgrading CAPI providers %v on %s", opts.UpgradeProviders, mgmtCluster.Name)
		up := UpgradeOptions{Providers: opts.Providers, Targets: opts.UpgradeProviders, Variables: variables}
		if err := m.installer.Upgrade(ctx, mgmtCluster, up); err != nil {
			return fmt.Errorf("upgrading CAPI providers: %w", err)
		}
	}
	if !opts.RemovedProviders.Empty() {
		m.logger.Printf("Deleting CAPI providers %v from %s", opts.RemovedProviders.Keys(), mgmtCluster.Name)
		if err := m.installer.Uninstall(ctx, mgmtCluster, UninstallOptions{Providers: opts.RemovedProviders}); err != nil {
			return fmt.Errorf("deleting CAPI providers: %w", err)
		}
	}
	return nil
}

// DeleteCluster deletes a CAPI cluster and optionally its bootstrap cluster.
// This mirrors EKS Anywhere's delete workflow: the Cluster object is deleted
// and waited for, so every machine and its infrastructure is released before
// anything that manages them goes away. A self-managed cluster first hands
// management to a fresh bootstrap cluster, the reverse of the create pivot,
// because deleting it from inside would power off its own controllers.
func (m *Manager) DeleteCluster(ctx context.Context, opts DeleteClusterOptions) error {
	m.logger.Printf("Deleting cluster %s", opts.Name)

	namespace := opts.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// An in-place cluster is torn down node by node: the management plane runs
	// on a member node, so deleting the Cluster object would power off the
	// controllers mid-delete. The objects die with the cluster.
	if opts.InPlace {
		td, ok := m.bootstrapper.(InPlaceTeardowner)
		if !ok {
			return fmt.Errorf("deleting cluster %s: in-place teardown is not supported by the %T bootstrapper", opts.Name, m.bootstrapper)
		}
		bootstrap := &Cluster{Name: opts.BootstrapName, ProviderSecrets: opts.ProviderSecrets}
		if bootstrap.Name == "" {
			bootstrap.Name = opts.Name + "-bootstrap"
		}
		var others []InventoryMachine
		for _, im := range opts.Inventory {
			if im.Hostname != opts.BootstrapMachine {
				others = append(others, im)
			}
		}
		if err := td.TeardownInPlace(ctx, bootstrap, others); err != nil {
			return fmt.Errorf("tearing down in-place cluster %s: %w", opts.Name, err)
		}
		m.logger.Printf("Cluster %s torn down", opts.Name)
		return nil
	}

	if opts.ManagementKubeconfig == "" {
		return fmt.Errorf("management kubeconfig is required for cluster deletion")
	}

	waitOpts := opts.Wait
	if waitOpts.Timeout == 0 {
		waitOpts = DefaultWaitOptions()
	}

	if opts.SelfManaged {
		if err := m.deleteSelfManaged(ctx, opts, namespace, waitOpts); err != nil {
			return err
		}
	} else {
		mgmtCluster := &Cluster{Name: "management", KubeconfigPath: opts.ManagementKubeconfig}
		if err := m.deleteClusterObject(ctx, mgmtCluster, opts.Name, namespace, waitOpts); err != nil {
			return err
		}
		if opts.UninstallProviders {
			m.logger.Printf("Deleting every CAPI provider from %s", mgmtCluster.Name)
			if err := m.installer.Uninstall(ctx, mgmtCluster, UninstallOptions{All: true}); err != nil {
				return fmt.Errorf("deleting CAPI providers: %w", err)
			}
		}
	}

	// Optionally delete the bootstrap cluster
	if opts.DeleteBootstrap && opts.BootstrapName != "" {
		bootstrapCluster := &Cluster{Name: opts.BootstrapName, ProviderSecrets: opts.ProviderSecrets}
		if err := m.bootstrapper.Delete(ctx, bootstrapCluster); err != nil {
			m.logger.Printf("Warning: failed to delete bootstrap cluster %s: %v", opts.BootstrapName, err)
		}
	}

	m.logger.Printf("Cluster %s deleted", opts.Name)
	return nil
}

// deleteClusterObject deletes the Cluster object and waits for it to
// disappear. A cluster that is already gone is not an error.
func (m *Manager) deleteClusterObject(ctx context.Context, mgmtCluster *Cluster, name, namespace string, waitOpts WaitOptions) error {
	err := m.applier.Delete(ctx, mgmtCluster, name, namespace)
	if errors.Is(err, ErrClusterNotFound) {
		m.logger.Printf("Cluster %s/%s is already gone", namespace, name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting cluster %s: %w", name, err)
	}
	m.logger.Printf("Waiting for cluster %s to be deleted", name)
	if err := m.waiter.WaitForClusterDeleted(ctx, mgmtCluster, name, namespace, waitOpts); err != nil {
		return fmt.Errorf("waiting for cluster %s to be deleted: %w", name, err)
	}
	return nil
}

// deleteSelfManaged deletes a cluster that manages itself: a bootstrap
// cluster is created and initialised like at create time (providers,
// management addons, inventory and management manifests, so rufio can power
// the machines off), management is moved to it once nothing is mid-provision,
// the Cluster object is deleted there and waited for, and the bootstrap
// cluster is torn down. A failure after the move started leaves the
// bootstrap cluster in place, since it may hold the only copy of the
// management objects; the next deletion resumes from it.
func (m *Manager) deleteSelfManaged(ctx context.Context, opts DeleteClusterOptions, namespace string, waitOpts WaitOptions) error {
	workload := &Cluster{Name: opts.Name, KubeconfigPath: opts.ManagementKubeconfig, Namespace: namespace}
	bootstrapName := fmt.Sprintf("%s-bootstrap", opts.Name)
	if _, err := m.applier.Get(ctx, workload, opts.Name, namespace); err != nil {
		if !errors.Is(err, ErrClusterNotFound) {
			return fmt.Errorf("reading cluster %s before deletion: %w", opts.Name, err)
		}
		// Gone from the workload cluster: deleted out of band, or moved by
		// an earlier deletion to a bootstrap cluster that outlived a failure.
		exists, err := m.bootstrapper.Exists(ctx, bootstrapName)
		if err != nil {
			return fmt.Errorf("checking for a bootstrap cluster left by an earlier deletion of %s: %w", opts.Name, err)
		}
		if !exists {
			m.logger.Printf("Cluster %s/%s no longer exists on itself; nothing to move", namespace, opts.Name)
			return nil
		}
		m.logger.Printf("Resuming the deletion of %s from bootstrap cluster %s", opts.Name, bootstrapName)
		bootstrap, err := m.bootstrapper.Create(ctx, BootstrapOptions{Name: bootstrapName, ProviderSecrets: opts.ProviderSecrets})
		if err != nil {
			return fmt.Errorf("reconnecting to bootstrap cluster %s: %w", bootstrapName, err)
		}
		if err := m.deleteClusterObject(ctx, bootstrap, opts.Name, namespace, waitOpts); err != nil {
			return fmt.Errorf("%w (bootstrap cluster %s left in place for recovery)", err, bootstrap.Name)
		}
		if err := m.bootstrapper.Delete(ctx, bootstrap); err != nil {
			m.logger.Printf("Warning: failed to delete bootstrap cluster %s: %v", bootstrap.Name, err)
		}
		return nil
	}

	m.logger.Printf("Creating bootstrap cluster to take over management of %s for deletion", opts.Name)
	bootstrap, err := m.bootstrapper.Create(ctx, BootstrapOptions{
		Name:              bootstrapName,
		KubernetesVersion: opts.Spec.KubernetesVersion,
		ProviderSecrets:   opts.ProviderSecrets,
	})
	if err != nil {
		return fmt.Errorf("creating bootstrap cluster for deletion: %w", err)
	}
	spec := opts.Spec
	spec.Name = opts.Name
	spec.Namespace = namespace
	prepare := func() error {
		if err := m.installer.Init(ctx, bootstrap, InitOptions{Providers: spec.Providers, Variables: initVariables(spec)}); err != nil {
			return fmt.Errorf("installing CAPI providers on the bootstrap cluster: %w", err)
		}
		if err := m.installAddons(ctx, bootstrap, spec.ManagementAddons, false); err != nil {
			return fmt.Errorf("installing management addons on the bootstrap cluster: %w", err)
		}
		var manifests [][]byte
		if inventory, err := inventoryManifest(spec, namespace, true); err != nil {
			return fmt.Errorf("rendering inventory: %w", err)
		} else if inventory != nil {
			manifests = append(manifests, inventory)
		}
		manifests = append(manifests, spec.ManagementManifests...)
		for i, manifest := range manifests {
			if len(manifest) == 0 {
				continue
			}
			if err := m.applier.Apply(ctx, bootstrap, manifest); err != nil {
				return fmt.Errorf("applying management manifest %d to the bootstrap cluster: %w", i, err)
			}
		}
		if err := m.quiescer.WaitForQuiescence(ctx, workload, namespace, waitOpts); err != nil {
			return fmt.Errorf("waiting for provisioning to go idle before the move: %w", err)
		}
		return nil
	}
	if err := prepare(); err != nil {
		m.cleanupOnError(ctx, bootstrap)
		return err
	}

	m.logger.Printf("Moving CAPI management of %s to %s", opts.Name, bootstrap.Name)
	if err := m.moveWithRetry(ctx, workload, bootstrap, MoveOptions{Namespace: namespace}); err != nil {
		return fmt.Errorf("moving CAPI management to %s (left in place for recovery): %w", bootstrap.Name, err)
	}
	if err := m.deleteClusterObject(ctx, bootstrap, opts.Name, namespace, waitOpts); err != nil {
		return fmt.Errorf("%w (bootstrap cluster %s left in place for recovery)", err, bootstrap.Name)
	}

	m.logger.Printf("Deleting bootstrap cluster %s", bootstrap.Name)
	if err := m.bootstrapper.Delete(ctx, bootstrap); err != nil {
		m.logger.Printf("Warning: failed to delete bootstrap cluster %s: %v", bootstrap.Name, err)
	}
	return nil
}

// DeleteBootstrapCluster deletes a bootstrap cluster by name, for one that
// outlived its pivot (its deletion failed at create time).
func (m *Manager) DeleteBootstrapCluster(ctx context.Context, name string, providerSecrets map[string]string) error {
	return m.bootstrapper.Delete(ctx, &Cluster{Name: name, ProviderSecrets: providerSecrets})
}

// GetClusterInfo retrieves current information about a cluster. A cluster
// whose Cluster object is gone returns an error wrapping ErrClusterNotFound;
// an unreachable management cluster returns its error unchanged, so the
// caller can tell the two apart.
func (m *Manager) GetClusterInfo(ctx context.Context, mgmtKubeconfig, clusterName, namespace string) (*ClusterResult, error) {
	mgmtCluster := &Cluster{
		Name:           "management",
		KubeconfigPath: mgmtKubeconfig,
	}

	if namespace == "" {
		namespace = "default"
	}

	obj, err := m.applier.Get(ctx, mgmtCluster, clusterName, namespace)
	if err != nil {
		return nil, fmt.Errorf("reading cluster %s: %w", clusterName, err)
	}

	result := &ClusterResult{
		Cluster: &Cluster{
			Name:      clusterName,
			Namespace: namespace,
		},
	}

	// Get kubeconfig
	kubeconfig, err := m.infoRetriever.GetKubeconfig(ctx, mgmtCluster, clusterName, namespace)
	if err != nil {
		m.logger.Printf("Warning: could not retrieve kubeconfig: %v", err)
	} else {
		result.Kubeconfig = kubeconfig
		result.Endpoint, result.CACertificate = kubeconfigEndpoint(kubeconfig)
	}
	if result.Endpoint == "" {
		result.Endpoint = controlPlaneEndpoint(obj)
	}

	// Get description
	description, err := m.describer.Describe(ctx, mgmtCluster, clusterName, namespace)
	if err != nil {
		m.logger.Printf("Warning: could not describe cluster: %v", err)
	} else {
		result.ClusterDescription = description
	}

	return result, nil
}

// kubeconfigEndpoint returns the API server URL and the PEM CA certificate
// of the kubeconfig's current context, or empty strings.
func kubeconfigEndpoint(kubeconfig string) (endpoint, caCertificate string) {
	cfg, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		return "", ""
	}
	name := cfg.CurrentContext
	if name == "" {
		for n := range cfg.Contexts {
			name = n
			break
		}
	}
	context, ok := cfg.Contexts[name]
	if !ok {
		return "", ""
	}
	cluster, ok := cfg.Clusters[context.Cluster]
	if !ok {
		return "", ""
	}
	return cluster.Server, string(cluster.CertificateAuthorityData)
}

// controlPlaneEndpoint renders Cluster.spec.controlPlaneEndpoint as a URL,
// or "" when unset.
func controlPlaneEndpoint(obj *unstructured.Unstructured) string {
	if obj == nil {
		return ""
	}
	host, _, _ := unstructured.NestedString(obj.Object, "spec", "controlPlaneEndpoint", "host")
	if host == "" {
		return ""
	}
	port, _, _ := unstructured.NestedInt64(obj.Object, "spec", "controlPlaneEndpoint", "port")
	if port == 0 {
		port = 6443
	}
	return fmt.Sprintf("https://%s:%d", host, port)
}

// inventoryManifest renders the Tinkerbell inventory for the create options,
// or nil when there is nothing to register: inventory management is off, the
// infrastructure provider is not tinkerbell, or no machines are listed. With a
// pivot the bootstrap machine still hosts the management cluster, so it is
// left out rather than offered to CAPT until afterPivot, when it is a normal
// netboot target; in place it is marked provisioned.
func inventoryManifest(opts CreateClusterOptions, namespace string, afterPivot bool) ([]byte, error) {
	infra, ok := opts.Providers.Infrastructure()
	if !opts.ManageInventory || !ok || !strings.EqualFold(infra.Name, "tinkerbell") || len(opts.Inventory) == 0 {
		return nil, nil
	}
	inv := InventoryOptions{Namespace: namespace, ClusterName: opts.Name}
	for _, im := range opts.Inventory {
		if im.Hostname == opts.BootstrapMachine && opts.BootstrapMachine != "" && !opts.InPlace && !afterPivot {
			continue
		}
		inv.Machines = append(inv.Machines, im)
	}
	if opts.InPlace {
		inv.BootstrapMachine = opts.BootstrapMachine
	}
	if len(inv.Machines) == 0 {
		return nil, nil
	}
	return InventoryManifests(inv)
}

// installAddons installs an addon set on a cluster. final is false on a
// cluster that will hand management over, which skips the set's
// prerequisites. An empty set is a no-op; a non-empty set needs an installer.
func (m *Manager) installAddons(ctx context.Context, cluster *Cluster, set AddonSet, final bool) error {
	if !final {
		set.Prerequisites = nil
	}
	if set.Empty() {
		return nil
	}
	if m.addons == nil {
		return fmt.Errorf("installing addons on %s: no addon installer configured", cluster.Name)
	}
	m.logger.Printf("Installing addons on %s", cluster.Name)
	return m.addons.Install(ctx, cluster, set)
}

// initVariables returns the clusterctl variables for init: the caller's plus
// CLUSTER_TOPOLOGY=true when a ClusterClass is generated, since the gate is
// off by default in cluster-api.
func initVariables(opts CreateClusterOptions) map[string]string {
	if !opts.ClusterClass {
		return opts.Variables
	}
	vars := make(map[string]string, len(opts.Variables)+1)
	for k, v := range opts.Variables {
		vars[k] = v
	}
	vars["CLUSTER_TOPOLOGY"] = "true"
	return vars
}

// cleanupOnError deletes the bootstrap cluster if creation fails partway through.
func (m *Manager) cleanupOnError(ctx context.Context, bootstrapCluster *Cluster) {
	if bootstrapCluster == nil {
		return
	}
	m.logger.Printf("Cleaning up bootstrap cluster %s after error", bootstrapCluster.Name)
	if err := m.bootstrapper.Delete(ctx, bootstrapCluster); err != nil {
		m.logger.Printf("Warning: failed to cleanup bootstrap cluster %s: %v", bootstrapCluster.Name, err)
	}
}

func (m *Manager) moveWithRetry(ctx context.Context, from, to *Cluster, opts MoveOptions) error {
	const maxAttempts = 4
	baseWait := 5 * time.Second

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := m.mover.Move(ctx, from, to, opts)
		if err == nil {
			return nil
		}

		lastErr = err
		if !isRetryableMoveError(err) || attempt == maxAttempts {
			break
		}

		wait := baseWait * time.Duration(1<<(attempt-1))
		m.logger.Printf("Retrying CAPI move after transient error (attempt %d/%d, wait=%s): %v", attempt, maxAttempts, wait, err)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}

	return lastErr
}

func isRetryableMoveError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	patterns := []string{
		"connection reset",
		"connection refused",
		"timeout",
		"i/o timeout",
		"eof",
		"temporary",
		"tls handshake timeout",
	}

	for _, pattern := range patterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}

	return false
}
