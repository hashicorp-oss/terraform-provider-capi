// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
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

	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
)

// Ensure provider defined types fully satisfy framework interfaces.
var _ resource.Resource = &ClusterResource{}
var _ resource.ResourceWithImportState = &ClusterResource{}

func NewClusterResource() resource.Resource {
	return &ClusterResource{}
}

// ClusterResource defines the resource implementation.
type ClusterResource struct {
	// We'll use the clusterctl client for cluster operations
}

// ClusterResourceModel describes the resource data model.
type ClusterResourceModel struct {
	Name                     types.String `tfsdk:"name"`
	KubeconfigPath           types.String `tfsdk:"kubeconfig_path"`
	ManagementKubeconfig     types.String `tfsdk:"management_kubeconfig"`
	SkipInit                 types.Bool   `tfsdk:"skip_init"`
	WaitForReady             types.Bool   `tfsdk:"wait_for_ready"`
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
}

func (r *ClusterResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func (r *ClusterResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Cluster API cluster using clusterctl",

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
				MarkdownDescription: "Path to the kubeconfig for the management cluster. If not provided, uses default kubeconfig",
				Optional:            true,
			},
			"skip_init": schema.BoolAttribute{
				MarkdownDescription: "Skip running clusterctl init on the management cluster",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"wait_for_ready": schema.BoolAttribute{
				MarkdownDescription: "Wait for cluster to be ready before returning",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
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
				MarkdownDescription: "Core provider version (e.g., 'cluster-api:v1.5.0')",
				Optional:            true,
			},
			"target_namespace": schema.StringAttribute{
				MarkdownDescription: "Target namespace for the cluster",
				Optional:            true,
				Computed:            true,
			},
			"kubernetes_version": schema.StringAttribute{
				MarkdownDescription: "Kubernetes version for the cluster",
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
		},
	}
}

func (r *ClusterResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	// Prevent panic if the provider has not been configured.
	if req.ProviderData == nil {
		return
	}
}

func (r *ClusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data ClusterResourceModel

	// Read Terraform plan data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating CAPI cluster", map[string]interface{}{
		"name": data.Name.ValueString(),
	})

	// Create clusterctl client
	configPath := ""
	if home, err := os.UserHomeDir(); err == nil {
		configPath = filepath.Join(home, ".cluster-api")
	}

	client, err := clusterctlclient.New(ctx, configPath)
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to create clusterctl client, got error: %s", err))
		return
	}

	// Initialize management cluster if not skipped
	if !data.SkipInit.ValueBool() {
		tflog.Info(ctx, "Initializing management cluster")

		initOpts := clusterctlclient.InitOptions{
			Kubeconfig: clusterctlclient.Kubeconfig{
				Path: data.ManagementKubeconfig.ValueString(),
			},
		}

		// Add providers to init options based on what's specified
		if !data.CoreProvider.IsNull() && data.CoreProvider.ValueString() != "" {
			initOpts.CoreProvider = data.CoreProvider.ValueString()
		}

		if !data.BootstrapProvider.IsNull() && data.BootstrapProvider.ValueString() != "" {
			initOpts.BootstrapProviders = []string{data.BootstrapProvider.ValueString()}
		}

		if !data.ControlPlaneProvider.IsNull() && data.ControlPlaneProvider.ValueString() != "" {
			initOpts.ControlPlaneProviders = []string{data.ControlPlaneProvider.ValueString()}
		}

		if !data.InfrastructureProvider.IsNull() && data.InfrastructureProvider.ValueString() != "" {
			initOpts.InfrastructureProviders = []string{data.InfrastructureProvider.ValueString()}
		}

		_, err = client.Init(ctx, initOpts)
		if err != nil {
			resp.Diagnostics.AddError("Initialization Error", fmt.Sprintf("Unable to initialize management cluster, got error: %s", err))
			return
		}
	}

	// Generate cluster template
	tflog.Info(ctx, "Generating cluster template")

	templateOpts := clusterctlclient.GetClusterTemplateOptions{
		Kubeconfig: clusterctlclient.Kubeconfig{
			Path: data.ManagementKubeconfig.ValueString(),
		},
		ClusterName:       data.Name.ValueString(),
		TargetNamespace:   data.TargetNamespace.ValueString(),
		KubernetesVersion: data.KubernetesVersion.ValueString(),
	}

	if !data.ControlPlaneMachineCount.IsNull() {
		count := data.ControlPlaneMachineCount.ValueInt64()
		templateOpts.ControlPlaneMachineCount = &count
	}

	if !data.WorkerMachineCount.IsNull() {
		count := data.WorkerMachineCount.ValueInt64()
		templateOpts.WorkerMachineCount = &count
	}

	if !data.InfrastructureProvider.IsNull() && data.InfrastructureProvider.ValueString() != "" {
		templateOpts.ProviderRepositorySource = &clusterctlclient.ProviderRepositorySourceOptions{
			InfrastructureProvider: data.InfrastructureProvider.ValueString(),
			Flavor:                 data.Flavor.ValueString(),
		}
	}

	template, err := client.GetClusterTemplate(ctx, templateOpts)
	if err != nil {
		resp.Diagnostics.AddError("Template Error", fmt.Sprintf("Unable to generate cluster template, got error: %s", err))
		return
	}

	templateYaml, err := template.Yaml()
	if err != nil {
		resp.Diagnostics.AddError("Template YAML Error", fmt.Sprintf("Unable to get template YAML, got error: %s", err))
		return
	}

	tflog.Debug(ctx, "Generated cluster template", map[string]interface{}{
		"template": string(templateYaml),
	})

	// At this point, we would apply the template to create the cluster
	// For now, we'll set the ID and basic computed values
	data.Id = types.StringValue(data.Name.ValueString())

	// Set kubeconfig path if not provided
	if data.KubeconfigPath.IsNull() {
		if home, err := os.UserHomeDir(); err == nil {
			data.KubeconfigPath = types.StringValue(filepath.Join(home, ".kube", fmt.Sprintf("%s.kubeconfig", data.Name.ValueString())))
		}
	}

	// Set target namespace to default if not provided
	if data.TargetNamespace.IsNull() {
		data.TargetNamespace = types.StringValue("default")
	}

	tflog.Trace(ctx, "Created CAPI cluster resource")

	// Save data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ClusterResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Reading CAPI cluster", map[string]interface{}{
		"name": data.Name.ValueString(),
	})

	// For now, we'll just verify the cluster still exists in state
	// In a full implementation, we would check the actual cluster status

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data ClusterResourceModel

	// Read Terraform plan data into the model
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Updating CAPI cluster", map[string]interface{}{
		"name": data.Name.ValueString(),
	})

	// Most cluster properties require replacement, but some like machine counts
	// could potentially be updated. For now, we'll just update the state.

	// Save updated data into Terraform state
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data ClusterResourceModel

	// Read Terraform prior state data into the model
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Deleting CAPI cluster", map[string]interface{}{
		"name": data.Name.ValueString(),
	})

	// In a full implementation, we would:
	// 1. Delete the cluster resources from the management cluster
	// 2. Clean up the kubeconfig file if it exists
	// 3. Optionally delete providers if this was the only cluster

	// For now, deletion is implicit - removing from Terraform state
}

func (r *ClusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
