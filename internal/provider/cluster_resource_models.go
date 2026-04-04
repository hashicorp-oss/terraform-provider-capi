// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// ClusterResourceModel describes the resource data model using nested attributes.
// This is the v1 schema model (nested-first design per terraform.instructions.md).
type ClusterResourceModel struct {
	Name              types.String `tfsdk:"name"`
	KubernetesVersion types.String `tfsdk:"kubernetes_version"`
	Flavor            types.String `tfsdk:"flavor"`
	Id                types.String `tfsdk:"id"`

	Management     types.Object `tfsdk:"management"`
	Infrastructure types.Object `tfsdk:"infrastructure"`
	Bootstrap      types.Object `tfsdk:"bootstrap"`
	ControlPlane   types.Object `tfsdk:"control_plane"`
	Core           types.Object `tfsdk:"core"`
	Workers        types.Object `tfsdk:"workers"`
	Addons         types.List   `tfsdk:"addons"`
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
	Namespace   types.String `tfsdk:"namespace"`
}

// InfrastructureModel groups attributes for the infrastructure provider.
type InfrastructureModel struct {
	Provider types.String `tfsdk:"provider"`
}

// BootstrapModel groups attributes for the bootstrap provider.
type BootstrapModel struct {
	Provider types.String `tfsdk:"provider"`
}

// ControlPlaneModel groups attributes for control plane configuration.
type ControlPlaneModel struct {
	Provider     types.String `tfsdk:"provider"`
	MachineCount types.Int64  `tfsdk:"machine_count"`
}

// CoreModel groups attributes for the core CAPI provider.
type CoreModel struct {
	Provider types.String `tfsdk:"provider"`
}

// WorkersModel groups attributes for worker node configuration.
type WorkersModel struct {
	MachineCount types.Int64 `tfsdk:"machine_count"`
}

// AddonModel describes a single CAPI addon provider, modeled after the
// cluster-api-operator AddonProvider CRD (operator.cluster.x-k8s.io/v1alpha2).
type AddonModel struct {
	Provider            types.String `tfsdk:"provider"`
	ConfigSecret        types.Object `tfsdk:"config_secret"`
	FetchConfig         types.Object `tfsdk:"fetch_config"`
	Deployment          types.Object `tfsdk:"deployment"`
	Manager             types.Object `tfsdk:"manager"`
	AdditionalManifests types.Object `tfsdk:"additional_manifests"`
	ManifestPatches     types.List   `tfsdk:"manifest_patches"`
	Patches             types.List   `tfsdk:"patches"`
}

// AddonSecretRefModel maps to the cluster-api-operator SecretReference.
type AddonSecretRefModel struct {
	Name      types.String `tfsdk:"name"`
	Namespace types.String `tfsdk:"namespace"`
}

// AddonConfigmapRefModel maps to the cluster-api-operator ConfigmapReference.
type AddonConfigmapRefModel struct {
	Name      types.String `tfsdk:"name"`
	Namespace types.String `tfsdk:"namespace"`
}

// AddonFetchConfigModel maps to FetchConfiguration (oci, url, or selector).
type AddonFetchConfigModel struct {
	URL      types.String `tfsdk:"url"`
	OCI      types.String `tfsdk:"oci"`
	Selector types.Object `tfsdk:"selector"`
}

// AddonLabelSelectorModel maps to metav1.LabelSelector (matchLabels only).
type AddonLabelSelectorModel struct {
	MatchLabels types.Map `tfsdk:"match_labels"`
}

// AddonDeploymentModel maps to the cluster-api-operator DeploymentSpec.
type AddonDeploymentModel struct {
	Replicas           types.Int64  `tfsdk:"replicas"`
	NodeSelector       types.Map    `tfsdk:"node_selector"`
	ServiceAccountName types.String `tfsdk:"service_account_name"`
	Containers         types.List   `tfsdk:"containers"`
}

// AddonContainerModel maps to the cluster-api-operator ContainerSpec.
type AddonContainerModel struct {
	Name     types.String `tfsdk:"name"`
	ImageURL types.String `tfsdk:"image_url"`
	Args     types.Map    `tfsdk:"args"`
	Command  types.List   `tfsdk:"command"`
}

