// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"time"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
)

// Cluster represents a Kubernetes cluster with connection information.
type Cluster struct {
	// Name is the cluster name.
	Name string

	// KubeconfigPath is the path to the kubeconfig file for this cluster.
	KubeconfigPath string

	// Namespace is the namespace where CAPI resources are managed.
	Namespace string

	// ProviderSecrets holds opaque, provider-specific secrets that must be
	// persisted and restored across operations (e.g. the Talos machine-secrets
	// bundle). Keys are provider-scoped. Nil when the provider keeps none.
	ProviderSecrets map[string]string
}

// BootstrapOptions configures bootstrap cluster creation.
type BootstrapOptions struct {
	// Name is the name for the bootstrap cluster. Auto-generated if empty.
	Name string

	// KubernetesVersion is the Kubernetes version for the bootstrap cluster.
	KubernetesVersion string

	// ExtraPortMappings adds port mappings to the bootstrap cluster nodes.
	ExtraPortMappings []PortMapping

	// ProviderSecrets seeds a bootstrapper with secrets persisted from a prior
	// operation, so it can recognize and manage a node it already provisioned.
	ProviderSecrets map[string]string
}

// PortMapping represents a port mapping for bootstrap cluster nodes.
type PortMapping struct {
	ContainerPort int32
	HostPort      int32
	Protocol      string
}

// InitOptions configures CAPI provider installation.
type InitOptions struct {
	// Kubeconfig is the path to the cluster's kubeconfig.
	Kubeconfig string

	// Providers lists every provider to install, grouped by type. Entries with
	// a version or fetch config register a URL override with clusterctl;
	// entries with customizations have their component YAML altered through
	// the injected repository factory.
	Providers ProviderSet

	// Variables are clusterctl variables (${VAR} in component and template
	// YAML). They take precedence over the clusterctl config file and environment.
	Variables map[string]string

	// NoDefaults stops clusterctl from adding the kubeadm bootstrap and
	// control-plane providers on a cluster's first init when Providers names
	// none (clusterctl's "-" provider). The core provider is still added
	// when missing, since nothing works without it.
	NoDefaults bool
}

// UpgradeOptions configures an in-place provider upgrade (clusterctl upgrade
// apply) on a management cluster.
type UpgradeOptions struct {
	// Providers is every configured provider, so URL overrides and
	// customizations resolve for the whole management cluster, not only for
	// the entries being upgraded.
	Providers ProviderSet

	// Targets are the providers to upgrade or re-render. An entry whose
	// configured Version matches the installed one is reinstalled at that
	// version, which re-applies its customizations.
	Targets []ProviderKey

	// Variables are clusterctl variables for component rendering.
	Variables map[string]string
}

// UninstallOptions configures provider removal (clusterctl delete). CRDs and
// namespaces are always kept, so objects of other clusters survive.
type UninstallOptions struct {
	// Providers are the providers to delete. Ignored when All is set.
	Providers ProviderSet

	// All deletes every provider in the management cluster's inventory.
	All bool
}

// MoveOptions configures CAPI management move operations.
type MoveOptions struct {
	// FromKubeconfig is the source cluster kubeconfig.
	FromKubeconfig string

	// ToKubeconfig is the target cluster kubeconfig.
	ToKubeconfig string

	// Namespace is the namespace to move resources from.
	Namespace string
}

// WaitOptions configures cluster readiness waiting.
type WaitOptions struct {
	// Timeout is the maximum time to wait for readiness.
	Timeout time.Duration

	// PollInterval is how frequently to check readiness.
	PollInterval time.Duration
}

// DefaultWaitOptions returns sensible default wait options.
func DefaultWaitOptions() WaitOptions {
	return WaitOptions{
		Timeout:      30 * time.Minute,
		PollInterval: 15 * time.Second,
	}
}

// ClusterResult contains the result of a cluster creation operation.
type ClusterResult struct {
	// Cluster is the workload cluster information.
	Cluster *Cluster

	// BootstrapCluster is the bootstrap cluster (if still running).
	BootstrapCluster *Cluster

	// Kubeconfig is the workload cluster kubeconfig content.
	Kubeconfig string

	// Endpoint is the API server endpoint.
	Endpoint string

	// CACertificate is the cluster CA certificate.
	CACertificate string

	// ClusterDescription is a human-readable cluster status description.
	ClusterDescription string

	// ProviderSecrets are provider-specific secrets to persist in state (see
	// Cluster.ProviderSecrets).
	ProviderSecrets map[string]string
}

