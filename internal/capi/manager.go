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
//  3. Generate workload cluster template (clusterctl generate)
//  4. Apply template to bootstrap cluster (kubectl apply)
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
)

// Manager orchestrates the full CAPI cluster lifecycle.
// It mirrors EKS Anywhere's Create workflow (management/create.go)
// by coordinating bootstrap creation, CAPI init, template apply,
// readiness waiting, management move, and bootstrap cleanup.
type Manager struct {
	bootstrapper  Bootstrapper
	installer     Installer
	templateGen   TemplateGenerator
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

// WithTemplateGenerator sets the template generator implementation.
func WithTemplateGenerator(t TemplateGenerator) ManagerOption {
	return func(m *Manager) { m.templateGen = t }
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
	configPath := ""
	if home, err := os.UserHomeDir(); err == nil {
		configPath = filepath.Join(home, ".cluster-api")
	}

	infoRetriever := NewClusterctlInfoRetriever(configPath)

	m := &Manager{
		bootstrapper:  NewKindBootstrapper(),
		installer:     NewClusterctlInstaller(configPath),
		templateGen:   NewClusterctlTemplateGenerator(configPath),
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
//  4. createWorkloadClusterTask (generate + apply template)
//  5. Wait for readiness
//  6. moveClusterManagementTask (optional, for self-managed clusters)
//  7. deleteBootstrapClusterTask
func (m *Manager) CreateCluster(ctx context.Context, opts CreateClusterOptions) (*ClusterResult, error) {
	m.logger.Printf("Starting cluster creation: %s", opts.Name)

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

	// Step 2: Install CAPI providers on management/bootstrap cluster
	if !opts.SkipInit {
		m.logger.Printf("Installing CAPI providers on %s", mgmtCluster.Name)
		initOpts := InitOptions{
			CoreProvider:            opts.CoreProvider,
			InfrastructureProviders: []string{opts.InfrastructureProvider},
		}
		if opts.BootstrapProvider != "" {
			initOpts.BootstrapProviders = []string{opts.BootstrapProvider}
		}
		if opts.ControlPlaneProvider != "" {
			initOpts.ControlPlaneProviders = []string{opts.ControlPlaneProvider}
		}

		if err := m.installer.Init(ctx, mgmtCluster, initOpts); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("installing CAPI providers: %w", err)
		}
		m.logger.Printf("CAPI providers installed on %s", mgmtCluster.Name)
	}

	// Step 3: Generate cluster template
	m.logger.Printf("Generating cluster template for %s", opts.Name)
	namespace := opts.Namespace
	if namespace == "" {
		namespace = "default"
	}

	templateOpts := TemplateOptions{
		ClusterName:              opts.Name,
		Namespace:                namespace,
		KubernetesVersion:        opts.KubernetesVersion,
		InfrastructureProvider:   opts.InfrastructureProvider,
		Flavor:                   opts.Flavor,
		ControlPlaneMachineCount: opts.ControlPlaneMachineCount,
		WorkerMachineCount:       opts.WorkerMachineCount,
	}

	manifest, err := m.templateGen.Generate(ctx, mgmtCluster, templateOpts)
	if err != nil {
		m.cleanupOnError(ctx, bootstrapCluster)
		return nil, fmt.Errorf("generating cluster template: %w", err)
	}

	// Step 4: Apply template to management cluster
	m.logger.Printf("Applying cluster template for %s", opts.Name)
	if err := m.applier.Apply(ctx, mgmtCluster, manifest); err != nil {
		m.cleanupOnError(ctx, bootstrapCluster)
		return nil, fmt.Errorf("applying cluster template: %w", err)
	}

	result := &ClusterResult{
		Cluster: &Cluster{
			Name:      opts.Name,
			Namespace: namespace,
		},
		BootstrapCluster: bootstrapCluster,
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
	// This mirrors EKS Anywhere's moveClusterManagementTask
	if opts.SelfManaged && needsBootstrap && kubeconfig != "" {
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
		initOpts := InitOptions{
			CoreProvider:            opts.CoreProvider,
			InfrastructureProviders: []string{opts.InfrastructureProvider},
		}
		if opts.BootstrapProvider != "" {
			initOpts.BootstrapProviders = []string{opts.BootstrapProvider}
		}
		if opts.ControlPlaneProvider != "" {
			initOpts.ControlPlaneProviders = []string{opts.ControlPlaneProvider}
		}

		if err := m.installer.Init(ctx, workloadCluster, initOpts); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("installing CAPI on workload cluster for pivot: %w", err)
		}

		// Move CAPI management from bootstrap to workload
		if err := m.mover.Move(ctx, mgmtCluster, workloadCluster, MoveOptions{
			Namespace: namespace,
		}); err != nil {
			m.cleanupOnError(ctx, bootstrapCluster)
			return nil, fmt.Errorf("moving CAPI management: %w", err)
		}

		// Update the result to reflect the new management cluster
		result.Cluster.KubeconfigPath = workloadKubeconfigPath
		m.logger.Printf("CAPI management pivoted to %s", opts.Name)
	}

	// Step 8: Clean up bootstrap cluster (if we created one and pivot succeeded)
	if needsBootstrap && opts.SelfManaged && bootstrapCluster != nil {
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
		bootstrapCluster := &Cluster{Name: opts.BootstrapName}
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
