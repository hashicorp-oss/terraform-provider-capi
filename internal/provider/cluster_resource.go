// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// Ensure provider defined types fully satisfy framework interfaces.
var _ resource.Resource = &ClusterResource{}
var _ resource.ResourceWithImportState = &ClusterResource{}

func NewClusterResource() resource.Resource {
	return &ClusterResource{}
}

// ClusterResource defines the resource implementation.
type ClusterResource struct {
	providerData *CapiProviderModel
	manager      *capi.Manager
}

// ClusterResourceModel describes the resource data model.
type ClusterResourceModel struct {
	Name                     types.String `tfsdk:"name"`
	KubeconfigPath           types.String `tfsdk:"kubeconfig_path"`
	ManagementKubeconfig     types.String `tfsdk:"management_kubeconfig"`
	SkipInit                 types.Bool   `tfsdk:"skip_init"`
	WaitForReady             types.Bool   `tfsdk:"wait_for_ready"`
	SelfManaged              types.Bool   `tfsdk:"self_managed"`
	InfrastructureProvider   types.String `tfsdk:"infrastructure_provider"`
	BootstrapProvider        types.String `tfsdk:"bootstrap_provider"`
	ControlPlaneProvider     types.String `tfsdk:"control_plane_provider"`
	CoreProvider             types.String `tfsdk:"core_provider"`
	TargetNamespace          types.String `tfsdk:"target_namespace"`
	KubernetesVersion        types.String `tfsdk:"kubernetes_version"`
	ControlPlaneMachineCount types.Int64  `tfsdk:"control_plane_machine_count"`
	WorkerMachineCount       types.Int64  `tfsdk:"worker_machine_count"`
	Flavor                   types.String `tfsdk:"flavor"`
	Id                       types.String `tfsdk:"id"`
	Endpoint                 types.String `tfsdk:"endpoint"`
	ClusterCACertificate     types.String `tfsdk:"cluster_ca_certificate"`
	Kubeconfig               types.String `tfsdk:"kubeconfig"`
	ClusterDescription       types.String `tfsdk:"cluster_description"`
	BootstrapClusterName     types.String `tfsdk:"bootstrap_cluster_name"`
}

func (r *ClusterResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func (r *ClusterResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Cluster API cluster using the CAPI management workflow (bootstrap -> init -> apply -> wait -> move)",

		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				MarkdownDescription: "The name of the cluster",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"kubeconfig_path": schema.StringAttribute{
				MarkdownDescription: "Path where the kubeconfig for the workload cluster will be written",
				Optional:            true,
				Computed:            true,
			},
			"management_kubeconfig": schema.StringAttribute{
				MarkdownDescription: "Path to the kubeconfig for an existing management cluster. If not provided, a bootstrap cluster (kind) is created automatically",
				Optional:            true,
			},
			"skip_init": schema.BoolAttribute{
				MarkdownDescription: "Skip running clusterctl init on the management cluster (use if CAPI is already installed)",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"wait_for_ready": schema.BoolAttribute{
				MarkdownDescription: "Wait for the workload cluster to become ready before returning",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
			},
			"self_managed": schema.BoolAttribute{
				MarkdownDescription: "Make the workload cluster self-managed by pivoting CAPI management from the bootstrap cluster (mirrors EKS Anywhere's move management pattern)",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"infrastructure_provider": schema.StringAttribute{
				MarkdownDescription: "Infrastructure provider to use (e.g., 'docker', 'aws', 'azure')",
				Required:            true,
			},
			"bootstrap_provider": schema.StringAttribute{
				MarkdownDescription: "Bootstrap provider to use (e.g., 'kubeadm')",
				Optional:            true,
			},
			"control_plane_provider": schema.StringAttribute{
				MarkdownDescription: "Control plane provider to use (e.g., 'kubeadm')",
				Optional:            true,
			},
			"core_provider": schema.StringAttribute{
				MarkdownDescription: "Core provider version (e.g., 'cluster-api:v1.7.0')",
				Optional:            true,
			},
			"target_namespace": schema.StringAttribute{
				MarkdownDescription: "Target namespace for the cluster",
				Optional:            true,
				Computed:            true,
			},
			"kubernetes_version": schema.StringAttribute{
				MarkdownDescription: "Kubernetes version for the workload cluster",
				Optional:            true,
			},
			"control_plane_machine_count": schema.Int64Attribute{
				MarkdownDescription: "Number of control plane machines",
				Optional:            true,
			},
			"worker_machine_count": schema.Int64Attribute{
				MarkdownDescription: "Number of worker machines",
				Optional:            true,
			},
			"flavor": schema.StringAttribute{
				MarkdownDescription: "Cluster template flavor to use",
				Optional:            true,
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Cluster identifier",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"endpoint": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Cluster API server endpoint",
			},
			"cluster_ca_certificate": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Cluster CA certificate",
				Sensitive:           true,
			},
			"kubeconfig": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Kubeconfig for accessing the workload cluster",
				Sensitive:           true,
			},
			"cluster_description": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Cluster status description (from clusterctl describe)",
			},
			"bootstrap_cluster_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Name of the bootstrap cluster (if one was created)",
			},
		},
	}
}

