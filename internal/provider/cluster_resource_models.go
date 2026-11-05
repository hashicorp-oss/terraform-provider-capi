// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"sigs.k8s.io/yaml"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// providerSecretsToDynamic wraps a provider-secrets map as a dynamic value
// holding a map of strings, or a known null dynamic when the map is empty.
func providerSecretsToDynamic(ctx context.Context, m map[string]string) (types.Dynamic, diag.Diagnostics) {
	if len(m) == 0 {
		return types.DynamicNull(), nil
	}
	mapVal, diags := types.MapValueFrom(ctx, types.StringType, m)
	if diags.HasError() {
		return types.DynamicNull(), diags
	}
	return types.DynamicValue(mapVal), diags
}

// providerSecretsFromDynamic reads a provider-secrets map from a dynamic value,
// returning nil when unset or when the underlying value is not a string map.
func providerSecretsFromDynamic(ctx context.Context, d types.Dynamic) (map[string]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if d.IsNull() || d.IsUnknown() {
		return nil, diags
	}
	mapVal, ok := d.UnderlyingValue().(types.Map)
	if !ok {
		return nil, diags
	}
	out := map[string]string{}
	diags = mapVal.ElementsAs(ctx, &out, false)
	return out, diags
}

// ClusterResourceModel describes the resource data model using nested attributes.
// This is the v1 schema model (nested-first design per terraform.instructions.md).
type ClusterResourceModel struct {
	Name              types.String `tfsdk:"name"`
	KubernetesVersion types.String `tfsdk:"kubernetes_version"`
	ClusterClass      types.Bool   `tfsdk:"cluster_class"`
	Id                types.String `tfsdk:"id"`

	ProviderSecrets types.Dynamic `tfsdk:"provider_secrets"`

	Management     types.Object `tfsdk:"management"`
	Core           types.Map    `tfsdk:"core"`
	Infrastructure types.Map    `tfsdk:"infrastructure"`
	Bootstrap      types.Map    `tfsdk:"bootstrap"`
	ControlPlane   types.Map    `tfsdk:"control_plane"`
	IPAM           types.Map    `tfsdk:"ipam"`
	Addon          types.Map    `tfsdk:"addon"`
	Topology       types.Object `tfsdk:"topology"`
	Network        types.Object `tfsdk:"network"`   // ClusterNetworkModel
	Manifests      types.Object `tfsdk:"manifests"` // ManifestsModel
	Inventory      types.Object `tfsdk:"inventory"`
	Wait           types.Object `tfsdk:"wait"`
	Output         types.Object `tfsdk:"output"`
	Status         types.Object `tfsdk:"status"`
}

// ManagementModel groups attributes related to cluster management configuration.
type ManagementModel struct {
	Kubeconfig  types.String `tfsdk:"kubeconfig"`
	SkipInit    types.Bool   `tfsdk:"skip_init"`
	SelfManaged types.Bool   `tfsdk:"self_managed"`
	Uninstall   types.Bool   `tfsdk:"uninstall"`
	Namespace   types.String `tfsdk:"namespace"`
	Bootstrap   types.Object `tfsdk:"bootstrap"` // ManagementBootstrapModel
}

// ManagementBootstrapModel configures the transient bootstrap cluster.
type ManagementBootstrapModel struct {
	Type    types.String `tfsdk:"type"`
	Machine types.String `tfsdk:"machine"`
	Boot    types.Object `tfsdk:"boot"`   // BootModel
	Kairos  types.Object `tfsdk:"kairos"` // KairosModel
	Addons  types.Object `tfsdk:"addons"` // BootstrapAddonsModel
}

// BootModel controls how the bootstrap node boots the live image.
type BootModel struct {
	Method   types.String `tfsdk:"method"`
	Timeout  types.String `tfsdk:"timeout"`
	Attempts types.Int64  `tfsdk:"attempts"`
	Device   types.String `tfsdk:"device"`
}

// KairosModel configures the disposable Kairos live node (type = "kairos").
type KairosModel struct {
	ISO     types.String `tfsdk:"iso"`
	Address types.String `tfsdk:"address"`
	SSH     types.Object `tfsdk:"ssh"` // KairosSSHModel
}

// KairosSSHModel is how the live node is reached.
type KairosSSHModel struct {
	User       types.String `tfsdk:"user"`
	Port       types.Int64  `tfsdk:"port"`
	PrivateKey types.String `tfsdk:"private_key"`
	HostKey    types.String `tfsdk:"host_key"`
}

// BootstrapAddonsModel lists Helm releases and manifests for the bootstrap cluster.
type BootstrapAddonsModel struct {
	Prerequisites types.List `tfsdk:"prerequisites"`
	Helm          types.List `tfsdk:"helm"` // []HelmReleaseModel
	Manifests     types.List `tfsdk:"manifests"`
}

// HelmReleaseModel describes one Helm release.
type HelmReleaseModel struct {
	Name       types.String `tfsdk:"name"`
	Namespace  types.String `tfsdk:"namespace"`
	Chart      types.String `tfsdk:"chart"`
	Repository types.String `tfsdk:"repository"`
	Version    types.String `tfsdk:"version"`
	Values     types.String `tfsdk:"values"`
	Timeout    types.String `tfsdk:"timeout"`
}

// ProviderModel is the shared nested object of every provider map (core,
// infrastructure, bootstrap, control_plane, ipam, addon), modeled after the
// cluster-api-operator provider CRDs. Customizations are applied natively by
// wrapping the clusterctl client's repository factory.
type ProviderModel struct {
	Version               types.String `tfsdk:"version"`
	FetchConfig           types.Object `tfsdk:"fetch_config"`
	ConfigVariables       types.Map    `tfsdk:"config_variables"`
	SecretConfigVariables types.Map    `tfsdk:"secret_config_variables"`
	Deployment            types.Object `tfsdk:"deployment"`
	Manager               types.Object `tfsdk:"manager"`
	AdditionalManifests   types.String `tfsdk:"additional_manifests"`
	ManifestPatches       types.List   `tfsdk:"manifest_patches"`
	Patches               types.List   `tfsdk:"patches"`
	ResourceConfig        types.String `tfsdk:"resource_config"` // YAML/JSON object merged into the provider's cluster object spec
}

// FetchConfigModel controls where provider components are fetched from.
type FetchConfigModel struct {
	Owner      types.String `tfsdk:"owner"`
	Repository types.String `tfsdk:"repository"`
	URL        types.String `tfsdk:"url"`
	OCI        types.String `tfsdk:"oci"`
}

// DeploymentModel maps to the cluster-api-operator DeploymentSpec.
type DeploymentModel struct {
	Replicas           types.Int64  `tfsdk:"replicas"`
	NodeSelector       types.Map    `tfsdk:"node_selector"`
	ServiceAccountName types.String `tfsdk:"service_account_name"`
	Containers         types.List   `tfsdk:"containers"`
}

// ContainerModel maps to the cluster-api-operator ContainerSpec.
type ContainerModel struct {
	Name     types.String `tfsdk:"name"`
	ImageURL types.String `tfsdk:"image_url"`
	Args     types.Map    `tfsdk:"args"`
	Command  types.List   `tfsdk:"command"`
}

// ManagerModel maps to the cluster-api-operator ManagerSpec.
type ManagerModel struct {
	ProfilerAddress         types.String `tfsdk:"profiler_address"`
	MaxConcurrentReconciles types.Int64  `tfsdk:"max_concurrent_reconciles"`
	Verbosity               types.Int64  `tfsdk:"verbosity"`
	FeatureGates            types.Map    `tfsdk:"feature_gates"`
	AdditionalArgs          types.Map    `tfsdk:"additional_args"`
}