// AddonManagerModel maps to the cluster-api-operator ManagerSpec.
type AddonManagerModel struct {
	ProfilerAddress         types.String `tfsdk:"profiler_address"`
	MaxConcurrentReconciles types.Int64  `tfsdk:"max_concurrent_reconciles"`
	Verbosity               types.Int64  `tfsdk:"verbosity"`
	FeatureGates            types.Map    `tfsdk:"feature_gates"`
	AdditionalArgs          types.Map    `tfsdk:"additional_args"`
}

// AddonPatchModel maps to the cluster-api-operator Patch.
type AddonPatchModel struct {
	Patch  types.String `tfsdk:"patch"`
	Target types.Object `tfsdk:"target"`
}

// AddonPatchSelectorModel maps to the cluster-api-operator PatchSelector.
type AddonPatchSelectorModel struct {
	Group         types.String `tfsdk:"group"`
	Version       types.String `tfsdk:"version"`
	Kind          types.String `tfsdk:"kind"`
	Name          types.String `tfsdk:"name"`
	Namespace     types.String `tfsdk:"namespace"`
	LabelSelector types.String `tfsdk:"label_selector"`
}

// InventoryModel groups attributes for hardware inventory.
type InventoryModel struct {
	Source  types.String `tfsdk:"source"`
	Machine types.List   `tfsdk:"machine"`
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
		"namespace":    types.StringType,
	}
}

func infrastructureAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"provider": types.StringType,
	}
}

func bootstrapAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"provider": types.StringType,
	}
}

func controlPlaneAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"provider":      types.StringType,
		"machine_count": types.Int64Type,
	}
}

func coreAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"provider": types.StringType,
	}
}

func workersAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"machine_count": types.Int64Type,
	}
}

func addonAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"provider":             types.StringType,
		"config_secret":        types.ObjectType{AttrTypes: addonSecretRefAttrTypes()},
		"fetch_config":         types.ObjectType{AttrTypes: addonFetchConfigAttrTypes()},
		"deployment":           types.ObjectType{AttrTypes: addonDeploymentAttrTypes()},
		"manager":              types.ObjectType{AttrTypes: addonManagerAttrTypes()},
		"additional_manifests": types.ObjectType{AttrTypes: addonConfigmapRefAttrTypes()},
		"manifest_patches":     types.ListType{ElemType: types.StringType},
		"patches":              types.ListType{ElemType: types.ObjectType{AttrTypes: addonPatchAttrTypes()}},
	}
}

func addonSecretRefAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":      types.StringType,
		"namespace": types.StringType,
	}
}

func addonConfigmapRefAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":      types.StringType,
		"namespace": types.StringType,
	}
}

func addonFetchConfigAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"url":      types.StringType,
		"oci":      types.StringType,
		"selector": types.ObjectType{AttrTypes: addonLabelSelectorAttrTypes()},
	}
}

func addonLabelSelectorAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"match_labels": types.MapType{ElemType: types.StringType},
	}
}

func addonDeploymentAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"replicas":              types.Int64Type,
		"node_selector":         types.MapType{ElemType: types.StringType},
		"service_account_name":  types.StringType,
		"containers":            types.ListType{ElemType: types.ObjectType{AttrTypes: addonContainerAttrTypes()}},
	}
}

func addonContainerAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":      types.StringType,
		"image_url": types.StringType,
		"args":      types.MapType{ElemType: types.StringType},
		"command":   types.ListType{ElemType: types.StringType},
	}
}

func addonManagerAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"profiler_address":          types.StringType,
		"max_concurrent_reconciles": types.Int64Type,
		"verbosity":                 types.Int64Type,
		"feature_gates":             types.MapType{ElemType: types.BoolType},
		"additional_args":           types.MapType{ElemType: types.StringType},
	}
}

func addonPatchAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"patch":  types.StringType,
		"target": types.ObjectType{AttrTypes: addonPatchSelectorAttrTypes()},
	}
}