// CreateClusterOptions configures the full cluster creation workflow.
type CreateClusterOptions struct {
	// Name is the cluster name.
	Name string

	// Namespace is the target namespace.
	Namespace string

	// Providers are the CAPI providers to install, grouped by type. Exactly one
	// infrastructure provider is required; with the control-plane and
	// bootstrap entries it selects the kinds the manifest generator emits.
	Providers ProviderSet

	// KubernetesVersion is the Kubernetes version.
	KubernetesVersion string

	// Network fills Cluster.spec.clusterNetwork and controlPlaneEndpoint.
	Network NetworkConfig

	// ControlPlane is the control plane topology (replicas and machine
	// template spec).
	ControlPlane ControlPlaneTopology

	// MachineDeployments are the worker MachineDeployments
	// (Cluster.spec.topology.workers.machineDeployments).
	MachineDeployments []MachineDeploymentTopology

	// ClusterClass generates a ClusterClass and a topology Cluster instead of
	// direct objects, enabling the ClusterTopology gate at init.
	ClusterClass bool

	// Variables are clusterctl variables (${VAR}) substituted into provider
	// component YAML at init.
	Variables map[string]string

	// ManagementKubeconfig is an existing management cluster kubeconfig.
	// If empty, a bootstrap cluster will be created.
	ManagementKubeconfig string

	// SkipInit skips running clusterctl init.
	SkipInit bool

	// WaitForReady waits for the cluster to become ready.
	WaitForReady bool

	// SelfManaged moves CAPI management to the workload cluster once its
	// control plane is initialized (the pivot), then deletes the bootstrap
	// cluster.
	SelfManaged bool

	// Wait configures timeout and poll options.
	Wait WaitOptions

	// KubeconfigOutputPath is where to write the workload cluster kubeconfig.
	KubeconfigOutputPath string

	// ProviderSecrets seeds provider-specific secrets persisted from a prior
	// apply (see Cluster.ProviderSecrets), so the bootstrapper can recognize a
	// node it already owns.
	ProviderSecrets map[string]string

	// ManageInventory renders Inventory as Tinkerbell Hardware, rufio Machine
	// and BMC Secret objects and applies them before the cluster manifest.
	// Only meaningful with the tinkerbell infrastructure provider.
	ManageInventory bool

	// Inventory is every bare-metal machine the cluster may use.
	Inventory []InventoryMachine

	// BootstrapMachine is the hostname of the Inventory machine that runs the
	// bootstrap cluster. It is left out of the inventory while it hosts the
	// management cluster and registered as a netboot target after the move.
	BootstrapMachine string

	// ManagementManifests are raw YAML manifests the caller wants on the
	// management cluster before the cluster manifest (IPAM pools, Secrets).
	ManagementManifests [][]byte

	// BootstrapAddons are the bootstrap cluster's own stack (a Tinkerbell on
	// host networking), installed right after clusterctl init there and
	// uninstalled before management moves away, so the workload cluster's
	// own instance is the only one running. The workload cluster's addons
	// are CAAPH HelmChartProxy objects, not this set.
	BootstrapAddons AddonSet

	// UpgradeProviders are configured providers whose settings changed since
	// the last apply: they are upgraded (or re-rendered at the same version)
	// in place with clusterctl upgrade apply, right after init. Ignored with
	// SkipInit.
	UpgradeProviders []ProviderKey

	// RemovedProviders are providers dropped from the configuration since the
	// last apply: they are deleted from the management cluster with
	// clusterctl delete, keeping CRDs and namespaces. Ignored with SkipInit.
	RemovedProviders ProviderSet
}

// HelmRelease describes one chart release the provider installs itself.
type HelmRelease struct {
	Name      string
	Namespace string
	// Chart is an oci:// reference or a chart name used with Repository.
	Chart      string
	Repository string
	Version    string
	// Values is a YAML document of chart values.
	Values  string
	Timeout time.Duration
}

// AddonSet is a group of addons the provider installs on a cluster in a fixed
// order: PreHelmManifestURLs, Prerequisites, Helm, Manifests.
type AddonSet struct {
	// PreHelmManifestURLs are raw manifests fetched by URL and applied first
	// (CRDs a chart renders custom resources against).
	PreHelmManifestURLs []string
	// Prerequisites are raw manifests applied before the Helm releases, for
	// objects the releases depend on. On a bootstrap cluster that is about to
	// hand over to a workload cluster they are skipped: they are meant for the
	// final management cluster (a Cilium load-balancer pool, say).
	Prerequisites []string
	// Helm are the releases, in order.
	Helm []HelmRelease
	// Manifests are raw manifests applied last.
	Manifests []string
}