// PatchModel maps to the cluster-api-operator Patch.
type PatchModel struct {
	Patch  types.String `tfsdk:"patch"`
	Target types.Object `tfsdk:"target"`
}

// PatchSelectorModel maps to the cluster-api-operator PatchSelector.
type PatchSelectorModel struct {
	Group         types.String `tfsdk:"group"`
	Version       types.String `tfsdk:"version"`
	Kind          types.String `tfsdk:"kind"`
	Name          types.String `tfsdk:"name"`
	Namespace     types.String `tfsdk:"namespace"`
	LabelSelector types.String `tfsdk:"label_selector"`
}

// TopologyModel mirrors Cluster.spec.topology.
type TopologyModel struct {
	ControlPlane types.Object `tfsdk:"control_plane"` // ControlPlaneTopologyModel
	Workers      types.Object `tfsdk:"workers"`       // WorkersTopologyModel
}

// ControlPlaneTopologyModel mirrors Cluster.spec.topology.controlPlane plus
// the control plane's machine template spec.
type ControlPlaneTopologyModel struct {
	Replicas       types.Int64  `tfsdk:"replicas"`
	Infrastructure types.String `tfsdk:"infrastructure"` // YAML/JSON <Infra>MachineTemplate spec
}

// WorkersTopologyModel mirrors Cluster.spec.topology.workers.
type WorkersTopologyModel struct {
	MachineDeployments types.List `tfsdk:"machine_deployments"` // []MachineDeploymentModel
}

// MachineDeploymentModel mirrors Cluster.spec.topology.workers.machineDeployments[].
type MachineDeploymentModel struct {
	Name           types.String `tfsdk:"name"`
	Class          types.String `tfsdk:"class"`
	Replicas       types.Int64  `tfsdk:"replicas"`
	FailureDomain  types.String `tfsdk:"failure_domain"`
	Metadata       types.Object `tfsdk:"metadata"`       // TopologyMetadataModel
	Infrastructure types.String `tfsdk:"infrastructure"` // YAML/JSON <Infra>MachineTemplate spec
	Bootstrap      types.String `tfsdk:"bootstrap"`      // YAML/JSON <Bootstrap>ConfigTemplate spec
}

// ClusterNetworkModel mirrors Cluster.spec.clusterNetwork and controlPlaneEndpoint.
type ClusterNetworkModel struct {
	ControlPlaneEndpoint types.Object `tfsdk:"control_plane_endpoint"` // EndpointModel
	Pods                 types.Object `tfsdk:"pods"`                   // NetworkRangesModel
	Services             types.Object `tfsdk:"services"`               // NetworkRangesModel
	ServiceDomain        types.String `tfsdk:"service_domain"`
}

// EndpointModel is Cluster.spec.controlPlaneEndpoint.
type EndpointModel struct {
	Host types.String `tfsdk:"host"`
	Port types.Int64  `tfsdk:"port"`
}

// NetworkRangesModel is Cluster.spec.clusterNetwork.pods / services.
type NetworkRangesModel struct {
	CIDRBlocks types.List `tfsdk:"cidr_blocks"`
}

// TopologyMetadataModel holds labels and annotations for topology machines.
type TopologyMetadataModel struct {
	Labels      types.Map `tfsdk:"labels"`
	Annotations types.Map `tfsdk:"annotations"`
}

// InventoryModel groups attributes for hardware inventory.
type InventoryModel struct {
	Manage  types.Bool   `tfsdk:"manage"`
	Source  types.String `tfsdk:"source"`
	Machine types.List   `tfsdk:"machine"`
}

// ManifestsModel carries raw manifests the provider applies for the user.
type ManifestsModel struct {
	Management types.List `tfsdk:"management"`
}

// MachineModel describes a single machine in the inventory.
type MachineModel struct {
	Hostname types.String `tfsdk:"hostname"`
	Network  types.Object `tfsdk:"network"`
	Disk     types.Object `tfsdk:"disk"`
	BMC      types.Object `tfsdk:"bmc"`
	Labels   types.Map    `tfsdk:"labels"`
}

// NetworkModel describes machine network configuration.
type NetworkModel struct {
	IPAddress   types.String `tfsdk:"ip_address"`
	Netmask     types.String `tfsdk:"netmask"`
	Gateway     types.String `tfsdk:"gateway"`
	MACAddress  types.String `tfsdk:"mac_address"`
	Nameservers types.List   `tfsdk:"nameservers"`
	VLANID      types.String `tfsdk:"vlan_id"`
}

// DiskModel describes machine disk configuration.
type DiskModel struct {
	Device types.String `tfsdk:"device"`
}

// BMCModel describes BMC (Baseboard Management Controller) configuration.
type BMCModel struct {
	Address  types.String `tfsdk:"address"`
	Username types.String `tfsdk:"username"`
	Password types.String `tfsdk:"password"`
}

// WaitModel groups attributes for readiness wait configuration.
type WaitModel struct {
	Enabled types.Bool   `tfsdk:"enabled"`
	Timeout types.String `tfsdk:"timeout"`
}

// OutputModel groups attributes for output configuration.
type OutputModel struct {
	KubeconfigPath types.String `tfsdk:"kubeconfig_path"`
}

// StatusModel groups all computed cluster status outputs.
type StatusModel struct {
	Endpoint         types.String `tfsdk:"endpoint"`
	Kubeconfig       types.String `tfsdk:"kubeconfig"`
	CACertificate    types.String `tfsdk:"ca_certificate"`
	Description      types.String `tfsdk:"description"`
	BootstrapCluster types.String `tfsdk:"bootstrap_cluster"`
}

// --- Attribute Type Maps ---
// Each nested model requires an attrTypes() function for types.ObjectValueFrom() and types.ObjectNull().

func managementAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"kubeconfig":   types.StringType,
		"skip_init":    types.BoolType,
		"self_managed": types.BoolType,
		"uninstall":    types.BoolType,
		"namespace":    types.StringType,
		"bootstrap":    types.ObjectType{AttrTypes: managementBootstrapAttrTypes()},
	}
}

func managementBootstrapAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"type":    types.StringType,
		"machine": types.StringType,
		"boot":    types.ObjectType{AttrTypes: bootAttrTypes()},
		"kairos":  types.ObjectType{AttrTypes: kairosAttrTypes()},
		"addons":  types.ObjectType{AttrTypes: bootstrapAddonsAttrTypes()},
	}
}

func bootAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"method":   types.StringType,
		"timeout":  types.StringType,
		"attempts": types.Int64Type,
		"device":   types.StringType,
	}
}

func kairosAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"iso":     types.StringType,
		"address": types.StringType,
		"ssh":     types.ObjectType{AttrTypes: kairosSSHAttrTypes()},
	}
}

func kairosSSHAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"user":        types.StringType,
		"port":        types.Int64Type,
		"private_key": types.StringType,
		"host_key":    types.StringType,
	}
}

func bootstrapAddonsAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"prerequisites": types.ListType{ElemType: types.StringType},
		"helm":          types.ListType{ElemType: types.ObjectType{AttrTypes: helmReleaseAttrTypes()}},
		"manifests":     types.ListType{ElemType: types.StringType},
	}
}

func helmReleaseAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":       types.StringType,
		"namespace":  types.StringType,
		"chart":      types.StringType,
		"repository": types.StringType,
		"version":    types.StringType,
		"values":     types.StringType,
		"timeout":    types.StringType,
	}
}

func providerAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"version":                 types.StringType,
		"fetch_config":            types.ObjectType{AttrTypes: fetchConfigAttrTypes()},
		"config_variables":        types.MapType{ElemType: types.StringType},
		"secret_config_variables": types.MapType{ElemType: types.StringType},
		"deployment":              types.ObjectType{AttrTypes: deploymentAttrTypes()},
		"manager":                 types.ObjectType{AttrTypes: managerAttrTypes()},
		"additional_manifests":    types.StringType,
		"manifest_patches":        types.ListType{ElemType: types.StringType},
		"patches":                 types.ListType{ElemType: types.ObjectType{AttrTypes: patchAttrTypes()}},
		"resource_config":         types.StringType,
	}
}

// providerMapType is the Terraform type of every provider map attribute.
func providerMapType() types.MapType {
	return types.MapType{ElemType: types.ObjectType{AttrTypes: providerAttrTypes()}}
}

func fetchConfigAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"owner":      types.StringType,
		"repository": types.StringType,
		"url":        types.StringType,
		"oci":        types.StringType,
	}
}

func topologyAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"control_plane": types.ObjectType{AttrTypes: controlPlaneTopologyAttrTypes()},
		"workers":       types.ObjectType{AttrTypes: workersTopologyAttrTypes()},
	}
}

func controlPlaneTopologyAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"replicas": types.Int64Type, "infrastructure": types.StringType}
}

func workersTopologyAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"machine_deployments": types.ListType{ElemType: types.ObjectType{AttrTypes: machineDeploymentAttrTypes()}},
	}
}

func machineDeploymentAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":           types.StringType,
		"class":          types.StringType,
		"replicas":       types.Int64Type,
		"failure_domain": types.StringType,
		"metadata":       types.ObjectType{AttrTypes: topologyMetadataAttrTypes()},
		"infrastructure": types.StringType,
		"bootstrap":      types.StringType,
	}
}

func clusterNetworkAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"control_plane_endpoint": types.ObjectType{AttrTypes: endpointAttrTypes()},
		"pods":                   types.ObjectType{AttrTypes: networkRangesAttrTypes()},
		"services":               types.ObjectType{AttrTypes: networkRangesAttrTypes()},
		"service_domain":         types.StringType,
	}
}

func endpointAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"host": types.StringType, "port": types.Int64Type}
}

func networkRangesAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"cidr_blocks": types.ListType{ElemType: types.StringType}}
}

func topologyMetadataAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"labels":      types.MapType{ElemType: types.StringType},
		"annotations": types.MapType{ElemType: types.StringType},
	}
}

func deploymentAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"replicas":             types.Int64Type,
		"node_selector":        types.MapType{ElemType: types.StringType},
		"service_account_name": types.StringType,
		"containers":           types.ListType{ElemType: types.ObjectType{AttrTypes: containerAttrTypes()}},
	}
}

func containerAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":      types.StringType,
		"image_url": types.StringType,
		"args":      types.MapType{ElemType: types.StringType},
		"command":   types.ListType{ElemType: types.StringType},
	}
}

func managerAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"profiler_address":          types.StringType,
		"max_concurrent_reconciles": types.Int64Type,
		"verbosity":                 types.Int64Type,
		"feature_gates":             types.MapType{ElemType: types.BoolType},
		"additional_args":           types.MapType{ElemType: types.StringType},
	}
}

func patchAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"patch":  types.StringType,
		"target": types.ObjectType{AttrTypes: patchSelectorAttrTypes()},
	}
}

func patchSelectorAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"group":          types.StringType,
		"version":        types.StringType,
		"kind":           types.StringType,
		"name":           types.StringType,
		"namespace":      types.StringType,
		"label_selector": types.StringType,
	}
}

func inventoryAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"manage":  types.BoolType,
		"source":  types.StringType,
		"machine": types.ListType{ElemType: types.ObjectType{AttrTypes: machineAttrTypes()}},
	}
}

func manifestsAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"management": types.ListType{ElemType: types.StringType},
	}
}

func machineAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"hostname": types.StringType,
		"network":  types.ObjectType{AttrTypes: networkAttrTypes()},
		"disk":     types.ObjectType{AttrTypes: diskAttrTypes()},
		"bmc":      types.ObjectType{AttrTypes: bmcAttrTypes()},
		"labels":   types.MapType{ElemType: types.StringType},
	}
}

func networkAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"ip_address":  types.StringType,
		"netmask":     types.StringType,
		"gateway":     types.StringType,
		"mac_address": types.StringType,
		"nameservers": types.ListType{ElemType: types.StringType},
		"vlan_id":     types.StringType,
	}
}

func diskAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"device": types.StringType,
	}
}

func bmcAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"address":  types.StringType,
		"username": types.StringType,
		"password": types.StringType,
	}
}

func waitAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"enabled": types.BoolType,
		"timeout": types.StringType,
	}
}

func outputAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"kubeconfig_path": types.StringType,
	}
}

func statusAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"endpoint":          types.StringType,
		"kubeconfig":        types.StringType,
		"ca_certificate":    types.StringType,
		"description":       types.StringType,
		"bootstrap_cluster": types.StringType,
	}
}

// --- Extraction Helpers ---
// Each extraction helper safely reads a nested object from the model.

func extractManagement(ctx context.Context, data *ClusterResourceModel) (*ManagementModel, diag.Diagnostics) {
	if data.Management.IsNull() || data.Management.IsUnknown() {
		return nil, nil
	}
	var mgmt ManagementModel
	diags := data.Management.As(ctx, &mgmt, basetypes.ObjectAsOptions{})
	return &mgmt, diags
}

func extractManagementBootstrap(ctx context.Context, mgmt *ManagementModel) (*ManagementBootstrapModel, diag.Diagnostics) {
	if mgmt == nil || mgmt.Bootstrap.IsNull() || mgmt.Bootstrap.IsUnknown() {
		return nil, nil
	}
	var bs ManagementBootstrapModel
	diags := mgmt.Bootstrap.As(ctx, &bs, basetypes.ObjectAsOptions{})
	return &bs, diags
}

func extractBoot(ctx context.Context, bs *ManagementBootstrapModel) (*BootModel, diag.Diagnostics) {
	if bs == nil || bs.Boot.IsNull() || bs.Boot.IsUnknown() {
		return nil, nil
	}
	var b BootModel
	diags := bs.Boot.As(ctx, &b, basetypes.ObjectAsOptions{})
	return &b, diags
}

func extractKairos(ctx context.Context, bs *ManagementBootstrapModel) (*KairosModel, diag.Diagnostics) {
	if bs == nil || bs.Kairos.IsNull() || bs.Kairos.IsUnknown() {
		return nil, nil
	}
	var k KairosModel
	diags := bs.Kairos.As(ctx, &k, basetypes.ObjectAsOptions{})
	return &k, diags
}

func extractKairosSSH(ctx context.Context, k *KairosModel) (*KairosSSHModel, diag.Diagnostics) {
	if k == nil || k.SSH.IsNull() || k.SSH.IsUnknown() {
		return nil, nil
	}
	var s KairosSSHModel
	diags := k.SSH.As(ctx, &s, basetypes.ObjectAsOptions{})
	return &s, diags
}

