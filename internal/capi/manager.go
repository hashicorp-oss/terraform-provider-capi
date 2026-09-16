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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
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
		m.logger.Printf("Installing CAPI providers on %s", mgmtCluster.Name)
		initOpts := InitOptions{Providers: opts.Providers, Variables: initVariables(opts)}

		if err := m.installer.Init(ctx, mgmtCluster, initOpts); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("installing CAPI providers: %w", err)
		}
		m.logger.Printf("CAPI providers installed on %s", mgmtCluster.Name)
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

	// Step 5: Wait for cluster readiness
	if opts.WaitForReady {
		m.logger.Printf("Waiting for cluster %s to become ready", opts.Name)
		waitOpts := opts.Wait
		if waitOpts.Timeout == 0 {
			waitOpts = DefaultWaitOptions()
		}

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

	// Step 7: Self-managed pivot (optional)
	// This mirrors EKS Anywhere's moveClusterManagementTask. In-place skips the
	// move entirely: the bootstrap cluster stays as the management cluster (for
	// Talos, the bootstrap node IS the cluster via adopted secrets).
	if opts.SelfManaged && !opts.InPlace && needsBootstrap && kubeconfig != "" {
		m.logger.Printf("Pivoting CAPI management to workload cluster %s", opts.Name)

		// Write workload kubeconfig for the move operation
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

		// Install CAPI on the workload cluster before move
		initOpts := InitOptions{Providers: opts.Providers, Variables: initVariables(opts)}

		if err := m.installer.Init(ctx, workloadCluster, initOpts); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("installing CAPI on workload cluster for pivot: %w", err)
		}

		// Move CAPI management from bootstrap to workload.
		// EKS Anywhere applies retry policy here to absorb transient network faults.
		if err := m.moveWithRetry(ctx, mgmtCluster, workloadCluster, MoveOptions{
			Namespace: namespace,
		}); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("moving CAPI management: %w", err)
		}

		// Update the result to reflect the new management cluster
		result.Cluster.KubeconfigPath = workloadKubeconfigPath
		m.logger.Printf("CAPI management pivoted to %s", opts.Name)
	}

	// Step 8: Clean up bootstrap cluster (only after a pivot; in-place keeps it)
	if needsBootstrap && opts.SelfManaged && !opts.InPlace && bootstrapCluster != nil {
		m.logger.Printf("Deleting bootstrap cluster %s", bootstrapCluster.Name)
		if err := m.bootstrapper.Delete(ctx, bootstrapCluster); err != nil {
			m.logger.Printf("Warning: failed to delete bootstrap cluster %s: %v", bootstrapCluster.Name, err)
			// Don't fail the operation - the workload cluster is running
		} else {
			result.BootstrapCluster = nil
		}
	}

	// Step 9: Get cluster description
	descCluster := mgmtCluster
	if opts.SelfManaged && result.Cluster.KubeconfigPath != "" {
		descCluster = result.Cluster
	}
	description, err := m.describer.Describe(ctx, descCluster, opts.Name, namespace)
	if err != nil {
		m.logger.Printf("Warning: could not describe cluster: %v", err)
	} else {
		result.ClusterDescription = description
	}

	m.logger.Printf("Cluster %s creation completed successfully", opts.Name)
	return result, nil
}

// DeleteCluster deletes a CAPI cluster and optionally its bootstrap cluster.
// This mirrors EKS Anywhere's delete workflow.
func (m *Manager) DeleteCluster(ctx context.Context, opts DeleteClusterOptions) error {
	m.logger.Printf("Deleting cluster %s", opts.Name)

	namespace := opts.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Determine management cluster
	var mgmtCluster *Cluster
	if opts.ManagementKubeconfig != "" {
		mgmtCluster = &Cluster{
			Name:           "management",
			KubeconfigPath: opts.ManagementKubeconfig,
		}
	} else {
		return fmt.Errorf("management kubeconfig is required for cluster deletion")
	}

	// Delete the CAPI cluster resources
	if err := m.applier.Delete(ctx, mgmtCluster, opts.Name, namespace); err != nil {
		return fmt.Errorf("deleting cluster %s: %w", opts.Name, err)
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

// GetClusterInfo retrieves current information about a cluster.
func (m *Manager) GetClusterInfo(ctx context.Context, mgmtKubeconfig, clusterName, namespace string) (*ClusterResult, error) {
	mgmtCluster := &Cluster{
		Name:           "management",
		KubeconfigPath: mgmtKubeconfig,
	}

	if namespace == "" {
		namespace = "default"
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