func (r *ClusterResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	// Prevent panic if the provider has not been configured.
	if req.ProviderData == nil {
		return
	}

	providerData, ok := req.ProviderData.(*CapiProviderModel)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *CapiProviderModel, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)
		return
	}

	r.providerData = providerData
	r.manager = capi.NewManager(
		capi.WithLogger(log.New(os.Stderr, "[capi-tf] ", log.LstdFlags)),
	)
}

func (r *ClusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data ClusterResourceModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating CAPI cluster", map[string]interface{}{
		"name": data.Name.ValueString(),
	})

	// Build creation options from Terraform config
	createOpts := capi.CreateClusterOptions{
		Name:                   data.Name.ValueString(),
		InfrastructureProvider: data.InfrastructureProvider.ValueString(),
		SkipInit:               data.SkipInit.ValueBool(),
		WaitForReady:           data.WaitForReady.ValueBool(),
		SelfManaged:            data.SelfManaged.ValueBool(),
		Wait:                   capi.DefaultWaitOptions(),
	}

	if !data.ManagementKubeconfig.IsNull() && data.ManagementKubeconfig.ValueString() != "" {
		createOpts.ManagementKubeconfig = data.ManagementKubeconfig.ValueString()
	}

	if !data.BootstrapProvider.IsNull() {
		createOpts.BootstrapProvider = data.BootstrapProvider.ValueString()
	}

	if !data.ControlPlaneProvider.IsNull() {
		createOpts.ControlPlaneProvider = data.ControlPlaneProvider.ValueString()
	}

	if !data.CoreProvider.IsNull() {
		createOpts.CoreProvider = data.CoreProvider.ValueString()
	}

	if !data.TargetNamespace.IsNull() {
		createOpts.Namespace = data.TargetNamespace.ValueString()
	}

	if !data.KubernetesVersion.IsNull() {
		createOpts.KubernetesVersion = data.KubernetesVersion.ValueString()
	}

	if !data.ControlPlaneMachineCount.IsNull() {
		count := data.ControlPlaneMachineCount.ValueInt64()
		createOpts.ControlPlaneMachineCount = &count
	}

	if !data.WorkerMachineCount.IsNull() {
		count := data.WorkerMachineCount.ValueInt64()
		createOpts.WorkerMachineCount = &count
	}

	if !data.Flavor.IsNull() {
		createOpts.Flavor = data.Flavor.ValueString()
	}

	// Set kubeconfig output path
	if !data.KubeconfigPath.IsNull() && data.KubeconfigPath.ValueString() != "" {
		createOpts.KubeconfigOutputPath = data.KubeconfigPath.ValueString()
	} else if home, err := os.UserHomeDir(); err == nil {
		createOpts.KubeconfigOutputPath = filepath.Join(home, ".kube", fmt.Sprintf("%s.kubeconfig", data.Name.ValueString()))
	}

	// Execute the CAPI management workflow
	result, err := r.manager.CreateCluster(ctx, createOpts)
	if err != nil {
		resp.Diagnostics.AddError("Cluster Creation Error", fmt.Sprintf("Failed to create cluster: %s", err))
		return
	}

	// Populate state from result
	data.Id = types.StringValue(data.Name.ValueString())

	if data.KubeconfigPath.IsNull() || data.KubeconfigPath.ValueString() == "" {
		data.KubeconfigPath = types.StringValue(createOpts.KubeconfigOutputPath)
	}

	if data.TargetNamespace.IsNull() || data.TargetNamespace.ValueString() == "" {
		data.TargetNamespace = types.StringValue("default")
	}

	if result.Kubeconfig != "" {
		data.Kubeconfig = types.StringValue(result.Kubeconfig)
	} else {
		data.Kubeconfig = types.StringNull()
	}

	if result.ClusterDescription != "" {
		data.ClusterDescription = types.StringValue(result.ClusterDescription)
	} else {
		data.ClusterDescription = types.StringNull()
	}

	if result.Endpoint != "" {
		data.Endpoint = types.StringValue(result.Endpoint)
	} else {
		data.Endpoint = types.StringNull()
	}

	if result.CACertificate != "" {
		data.ClusterCACertificate = types.StringValue(result.CACertificate)
	} else {
		data.ClusterCACertificate = types.StringNull()
	}

	if result.BootstrapCluster != nil {
		data.BootstrapClusterName = types.StringValue(result.BootstrapCluster.Name)
	} else {
		data.BootstrapClusterName = types.StringNull()
	}

	tflog.Trace(ctx, "Created CAPI cluster resource")
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ClusterResourceModel

	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Reading CAPI cluster", map[string]interface{}{
		"name": data.Name.ValueString(),
	})

	// Determine management kubeconfig for reading cluster info
	mgmtKubeconfig := ""
	if !data.ManagementKubeconfig.IsNull() {
		mgmtKubeconfig = data.ManagementKubeconfig.ValueString()
	} else if !data.BootstrapClusterName.IsNull() {
		// If we have a bootstrap cluster, get its kubeconfig
		mgmtKubeconfig = filepath.Join(os.TempDir(), fmt.Sprintf("kind-%s-kubeconfig", data.BootstrapClusterName.ValueString()))
	}

	if mgmtKubeconfig == "" {
		// No management cluster info available - keep existing state
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	namespace := "default"
	if !data.TargetNamespace.IsNull() {
		namespace = data.TargetNamespace.ValueString()
	}

	result, err := r.manager.GetClusterInfo(ctx, mgmtKubeconfig, data.Name.ValueString(), namespace)
	if err != nil {
		tflog.Warn(ctx, "Unable to read cluster info", map[string]interface{}{
			"error": err.Error(),
		})
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	if result.Kubeconfig != "" {
		data.Kubeconfig = types.StringValue(result.Kubeconfig)
	}
	if result.ClusterDescription != "" {
		data.ClusterDescription = types.StringValue(result.ClusterDescription)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data ClusterResourceModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Updating CAPI cluster", map[string]interface{}{
		"name": data.Name.ValueString(),
	})

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data ClusterResourceModel

	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Deleting CAPI cluster", map[string]interface{}{
		"name": data.Name.ValueString(),
	})

	// Determine management kubeconfig
	mgmtKubeconfig := ""
	if !data.ManagementKubeconfig.IsNull() {
		mgmtKubeconfig = data.ManagementKubeconfig.ValueString()
	} else if !data.BootstrapClusterName.IsNull() {
		mgmtKubeconfig = filepath.Join(os.TempDir(), fmt.Sprintf("kind-%s-kubeconfig", data.BootstrapClusterName.ValueString()))
	}

	namespace := "default"
	if !data.TargetNamespace.IsNull() {
		namespace = data.TargetNamespace.ValueString()
	}

	deleteOpts := capi.DeleteClusterOptions{
		Name:                 data.Name.ValueString(),
		Namespace:            namespace,
		ManagementKubeconfig: mgmtKubeconfig,
	}

	// Clean up bootstrap cluster if we created one
	if !data.BootstrapClusterName.IsNull() {
		deleteOpts.DeleteBootstrap = true
		deleteOpts.BootstrapName = data.BootstrapClusterName.ValueString()
	}

	if mgmtKubeconfig != "" {
		if err := r.manager.DeleteCluster(ctx, deleteOpts); err != nil {
			resp.Diagnostics.AddWarning("Cluster Deletion Warning",
				fmt.Sprintf("Error deleting cluster (it may have already been removed): %s", err))
		}
	} else {
		tflog.Warn(ctx, "No management kubeconfig available for deletion - cluster resources may need manual cleanup")
	}
}

func (r *ClusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