func extractBootstrapAddons(ctx context.Context, bs *ManagementBootstrapModel) (*BootstrapAddonsModel, diag.Diagnostics) {
	if bs == nil || bs.Addons.IsNull() || bs.Addons.IsUnknown() {
		return nil, nil
	}
	var ad BootstrapAddonsModel
	diags := bs.Addons.As(ctx, &ad, basetypes.ObjectAsOptions{})
	return &ad, diags
}

// defaultHelmTimeout bounds a Helm install when the release sets no timeout.
const defaultHelmTimeout = 10 * time.Minute

// addonSetFromModel converts an addons block to the backend set.
func addonSetFromModel(ctx context.Context, ad *BootstrapAddonsModel) (capi.AddonSet, diag.Diagnostics) {
	var set capi.AddonSet
	var diags diag.Diagnostics
	if ad == nil {
		return set, diags
	}
	var d diag.Diagnostics
	set.Prerequisites, d = stringList(ctx, ad.Prerequisites)
	diags.Append(d...)
	var rels []HelmReleaseModel
	if !ad.Helm.IsNull() && !ad.Helm.IsUnknown() {
		diags.Append(ad.Helm.ElementsAs(ctx, &rels, false)...)
	}
	for _, rel := range rels {
		timeout := defaultHelmTimeout
		if s := rel.Timeout.ValueString(); s != "" {
			if dur, err := time.ParseDuration(s); err == nil {
				timeout = dur
			}
		}
		set.Helm = append(set.Helm, capi.HelmRelease{
			Name:       rel.Name.ValueString(),
			Namespace:  rel.Namespace.ValueString(),
			Chart:      rel.Chart.ValueString(),
			Repository: rel.Repository.ValueString(),
			Version:    rel.Version.ValueString(),
			Values:     rel.Values.ValueString(),
			Timeout:    timeout,
		})
	}
	set.Manifests, d = stringList(ctx, ad.Manifests)
	diags.Append(d...)
	return set, diags
}

// findInventoryMachine returns the inventory machine with the given hostname, or nil.
func findInventoryMachine(ctx context.Context, data *ClusterResourceModel, hostname string) (*MachineModel, diag.Diagnostics) {
	inv, diags := extractInventory(ctx, data)
	if inv == nil || inv.Machine.IsNull() || inv.Machine.IsUnknown() {
		return nil, diags
	}
	var machines []MachineModel
	diags.Append(inv.Machine.ElementsAs(ctx, &machines, false)...)
	if diags.HasError() {
		return nil, diags
	}
	for i := range machines {
		if machines[i].Hostname.ValueString() == hostname {
			return &machines[i], diags
		}
	}
	return nil, diags
}

// stringList converts a list of strings; null or unknown yields nil.
func stringList(ctx context.Context, l types.List) ([]string, diag.Diagnostics) {
	if l.IsNull() || l.IsUnknown() {
		return nil, nil
	}
	var out []string
	diags := l.ElementsAs(ctx, &out, false)
	return out, diags
}

// extractProviders decodes a provider map. It returns nil for a null or
// unknown map so callers can treat "not configured" uniformly.
func extractProviders(ctx context.Context, m types.Map) (map[string]ProviderModel, diag.Diagnostics) {
	if m.IsNull() || m.IsUnknown() {
		return nil, nil
	}
	out := map[string]ProviderModel{}
	diags := m.ElementsAs(ctx, &out, false)
	return out, diags
}

// sortedProviderNames returns map keys in a stable order.
func sortedProviderNames(m map[string]ProviderModel) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// typedProviderMap pairs a provider map attribute with its type.
type typedProviderMap struct {
	Type capi.ProviderType
	Map  types.Map
}

// providerMaps lists the six provider map attributes in clusterctl install order.
func (data *ClusterResourceModel) providerMaps() []typedProviderMap {
	return []typedProviderMap{
		{capi.ProviderTypeCore, data.Core},
		{capi.ProviderTypeBootstrap, data.Bootstrap},
		{capi.ProviderTypeControlPlane, data.ControlPlane},
		{capi.ProviderTypeInfrastructure, data.Infrastructure},
		{capi.ProviderTypeIPAM, data.IPAM},
		{capi.ProviderTypeAddon, data.Addon},
	}
}

// extractProviderSet flattens all six maps into a capi.ProviderSet.
func extractProviderSet(ctx context.Context, data *ClusterResourceModel) (capi.ProviderSet, diag.Diagnostics) {
	var diags diag.Diagnostics
	set := capi.ProviderSet{}
	for _, pm := range data.providerMaps() {
		entries, d := extractProviders(ctx, pm.Map)
		diags.Append(d...)
		for _, name := range sortedProviderNames(entries) {
			cfg, d := buildProviderConfig(ctx, name, pm.Type, entries[name])
			diags.Append(d...)
			set.Add(cfg)
		}
	}
	return set, diags
}

