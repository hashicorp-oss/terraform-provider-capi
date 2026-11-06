// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

// Package capi implements the Cluster API (CAPI) management workflow
// modeled after the EKS Anywhere patterns for bootstrap cluster lifecycle
// management, CAPI component installation, workload cluster creation,
// and management pivot operations.
//
// The core workflow follows EKS Anywhere's management/create.go task sequence:
//
//  1. Create bootstrap cluster (kind, or a disposable Kairos live node)
//  2. Install CAPI providers on bootstrap (clusterctl init) and its own
//     addon stack (a Tinkerbell on host networking)
//  3. Generate the workload cluster manifest from the configured providers
//  4. Apply the manifest to the bootstrap cluster (server-side apply)
//  5. Wait for the control plane to initialize
//  6. Retrieve workload cluster kubeconfig
//  7. (Self-managed) hand over: pause the Cluster, drain provisioning,
//     uninstall the bootstrap stack, wait for the workload's own addons
//     (CAAPH), install CAPI on the workload cluster, move management
//  8. Clean up bootstrap cluster, capture the root credential, wait for Ready
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
	secrets       SecretReader
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

// WithAddonInstaller sets the installer for BootstrapAddons.
func WithAddonInstaller(a AddonInstaller) ManagerOption {
	return func(m *Manager) { m.addons = a }
}