// Empty reports whether nothing is configured.
func (a AddonSet) Empty() bool {
	return len(a.PreHelmManifestURLs) == 0 && len(a.Prerequisites) == 0 && len(a.Helm) == 0 && len(a.Manifests) == 0
}

// MachineDeploymentTopology mirrors Cluster.spec.topology.workers.machineDeployments[].
type MachineDeploymentTopology struct {
	Name          string
	Class         string
	Replicas      *int64
	FailureDomain string
	Labels        map[string]string
	Annotations   map[string]string

	// Infrastructure is the spec of this deployment's <Infra>MachineTemplate.
	Infrastructure map[string]any

	// Bootstrap is the spec of this deployment's <Bootstrap>ConfigTemplate.
	// Nil uses the bootstrap provider's ResourceConfig.
	Bootstrap map[string]any
}

// ControlPlaneTopology mirrors Cluster.spec.topology.controlPlane plus the
// spec of the control plane's <Infra>MachineTemplate.
type ControlPlaneTopology struct {
	Replicas       *int64
	Infrastructure map[string]any
}

// APIEndpoint is Cluster.spec.controlPlaneEndpoint.
type APIEndpoint struct {
	Host string
	Port int32
}

// NetworkConfig carries the Cluster.spec.clusterNetwork and controlPlaneEndpoint settings.
type NetworkConfig struct {
	ControlPlaneEndpoint *APIEndpoint
	PodCIDRs             []string
	ServiceCIDRs         []string
	ServiceDomain        string
}

// GenerateOptions configures cluster manifest generation.
type GenerateOptions struct {
	// ClusterName is the name of the cluster to generate.
	ClusterName string

	// Namespace is the target namespace for every object.
	Namespace string

	// KubernetesVersion is the Kubernetes version for the workload cluster.
	KubernetesVersion string

	// Providers is the full provider set. The infrastructure, control-plane
	// and bootstrap entries select the kinds to generate and carry the
	// ResourceConfig merged into each object's spec.
	Providers ProviderSet

	// Network fills Cluster.spec.clusterNetwork and controlPlaneEndpoint.
	Network NetworkConfig

	// ControlPlane sets the control plane replicas and machine template spec.
	ControlPlane ControlPlaneTopology

	// MachineDeployments are the worker MachineDeployments (or worker classes
	// and topology entries when ClusterClass is set).
	MachineDeployments []MachineDeploymentTopology

	// ClusterClass generates a ClusterClass and a topology Cluster instead of
	// direct objects.
	ClusterClass bool
}

// ProviderType is a CAPI provider category, spelled the way clusterctl names
// component files (e.g. "control-plane" -> control-plane-components.yaml).
type ProviderType string

const (
	ProviderTypeCore           ProviderType = "core"
	ProviderTypeInfrastructure ProviderType = "infrastructure"
	ProviderTypeBootstrap      ProviderType = "bootstrap"
	ProviderTypeControlPlane   ProviderType = "control-plane"
	ProviderTypeIPAM           ProviderType = "ipam"
	ProviderTypeAddon          ProviderType = "addon"
)

// AllProviderTypes returns every ProviderType in clusterctl install order.
func AllProviderTypes() []ProviderType {
	return []ProviderType{
		ProviderTypeCore, ProviderTypeBootstrap, ProviderTypeControlPlane,
		ProviderTypeInfrastructure, ProviderTypeIPAM, ProviderTypeAddon,
	}
}

// ComponentsFile returns the release asset name clusterctl fetches for this type.
func (t ProviderType) ComponentsFile() string {
	return string(t) + "-components.yaml"
}

// Clusterctl maps to the clusterctl API enum.
func (t ProviderType) Clusterctl() clusterctlv1.ProviderType {
	switch t {
	case ProviderTypeCore:
		return clusterctlv1.CoreProviderType
	case ProviderTypeInfrastructure:
		return clusterctlv1.InfrastructureProviderType
	case ProviderTypeBootstrap:
		return clusterctlv1.BootstrapProviderType
	case ProviderTypeControlPlane:
		return clusterctlv1.ControlPlaneProviderType
	case ProviderTypeIPAM:
		return clusterctlv1.IPAMProviderType
	case ProviderTypeAddon:
		return clusterctlv1.AddonProviderType
	}
	return clusterctlv1.ProviderTypeUnknown
}