// extractTopology returns the control plane topology and the machine deployments.
func extractTopology(ctx context.Context, data *ClusterResourceModel) (cp capi.ControlPlaneTopology, mds []capi.MachineDeploymentTopology, diags diag.Diagnostics) {
	if data.Topology.IsNull() || data.Topology.IsUnknown() {
		return cp, nil, nil
	}
	var topo TopologyModel
	diags.Append(data.Topology.As(ctx, &topo, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return cp, nil, diags
	}
	if !topo.ControlPlane.IsNull() && !topo.ControlPlane.IsUnknown() {
		var c ControlPlaneTopologyModel
		diags.Append(topo.ControlPlane.As(ctx, &c, basetypes.ObjectAsOptions{})...)
		if !c.Replicas.IsNull() && !c.Replicas.IsUnknown() {
			v := c.Replicas.ValueInt64()
			cp.Replicas = &v
		}
		infra, d := yamlObject(c.Infrastructure, "topology.control_plane.infrastructure")
		diags.Append(d...)
		cp.Infrastructure = infra
	}
	if !topo.Workers.IsNull() && !topo.Workers.IsUnknown() {
		var w WorkersTopologyModel
		diags.Append(topo.Workers.As(ctx, &w, basetypes.ObjectAsOptions{})...)
		if !w.MachineDeployments.IsNull() && !w.MachineDeployments.IsUnknown() {
			var models []MachineDeploymentModel
			diags.Append(w.MachineDeployments.ElementsAs(ctx, &models, false)...)
			for i, m := range models {
				md := capi.MachineDeploymentTopology{Name: m.Name.ValueString()}
				var d diag.Diagnostics
				md.Infrastructure, d = yamlObject(m.Infrastructure, fmt.Sprintf("topology.workers.machine_deployments[%d].infrastructure", i))
				diags.Append(d...)
				md.Bootstrap, d = yamlObject(m.Bootstrap, fmt.Sprintf("topology.workers.machine_deployments[%d].bootstrap", i))
				diags.Append(d...)
				if !m.Class.IsNull() {
					md.Class = m.Class.ValueString()
				}
				if !m.Replicas.IsNull() && !m.Replicas.IsUnknown() {
					v := m.Replicas.ValueInt64()
					md.Replicas = &v
				}
				if !m.FailureDomain.IsNull() {
					md.FailureDomain = m.FailureDomain.ValueString()
				}
				if !m.Metadata.IsNull() && !m.Metadata.IsUnknown() {
					var meta TopologyMetadataModel
					diags.Append(m.Metadata.As(ctx, &meta, basetypes.ObjectAsOptions{})...)
					if !meta.Labels.IsNull() {
						md.Labels = map[string]string{}
						diags.Append(meta.Labels.ElementsAs(ctx, &md.Labels, false)...)
					}
					if !meta.Annotations.IsNull() {
						md.Annotations = map[string]string{}
						diags.Append(meta.Annotations.ElementsAs(ctx, &md.Annotations, false)...)
					}
				}
				mds = append(mds, md)
			}
		}
	}
	return cp, mds, diags
}

// extractNetwork returns the Cluster.spec network settings (zero when unset).
func extractNetwork(ctx context.Context, data *ClusterResourceModel) (capi.NetworkConfig, diag.Diagnostics) {
	var out capi.NetworkConfig
	var diags diag.Diagnostics
	if data.Network.IsNull() || data.Network.IsUnknown() {
		return out, diags
	}
	var nm ClusterNetworkModel
	diags.Append(data.Network.As(ctx, &nm, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return out, diags
	}
	if !nm.ControlPlaneEndpoint.IsNull() && !nm.ControlPlaneEndpoint.IsUnknown() {
		var ep EndpointModel
		diags.Append(nm.ControlPlaneEndpoint.As(ctx, &ep, basetypes.ObjectAsOptions{})...)
		out.ControlPlaneEndpoint = &capi.APIEndpoint{Host: ep.Host.ValueString(), Port: int32(ep.Port.ValueInt64())}
	}
	ranges := func(obj types.Object) []string {
		if obj.IsNull() || obj.IsUnknown() {
			return nil
		}
		var r NetworkRangesModel
		diags.Append(obj.As(ctx, &r, basetypes.ObjectAsOptions{})...)
		cidrs, d := stringList(ctx, r.CIDRBlocks)
		diags.Append(d...)
		return cidrs
	}
	out.PodCIDRs = ranges(nm.Pods)
	out.ServiceCIDRs = ranges(nm.Services)
	if !nm.ServiceDomain.IsNull() && !nm.ServiceDomain.IsUnknown() {
		out.ServiceDomain = nm.ServiceDomain.ValueString()
	}
	return out, diags
}

// yamlObject decodes a YAML or JSON object attribute (e.g. yamlencode({...}))
// into plain Go values for the backend: integral numbers become int64, other
// numbers float64, sequences []any, mappings map[string]any, null nil. Null,
// unknown and blank yield nil; anything but an object is an error naming the
// attribute.
func yamlObject(s types.String, where string) (map[string]any, diag.Diagnostics) {
	var diags diag.Diagnostics
	if s.IsNull() || s.IsUnknown() || strings.TrimSpace(s.ValueString()) == "" {
		return nil, diags
	}
	jsonBytes, err := yaml.YAMLToJSON([]byte(s.ValueString()))
	if err != nil {
		diags.AddError("Invalid "+where, fmt.Sprintf("%s is not valid YAML or JSON: %s.", where, err))
		return nil, diags
	}
	dec := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		diags.AddError("Invalid "+where, fmt.Sprintf("%s is not valid YAML or JSON: %s.", where, err))
		return nil, diags
	}
	m, ok := normalizeJSON(v).(map[string]any)
	if !ok {
		diags.AddError("Invalid "+where, fmt.Sprintf("%s must be a YAML/JSON object with the CRD's camelCase keys, e.g. yamlencode({ ... }).", where))
		return nil, diags
	}
	return m, diags
}

// normalizeJSON turns json.Number into int64 (integral) or float64, recursively.
func normalizeJSON(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		for k, x := range t {
			t[k] = normalizeJSON(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normalizeJSON(x)
		}
		return t
	}
	return v
}

// legacyTopologyObject builds a topology from the pre-v2 two counts: the
// worker count becomes one MachineDeployment named "md-0". Used by upgraders.
func legacyTopologyObject(ctx context.Context, cpCount, workerCount types.Int64) (types.Object, diag.Diagnostics) {
	var diags diag.Diagnostics
	cpObj := types.ObjectNull(controlPlaneTopologyAttrTypes())
	if !cpCount.IsNull() && !cpCount.IsUnknown() {
		var d diag.Diagnostics
		cpObj, d = types.ObjectValueFrom(ctx, controlPlaneTopologyAttrTypes(), ControlPlaneTopologyModel{Replicas: cpCount, Infrastructure: types.StringNull()})
		diags.Append(d...)
	}
	wObj := types.ObjectNull(workersTopologyAttrTypes())
	if !workerCount.IsNull() && !workerCount.IsUnknown() {
		md := MachineDeploymentModel{
			Name: types.StringValue("md-0"), Class: types.StringNull(), Replicas: workerCount,
			FailureDomain: types.StringNull(), Metadata: types.ObjectNull(topologyMetadataAttrTypes()),
			Infrastructure: types.StringNull(), Bootstrap: types.StringNull(),
		}
		list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: machineDeploymentAttrTypes()}, []MachineDeploymentModel{md})
		diags.Append(d...)
		wObj, d = types.ObjectValueFrom(ctx, workersTopologyAttrTypes(), WorkersTopologyModel{MachineDeployments: list})
		diags.Append(d...)
	}
	if cpObj.IsNull() && wObj.IsNull() {
		return types.ObjectNull(topologyAttrTypes()), diags
	}
	obj, d := types.ObjectValueFrom(ctx, topologyAttrTypes(), TopologyModel{ControlPlane: cpObj, Workers: wObj})
	diags.Append(d...)
	return obj, diags
}

// splitNameVersion splits "name:version" (version optional).
func splitNameVersion(s string) (string, string) {
	parts := strings.SplitN(s, ":", 2)
	name := strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		return name, strings.TrimSpace(parts[1])
	}
	return name, ""
}

// nullProviderModel is a ProviderModel with every attribute null.
func nullProviderModel() ProviderModel {
	return ProviderModel{
		Version:               types.StringNull(),
		FetchConfig:           types.ObjectNull(fetchConfigAttrTypes()),
		ConfigVariables:       types.MapNull(types.StringType),
		SecretConfigVariables: types.MapNull(types.StringType),
		Deployment:            types.ObjectNull(deploymentAttrTypes()),
		Manager:               types.ObjectNull(managerAttrTypes()),
		AdditionalManifests:   types.StringNull(),
		ManifestPatches:       types.ListNull(types.StringType),
		Patches:               types.ListNull(types.ObjectType{AttrTypes: patchAttrTypes()}),
		ResourceConfig:        types.StringNull(),
	}
}

// providerMapValue builds a one-entry provider map from a legacy
// "name:version" string. Used by the state upgraders.
func providerMapValue(ctx context.Context, nameVersion string) (types.Map, diag.Diagnostics) {
	name, version := splitNameVersion(nameVersion)
	pm := nullProviderModel()
	if version != "" {
		pm.Version = types.StringValue(version)
	}
	return types.MapValueFrom(ctx, providerMapType().ElemType, map[string]ProviderModel{name: pm})
}

func extractInventory(ctx context.Context, data *ClusterResourceModel) (*InventoryModel, diag.Diagnostics) {
	if data.Inventory.IsNull() || data.Inventory.IsUnknown() {
		return nil, nil
	}
	var inv InventoryModel
	diags := data.Inventory.As(ctx, &inv, basetypes.ObjectAsOptions{})
	return &inv, diags
}

func extractWait(ctx context.Context, data *ClusterResourceModel) (*WaitModel, diag.Diagnostics) {
	if data.Wait.IsNull() || data.Wait.IsUnknown() {
		return nil, nil
	}
	var w WaitModel
	diags := data.Wait.As(ctx, &w, basetypes.ObjectAsOptions{})
	return &w, diags
}