// WithSecretReader sets how the root credential of a self-managed cluster
// is read after the pivot.
func WithSecretReader(s SecretReader) ManagerOption {
	return func(m *Manager) { m.secrets = s }
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
	applier := NewDynamicApplier()

	m := &Manager{
		bootstrapper:  NewKindBootstrapper(),
		installer:     NewClusterctlInstaller(configPath),
		generator:     NewManifestGenerator(NewCRDKindResolver()),
		applier:       applier,
		secrets:       applier,
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
func (m *Manager) CreateCluster(
	ctx context.Context,
	opts CreateClusterOptions,
) (*ClusterResult, error) {
	m.logger.Printf("Starting cluster creation: %s", opts.Name)

	if opts.Class == nil && opts.Infrastructure.Provider == "" {
		if _, ok := opts.Providers.Infrastructure(); !ok {
			return nil, fmt.Errorf(
				"creating cluster %s: no infrastructure provider configured",
				opts.Name,
			)
		}
	}

	// Determine the management cluster to use
	var mgmtCluster *Cluster
	var bootstrapCluster *Cluster
	needsBootstrap := opts.ManagementKubeconfig == ""

	// The workflow's progress, reported to the caller and carried by every
	// error so a failed run can be recorded and resumed.
	phase := PhaseBootstrapping
	advance := func(p Phase) {
		phase = p
		if opts.OnPhase != nil {
			opts.OnPhase(p)
		}
	}
	fail := func(err error) (*ClusterResult, error) {
		if !opts.KeepBootstrapOnFailure {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, &PhaseError{Phase: phase, Err: err}
		}
		return nil, &PhaseError{Phase: phase, Bootstrap: bootstrapCluster, Err: err}
	}

	cp := opts.Checkpoint
	if cp == nil {
		cp = &Checkpoint{}
	}
	namespace := opts.Namespace
	if namespace == "" {
		namespace = "default"
	}
	waitOpts := opts.Wait
	if waitOpts.Timeout == 0 {
		waitOpts = DefaultWaitOptions()
	}

	// A self-managed create that already moved management: the bootstrap
	// cluster holds none of the cluster's objects any more, so the run
	// continues on the cluster itself and never touches the bootstrap's
	// providers, addons or objects again.
	if needsBootstrap && opts.SelfManaged && cp.Done(StepMoved) {
		return m.resumeAfterMove(
			ctx,
			opts,
			cp,
			namespace,
			waitOpts,
			advance,
			func() Phase { return phase },
		)
	}

	// Step 1: Create or use existing management cluster
	if needsBootstrap {
		advance(PhaseBootstrapping)
		m.logger.Printf("Creating bootstrap cluster for %s", opts.Name)
		var err error
		bootstrapCluster, err = m.bootstrapper.Create(ctx, BootstrapOptions{
			Name:              fmt.Sprintf("%s-bootstrap", opts.Name),
			KubernetesVersion: opts.KubernetesVersion,
			ProviderSecrets:   opts.ProviderSecrets,
		})
		if err != nil {
			return fail(fmt.Errorf("creating bootstrap cluster: %w", err))
		}
		cp.recordBootstrap(bootstrapCluster.Name)
		mgmtCluster = bootstrapCluster
		m.logger.Printf(
			"Bootstrap cluster created: %s (kubeconfig: %s)",
			bootstrapCluster.Name,
			bootstrapCluster.KubeconfigPath,
		)
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
	if !opts.SkipInit {
		if err := m.reconcileProviders(ctx, mgmtCluster, opts); err != nil {
			return fail(err)
		}
	}

	pivot := opts.SelfManaged && needsBootstrap

	// Step 2b: the bootstrap cluster's own stack (a Tinkerbell on host
	// networking), uninstalled again before management moves away. A resume
	// after the handover removed it must not bring it back: the workload's
	// Tinkerbell already answers DHCP.
	if needsBootstrap && !cp.Done(StepBootstrapAddonsRemoved) {
		if err := m.installAddons(ctx, mgmtCluster, opts.BootstrapAddons); err != nil {
			return fail(fmt.Errorf("installing bootstrap addons: %w", err))
		}
	}
	advance(PhaseInitialized)

	// Step 3: Generate the cluster manifest from the installed provider CRDs
	m.logger.Printf("Generating cluster manifest for %s", opts.Name)

	manifest, err := m.generator.Generate(ctx, mgmtCluster, GenerateOptions{
		ClusterName:       opts.Name,
		Namespace:         namespace,
		KubernetesVersion: opts.KubernetesVersion,
		Providers:         opts.Providers,
		Network:           opts.Network,
		Infrastructure:    opts.Infrastructure,
		ControlPlane:      opts.ControlPlane,
		Class:             opts.Class,
	})
	if err != nil {
		return fail(fmt.Errorf("generating cluster manifest: %w", err))
	}

	// Step 4a: the Tinkerbell inventory (Hardware, rufio Machine, BMC Secret)
	// and the caller's management manifests, so CAPT has hardware to claim
	// and IPAM pools exist before the first Machine is created.
	var preManifests [][]byte
	if inventory, err := inventoryManifest(opts, namespace, false); err != nil {
		return fail(fmt.Errorf("rendering inventory: %w", err))
	} else if inventory != nil {
		preManifests = append(preManifests, inventory)
	}
	preManifests = append(preManifests, opts.ManagementManifests...)
	for i, pre := range preManifests {
		if len(pre) == 0 {
			continue
		}
		if err := m.applier.Apply(ctx, mgmtCluster, pre); err != nil {
			return fail(fmt.Errorf("applying pre-template manifest %d: %w", i, err))
		}
	}

	// Step 4: Apply the manifest to the management cluster
	m.logger.Printf("Applying cluster manifest for %s", opts.Name)
	if err := m.applier.Apply(ctx, mgmtCluster, manifest); err != nil {
		return fail(fmt.Errorf("applying cluster manifest: %w", err))
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

	// Step 5: Wait for cluster readiness. A pivot only waits for the control
	// plane to initialize here (phase one); full readiness is waited for on
	// the new management cluster after the move (phase two), because machines
	// that need the bootstrap node's hardware only complete then.
	switch {
	case pivot:
		m.logger.Printf("Waiting for cluster %s control plane to initialize", opts.Name)
		if err := m.waiter.WaitForControlPlane(
			ctx,
			mgmtCluster,
			opts.Name,
			namespace,
			waitOpts,
		); err != nil {
			return fail(fmt.Errorf("waiting for control plane: %w", err))
		}
		advance(PhaseControlPlaneInitialized)
	case opts.WaitForReady:
		m.logger.Printf("Waiting for cluster %s to become ready", opts.Name)
		if err := m.waiter.WaitForClusterReady(
			ctx,
			mgmtCluster,
			opts.Name,
			namespace,
			waitOpts,
		); err != nil {
			return fail(fmt.Errorf("waiting for cluster readiness: %w", err))
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
	}

	// Step 7: Self-managed pivot. This mirrors EKS Anywhere's
	// moveClusterManagementTask, with a handover of the provisioning engine
	// in between so at most one Tinkerbell answers DHCP at any time.
	if pivot {
		if kubeconfig == "" {
			return fail(
				fmt.Errorf("pivoting CAPI management: no kubeconfig for cluster %s", opts.Name),
			)
		}
		m.logger.Printf("Pivoting CAPI management to workload cluster %s", opts.Name)

		workloadKubeconfigPath := filepath.Join(
			os.TempDir(),
			fmt.Sprintf("%s-workload-kubeconfig", opts.Name),
		)
		if err := os.WriteFile(workloadKubeconfigPath, []byte(kubeconfig), 0o600); err != nil {
			return fail(fmt.Errorf("writing workload kubeconfig: %w", err))
		}
		workloadCluster := &Cluster{
			Name:           opts.Name,
			KubeconfigPath: workloadKubeconfigPath,
			Namespace:      namespace,
		}

		// 7a–7d: the handover. Pause the Cluster so CAPT starts no further
		// Machines, let in-flight Workflows finish, take the bootstrap
		// Tinkerbell down, and wait for the workload's own addons (CAAPH
		// installs them once the control plane is initialized) so its
		// Tinkerbell is up before anything else needs provisioning.
		if err := m.handOver(
			ctx,
			mgmtCluster,
			workloadCluster,
			opts,
			cp,
			namespace,
			waitOpts,
			advance,
		); err != nil {
			return fail(err)
		}

		// 7e: CAPI providers on the workload cluster, then the move. EKS
		// Anywhere applies retry policy here to absorb transient faults.
		// clusterctl resumes the Cluster on the target.
		initOpts := InitOptions{Providers: opts.Providers, Variables: initVariables(opts)}
		if err := m.installer.Init(ctx, workloadCluster, initOpts); err != nil {
			return fail(fmt.Errorf("installing CAPI on workload cluster for pivot: %w", err))
		}
		if err := m.moveWithRetry(
			ctx,
			mgmtCluster,
			workloadCluster,
			MoveOptions{Namespace: namespace},
		); err != nil {
			return fail(fmt.Errorf("moving CAPI management: %w", err))
		}
		// The point of no return: the objects live on the cluster now. Record
		// it with what reaches the cluster before anything else can fail.
		cp.Mark(StepMoved)
		cp.WorkloadKubeconfig = kubeconfig
		advance(PhaseMoved)

		if err := m.finishPivot(
			ctx,
			opts,
			cp,
			namespace,
			waitOpts,
			bootstrapCluster,
			workloadCluster,
			result,
			advance,
			func() Phase { return phase },
		); err != nil {
			return nil, err
		}
		mgmtCluster = workloadCluster
	}

	return m.complete(ctx, mgmtCluster, opts, namespace, result, advance), nil
}

// resumeAfterMove continues a self-managed create whose clusterctl move
// already finished: on the cluster itself, through the kubeconfig the
// checkpoint recorded at the move.
func (m *Manager) resumeAfterMove(
	ctx context.Context,
	opts CreateClusterOptions,
	cp *Checkpoint,
	namespace string,
	waitOpts WaitOptions,
	advance func(Phase),
	phase func() Phase,
) (*ClusterResult, error) {
	advance(PhaseMoved)
	if cp.WorkloadKubeconfig == "" {
		return nil, &PhaseError{
			Phase: phase(),
			Err: fmt.Errorf(
				"resuming cluster %s after the move: the checkpoint records no kubeconfig for it",
				opts.Name,
			),
		}
	}
	m.logger.Printf("Resuming cluster %s on itself: management already moved there", opts.Name)
	path := filepath.Join(os.TempDir(), fmt.Sprintf("%s-workload-kubeconfig", opts.Name))
	if err := os.WriteFile(path, []byte(cp.WorkloadKubeconfig), 0o600); err != nil {
		return nil, &PhaseError{
			Phase: phase(),
			Err:   fmt.Errorf("writing workload kubeconfig: %w", err),
		}
	}
	workload := &Cluster{Name: opts.Name, KubeconfigPath: path, Namespace: namespace}
	var bootstrap *Cluster
	if !cp.Done(StepBootstrapDeleted) && cp.Bootstrap != nil && cp.Bootstrap.Name != "" {
		bootstrap = &Cluster{Name: cp.Bootstrap.Name, ProviderSecrets: opts.ProviderSecrets}
	}
	result := &ClusterResult{
		Cluster:          &Cluster{Name: opts.Name, Namespace: namespace},
		BootstrapCluster: bootstrap,
		Kubeconfig:       cp.WorkloadKubeconfig,
	}
	result.Endpoint, result.CACertificate = kubeconfigEndpoint(cp.WorkloadKubeconfig)
	if err := m.finishPivot(
		ctx,
		opts,
		cp,
		namespace,
		waitOpts,
		bootstrap,
		workload,
		result,
		advance,
		phase,
	); err != nil {
		return nil, err
	}
	return m.complete(ctx, workload, opts, namespace, result, advance), nil
}

// finishPivot is what follows the move: the inventory and management
// manifests the move does not carry, the bootstrap cluster's teardown, the
// root credential, and full readiness on the cluster. Every step is safe to
// repeat except the teardown, which the checkpoint records. A failure keeps
// the bootstrap cluster (it holds nothing any more) and returns what the run
// has so far, so a resume can connect to the cluster.
func (m *Manager) finishPivot(
	ctx context.Context,
	opts CreateClusterOptions,
	cp *Checkpoint,
	namespace string,
	waitOpts WaitOptions,
	bootstrap, workload *Cluster,
	result *ClusterResult,
	advance func(Phase),
	phase func() Phase,
) error {
	failed := func(err error) error {
		return &PhaseError{Phase: phase(), Bootstrap: bootstrap, Result: result, Err: err}
	}

	// 7f: what the move does not carry: the inventory (now including the
	// bootstrap machine as a normal netboot target; clusterctl moves
	// Hardware and rufio Machines but not their BMC Secrets) and the
	// caller's management manifests.
	var post [][]byte
	if inventory, err := inventoryManifest(opts, namespace, true); err != nil {
		return failed(fmt.Errorf("rendering inventory for the workload cluster: %w", err))
	} else if inventory != nil {
		post = append(post, inventory)
	}
	post = append(post, opts.ManagementManifests...)
	for i, manifest := range post {
		if err := m.applier.Apply(ctx, workload, manifest); err != nil {
			return failed(
				fmt.Errorf("applying post-move manifest %d to the workload cluster: %w", i, err),
			)
		}
	}

	result.Cluster.KubeconfigPath = workload.KubeconfigPath
	m.logger.Printf("CAPI management pivoted to %s", opts.Name)

	// Step 8: Clean up the bootstrap cluster; its provisioning stack goes
	// with it. Once done it is never attempted again: a Kairos bootstrap
	// node is provisioned as a cluster member from here on.
	if bootstrap != nil && !cp.Done(StepBootstrapDeleted) {
		m.logger.Printf("Deleting bootstrap cluster %s", bootstrap.Name)
		if err := m.bootstrapper.Delete(ctx, bootstrap); err != nil {
			// Not fatal: the workload cluster runs; a resume retries.
			m.logger.Printf(
				"Warning: failed to delete bootstrap cluster %s: %v",
				bootstrap.Name,
				err,
			)
		} else {
			cp.Mark(StepBootstrapDeleted)
			bootstrap = nil
			result.BootstrapCluster = nil
		}
	}

	// Step 8b: the root credential of a cluster that now manages itself:
	// the secrets bundle its providers generated, which is all a later
	// run needs to derive a kubeconfig when nothing else holds one.
	if secrets := m.rootSecrets(ctx, workload, opts.Name, namespace); len(secrets) > 0 {
		result.ProviderSecrets = secrets
	}
	advance(PhasePivoted)

	// Phase two: full readiness on the new management cluster. A failure
	// here resumes against the cluster itself.
	if opts.WaitForReady {
		m.logger.Printf("Waiting for cluster %s to become ready", opts.Name)
		if err := m.waiter.WaitForClusterReady(
			ctx,
			workload,
			opts.Name,
			namespace,
			waitOpts,
		); err != nil {
			return failed(fmt.Errorf("waiting for cluster readiness after the pivot: %w", err))
		}
		m.logger.Printf("Cluster %s is ready", opts.Name)
	}
	return nil
}

// complete describes the cluster and reads its conditions on the cluster
// that now manages it, and finishes the workflow.
func (m *Manager) complete(
	ctx context.Context,
	mgmtCluster *Cluster,
	opts CreateClusterOptions,
	namespace string,
	result *ClusterResult,
	advance func(Phase),
) *ClusterResult {
	// Step 9: Get cluster description and conditions
	description, err := m.describer.Describe(ctx, mgmtCluster, opts.Name, namespace)
	if err != nil {
		m.logger.Printf("Warning: could not describe cluster: %v", err)
	} else {
		result.ClusterDescription = description
	}
	if obj, err := m.applier.Get(ctx, mgmtCluster, opts.Name, namespace); err == nil {
		result.Conditions = ClusterConditions(obj)
	}

	advance(PhaseReady)
	result.Phase = PhaseReady
	m.logger.Printf("Cluster %s creation completed successfully", opts.Name)
	return result
}

// reconcileProviders converges a management cluster on the configured
// providers: clusterctl init for what is missing (and for pinned versions
// that differ from the installed ones), clusterctl upgrade apply for the
// entries whose settings changed, clusterctl delete for the entries dropped
// from the configuration.
func (m *Manager) reconcileProviders(
	ctx context.Context,
	mgmtCluster *Cluster,
	opts CreateClusterOptions,
) error {
	m.logger.Printf("Installing CAPI providers on %s", mgmtCluster.Name)
	variables := initVariables(opts)
	if err := m.installer.Init(
		ctx,
		mgmtCluster,
		InitOptions{Providers: opts.Providers, Variables: variables},
	); err != nil {
		return fmt.Errorf("installing CAPI providers: %w", err)
	}
	m.logger.Printf("CAPI providers installed on %s", mgmtCluster.Name)

	if len(opts.UpgradeProviders) > 0 {
		m.logger.Printf(
			"Upgrading CAPI providers %v on %s",
			opts.UpgradeProviders,
			mgmtCluster.Name,
		)
		up := UpgradeOptions{
			Providers: opts.Providers,
			Targets:   opts.UpgradeProviders,
			Variables: variables,
		}
		if err := m.installer.Upgrade(ctx, mgmtCluster, up); err != nil {
			return fmt.Errorf("upgrading CAPI providers: %w", err)
		}
	}
	if !opts.RemovedProviders.Empty() {
		m.logger.Printf(
			"Deleting CAPI providers %v from %s",
			opts.RemovedProviders.Keys(),
			mgmtCluster.Name,
		)
		if err := m.installer.Uninstall(
			ctx,
			mgmtCluster,
			UninstallOptions{Providers: opts.RemovedProviders},
		); err != nil {
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
		if err := m.deleteClusterObject(
			ctx,
			mgmtCluster,
			opts.Name,
			namespace,
			waitOpts,
		); err != nil {
			return err
		}
		if opts.UninstallProviders {
			m.logger.Printf("Deleting every CAPI provider from %s", mgmtCluster.Name)
			if err := m.installer.Uninstall(
				ctx,
				mgmtCluster,
				UninstallOptions{All: true},
			); err != nil {
				return fmt.Errorf("deleting CAPI providers: %w", err)
			}
		}
	}

	// Optionally delete the bootstrap cluster
	if opts.DeleteBootstrap && opts.BootstrapName != "" {
		bootstrapCluster := &Cluster{
			Name:            opts.BootstrapName,
			ProviderSecrets: opts.ProviderSecrets,
		}
		if err := m.bootstrapper.Delete(ctx, bootstrapCluster); err != nil {
			m.logger.Printf(
				"Warning: failed to delete bootstrap cluster %s: %v",
				opts.BootstrapName,
				err,
			)
		}
	}

	m.logger.Printf("Cluster %s deleted", opts.Name)
	return nil
}

// deleteClusterObject deletes the Cluster object and waits for it to
// disappear. A cluster that is already gone is not an error.
func (m *Manager) deleteClusterObject(
	ctx context.Context,
	mgmtCluster *Cluster,
	name, namespace string,
	waitOpts WaitOptions,
) error {
	err := m.applier.Delete(ctx, mgmtCluster, name, namespace)
	if errors.Is(err, ErrClusterNotFound) {
		m.logger.Printf("Cluster %s/%s is already gone", namespace, name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting cluster %s: %w", name, err)
	}
	m.logger.Printf("Waiting for cluster %s to be deleted", name)
	if err := m.waiter.WaitForClusterDeleted(
		ctx,
		mgmtCluster,
		name,
		namespace,
		waitOpts,
	); err != nil {
		return fmt.Errorf("waiting for cluster %s to be deleted: %w", name, err)
	}
	return nil
}

// deleteSelfManaged deletes a cluster that manages itself: a bootstrap
// cluster is created and initialised like at create time (providers, the
// bootstrap addons so rufio can power the machines off, inventory and
// management manifests), management is moved to it once the Cluster is
// paused and nothing is mid-provision, the Cluster object is deleted there
// and waited for, and the bootstrap cluster is torn down. A failure after
// the move started leaves the bootstrap cluster in place, since it may hold
// the only copy of the management objects; the next deletion resumes from it.
func (m *Manager) deleteSelfManaged(
	ctx context.Context,
	opts DeleteClusterOptions,
	namespace string,
	waitOpts WaitOptions,
) error {
	workload := &Cluster{
		Name:           opts.Name,
		KubeconfigPath: opts.ManagementKubeconfig,
		Namespace:      namespace,
	}
	bootstrapName := fmt.Sprintf("%s-bootstrap", opts.Name)
	if _, err := m.applier.Get(ctx, workload, opts.Name, namespace); err != nil {
		if !errors.Is(err, ErrClusterNotFound) {
			return fmt.Errorf("reading cluster %s before deletion: %w", opts.Name, err)
		}
		// Gone from the workload cluster: deleted out of band, or moved by
		// an earlier deletion to a bootstrap cluster that outlived a failure.
		if opts.Checkpoint != nil && opts.Checkpoint.MovedTo != nil &&
			opts.Checkpoint.MovedTo.Name != "" {
			bootstrapName = opts.Checkpoint.MovedTo.Name
		}
		exists, err := m.bootstrapper.Exists(ctx, bootstrapName)
		if err != nil {
			return fmt.Errorf(
				"checking for a bootstrap cluster left by an earlier deletion of %s: %w",
				opts.Name,
				err,
			)
		}
		if !exists {
			if opts.Checkpoint != nil && opts.Checkpoint.MovedTo != nil {
				// An earlier deletion moved the objects there: they are lost
				// with it, and the machines may still be provisioned.
				return fmt.Errorf(
					"an earlier deletion of %s moved its CAPI objects to bootstrap cluster %s, which no longer exists: the machines may still be provisioned and powered on; release them by hand, then remove the resource from state",
					opts.Name,
					bootstrapName,
				)
			}
			m.logger.Printf(
				"Cluster %s/%s no longer exists on itself; nothing to move",
				namespace,
				opts.Name,
			)
			return nil
		}
		m.logger.Printf(
			"Resuming the deletion of %s from bootstrap cluster %s",
			opts.Name,
			bootstrapName,
		)
		bootstrap, err := m.bootstrapper.Create(
			ctx,
			BootstrapOptions{Name: bootstrapName, ProviderSecrets: opts.ProviderSecrets},
		)
		if err != nil {
			return fmt.Errorf("reconnecting to bootstrap cluster %s: %w", bootstrapName, err)
		}
		if err := m.deleteClusterObject(
			ctx,
			bootstrap,
			opts.Name,
			namespace,
			waitOpts,
		); err != nil {
			return fmt.Errorf(
				"%w (bootstrap cluster %s left in place for recovery)",
				err,
				bootstrap.Name,
			)
		}
		if err := m.bootstrapper.Delete(ctx, bootstrap); err != nil {
			m.logger.Printf(
				"Warning: failed to delete bootstrap cluster %s: %v",
				bootstrap.Name,
				err,
			)
		}
		return nil
	}

	m.logger.Printf(
		"Creating bootstrap cluster to take over management of %s for deletion",
		opts.Name,
	)
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
		if err := m.installer.Init(
			ctx,
			bootstrap,
			InitOptions{Providers: spec.Providers, Variables: initVariables(spec)},
		); err != nil {
			return fmt.Errorf("installing CAPI providers on the bootstrap cluster: %w", err)
		}
		if err := m.installAddons(ctx, bootstrap, spec.BootstrapAddons); err != nil {
			return fmt.Errorf("installing bootstrap addons on the bootstrap cluster: %w", err)
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
				return fmt.Errorf(
					"applying management manifest %d to the bootstrap cluster: %w",
					i,
					err,
				)
			}
		}
		if err := m.applier.Pause(ctx, workload, opts.Name, namespace); err != nil {
			return fmt.Errorf("pausing cluster %s before the move: %w", opts.Name, err)
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
	if err := m.moveWithRetry(
		ctx,
		workload,
		bootstrap,
		MoveOptions{Namespace: namespace},
	); err != nil {
		return fmt.Errorf(
			"moving CAPI management to %s (left in place for recovery): %w",
			bootstrap.Name,
			err,
		)
	}
	// From here the bootstrap cluster holds the only copy of the objects.
	if opts.Checkpoint != nil {
		if opts.Checkpoint.MovedTo == nil {
			opts.Checkpoint.MovedTo = &BootstrapRecord{}
		}
		opts.Checkpoint.MovedTo.Name = bootstrap.Name
	}
	if err := m.deleteClusterObject(ctx, bootstrap, opts.Name, namespace, waitOpts); err != nil {
		return fmt.Errorf(
			"%w (bootstrap cluster %s left in place for recovery)",
			err,
			bootstrap.Name,
		)
	}

	m.logger.Printf("Deleting bootstrap cluster %s", bootstrap.Name)
	if err := m.bootstrapper.Delete(ctx, bootstrap); err != nil {
		m.logger.Printf("Warning: failed to delete bootstrap cluster %s: %v", bootstrap.Name, err)
	}
	return nil
}

// DeleteBootstrapCluster deletes a bootstrap cluster by name, for one that
// outlived its pivot (its deletion failed at create time).
func (m *Manager) DeleteBootstrapCluster(
	ctx context.Context,
	name string,
	providerSecrets map[string]string,
) error {
	return m.bootstrapper.Delete(ctx, &Cluster{Name: name, ProviderSecrets: providerSecrets})
}

// GetClusterInfo retrieves current information about a cluster. A cluster
// whose Cluster object is gone returns an error wrapping ErrClusterNotFound;
// an unreachable management cluster returns its error unchanged, so the
// caller can tell the two apart.
func (m *Manager) GetClusterInfo(
	ctx context.Context,
	mgmtKubeconfig, clusterName, namespace string,
) (*ClusterResult, error) {
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
	result.Conditions = ClusterConditions(obj)
	result.Phase = PhaseReady

	return result, nil
}

// ClusterConditions flattens an object's status.conditions.
func ClusterConditions(obj *unstructured.Unstructured) []Condition {
	if obj == nil {
		return nil
	}
	raw, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	out := make([]Condition, 0, len(raw))
	for _, c := range raw {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		cond := Condition{}
		cond.Type, _, _ = unstructured.NestedString(m, "type")
		cond.Status, _, _ = unstructured.NestedString(m, "status")
		cond.Reason, _, _ = unstructured.NestedString(m, "reason")
		cond.Message, _, _ = unstructured.NestedString(m, "message")
		out = append(out, cond)
	}
	return out
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

// handOver prepares the move of management from the bootstrap cluster to the
// workload cluster so that at most one provisioning engine runs at any time:
// the Cluster is paused (no new Machines or Workflows), in-flight Workflows
// drain, the bootstrap addons are uninstalled, and the workload cluster's
// own addons, declared as CAAPH HelmChartProxies on the bootstrap cluster,
// are waited for. From here the workload Tinkerbell is the only responder.
func (m *Manager) handOver(
	ctx context.Context,
	bootstrap, workload *Cluster,
	opts CreateClusterOptions,
	cp *Checkpoint,
	namespace string,
	waitOpts WaitOptions,
	advance func(Phase),
) error {
	m.logger.Printf("Pausing cluster %s for the handover", opts.Name)
	if err := m.applier.Pause(ctx, bootstrap, opts.Name, namespace); err != nil {
		return fmt.Errorf("pausing cluster %s before the move: %w", opts.Name, err)
	}
	if err := m.quiescer.WaitForQuiescence(ctx, bootstrap, namespace, waitOpts); err != nil {
		return fmt.Errorf("waiting for provisioning to go idle before the move: %w", err)
	}
	advance(PhasePaused)
	if !opts.BootstrapAddons.Empty() && !cp.Done(StepBootstrapAddonsRemoved) {
		m.logger.Printf("Uninstalling the bootstrap addons from %s", bootstrap.Name)
		if m.addons == nil {
			return fmt.Errorf(
				"uninstalling addons on %s: no addon installer configured",
				bootstrap.Name,
			)
		}
		if err := m.addons.Uninstall(ctx, bootstrap, opts.BootstrapAddons); err != nil {
			return fmt.Errorf("uninstalling the bootstrap addons: %w", err)
		}
		cp.Mark(StepBootstrapAddonsRemoved)
	}
	m.logger.Printf("Waiting for the addons of cluster %s", workload.Name)
	if err := m.waiter.WaitForAddons(ctx, bootstrap, opts.Name, namespace, waitOpts); err != nil {
		return fmt.Errorf("waiting for the workload cluster's addons before the move: %w", err)
	}
	advance(PhaseHandedOver)
	return nil
}

// rootSecrets reads the secrets bundle the cluster's providers generated:
// the Talos machine secrets (<name>-talos) when CABPT is in use, else the
// kubeadm CA key pair (<name>-ca). Keys are prefixed with the Secret name.
// Nothing found is not an error: the caller then has no root credential to
// persist, which is the case for clusters whose providers keep none.
func (m *Manager) rootSecrets(
	ctx context.Context,
	cluster *Cluster,
	name, namespace string,
) map[string]string {
	if m.secrets == nil {
		return nil
	}
	for _, secret := range []string{name + "-talos", name + "-ca"} {
		data, err := m.secrets.ReadSecret(ctx, cluster, namespace, secret)
		if errors.Is(err, ErrClusterNotFound) {
			continue
		}
		if err != nil {
			m.logger.Printf("Warning: could not read %s/%s: %v", namespace, secret, err)
			return nil
		}
		out := make(map[string]string, len(data))
		for k, v := range data {
			out[secret+"/"+k] = v
		}
		return out
	}
	return nil
}

// inventoryManifest renders the Tinkerbell inventory for the create options,
// or nil when there is nothing to register: inventory management is off, the
// infrastructure provider is not tinkerbell, or no machines are listed. With a
// pivot the bootstrap machine still hosts the management cluster, so it is
// left out rather than offered to CAPT until afterPivot, when it is a normal
// netboot target.
func inventoryManifest(
	opts CreateClusterOptions,
	namespace string,
	afterPivot bool,
) ([]byte, error) {
	infra := opts.Infrastructure.Provider
	if infra == "" {
		if p, ok := opts.Providers.Infrastructure(); ok {
			infra = p.Name
		}
	}
	// Only Tinkerbell has an inventory; a class cluster that names no
	// provider is given the benefit of the doubt.
	if !opts.ManageInventory || len(opts.Inventory) == 0 ||
		(infra != "" && !strings.EqualFold(infra, "tinkerbell")) {
		return nil, nil
	}
	inv := InventoryOptions{Namespace: namespace, ClusterName: opts.Name}
	for _, im := range opts.Inventory {
		if im.Hostname == opts.BootstrapMachine && opts.BootstrapMachine != "" && !afterPivot {
			continue
		}
		inv.Machines = append(inv.Machines, im)
	}
	if len(inv.Machines) == 0 {
		return nil, nil
	}
	return InventoryManifests(inv)
}

// installAddons installs an addon set on a cluster. An empty set is a no-op;
// a non-empty set needs an installer.
func (m *Manager) installAddons(ctx context.Context, cluster *Cluster, set AddonSet) error {
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
	if opts.Class == nil {
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
		m.logger.Printf(
			"Warning: failed to cleanup bootstrap cluster %s: %v",
			bootstrapCluster.Name,
			err,
		)
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
		m.logger.Printf(
			"Retrying CAPI move after transient error (attempt %d/%d, wait=%s): %v",
			attempt,
			maxAttempts,
			wait,
			err,
		)

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