func addonPatchSelectorAttrTypes() map[string]attr.Type {
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
		"source":  types.StringType,
		"machine": types.ListType{ElemType: types.ObjectType{AttrTypes: machineAttrTypes()}},
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

func extractInfrastructure(ctx context.Context, data *ClusterResourceModel) (*InfrastructureModel, diag.Diagnostics) {
	if data.Infrastructure.IsNull() || data.Infrastructure.IsUnknown() {
		return nil, nil
	}
	var infra InfrastructureModel
	diags := data.Infrastructure.As(ctx, &infra, basetypes.ObjectAsOptions{})
	return &infra, diags
}

func extractBootstrap(ctx context.Context, data *ClusterResourceModel) (*BootstrapModel, diag.Diagnostics) {
	if data.Bootstrap.IsNull() || data.Bootstrap.IsUnknown() {
		return nil, nil
	}
	var bs BootstrapModel
	diags := data.Bootstrap.As(ctx, &bs, basetypes.ObjectAsOptions{})
	return &bs, diags
}

func extractControlPlane(ctx context.Context, data *ClusterResourceModel) (*ControlPlaneModel, diag.Diagnostics) {
	if data.ControlPlane.IsNull() || data.ControlPlane.IsUnknown() {
		return nil, nil
	}
	var cp ControlPlaneModel
	diags := data.ControlPlane.As(ctx, &cp, basetypes.ObjectAsOptions{})
	return &cp, diags
}

func extractCore(ctx context.Context, data *ClusterResourceModel) (*CoreModel, diag.Diagnostics) {
	if data.Core.IsNull() || data.Core.IsUnknown() {
		return nil, nil
	}
	var core CoreModel
	diags := data.Core.As(ctx, &core, basetypes.ObjectAsOptions{})
	return &core, diags
}

func extractWorkers(ctx context.Context, data *ClusterResourceModel) (*WorkersModel, diag.Diagnostics) {
	if data.Workers.IsNull() || data.Workers.IsUnknown() {
		return nil, nil
	}
	var w WorkersModel
	diags := data.Workers.As(ctx, &w, basetypes.ObjectAsOptions{})
	return &w, diags
}

func extractAddons(ctx context.Context, data *ClusterResourceModel) ([]AddonModel, diag.Diagnostics) {
	if data.Addons.IsNull() || data.Addons.IsUnknown() {
		return nil, nil
	}
	var addons []AddonModel
	diags := data.Addons.ElementsAs(ctx, &addons, false)
	return addons, diags
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
	if !data.Flavor.IsNull() {
		opts.Flavor = data.Flavor.ValueString()
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

	// Infrastructure (required)
	infra, d := extractInfrastructure(ctx, data)
	diags.Append(d...)
	if infra != nil {
		opts.InfrastructureProvider = infra.Provider.ValueString()
	}

	// Bootstrap
	bs, d := extractBootstrap(ctx, data)
	diags.Append(d...)
	if bs != nil {
		opts.BootstrapProvider = bs.Provider.ValueString()
	}

	// Control Plane
	cp, d := extractControlPlane(ctx, data)
	diags.Append(d...)
	if cp != nil {
		if !cp.Provider.IsNull() {
			opts.ControlPlaneProvider = cp.Provider.ValueString()
		}
		if !cp.MachineCount.IsNull() {
			count := cp.MachineCount.ValueInt64()
			opts.ControlPlaneMachineCount = &count
		}
	}

	// Core
	core, d := extractCore(ctx, data)
	diags.Append(d...)
	if core != nil {
		opts.CoreProvider = core.Provider.ValueString()
	}

	// Workers
	w, d := extractWorkers(ctx, data)
	diags.Append(d...)
	if w != nil {
		if !w.MachineCount.IsNull() {
			count := w.MachineCount.ValueInt64()
			opts.WorkerMachineCount = &count
		}
	}

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

	// Addons
	addons, d := extractAddons(ctx, data)
	diags.Append(d...)
	for _, addon := range addons {
		if !addon.Provider.IsNull() {
			opts.AddonProviders = append(opts.AddonProviders, addon.Provider.ValueString())
		}
	}

	return opts, diags
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