func extractOutput(ctx context.Context, data *ClusterResourceModel) (*OutputModel, diag.Diagnostics) {
	if data.Output.IsNull() || data.Output.IsUnknown() {
		return nil, nil
	}
	var out OutputModel
	diags := data.Output.As(ctx, &out, basetypes.ObjectAsOptions{})
	return &out, diags
}

func extractStatus(ctx context.Context, data *ClusterResourceModel) (*StatusModel, diag.Diagnostics) {
	if data.Status.IsNull() || data.Status.IsUnknown() {
		return nil, nil
	}
	var st StatusModel
	diags := data.Status.As(ctx, &st, basetypes.ObjectAsOptions{})
	return &st, diags
}

// --- State Population Helpers ---

func stringOrNull(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

func setStatus(ctx context.Context, data *ClusterResourceModel, result *capi.ClusterResult) diag.Diagnostics {
	bootstrapName := ""
	if result.BootstrapCluster != nil {
		bootstrapName = result.BootstrapCluster.Name
	}

	status := StatusModel{
		Endpoint:         stringOrNull(result.Endpoint),
		Kubeconfig:       stringOrNull(result.Kubeconfig),
		CACertificate:    stringOrNull(result.CACertificate),
		Description:      stringOrNull(result.ClusterDescription),
		BootstrapCluster: stringOrNull(bootstrapName),
	}

	val, diags := types.ObjectValueFrom(ctx, statusAttrTypes(), status)
	if diags.HasError() {
		return diags
	}
	data.Status = val
	return nil
}

func setStatusWithFallback(ctx context.Context, data *ClusterResourceModel, result *capi.ClusterResult, prevStatus *StatusModel) diag.Diagnostics {
	bootstrapName := ""
	if result.BootstrapCluster != nil {
		bootstrapName = result.BootstrapCluster.Name
	} else if prevStatus != nil && !prevStatus.BootstrapCluster.IsNull() {
		bootstrapName = prevStatus.BootstrapCluster.ValueString()
	}

	endpoint := result.Endpoint
	if endpoint == "" && prevStatus != nil && !prevStatus.Endpoint.IsNull() {
		endpoint = prevStatus.Endpoint.ValueString()
	}

	kubeconfig := result.Kubeconfig
	if kubeconfig == "" && prevStatus != nil && !prevStatus.Kubeconfig.IsNull() {
		kubeconfig = prevStatus.Kubeconfig.ValueString()
	}

	caCert := result.CACertificate
	if caCert == "" && prevStatus != nil && !prevStatus.CACertificate.IsNull() {
		caCert = prevStatus.CACertificate.ValueString()
	}

	desc := result.ClusterDescription
	if desc == "" && prevStatus != nil && !prevStatus.Description.IsNull() {
		desc = prevStatus.Description.ValueString()
	}

	status := StatusModel{
		Endpoint:         stringOrNull(endpoint),
		Kubeconfig:       stringOrNull(kubeconfig),
		CACertificate:    stringOrNull(caCert),
		Description:      stringOrNull(desc),
		BootstrapCluster: stringOrNull(bootstrapName),
	}

	val, diags := types.ObjectValueFrom(ctx, statusAttrTypes(), status)
	if diags.HasError() {
		return diags
	}
	data.Status = val
	return nil
}

func nullStatus(ctx context.Context, data *ClusterResourceModel) diag.Diagnostics {
	status := StatusModel{
		Endpoint:         types.StringNull(),
		Kubeconfig:       types.StringNull(),
		CACertificate:    types.StringNull(),
		Description:      types.StringNull(),
		BootstrapCluster: types.StringNull(),
	}
	val, diags := types.ObjectValueFrom(ctx, statusAttrTypes(), status)
	if diags.HasError() {
		return diags
	}
	data.Status = val
	return nil
}

// --- Option Builders ---

func buildCreateOptions(ctx context.Context, data *ClusterResourceModel) (*capi.CreateClusterOptions, diag.Diagnostics) {
	var diags diag.Diagnostics
	opts := &capi.CreateClusterOptions{
		Name: data.Name.ValueString(),
		Wait: capi.DefaultWaitOptions(),
	}

	if !data.KubernetesVersion.IsNull() {
		opts.KubernetesVersion = data.KubernetesVersion.ValueString()
	}
	// Management
	mgmt, d := extractManagement(ctx, data)
	diags.Append(d...)
	if mgmt != nil {
		if !mgmt.Kubeconfig.IsNull() {
			opts.ManagementKubeconfig = mgmt.Kubeconfig.ValueString()
		}
		opts.SkipInit = mgmt.SkipInit.ValueBool()
		opts.SelfManaged = mgmt.SelfManaged.ValueBool()
		if !mgmt.Namespace.IsNull() {
			opts.Namespace = mgmt.Namespace.ValueString()
		}
	}

	// Providers
	providers, d := extractProviderSet(ctx, data)
	diags.Append(d...)
	opts.Providers = providers

	// Generation mode and Cluster.spec network settings
	opts.ClusterClass = data.ClusterClass.ValueBool()
	network, d := extractNetwork(ctx, data)
	diags.Append(d...)
	opts.Network = network

	// Topology
	cp, mds, d := extractTopology(ctx, data)
	diags.Append(d...)
	opts.ControlPlane = cp
	opts.MachineDeployments = mds

	// Wait
	wait, d := extractWait(ctx, data)
	diags.Append(d...)
	if wait != nil {
		opts.WaitForReady = wait.Enabled.ValueBool()
		if !wait.Timeout.IsNull() && wait.Timeout.ValueString() != "" {
			timeout, err := time.ParseDuration(wait.Timeout.ValueString())
			if err != nil {
				diags.AddError("Invalid wait timeout", fmt.Sprintf("Cannot parse timeout %q: %s", wait.Timeout.ValueString(), err))
			} else {
				opts.Wait.Timeout = timeout
			}
		}
	} else {
		opts.WaitForReady = true
	}

	// Output
	out, d := extractOutput(ctx, data)
	diags.Append(d...)
	if out != nil && !out.KubeconfigPath.IsNull() {
		opts.KubeconfigOutputPath = out.KubeconfigPath.ValueString()
	}

	// Inventory: the machines CAPT may claim, and which of them hosts the
	// bootstrap cluster.
	inv, d := extractInventory(ctx, data)
	diags.Append(d...)
	if inv != nil {
		opts.ManageInventory = inv.Manage.IsNull() || inv.Manage.IsUnknown() || inv.Manage.ValueBool()
		machines, d := extractInventoryMachines(ctx, inv)
		diags.Append(d...)
		opts.Inventory = machines
	}
	opts.BootstrapMachine = bootstrapMachineHostname(ctx, data)

	// Raw manifests for the management cluster.
	manifests, d := extractManifests(ctx, data)
	diags.Append(d...)
	opts.ManagementManifests = manifests

	// The bootstrap cluster's own stack, uninstalled again before the pivot.
	if mgmt != nil {
		bs, d := extractManagementBootstrap(ctx, mgmt)
		diags.Append(d...)
		ad, d := extractBootstrapAddons(ctx, bs)
		diags.Append(d...)
		opts.BootstrapAddons, d = addonSetFromModel(ctx, ad)
		diags.Append(d...)
	}

	return opts, diags
}

// bootstrapType is management.bootstrap.type ("kind" when the block sets
// none), or "" without a bootstrap block.
func bootstrapType(ctx context.Context, data *ClusterResourceModel) string {
	mgmt, _ := extractManagement(ctx, data)
	bs, _ := extractManagementBootstrap(ctx, mgmt)
	if bs == nil {
		return ""
	}
	if bs.Type.IsNull() || bs.Type.IsUnknown() || bs.Type.ValueString() == "" {
		return "kind"
	}
	return bs.Type.ValueString()
}

// bootstrapMachineHostname is management.bootstrap.machine when the bootstrap
// cluster runs on an inventory machine (type "kairos"), else "".
func bootstrapMachineHostname(ctx context.Context, data *ClusterResourceModel) string {
	mgmt, _ := extractManagement(ctx, data)
	bs, _ := extractManagementBootstrap(ctx, mgmt)
	if bs == nil || bs.Type.ValueString() != "kairos" {
		return ""
	}
	if bs.Machine.IsNull() || bs.Machine.IsUnknown() {
		return ""
	}
	return bs.Machine.ValueString()
}

// extractInventoryMachines converts inventory.machine entries to the backend type.
func extractInventoryMachines(ctx context.Context, inv *InventoryModel) ([]capi.InventoryMachine, diag.Diagnostics) {
	var diags diag.Diagnostics
	if inv == nil || inv.Machine.IsNull() || inv.Machine.IsUnknown() {
		return nil, diags
	}
	var models []MachineModel
	diags.Append(inv.Machine.ElementsAs(ctx, &models, false)...)
	if diags.HasError() {
		return nil, diags
	}
	out := make([]capi.InventoryMachine, 0, len(models))
	for _, m := range models {
		im := capi.InventoryMachine{Hostname: m.Hostname.ValueString()}
		if !m.Network.IsNull() && !m.Network.IsUnknown() {
			var n NetworkModel
			diags.Append(m.Network.As(ctx, &n, basetypes.ObjectAsOptions{})...)
			im.IP = n.IPAddress.ValueString()
			im.Netmask = n.Netmask.ValueString()
			im.Gateway = n.Gateway.ValueString()
			im.MAC = n.MACAddress.ValueString()
			im.VLANID = n.VLANID.ValueString()
			ns, d := stringList(ctx, n.Nameservers)
			diags.Append(d...)
			im.Nameservers = ns
		}
		if !m.Disk.IsNull() && !m.Disk.IsUnknown() {
			var d DiskModel
			diags.Append(m.Disk.As(ctx, &d, basetypes.ObjectAsOptions{})...)
			im.Disk = d.Device.ValueString()
		}
		if !m.BMC.IsNull() && !m.BMC.IsUnknown() {
			var b BMCModel
			diags.Append(m.BMC.As(ctx, &b, basetypes.ObjectAsOptions{})...)
			im.BMC = &capi.InventoryBMC{Address: b.Address.ValueString(), Username: b.Username.ValueString(), Password: b.Password.ValueString()}
		}
		if !m.Labels.IsNull() && !m.Labels.IsUnknown() {
			im.Labels = map[string]string{}
			diags.Append(m.Labels.ElementsAs(ctx, &im.Labels, false)...)
		}
		out = append(out, im)
	}
	return out, diags
}

// extractManifests returns manifests.management as raw YAML documents.
func extractManifests(ctx context.Context, data *ClusterResourceModel) ([][]byte, diag.Diagnostics) {
	var diags diag.Diagnostics
	if data.Manifests.IsNull() || data.Manifests.IsUnknown() {
		return nil, diags
	}
	var m ManifestsModel
	diags.Append(data.Manifests.As(ctx, &m, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nil, diags
	}
	docs, d := stringList(ctx, m.Management)
	diags.Append(d...)
	out := make([][]byte, 0, len(docs))
	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		out = append(out, []byte(doc))
	}
	return out, diags
}

// buildProviderConfig converts one provider map entry into a capi.ProviderConfig.
func buildProviderConfig(ctx context.Context, name string, typ capi.ProviderType, pm ProviderModel) (capi.ProviderConfig, diag.Diagnostics) {
	var diags diag.Diagnostics
	ac := capi.ProviderConfig{Name: name, Type: typ}
	if !pm.Version.IsNull() && !pm.Version.IsUnknown() {
		ac.Version = pm.Version.ValueString()
	}

	// ResourceConfig
	rc, d := yamlObject(pm.ResourceConfig, fmt.Sprintf("%s[%q].resource_config", providerAttrName(typ), name))
	diags.Append(d...)
	ac.ResourceConfig = rc

	// ConfigVariables
	if !pm.ConfigVariables.IsNull() && !pm.ConfigVariables.IsUnknown() {
		vars := map[string]string{}
		diags.Append(pm.ConfigVariables.ElementsAs(ctx, &vars, false)...)
		ac.ConfigVariables = vars
	}

	// SecretConfigVariables
	if !pm.SecretConfigVariables.IsNull() && !pm.SecretConfigVariables.IsUnknown() {
		vars := map[string]string{}
		diags.Append(pm.SecretConfigVariables.ElementsAs(ctx, &vars, false)...)
		ac.SecretConfigVariables = vars
	}

	// FetchConfig
	if !pm.FetchConfig.IsNull() && !pm.FetchConfig.IsUnknown() {
		var fc FetchConfigModel
		diags.Append(pm.FetchConfig.As(ctx, &fc, basetypes.ObjectAsOptions{})...)
		ac.FetchConfig = &capi.FetchConfig{
			Owner:      fc.Owner.ValueString(),
			Repository: fc.Repository.ValueString(),
			URL:        fc.URL.ValueString(),
			OCI:        fc.OCI.ValueString(),
		}
	}

	// Deployment
	if !pm.Deployment.IsNull() && !pm.Deployment.IsUnknown() {
		var dep DeploymentModel
		diags.Append(pm.Deployment.As(ctx, &dep, basetypes.ObjectAsOptions{})...)
		ac.Deployment = &capi.DeploymentConfig{}
		if !dep.Replicas.IsNull() {
			r := dep.Replicas.ValueInt64()
			ac.Deployment.Replicas = &r
		}
		if !dep.NodeSelector.IsNull() {
			ns := map[string]string{}
			diags.Append(dep.NodeSelector.ElementsAs(ctx, &ns, false)...)
			ac.Deployment.NodeSelector = ns
		}
		if !dep.ServiceAccountName.IsNull() {
			ac.Deployment.ServiceAccountName = dep.ServiceAccountName.ValueString()
		}
		if !dep.Containers.IsNull() && !dep.Containers.IsUnknown() {
			var containers []ContainerModel
			diags.Append(dep.Containers.ElementsAs(ctx, &containers, false)...)
			for _, c := range containers {
				cc := capi.ContainerConfig{Name: c.Name.ValueString()}
				if !c.ImageURL.IsNull() {
					cc.ImageURL = c.ImageURL.ValueString()
				}
				if !c.Args.IsNull() {
					args := map[string]string{}
					diags.Append(c.Args.ElementsAs(ctx, &args, false)...)
					cc.Args = args
				}
				if !c.Command.IsNull() {
					var cmd []string
					diags.Append(c.Command.ElementsAs(ctx, &cmd, false)...)
					cc.Command = cmd
				}
				ac.Deployment.Containers = append(ac.Deployment.Containers, cc)
			}
		}
	}

	// Manager
	if !pm.Manager.IsNull() && !pm.Manager.IsUnknown() {
		var mgr ManagerModel
		diags.Append(pm.Manager.As(ctx, &mgr, basetypes.ObjectAsOptions{})...)
		ac.Manager = &capi.ManagerConfig{}
		if !mgr.ProfilerAddress.IsNull() {
			ac.Manager.ProfilerAddress = mgr.ProfilerAddress.ValueString()
		}
		if !mgr.MaxConcurrentReconciles.IsNull() {
			v := mgr.MaxConcurrentReconciles.ValueInt64()
			ac.Manager.MaxConcurrentReconciles = &v
		}
		if !mgr.Verbosity.IsNull() {
			v := mgr.Verbosity.ValueInt64()
			ac.Manager.Verbosity = &v
		}
		if !mgr.FeatureGates.IsNull() {
			gates := map[string]bool{}
			diags.Append(mgr.FeatureGates.ElementsAs(ctx, &gates, false)...)
			ac.Manager.FeatureGates = gates
		}
		if !mgr.AdditionalArgs.IsNull() {
			args := map[string]string{}
			diags.Append(mgr.AdditionalArgs.ElementsAs(ctx, &args, false)...)
			ac.Manager.AdditionalArgs = args
		}
	}

	// AdditionalManifests
	if !pm.AdditionalManifests.IsNull() && !pm.AdditionalManifests.IsUnknown() {
		ac.AdditionalManifests = pm.AdditionalManifests.ValueString()
	}

	// ManifestPatches
	if !pm.ManifestPatches.IsNull() && !pm.ManifestPatches.IsUnknown() {
		var patches []string
		diags.Append(pm.ManifestPatches.ElementsAs(ctx, &patches, false)...)
		ac.ManifestPatches = patches
	}

	// Patches
	if !pm.Patches.IsNull() && !pm.Patches.IsUnknown() {
		var patchModels []PatchModel
		diags.Append(pm.Patches.ElementsAs(ctx, &patchModels, false)...)
		for _, pm := range patchModels {
			pc := capi.PatchConfig{}
			if !pm.Patch.IsNull() {
				pc.Patch = pm.Patch.ValueString()
			}
			if !pm.Target.IsNull() && !pm.Target.IsUnknown() {
				var sel PatchSelectorModel
				diags.Append(pm.Target.As(ctx, &sel, basetypes.ObjectAsOptions{})...)
				pc.Target = &capi.PatchSelector{}
				if !sel.Group.IsNull() {
					pc.Target.Group = sel.Group.ValueString()
				}
				if !sel.Version.IsNull() {
					pc.Target.Version = sel.Version.ValueString()
				}
				if !sel.Kind.IsNull() {
					pc.Target.Kind = sel.Kind.ValueString()
				}
				if !sel.Name.IsNull() {
					pc.Target.Name = sel.Name.ValueString()
				}
				if !sel.Namespace.IsNull() {
					pc.Target.Namespace = sel.Namespace.ValueString()
				}
				if !sel.LabelSelector.IsNull() {
					pc.Target.LabelSelector = sel.LabelSelector.ValueString()
				}
			}
			ac.Patches = append(ac.Patches, pc)
		}
	}

	return ac, diags
}

// --- Inventory Validation ---

func validateInventory(ctx context.Context, inv *InventoryModel, cpCount, workerCount int64, diags *diag.Diagnostics) {
	if inv == nil {
		return
	}

	hasSource := !inv.Source.IsNull() && inv.Source.ValueString() != ""
	hasMachines := !inv.Machine.IsNull() && !inv.Machine.IsUnknown()
	if hasSource && hasMachines {
		diags.AddError("Invalid inventory", "Specify either source or machine, not both.")
		return
	}

	if !hasMachines {
		return
	}

	var machines []MachineModel
	diags.Append(inv.Machine.ElementsAs(ctx, &machines, false)...)
	if diags.HasError() {
		return
	}

	hostnames := map[string]bool{}
	ips := map[string]bool{}
	macs := map[string]bool{}

	for _, m := range machines {
		h := m.Hostname.ValueString()
		if hostnames[h] {
			diags.AddError("Duplicate hostname", fmt.Sprintf("hostname %q appears more than once", h))
		}
		hostnames[h] = true

		var net NetworkModel
		diags.Append(m.Network.As(ctx, &net, basetypes.ObjectAsOptions{})...)
		if diags.HasError() {
			return
		}

		ip := net.IPAddress.ValueString()
		if ips[ip] {
			diags.AddError("Duplicate IP", fmt.Sprintf("ip_address %q appears more than once", ip))
		}
		ips[ip] = true

		mac := net.MACAddress.ValueString()
		if macs[mac] {
			diags.AddError("Duplicate MAC", fmt.Sprintf("mac_address %q appears more than once", mac))
		}
		macs[mac] = true
	}

	// Role counting
	cpMachines := 0
	workerMachines := 0
	for _, m := range machines {
		labels := map[string]string{}
		if !m.Labels.IsNull() {
			diags.Append(m.Labels.ElementsAs(ctx, &labels, false)...)
		}
		switch labels["type"] {
		case "cp":
			cpMachines++
		default:
			workerMachines++
		}
	}
	if int64(cpMachines) < cpCount {
		diags.AddError("Insufficient hardware",
			fmt.Sprintf("Need %d control plane machines (type=cp label), have %d", cpCount, cpMachines))
	}
	if int64(workerMachines) < workerCount {
		diags.AddError("Insufficient hardware",
			fmt.Sprintf("Need %d worker machines, have %d", workerCount, workerMachines))
	}
}

// --- Provider Validation ---

// validateProviderEntry checks the static rules of one provider map entry.
func validateProviderEntry(ctx context.Context, typ capi.ProviderType, name string, pm ProviderModel, diags *diag.Diagnostics) {
	where := fmt.Sprintf("%s[%q]", providerAttrName(typ), name)

	if !pm.FetchConfig.IsNull() && !pm.FetchConfig.IsUnknown() {
		var fc FetchConfigModel
		diags.Append(pm.FetchConfig.As(ctx, &fc, basetypes.ObjectAsOptions{})...)
		set := func(s types.String) bool { return !s.IsNull() && !s.IsUnknown() && s.ValueString() != "" }
		hasURL, hasOCI := set(fc.URL), set(fc.OCI)
		hasRepoParts := set(fc.Owner) || set(fc.Repository)
		switch {
		case hasURL && hasOCI:
			diags.AddError("Invalid fetch_config", where+".fetch_config: url and oci are mutually exclusive.")
		case (hasURL || hasOCI) && hasRepoParts:
			diags.AddError("Invalid fetch_config", where+".fetch_config: owner/repository cannot be combined with url or oci.")
		}
	}

	hasManifestPatches := !pm.ManifestPatches.IsNull() && !pm.ManifestPatches.IsUnknown() && len(pm.ManifestPatches.Elements()) > 0
	hasPatches := !pm.Patches.IsNull() && !pm.Patches.IsUnknown() && len(pm.Patches.Elements()) > 0
	if hasManifestPatches && hasPatches {
		diags.AddError("Invalid provider patches", where+": manifest_patches and patches cannot be used together.")
	}

	if !pm.ResourceConfig.IsNull() && !pm.ResourceConfig.IsUnknown() {
		switch typ {
		case capi.ProviderTypeInfrastructure, capi.ProviderTypeControlPlane, capi.ProviderTypeBootstrap:
			_, d := yamlObject(pm.ResourceConfig, where+".resource_config")
			diags.Append(d...)
		default:
			diags.AddError("Invalid resource_config", where+".resource_config: only infrastructure, control_plane and bootstrap providers own a cluster object; remove it here.")
		}
	}
}

// providerAttrName maps a provider type to its schema attribute name.
func providerAttrName(typ capi.ProviderType) string {
	if typ == capi.ProviderTypeControlPlane {
		return "control_plane"
	}
	return string(typ)
}