// ProviderTypeFromClusterctl is the inverse of Clusterctl. ok is false for
// types this provider does not manage (runtime extensions, unknown).
func ProviderTypeFromClusterctl(t clusterctlv1.ProviderType) (ProviderType, bool) {
	for _, pt := range AllProviderTypes() {
		if pt.Clusterctl() == t {
			return pt, true
		}
	}
	return "", false
}

// FetchConfig configures where provider components are fetched from.
// URL and OCI are verbatim clusterctl references. Owner and Repository build
// a GitHub releases URL together with the provider version; both default from
// clusterctl's built-in entry for the provider when unset.
type FetchConfig struct {
	Owner      string
	Repository string
	URL        string
	OCI        string
}

// ProviderConfig carries the full configuration for one CAPI provider,
// modeled after the cluster-api-operator provider CRDs. Customizations are
// applied natively by wrapping the clusterctl client's repository factory;
// the operator itself is not required.
type ProviderConfig struct {
	// Name is the provider name as clusterctl knows it (e.g. "tinkerbell").
	Name string

	// Type is the provider category.
	Type ProviderType

	// Version is the release tag (e.g. "v0.7.9"). Empty means clusterctl's latest.
	Version string

	// FetchConfig overrides where components are fetched from. Nil uses clusterctl defaults.
	FetchConfig *FetchConfig

	// ConfigVariables are template variables injected into the provider's
	// component YAML during processing (${VAR} substitution).
	ConfigVariables map[string]string

	// SecretConfigVariables are sensitive template variables.
	SecretConfigVariables map[string]string

	// Deployment customizes the provider controller deployment.
	Deployment *DeploymentConfig

	// Manager configures the controller manager.
	Manager *ManagerConfig

	// AdditionalManifests is inline YAML applied along with the provider components.
	AdditionalManifests string

	// ManifestPatches are JSON merge patches (RFC 7396). Mutually exclusive with Patches.
	ManifestPatches []string

	// Patches are strategic merge or RFC6902 patches with target selectors.
	Patches []PatchConfig

	// ResourceConfig is deep-merged into the spec of the provider's
	// cluster-scoped CAPI object (the <Infra>Cluster, <CP>ControlPlane or
	// default <Bootstrap>ConfigTemplate). Nil leaves only defaults and wiring.
	// It never alters component YAML, so it does not count as a customization.
	ResourceConfig map[string]any
}

// InitString renders the "name" or "name:version" form clusterctl init expects.
func (p ProviderConfig) InitString() string {
	if p.Version == "" {
		return p.Name
	}
	return p.Name + ":" + p.Version
}

// HasCustomizations reports whether the component YAML must be altered.
// Version and FetchConfig only affect where components come from.
func (p ProviderConfig) HasCustomizations() bool {
	return len(p.ConfigVariables) > 0 || len(p.SecretConfigVariables) > 0 ||
		p.Deployment != nil || p.Manager != nil || p.AdditionalManifests != "" ||
		len(p.ManifestPatches) > 0 || len(p.Patches) > 0
}

// NeedsURLOverride reports whether a clusterctl provider URL must be
// registered for this entry (a pinned version or any fetch config).
func (p ProviderConfig) NeedsURLOverride() bool {
	return p.Version != "" || p.FetchConfig != nil
}

// ProviderKey identifies a provider by type and name. Bootstrap "talos" and
// control-plane "talos" are different providers.
type ProviderKey struct {
	Type ProviderType
	Name string
}

// ProviderSet groups providers by type, preserving insertion order per type.
type ProviderSet map[ProviderType][]ProviderConfig

// Add appends p under p.Type.
func (s ProviderSet) Add(p ProviderConfig) {
	s[p.Type] = append(s[p.Type], p)
}

// Infrastructure returns the single infrastructure provider, if present.
func (s ProviderSet) Infrastructure() (ProviderConfig, bool) {
	if infra := s[ProviderTypeInfrastructure]; len(infra) > 0 {
		return infra[0], true
	}
	return ProviderConfig{}, false
}

