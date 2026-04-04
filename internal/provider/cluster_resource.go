// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// Ensure provider defined types fully satisfy framework interfaces.
var _ resource.Resource = &ClusterResource{}
var _ resource.ResourceWithImportState = &ClusterResource{}
var _ resource.ResourceWithUpgradeState = &ClusterResource{}

func NewClusterResource() resource.Resource {
	return &ClusterResource{}
}

// ClusterResource defines the resource implementation.
type ClusterResource struct {
	providerData *CapiProviderModel
	manager      *capi.Manager
}

func (r *ClusterResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func (r *ClusterResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version:             1,
		MarkdownDescription: "Manages a Cluster API cluster using the CAPI management workflow (bootstrap -> init -> apply -> wait -> move).",

		Attributes: map[string]schema.Attribute{
			// --- Top-level identity attributes ---
			"name": schema.StringAttribute{
				MarkdownDescription: "The name of the cluster. Must be a valid DNS-1123 subdomain.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"kubernetes_version": schema.StringAttribute{
				MarkdownDescription: "Kubernetes version for the workload cluster (e.g., `v1.31.0`).",
				Optional:            true,
			},
			"flavor": schema.StringAttribute{
				MarkdownDescription: "Cluster template flavor to use. Maps to clusterctl template flavors.",
				Optional:            true,
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Cluster identifier.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},

			// --- management ---
			"management": schema.SingleNestedAttribute{
				MarkdownDescription: "Management cluster configuration. Controls how the CAPI lifecycle is managed.",
				Optional:            true,
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"kubeconfig": schema.StringAttribute{
						MarkdownDescription: "Path to the kubeconfig for an existing management cluster. If not provided, a bootstrap cluster (kind) is created automatically.",
						Optional:            true,
						PlanModifiers: []planmodifier.String{
							stringplanmodifier.RequiresReplace(),
						},
					},
					"skip_init": schema.BoolAttribute{
						MarkdownDescription: "Skip running clusterctl init on the management cluster. Use when CAPI providers are already installed.",
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(false),
					},
					"self_managed": schema.BoolAttribute{
						MarkdownDescription: "Pivot CAPI management from bootstrap to workload cluster (clusterctl move). Required `true` for Tinkerbell provider.",
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(false),
						PlanModifiers: []planmodifier.Bool{
							boolplanmodifier.RequiresReplace(),
						},
					},
					"namespace": schema.StringAttribute{
						MarkdownDescription: "Namespace on the management cluster where CAPI resources are created.",
						Optional:            true,
						Computed:            true,
						PlanModifiers: []planmodifier.String{
							stringplanmodifier.RequiresReplace(),
						},
					},
				},
			},

			// --- infrastructure ---
			"infrastructure": schema.SingleNestedAttribute{
				MarkdownDescription: "Infrastructure provider configuration.",
				Required:            true,
				PlanModifiers: []planmodifier.Object{
					objectplanmodifier.RequiresReplace(),
				},
				Attributes: map[string]schema.Attribute{
					"provider": schema.StringAttribute{
						MarkdownDescription: "Infrastructure provider name and optional version (e.g., `docker`, `tinkerbell:v0.5.4`).",
						Required:            true,
					},
				},
			},

			// --- bootstrap ---
			"bootstrap": schema.SingleNestedAttribute{
				MarkdownDescription: "Bootstrap provider configuration (e.g., kubeadm, talos).",
				Optional:            true,
				PlanModifiers: []planmodifier.Object{
					objectplanmodifier.RequiresReplace(),
				},
				Attributes: map[string]schema.Attribute{
					"provider": schema.StringAttribute{
						MarkdownDescription: "Bootstrap provider name and optional version (e.g., `kubeadm:v1.12.2`, `talos:v0.6.7`).",
						Required:            true,
					},
				},
			},

			// --- control_plane ---
			"control_plane": schema.SingleNestedAttribute{
				MarkdownDescription: "Control plane configuration.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"provider": schema.StringAttribute{
						MarkdownDescription: "Control plane provider name and optional version (e.g., `kubeadm:v1.12.2`, `talos:v0.6.7`).",
						Optional:            true,
						PlanModifiers: []planmodifier.String{
							stringplanmodifier.RequiresReplace(),
						},
					},
					"machine_count": schema.Int64Attribute{
						MarkdownDescription: "Number of control plane machines.",
						Optional:            true,
					},
				},
			},

			// --- core ---
			"core": schema.SingleNestedAttribute{
				MarkdownDescription: "Core CAPI provider configuration.",
				Optional:            true,
				PlanModifiers: []planmodifier.Object{
					objectplanmodifier.RequiresReplace(),
				},
				Attributes: map[string]schema.Attribute{
					"provider": schema.StringAttribute{
						MarkdownDescription: "Core provider name and version (e.g., `cluster-api:v1.12.2`).",
						Required:            true,
					},
				},
			},

			// --- workers ---
			"workers": schema.SingleNestedAttribute{
				MarkdownDescription: "Worker node configuration.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"machine_count": schema.Int64Attribute{
						MarkdownDescription: "Number of worker machines.",
						Optional:            true,
					},
				},
			},

			// --- addons ---
			"addons": schema.ListNestedAttribute{
				MarkdownDescription: "Addon provider configurations modeled after the cluster-api-operator AddonProvider CRD (`operator.cluster.x-k8s.io/v1alpha2`). Each element installs one addon provider via `clusterctl init`. Customizations (deployment, manager, patches) are applied natively by wrapping the clusterctl client's repository factory — the operator itself is not required.",
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"provider": schema.StringAttribute{
							MarkdownDescription: "Addon provider name and optional version (e.g., `helm:v0.2.12`).",
							Required:            true,
						},
						"config_variables": schema.MapAttribute{
							MarkdownDescription: "Template variables injected into the provider's component YAML during processing (`${VAR}` substitution). These take precedence over clusterctl config and environment variables.",
							ElementType:         types.StringType,
							Optional:            true,
						},
						"secret_config_variables": schema.MapAttribute{
							MarkdownDescription: "Sensitive template variables injected into the provider's component YAML. Same mechanism as `config_variables` but for secret values.",
							ElementType:         types.StringType,
							Optional:            true,
							Sensitive:          true,
						},
						"fetch_config": schema.SingleNestedAttribute{
							MarkdownDescription: "Determines how the provider fetches components and metadata. Exactly one of `url` or `oci` must be specified.",
							Optional:            true,
							Attributes: map[string]schema.Attribute{
								"url": schema.StringAttribute{
									MarkdownDescription: "URL for fetching provider components from a remote GitHub repository (e.g., `https://github.com/{owner}/{repo}/releases`).",
									Optional:            true,
								},
								"oci": schema.StringAttribute{
									MarkdownDescription: "OCI artifact reference for fetching provider components (e.g., `oci://ghcr.io/org/provider`).",
									Optional:            true,
								},
							},
						},
						"deployment": schema.SingleNestedAttribute{
							MarkdownDescription: "Deployment customization for the addon provider controller.",
							Optional:            true,
							Attributes: map[string]schema.Attribute{
								"replicas": schema.Int64Attribute{
									MarkdownDescription: "Number of desired pods. Defaults to 1.",
									Optional:            true,
								},
								"node_selector": schema.MapAttribute{
									MarkdownDescription: "Node selector labels for pod scheduling.",
									ElementType:         types.StringType,
									Optional:            true,
								},
								"service_account_name": schema.StringAttribute{
									MarkdownDescription: "Service account name for the provider pod.",
									Optional:            true,
								},
								"containers": schema.ListNestedAttribute{
									MarkdownDescription: "Container overrides for the provider deployment.",
									Optional:            true,
									NestedObject: schema.NestedAttributeObject{
										Attributes: map[string]schema.Attribute{
											"name": schema.StringAttribute{
												MarkdownDescription: "Container name. Must match an existing container in the deployment.",
												Required:            true,
											},
											"image_url": schema.StringAttribute{
												MarkdownDescription: "Container image URL override.",
												Optional:            true,
											},
											"args": schema.MapAttribute{
												MarkdownDescription: "Extra arguments passed to the container entrypoint. Explicit ManagerSpec values take precedence.",
												ElementType:         types.StringType,
												Optional:            true,
											},
											"command": schema.ListAttribute{
												MarkdownDescription: "Override for the container entrypoint command.",
												ElementType:         types.StringType,
												Optional:            true,
											},
										},
									},
								},
							},
						},
						"manager": schema.SingleNestedAttribute{
							MarkdownDescription: "Controller manager configuration for the addon provider.",
							Optional:            true,
							Attributes: map[string]schema.Attribute{
								"profiler_address": schema.StringAttribute{
									MarkdownDescription: "Bind address for the pprof profiler (e.g., `localhost:6060`). Empty disables profiling.",
									Optional:            true,
								},
								"max_concurrent_reconciles": schema.Int64Attribute{
									MarkdownDescription: "Maximum number of concurrent reconciles.",
									Optional:            true,
								},
								"verbosity": schema.Int64Attribute{
									MarkdownDescription: "Log verbosity level. Defaults to 1.",
									Optional:            true,
								},
								"feature_gates": schema.MapAttribute{
									MarkdownDescription: "Provider-specific feature gates passed as `--feature-gates` to the controller manager.",
									ElementType:         types.BoolType,
									Optional:            true,
								},
								"additional_args": schema.MapAttribute{
									MarkdownDescription: "Additional arguments passed as container args to the controller manager.",
									ElementType:         types.StringType,
									Optional:            true,
								},
							},
						},
						"additional_manifests": schema.StringAttribute{
							MarkdownDescription: "Inline YAML content of additional manifests to apply along with the provider components. Supports multi-document YAML (separated by `---`).",
							Optional:            true,
						},
						"manifest_patches": schema.ListAttribute{
							MarkdownDescription: "JSON merge patches applied to rendered provider manifests. Each entry is an inline YAML/JSON blob string (RFC 7396). Cannot be used together with `patches`.",
							ElementType:         types.StringType,
							Optional:            true,
						},
						"patches": schema.ListNestedAttribute{
							MarkdownDescription: "Strategic merge patches or RFC 6902 JSON patches applied to rendered provider manifests. Cannot be used together with `manifest_patches`.",
							Optional:            true,
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"patch": schema.StringAttribute{
										MarkdownDescription: "Inline YAML/JSON patch content.",
										Optional:            true,
									},
									"target": schema.SingleNestedAttribute{
										MarkdownDescription: "Target object selector for the patch.",
										Optional:            true,
										Attributes: map[string]schema.Attribute{
											"group":          schema.StringAttribute{Optional: true, MarkdownDescription: "API group of the target."},
											"version":        schema.StringAttribute{Optional: true, MarkdownDescription: "API version of the target."},
											"kind":           schema.StringAttribute{Optional: true, MarkdownDescription: "Kind of the target."},
											"name":           schema.StringAttribute{Optional: true, MarkdownDescription: "Name of the target."},
											"namespace":      schema.StringAttribute{Optional: true, MarkdownDescription: "Namespace of the target."},
											"label_selector": schema.StringAttribute{Optional: true, MarkdownDescription: "Label selector expression."},
										},
									},
								},
							},
						},
					},
				},
			},

			// --- inventory ---
			"inventory": schema.SingleNestedAttribute{
				MarkdownDescription: "Hardware inventory for bare-metal provisioning.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"source": schema.StringAttribute{
						MarkdownDescription: "Path to a hardware inventory file (CSV or YAML).",
						Optional:            true,
					},
					"machine": schema.ListNestedAttribute{
						MarkdownDescription: "Inline machine definitions.",
						Optional:            true,
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"hostname": schema.StringAttribute{
									MarkdownDescription: "Machine hostname. Must be unique.",
									Required:            true,
								},
								"network": schema.SingleNestedAttribute{
									MarkdownDescription: "Network configuration.",
									Required:            true,
									Attributes: map[string]schema.Attribute{
										"ip_address":  schema.StringAttribute{Required: true, MarkdownDescription: "Primary IP address."},
										"netmask":     schema.StringAttribute{Required: true, MarkdownDescription: "Network mask."},
										"gateway":     schema.StringAttribute{Required: true, MarkdownDescription: "Default gateway."},
										"mac_address": schema.StringAttribute{Required: true, MarkdownDescription: "Primary NIC MAC address."},
										"nameservers": schema.ListAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "DNS nameservers."},
										"vlan_id":     schema.StringAttribute{Optional: true, MarkdownDescription: "VLAN ID."},
									},
								},
								"disk": schema.SingleNestedAttribute{
									MarkdownDescription: "Boot disk configuration.",
									Optional:            true,
									Attributes: map[string]schema.Attribute{
										"device": schema.StringAttribute{Required: true, MarkdownDescription: "Disk device path."},
									},
								},
								"bmc": schema.SingleNestedAttribute{
									MarkdownDescription: "BMC configuration.",
									Optional:            true,
									Attributes: map[string]schema.Attribute{
										"address":  schema.StringAttribute{Required: true, MarkdownDescription: "BMC endpoint."},
										"username": schema.StringAttribute{Required: true, MarkdownDescription: "BMC username."},
										"password": schema.StringAttribute{Required: true, Sensitive: true, MarkdownDescription: "BMC password."},
									},
								},
								"labels": schema.MapAttribute{
									Optional:            true,
									ElementType:         types.StringType,
									MarkdownDescription: "Labels. Use `type=cp` for control plane, `type=worker` for workers.",
								},
							},
						},
					},
				},
			},

			// --- wait ---
			"wait": schema.SingleNestedAttribute{
				MarkdownDescription: "Readiness wait configuration.",
				Optional:            true,
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{
						MarkdownDescription: "Wait for readiness.",
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(true),
					},
					"timeout": schema.StringAttribute{
						MarkdownDescription: "Max wait time (Go duration, e.g., `30m`). Default: `30m`.",
						Optional:            true,
						Computed:            true,
					},
				},
			},

			// --- output ---
			"output": schema.SingleNestedAttribute{
				MarkdownDescription: "Output configuration.",
				Optional:            true,
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"kubeconfig_path": schema.StringAttribute{
						MarkdownDescription: "File path for the workload cluster kubeconfig.",
						Optional:            true,
						Computed:            true,
					},
				},
			},

			// --- status ---
			"status": schema.SingleNestedAttribute{
				MarkdownDescription: "Computed cluster status.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"endpoint":          schema.StringAttribute{Computed: true, MarkdownDescription: "API server endpoint."},
					"kubeconfig":        schema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "Kubeconfig content."},
					"ca_certificate":    schema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "CA certificate (PEM)."},
					"description":       schema.StringAttribute{Computed: true, MarkdownDescription: "Cluster description."},
					"bootstrap_cluster": schema.StringAttribute{Computed: true, MarkdownDescription: "Bootstrap cluster name."},
				},
			},
		},
	}
}