// InitStrings returns the clusterctl init strings for one type.
func (s ProviderSet) InitStrings(t ProviderType) []string {
	out := make([]string, 0, len(s[t]))
	for _, p := range s[t] {
		out = append(out, p.InitString())
	}
	return out
}

// Customized returns providers whose component YAML must be altered, keyed by type and name.
func (s ProviderSet) Customized() map[ProviderKey]ProviderConfig {
	out := map[ProviderKey]ProviderConfig{}
	for _, p := range s.All() {
		if p.HasCustomizations() {
			out[ProviderKey{Type: p.Type, Name: p.Name}] = p
		}
	}
	return out
}

// All flattens the set in clusterctl install order.
func (s ProviderSet) All() []ProviderConfig {
	var out []ProviderConfig
	for _, t := range AllProviderTypes() {
		out = append(out, s[t]...)
	}
	return out
}

// Find returns the provider with the given type and name.
func (s ProviderSet) Find(key ProviderKey) (ProviderConfig, bool) {
	for _, p := range s[key.Type] {
		if p.Name == key.Name {
			return p, true
		}
	}
	return ProviderConfig{}, false
}

// Subset returns the providers with the given keys, in install order.
// Unknown keys are skipped.
func (s ProviderSet) Subset(keys []ProviderKey) ProviderSet {
	out := ProviderSet{}
	for _, p := range s.All() {
		for _, k := range keys {
			if p.Type == k.Type && p.Name == k.Name {
				out.Add(p)
				break
			}
		}
	}
	return out
}

// Empty reports whether the set has no providers.
func (s ProviderSet) Empty() bool {
	return len(s.All()) == 0
}

// Keys returns the keys of every provider, in install order.
func (s ProviderSet) Keys() []ProviderKey {
	var out []ProviderKey
	for _, p := range s.All() {
		out = append(out, ProviderKey{Type: p.Type, Name: p.Name})
	}
	return out
}

// DeploymentConfig customizes a provider controller deployment.
type DeploymentConfig struct {
	Replicas           *int64
	NodeSelector       map[string]string
	ServiceAccountName string
	Containers         []ContainerConfig
}

// ContainerConfig overrides container settings in a provider deployment.
type ContainerConfig struct {
	Name     string
	ImageURL string
	Args     map[string]string
	Command  []string
}

// ManagerConfig configures the provider controller manager.
type ManagerConfig struct {
	ProfilerAddress         string
	MaxConcurrentReconciles *int64
	Verbosity               *int64
	FeatureGates            map[string]bool
	AdditionalArgs          map[string]string
}

// PatchConfig is a patch with an optional target selector.
type PatchConfig struct {
	Patch  string
	Target *PatchSelector
}

// PatchSelector selects objects to apply a patch to.
type PatchSelector struct {
	Group         string
	Version       string
	Kind          string
	Name          string
	Namespace     string
	LabelSelector string
}

// DeleteClusterOptions configures cluster deletion.
type DeleteClusterOptions struct {
	// Name is the cluster name.
	Name string

	// Namespace is the cluster namespace.
	Namespace string

	// ManagementKubeconfig is the management cluster kubeconfig.
	ManagementKubeconfig string

	// DeleteBootstrap indicates whether to delete the bootstrap cluster too.
	DeleteBootstrap bool

	// BootstrapName is the name of the bootstrap cluster to delete.
	BootstrapName string

	// ProviderSecrets are provider-specific secrets persisted from create, used
	// to reach a node we own during teardown (see Cluster.ProviderSecrets).
	ProviderSecrets map[string]string

	// SelfManaged marks a cluster that manages itself after a pivot. Deleting
	// its Cluster object from inside would power off the machines running
	// the controllers, so management is first moved to a fresh bootstrap
	// cluster (the reverse of the create pivot) and the cluster is deleted
	// from there.
	SelfManaged bool

	// Spec is the configuration the cluster was created with. A self-managed
	// deletion needs it to rebuild a management cluster: providers, their
	// variables, management addons and manifests, and the inventory (BMC
	// Secrets do not move with clusterctl, and rufio needs them to power
	// machines off).
	Spec CreateClusterOptions

	// Wait bounds the wait for the Cluster object to disappear after it is
	// deleted. Zero uses DefaultWaitOptions.
	Wait WaitOptions

	// UninstallProviders removes every provider from the management cluster
	// (clusterctl delete --all, keeping CRDs and namespaces) once the cluster
	// is gone. Meant for a management cluster this resource initialised.
	UninstallProviders bool
}