func (r *ClusterResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	providerData, ok := req.ProviderData.(*CapiProviderModel)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *CapiProviderModel, got: %T.", req.ProviderData),
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

	r.validateLifecycleConfig(ctx, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating CAPI cluster", map[string]interface{}{
		"name": data.Name.ValueString(),
	})

	createOpts, diags := buildCreateOptions(ctx, &data)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Default kubeconfig output path
	if createOpts.KubeconfigOutputPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			createOpts.KubeconfigOutputPath = filepath.Join(home, ".kube", fmt.Sprintf("%s.kubeconfig", data.Name.ValueString()))
		}
	}

	result, err := r.manager.CreateCluster(ctx, *createOpts)
	if err != nil {
		resp.Diagnostics.AddError("Cluster Creation Error", fmt.Sprintf("Failed to create cluster: %s", err))
		return
	}

	data.Id = types.StringValue(data.Name.ValueString())
	r.ensureManagementComputed(ctx, &data, createOpts)
	r.ensureOutputComputed(ctx, &data, createOpts.KubeconfigOutputPath)
	r.ensureWaitComputed(ctx, &data)

	resp.Diagnostics.Append(setStatus(ctx, &data, result)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ClusterResourceModel

	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	mgmtKubeconfig := r.resolveManagementKubeconfig(ctx, &data, nil)
	if mgmtKubeconfig == "" {
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	namespace := "default"
	mgmt, _ := extractManagement(ctx, &data)
	if mgmt != nil && !mgmt.Namespace.IsNull() {
		namespace = mgmt.Namespace.ValueString()
	}

	result, err := r.manager.GetClusterInfo(ctx, mgmtKubeconfig, data.Name.ValueString(), namespace)
	if err != nil {
		tflog.Warn(ctx, "Unable to read cluster info", map[string]interface{}{"error": err.Error()})
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}

	prevStatus, _ := extractStatus(ctx, &data)
	resp.Diagnostics.Append(setStatusWithFallback(ctx, &data, result, prevStatus)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ClusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state ClusterResourceModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.validateLifecycleConfig(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	managementKubeconfig := r.resolveManagementKubeconfig(ctx, &plan, &state)
	if managementKubeconfig == "" {
		resp.Diagnostics.AddError("Cluster Update Error", "Unable to determine management kubeconfig.")
		return
	}

	namespace := "default"
	mgmt, _ := extractManagement(ctx, &plan)
	if mgmt != nil && !mgmt.Namespace.IsNull() && mgmt.Namespace.ValueString() != "" {
		namespace = mgmt.Namespace.ValueString()
	}

	reconcileOpts, diags := buildCreateOptions(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	reconcileOpts.ManagementKubeconfig = managementKubeconfig
	reconcileOpts.Namespace = namespace
	reconcileOpts.SkipInit = true
	reconcileOpts.SelfManaged = false

	result, err := r.manager.CreateCluster(ctx, *reconcileOpts)
	if err != nil {
		resp.Diagnostics.AddError("Cluster Update Error", fmt.Sprintf("Failed to reconcile cluster: %s", err))
		return
	}

	plan.Id = types.StringValue(plan.Name.ValueString())
	r.ensureManagementComputed(ctx, &plan, reconcileOpts)
	r.ensureOutputComputed(ctx, &plan, reconcileOpts.KubeconfigOutputPath)
	r.ensureWaitComputed(ctx, &plan)

	prevStatus, _ := extractStatus(ctx, &state)
	resp.Diagnostics.Append(setStatusWithFallback(ctx, &plan, result, prevStatus)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *ClusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data ClusterResourceModel

	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	mgmtKubeconfig := r.resolveManagementKubeconfig(ctx, &data, nil)

	namespace := "default"
	mgmt, _ := extractManagement(ctx, &data)
	if mgmt != nil && !mgmt.Namespace.IsNull() {
		namespace = mgmt.Namespace.ValueString()
	}

	deleteOpts := capi.DeleteClusterOptions{
		Name:                 data.Name.ValueString(),
		Namespace:            namespace,
		ManagementKubeconfig: mgmtKubeconfig,
	}

	status, _ := extractStatus(ctx, &data)
	if status != nil && !status.BootstrapCluster.IsNull() {
		deleteOpts.DeleteBootstrap = true
		deleteOpts.BootstrapName = status.BootstrapCluster.ValueString()
	}

	if mgmtKubeconfig != "" {
		if err := r.manager.DeleteCluster(ctx, deleteOpts); err != nil {
			resp.Diagnostics.AddWarning("Cluster Deletion Warning",
				fmt.Sprintf("Error deleting cluster (may already be removed): %s", err))
		}
	} else {
		tflog.Warn(ctx, "No management kubeconfig for deletion - manual cleanup may be needed")
	}
}

func (r *ClusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// UpgradeState migrates v0 (flat) state to v1 (nested).
func (r *ClusterResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
	v0Schema := clusterResourceSchemaV0()
	return map[int64]resource.StateUpgrader{
		0: {
			PriorSchema: &v0Schema,
			StateUpgrader: func(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
				var v0 clusterResourceModelV0
				resp.Diagnostics.Append(req.State.Get(ctx, &v0)...)
				if resp.Diagnostics.HasError() {
					return
				}

				v1 := ClusterResourceModel{
					Name:              v0.Name,
					KubernetesVersion: v0.KubernetesVersion,
					Flavor:            v0.Flavor,
					Id:                v0.Id,
				}

				// Management
				ns := v0.TargetNamespace
				if ns.IsNull() {
					ns = types.StringValue("default")
				}
				mgmt := ManagementModel{
					Kubeconfig:  v0.ManagementKubeconfig,
					SkipInit:    v0.SkipInit,
					SelfManaged: v0.SelfManaged,
					Namespace:   ns,
				}
				mgmtVal, d := types.ObjectValueFrom(ctx, managementAttrTypes(), mgmt)
				resp.Diagnostics.Append(d...)
				v1.Management = mgmtVal

				// Infrastructure
				infra := InfrastructureModel{Provider: v0.InfrastructureProvider}
				infraVal, d := types.ObjectValueFrom(ctx, infrastructureAttrTypes(), infra)
				resp.Diagnostics.Append(d...)
				v1.Infrastructure = infraVal

				// Bootstrap
				if !v0.BootstrapProvider.IsNull() {
					bs := BootstrapModel{Provider: v0.BootstrapProvider}
					bsVal, d := types.ObjectValueFrom(ctx, bootstrapAttrTypes(), bs)
					resp.Diagnostics.Append(d...)
					v1.Bootstrap = bsVal
				} else {
					v1.Bootstrap = types.ObjectNull(bootstrapAttrTypes())
				}

				// Control plane
				if !v0.ControlPlaneProvider.IsNull() || !v0.ControlPlaneMachineCount.IsNull() {
					cp := ControlPlaneModel{Provider: v0.ControlPlaneProvider, MachineCount: v0.ControlPlaneMachineCount}
					cpVal, d := types.ObjectValueFrom(ctx, controlPlaneAttrTypes(), cp)
					resp.Diagnostics.Append(d...)
					v1.ControlPlane = cpVal
				} else {
					v1.ControlPlane = types.ObjectNull(controlPlaneAttrTypes())
				}

				// Core
				if !v0.CoreProvider.IsNull() {
					core := CoreModel{Provider: v0.CoreProvider}
					coreVal, d := types.ObjectValueFrom(ctx, coreAttrTypes(), core)
					resp.Diagnostics.Append(d...)
					v1.Core = coreVal
				} else {
					v1.Core = types.ObjectNull(coreAttrTypes())
				}

				// Workers
				if !v0.WorkerMachineCount.IsNull() {
					w := WorkersModel{MachineCount: v0.WorkerMachineCount}
					wVal, d := types.ObjectValueFrom(ctx, workersAttrTypes(), w)
					resp.Diagnostics.Append(d...)
					v1.Workers = wVal
				} else {
					v1.Workers = types.ObjectNull(workersAttrTypes())
				}

				v1.Inventory = types.ObjectNull(inventoryAttrTypes())

				v1.Addons = types.ListNull(types.ObjectType{AttrTypes: addonAttrTypes()})

				wait := WaitModel{Enabled: v0.WaitForReady, Timeout: types.StringNull()}
				waitVal, d := types.ObjectValueFrom(ctx, waitAttrTypes(), wait)
				resp.Diagnostics.Append(d...)
				v1.Wait = waitVal

				out := OutputModel{KubeconfigPath: v0.KubeconfigPath}
				outVal, d := types.ObjectValueFrom(ctx, outputAttrTypes(), out)
				resp.Diagnostics.Append(d...)
				v1.Output = outVal

				status := StatusModel{
					Endpoint:         v0.Endpoint,
					Kubeconfig:       v0.Kubeconfig,
					CACertificate:    v0.ClusterCACertificate,
					Description:      v0.ClusterDescription,
					BootstrapCluster: v0.BootstrapClusterName,
				}
				statusVal, d := types.ObjectValueFrom(ctx, statusAttrTypes(), status)
				resp.Diagnostics.Append(d...)
				v1.Status = statusVal

				resp.Diagnostics.Append(resp.State.Set(ctx, v1)...)
			},
		},
	}
}

// --- Validation ---

func (r *ClusterResource) validateLifecycleConfig(ctx context.Context, data *ClusterResourceModel, diags *diag.Diagnostics) {
	infra, d := extractInfrastructure(ctx, data)
	diags.Append(d...)
	if diags.HasError() || infra == nil {
		return
	}

	provider := strings.Split(infra.Provider.ValueString(), ":")[0]
	provider = strings.ToLower(provider)

	supportedProviders := map[string]struct{}{
		"aws": {}, "azure": {}, "docker": {}, "openstack": {}, "tinkerbell": {}, "vsphere": {},
	}

	if _, ok := supportedProviders[provider]; !ok {
		diags.AddError(
			"Unsupported infrastructure provider",
			fmt.Sprintf("infrastructure.provider %q is not supported. Supported: aws, azure, docker, openstack, tinkerbell, vsphere", provider),
		)
	}

	if provider == "tinkerbell" {
		mgmt, d := extractManagement(ctx, data)
		diags.Append(d...)
		if mgmt == nil || mgmt.SelfManaged.IsNull() || !mgmt.SelfManaged.ValueBool() {
			diags.AddError("Invalid Tinkerbell configuration", "Tinkerbell clusters must have management.self_managed = true.")
		}

		bs, d := extractBootstrap(ctx, data)
		diags.Append(d...)
		if bs != nil && !bs.Provider.IsNull() && bs.Provider.ValueString() != "" {
			bsName := strings.Split(bs.Provider.ValueString(), ":")[0]
			if n := strings.ToLower(bsName); n != "kubeadm" && n != "talos" {
				diags.AddError("Invalid bootstrap provider for Tinkerbell", "Tinkerbell supports bootstrap.provider = \"kubeadm\" or \"talos\".")
			}
		}

		cp, d := extractControlPlane(ctx, data)
		diags.Append(d...)
		if cp != nil && !cp.Provider.IsNull() && cp.Provider.ValueString() != "" {
			cpName := strings.Split(cp.Provider.ValueString(), ":")[0]
			if n := strings.ToLower(cpName); n != "kubeadm" && n != "talos" {
				diags.AddError("Invalid control plane provider for Tinkerbell", "Tinkerbell supports control_plane.provider = \"kubeadm\" or \"talos\".")
			}
		}
	}

	// Validate inventory
	inv, d := extractInventory(ctx, data)
	diags.Append(d...)
	if inv != nil {
		var cpCount, workerCount int64
		cp, d := extractControlPlane(ctx, data)
		diags.Append(d...)
		if cp != nil && !cp.MachineCount.IsNull() {
			cpCount = cp.MachineCount.ValueInt64()
		}
		w, d := extractWorkers(ctx, data)
		diags.Append(d...)
		if w != nil && !w.MachineCount.IsNull() {
			workerCount = w.MachineCount.ValueInt64()
		}
		validateInventory(ctx, inv, cpCount, workerCount, diags)
	}

	// Validate addons
	validateAddons(ctx, data, diags)
}

func (r *ClusterResource) resolveManagementKubeconfig(ctx context.Context, plan *ClusterResourceModel, state *ClusterResourceModel) string {
	mgmt, _ := extractManagement(ctx, plan)
	if mgmt != nil && !mgmt.Kubeconfig.IsNull() && mgmt.Kubeconfig.ValueString() != "" {
		return mgmt.Kubeconfig.ValueString()
	}

	if state != nil {
		stateMgmt, _ := extractManagement(ctx, state)
		if stateMgmt != nil && !stateMgmt.Kubeconfig.IsNull() && stateMgmt.Kubeconfig.ValueString() != "" {
			return stateMgmt.Kubeconfig.ValueString()
		}
	}

	if mgmt != nil && !mgmt.SelfManaged.IsNull() && mgmt.SelfManaged.ValueBool() {
		out, _ := extractOutput(ctx, plan)
		if out != nil && !out.KubeconfigPath.IsNull() && out.KubeconfigPath.ValueString() != "" {
			return out.KubeconfigPath.ValueString()
		}
		if state != nil {
			stateOut, _ := extractOutput(ctx, state)
			if stateOut != nil && !stateOut.KubeconfigPath.IsNull() && stateOut.KubeconfigPath.ValueString() != "" {
				return stateOut.KubeconfigPath.ValueString()
			}
		}
	}

	status, _ := extractStatus(ctx, plan)
	if status != nil && !status.BootstrapCluster.IsNull() {
		return filepath.Join(os.TempDir(), fmt.Sprintf("kind-%s-kubeconfig", status.BootstrapCluster.ValueString()))
	}
	if state != nil {
		stateStatus, _ := extractStatus(ctx, state)
		if stateStatus != nil && !stateStatus.BootstrapCluster.IsNull() {
			return filepath.Join(os.TempDir(), fmt.Sprintf("kind-%s-kubeconfig", stateStatus.BootstrapCluster.ValueString()))
		}
	}

	return ""
}

// --- Computed Field Helpers ---

func (r *ClusterResource) ensureManagementComputed(ctx context.Context, data *ClusterResourceModel, opts *capi.CreateClusterOptions) {
	mgmt, _ := extractManagement(ctx, data)

	namespace := "default"
	if opts.Namespace != "" {
		namespace = opts.Namespace
	}

	if mgmt == nil {
		mgmt = &ManagementModel{
			Kubeconfig:  types.StringNull(),
			SkipInit:    types.BoolValue(opts.SkipInit),
			SelfManaged: types.BoolValue(opts.SelfManaged),
			Namespace:   types.StringValue(namespace),
		}
	} else {
		if mgmt.Namespace.IsNull() || mgmt.Namespace.ValueString() == "" {
			mgmt.Namespace = types.StringValue(namespace)
		}
	}

	val, _ := types.ObjectValueFrom(ctx, managementAttrTypes(), mgmt)
	data.Management = val
}

func (r *ClusterResource) ensureOutputComputed(ctx context.Context, data *ClusterResourceModel, kubeconfigPath string) {
	out, _ := extractOutput(ctx, data)
	if out == nil {
		out = &OutputModel{KubeconfigPath: types.StringValue(kubeconfigPath)}
	} else if out.KubeconfigPath.IsNull() || out.KubeconfigPath.ValueString() == "" {
		out.KubeconfigPath = types.StringValue(kubeconfigPath)
	}

	val, _ := types.ObjectValueFrom(ctx, outputAttrTypes(), out)
	data.Output = val
}

func (r *ClusterResource) ensureWaitComputed(ctx context.Context, data *ClusterResourceModel) {
	wait, _ := extractWait(ctx, data)
	if wait == nil {
		wait = &WaitModel{Enabled: types.BoolValue(true), Timeout: types.StringValue("30m")}
	}
	if wait.Timeout.IsNull() || wait.Timeout.ValueString() == "" {
		wait.Timeout = types.StringValue("30m")
	}

	val, _ := types.ObjectValueFrom(ctx, waitAttrTypes(), wait)
	data.Wait = val
}

// --- v0 Schema (flat) for state migration ---

type clusterResourceModelV0 struct {
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

func clusterResourceSchemaV0() schema.Schema {
	return schema.Schema{
		Attributes: map[string]schema.Attribute{
			"name":                        schema.StringAttribute{Required: true},
			"kubeconfig_path":             schema.StringAttribute{Optional: true, Computed: true},
			"management_kubeconfig":       schema.StringAttribute{Optional: true},
			"skip_init":                   schema.BoolAttribute{Optional: true, Computed: true},
			"wait_for_ready":              schema.BoolAttribute{Optional: true, Computed: true},
			"self_managed":                schema.BoolAttribute{Optional: true, Computed: true},
			"infrastructure_provider":     schema.StringAttribute{Required: true},
			"bootstrap_provider":          schema.StringAttribute{Optional: true},
			"control_plane_provider":      schema.StringAttribute{Optional: true},
			"core_provider":               schema.StringAttribute{Optional: true},
			"target_namespace":            schema.StringAttribute{Optional: true, Computed: true},
			"kubernetes_version":          schema.StringAttribute{Optional: true},
			"control_plane_machine_count": schema.Int64Attribute{Optional: true},
			"worker_machine_count":        schema.Int64Attribute{Optional: true},
			"flavor":                      schema.StringAttribute{Optional: true},
			"id":                          schema.StringAttribute{Computed: true},
			"endpoint":                    schema.StringAttribute{Computed: true},
			"cluster_ca_certificate":      schema.StringAttribute{Computed: true, Sensitive: true},
			"kubeconfig":                  schema.StringAttribute{Computed: true, Sensitive: true},
			"cluster_description":         schema.StringAttribute{Computed: true},
			"bootstrap_cluster_name":      schema.StringAttribute{Computed: true},
		},
	}
}
