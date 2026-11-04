// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/dynamicplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"sigs.k8s.io/yaml"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/talos"
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
		Version:             3,
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
			"cluster_class": schema.BoolAttribute{
				MarkdownDescription: "Generate a `ClusterClass` and a `Cluster` with `spec.topology` instead of direct objects. Every provider must ship `*Template` kinds; the `ClusterTopology` feature gate is enabled at init. Default `false`.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
				},
			},
			"network": schema.SingleNestedAttribute{
				MarkdownDescription: "Cluster networking, as in `Cluster.spec.clusterNetwork` and `Cluster.spec.controlPlaneEndpoint`. Omit for providers that fill the endpoint themselves (e.g. Docker).",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"control_plane_endpoint": schema.SingleNestedAttribute{
						MarkdownDescription: "API server endpoint (`Cluster.spec.controlPlaneEndpoint`); on bare metal, the control plane VIP.",
						Optional:            true,
						Attributes: map[string]schema.Attribute{
							"host": schema.StringAttribute{
								MarkdownDescription: "Endpoint host or IP.",
								Required:            true,
							},
							"port": schema.Int64Attribute{
								MarkdownDescription: "Endpoint port. Defaults to 6443.",
								Optional:            true,
							},
						},
					},
					"pods": schema.SingleNestedAttribute{
						MarkdownDescription: "Pod network ranges (`clusterNetwork.pods`).",
						Optional:            true,
						Attributes: map[string]schema.Attribute{
							"cidr_blocks": schema.ListAttribute{
								MarkdownDescription: "Pod CIDR blocks.",
								ElementType:         types.StringType,
								Optional:            true,
							},
						},
					},
					"services": schema.SingleNestedAttribute{
						MarkdownDescription: "Service network ranges (`clusterNetwork.services`).",
						Optional:            true,
						Attributes: map[string]schema.Attribute{
							"cidr_blocks": schema.ListAttribute{
								MarkdownDescription: "Service CIDR blocks.",
								ElementType:         types.StringType,
								Optional:            true,
							},
						},
					},
					"service_domain": schema.StringAttribute{
						MarkdownDescription: "Cluster DNS domain (`clusterNetwork.serviceDomain`), e.g. `cluster.local`.",
						Optional:            true,
					},
				},
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Cluster identifier.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"provider_secrets": schema.DynamicAttribute{
				MarkdownDescription: "Opaque, provider-specific secrets persisted so later operations can recognize and manage what this provider created — for the Talos bootstrapper, the machine-secrets bundle keyed by provider. Computed and sensitive; never set by practitioners.",
				Computed:            true,
				Sensitive:           true,
				PlanModifiers: []planmodifier.Dynamic{
					dynamicplanmodifier.UseNonNullStateForUnknown(),
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
						MarkdownDescription: "Skip running clusterctl init on the management cluster. Use when CAPI providers are already installed and managed elsewhere: the provider maps are then never installed, upgraded or deleted by this resource.",
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(false),
					},
					"uninstall": schema.BoolAttribute{
						MarkdownDescription: "On destroy, remove every CAPI provider from the management cluster once the cluster is gone (`clusterctl delete --all`, keeping CRDs and namespaces). Only for a management cluster given by `kubeconfig` that this resource initialised (`skip_init = false`); a bootstrap or self-managed cluster is deleted anyway. Leave unset when the management cluster is shared.",
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
							// Keep the computed value across updates; without this it
							// recomputes to unknown on any in-place change and, being
							// RequiresReplace, forces a spurious full rebuild.
							stringplanmodifier.UseStateForUnknown(),
							stringplanmodifier.RequiresReplace(),
						},
					},

					"bootstrap": schema.SingleNestedAttribute{
						MarkdownDescription: "Transient bootstrap cluster configuration. `type = \"kind\"` (default) creates a kind cluster. `type = \"talos\"` provisions one `inventory.machine` entry as a single-node Talos cluster through its BMC. `type = \"kairos\"` boots one `inventory.machine` entry from a user-built Kairos live ISO that runs k3s from RAM (see `kairos`). The bootstrap cluster is torn down after the self-managed pivot and nothing about it is kept in state.",
						Optional:            true,
						PlanModifiers: []planmodifier.Object{
							objectplanmodifier.RequiresReplace(),
						},
						Attributes: map[string]schema.Attribute{
							"type": schema.StringAttribute{
								MarkdownDescription: "Bootstrap cluster type: `kind`, `talos` or `kairos`.",
								Optional:            true,
								Computed:            true,
								Default:             stringdefault.StaticString("kind"),
							},
							"mode": schema.StringAttribute{
								MarkdownDescription: "How the bootstrap cluster becomes the management cluster: `pivot` (default) moves CAPI to the workload cluster and tears the bootstrap cluster down; `in_place` keeps the bootstrap node as the self-managed cluster (no pivot) — for Talos, the CAPI providers adopt the bootstrap node's pre-created secrets so it is the same cluster.",
								Optional:            true,
								Computed:            true,
								Default:             stringdefault.StaticString("pivot"),
							},
							"state_dir": schema.StringAttribute{
								MarkdownDescription: "Optional override for where the Talos machine-secrets bundle is cached between applies. The cache lets a failed apply be retried without wiping and re-installing the node: the next `terraform apply` creates the resource but resumes from the node's current state (recognized as ours via the cached secrets), and the cache is removed on destroy. Defaults to a per-user cache directory (`os.UserCacheDir()`, falling back to the temp dir), so caching is on without configuration.",
								Optional:            true,
							},
							"machine": schema.StringAttribute{
								MarkdownDescription: "Hostname of the `inventory.machine` entry to use as the bootstrap node. Its `bmc`, `network.ip_address`, and `disk.device` are used (a `kairos` node writes nothing to disk and needs no `disk`). Required when `type` is `talos` or `kairos`.",
								Optional:            true,
							},
							"boot": schema.SingleNestedAttribute{
								MarkdownDescription: "How the node is booted into the Talos installer.",
								Optional:            true,
								Attributes: map[string]schema.Attribute{
									"method": schema.StringAttribute{
										MarkdownDescription: "`auto` (virtual media, then UEFI HTTP boot), `virtual_media`, or `http`.",
										Optional:            true,
										Computed:            true,
										Default:             stringdefault.StaticString("auto"),
									},
									"timeout": schema.StringAttribute{
										MarkdownDescription: "Per-attempt boot and install timeout as a Go duration (e.g. `15m`).",
										Optional:            true,
										Computed:            true,
										Default:             stringdefault.StaticString("15m"),
									},
									"attempts": schema.Int64Attribute{
										MarkdownDescription: "Boot attempts before giving up.",
										Optional:            true,
										Computed:            true,
										Default:             int64default.StaticInt64(3),
									},
									"device": schema.StringAttribute{
										MarkdownDescription: "Firmware boot target for attached virtual media: `cdrom` (default) or `usb`. Set `usb` for BMCs that expose virtual media as a USB device (many KVM-over-IP dongles such as NanoKVM), where a CD-ROM boot override never selects the media.",
										Optional:            true,
										Computed:            true,
										Default:             stringdefault.StaticString("cdrom"),
									},
								},
							},
							"talos": schema.SingleNestedAttribute{
								MarkdownDescription: "Talos version, images, and machine config patches.",
								Optional:            true,
								Attributes: map[string]schema.Attribute{
									"version": schema.StringAttribute{
										MarkdownDescription: "Talos version (e.g. `v1.13.6`). Required when `type = \"talos\"`.",
										Optional:            true,
									},
									"architecture": schema.StringAttribute{
										MarkdownDescription: "`amd64` or `arm64`.",
										Optional:            true,
										Computed:            true,
										Default:             stringdefault.StaticString("amd64"),
									},
									"endpoint": schema.StringAttribute{
										MarkdownDescription: "Cluster endpoint written into the machine config. Defaults to `https://<machine ip>:6443`.",
										Optional:            true,
									},
									"image": schema.SingleNestedAttribute{
										MarkdownDescription: "Image Factory schematic or explicit image overrides.",
										Optional:            true,
										Attributes: map[string]schema.Attribute{
											"factory": schema.StringAttribute{
												MarkdownDescription: "Image Factory base URL.",
												Optional:            true,
												Computed:            true,
												Default:             stringdefault.StaticString("https://factory.talos.dev"),
											},
											"schematic": schema.StringAttribute{
												MarkdownDescription: "Precomputed schematic id. Conflicts with `extensions`, `kernel_args`, `iso`, and `installer`.",
												Optional:            true,
											},
											"extensions": schema.ListAttribute{
												MarkdownDescription: "Official system extension names for a new schematic.",
												ElementType:         types.StringType,
												Optional:            true,
											},
											"kernel_args": schema.ListAttribute{
												MarkdownDescription: "Extra kernel arguments for a new schematic.",
												ElementType:         types.StringType,
												Optional:            true,
											},
											"iso": schema.StringAttribute{
												MarkdownDescription: "Explicit ISO URL. Must be set together with `installer`; disables UEFI HTTP boot.",
												Optional:            true,
											},
											"installer": schema.StringAttribute{
												MarkdownDescription: "Explicit installer image reference. Must be set together with `iso`.",
												Optional:            true,
											},
										},
									},
									"config_patches": schema.ListAttribute{
										MarkdownDescription: "Machine config patches (YAML strings) applied in order after the built-in install patch.",
										ElementType:         types.StringType,
										Optional:            true,
									},
								},
							},
							"kairos": schema.SingleNestedAttribute{
								MarkdownDescription: "Disposable live node for `type = \"kairos\"`: a user-built Kairos ISO (AuroraBoot's `kairos.iso.custom.iso` with the cloud-config embedded: `k3s.enabled`, the SSH key, the node's static address, `kairos.ram` on the live command line) booted from virtual media through the machine's BMC. The provider reads the k3s kubeconfig over SSH, uses the node as the management cluster until the pivot, then powers it off and ejects the media. Nothing is written to its disk, so `mode` must be `pivot` and `management.self_managed` must be true.",
								Optional:            true,
								Attributes: map[string]schema.Attribute{
									"iso": schema.StringAttribute{
										MarkdownDescription: "The live image: an `http(s)` URL the BMC fetches as virtual media, or with Intel AMT storage redirection also a local path.",
										Required:            true,
									},
									"address": schema.StringAttribute{
										MarkdownDescription: "Address the live node takes (SSH and the Kubernetes API). Defaults to the bootstrap machine's `network.ip_address`.",
										Optional:            true,
									},
									"ssh": schema.SingleNestedAttribute{
										MarkdownDescription: "How the live node is reached.",
										Required:            true,
										Attributes: map[string]schema.Attribute{
											"user": schema.StringAttribute{
												MarkdownDescription: "Login the cloud-config authorizes.",
												Optional:            true,
												Computed:            true,
												Default:             stringdefault.StaticString("kairos"),
											},
											"port": schema.Int64Attribute{
												MarkdownDescription: "SSH port.",
												Optional:            true,
												Computed:            true,
												Default:             int64default.StaticInt64(22),
											},
											"private_key": schema.StringAttribute{
												MarkdownDescription: "PEM-encoded private key whose public half is in the cloud-config's `ssh_authorized_keys`.",
												Required:            true,
												Sensitive:           true,
											},
											"host_key": schema.StringAttribute{
												MarkdownDescription: "The node's SSH host public key in `authorized_keys` format, when the ISO embeds one; any other host key is then refused. A live image that runs from RAM generates a new host key at every boot, so by default none is checked.",
												Optional:            true,
											},
										},
									},
								},
							},
							"addons": addonsAttribute("Helm releases and manifests installed on the bootstrap node before CAPI is initialized, in this order: the `cni` release, `prerequisites`, `helm`, `manifests`. For releases only the bootstrap node needs; the Tinkerbell stack belongs in `management.addons`."),
						},
					},
					"addons": addonsAttribute("Management-plane addons, installed by the provider's own Helm and apply clients on every cluster that takes the management role, right after its CAPI providers are initialized: the bootstrap cluster, and with a pivot the workload cluster again before the move. The Tinkerbell stack lives here. `prerequisites` are applied only on the final management cluster (they are objects such as a Cilium load-balancer pool whose CRDs a transient bootstrap cluster may not have); `helm` and `manifests` are installed on both."),
				},
			},

			// --- providers (cluster-api-operator style maps keyed by provider name) ---
			"core":           providerMapAttribute("Core CAPI provider, keyed by name (`cluster-api`). Omit to let clusterctl install the default.", false, providerMapPinned),
			"infrastructure": providerMapAttribute("Infrastructure provider, keyed by name (e.g. `docker`, `tinkerbell`). Exactly one entry is required; with `control_plane` and `bootstrap` (default `kubeadm`) it selects the kinds of the generated cluster objects.", true, providerMapStructural),
			"bootstrap":      providerMapAttribute("Bootstrap providers, keyed by name (e.g. `kubeadm`, `talos`).", false, providerMapStructural),
			"control_plane":  providerMapAttribute("Control plane providers, keyed by name (e.g. `kubeadm`, `talos`).", false, providerMapStructural),
			"ipam":           providerMapAttribute("IPAM providers, keyed by name (e.g. `in-cluster`, `unifi`).", false, providerMapConverged),
			"addon":          providerMapAttribute("Addon providers, keyed by name (e.g. `helm`). An empty object installs the default release.", false, providerMapConverged),

			// --- topology ---
			"topology": schema.SingleNestedAttribute{
				MarkdownDescription: "Workload cluster topology. Mirrors `Cluster.spec.topology`.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"control_plane": schema.SingleNestedAttribute{
						MarkdownDescription: "Control plane topology.",
						Optional:            true,
						Attributes: map[string]schema.Attribute{
							"replicas": schema.Int64Attribute{
								MarkdownDescription: "Number of control plane machines. Use an odd number for HA (1, 3, 5).",
								Optional:            true,
							},
							"infrastructure": schema.StringAttribute{
								MarkdownDescription: "Spec of the control plane's `<Infra>MachineTemplate` (e.g. Tinkerbell `hardwareAffinity`) as YAML or JSON with the CRD's camelCase keys, typically `yamlencode({ ... })`. A change creates a new template and rolls the control plane.",
								Optional:            true,
							},
						},
					},
					"workers": schema.SingleNestedAttribute{
						MarkdownDescription: "Worker topology.",
						Optional:            true,
						Attributes: map[string]schema.Attribute{
							"machine_deployments": schema.ListNestedAttribute{
								MarkdownDescription: "Worker MachineDeployments, as in `Cluster.spec.topology.workers.machineDeployments`. Each entry becomes a MachineDeployment with its own machine and bootstrap config templates (or, with `cluster_class`, a worker class and a topology entry).",
								Optional:            true,
								NestedObject: schema.NestedAttributeObject{
									Attributes: map[string]schema.Attribute{
										"name": schema.StringAttribute{
											MarkdownDescription: "MachineDeployment name. Must be unique within the cluster.",
											Required:            true,
										},
										"class": schema.StringAttribute{
											MarkdownDescription: "Worker class name, used with `cluster_class`: deployments sharing a class share templates and must agree on `infrastructure` and `bootstrap`. Defaults to `name`.",
											Optional:            true,
										},
										"infrastructure": schema.StringAttribute{
											MarkdownDescription: "Spec of this deployment's `<Infra>MachineTemplate` as YAML or JSON with the CRD's camelCase keys, typically `yamlencode({ ... })`. A change creates a new template and rolls the deployment.",
											Optional:            true,
										},
										"bootstrap": schema.StringAttribute{
											MarkdownDescription: "Spec of this deployment's `<Bootstrap>ConfigTemplate` as YAML or JSON. Omit to use `bootstrap.<name>.resource_config`.",
											Optional:            true,
										},
										"replicas": schema.Int64Attribute{
											MarkdownDescription: "Number of machines.",
											Optional:            true,
										},
										"failure_domain": schema.StringAttribute{
											MarkdownDescription: "Failure domain the machines are placed in.",
											Optional:            true,
										},
										"metadata": schema.SingleNestedAttribute{
											MarkdownDescription: "Labels and annotations applied to the machines.",
											Optional:            true,
											Attributes: map[string]schema.Attribute{
												"labels":      schema.MapAttribute{ElementType: types.StringType, Optional: true, MarkdownDescription: "Machine labels."},
												"annotations": schema.MapAttribute{ElementType: types.StringType, Optional: true, MarkdownDescription: "Machine annotations."},
											},
										},
									},
								},
							},
						},
					},
				},
			},

			// --- cni ---
			"cni": schema.SingleNestedAttribute{
				MarkdownDescription: "Container Network Interface for the workload cluster, replacing Talos's built-in flannel. Valid with a `talos` management bootstrap, or a `kairos` one whose cluster has a `control_plane[\"talos\"]` entry; it disables the built-in CNI and kube-proxy (KubePrism handles the API server) in every Talos machine config and installs the CNI as a Helm release on the cluster.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"provider": schema.StringAttribute{
						MarkdownDescription: "CNI provider. Only `cilium` is supported today.",
						Required:            true,
					},
					"version": schema.StringAttribute{
						MarkdownDescription: "CNI Helm chart version. Omit to use the chart's latest release.",
						Optional:            true,
					},
					"values": schema.StringAttribute{
						MarkdownDescription: "Extra Helm values (YAML), deep-merged over the defaults (`kubeProxyReplacement`, KubePrism host/port, Kubernetes IPAM).",
						Optional:            true,
					},
					"gateway_api": schema.SingleNestedAttribute{
						MarkdownDescription: "Install the Gateway API CRDs before Cilium and enable `gatewayAPI` in the Cilium values. Cilium does not bundle the CRDs; without them the chart's GatewayClass fails to apply.",
						Optional:            true,
						Attributes: map[string]schema.Attribute{
							"enabled": schema.BoolAttribute{
								MarkdownDescription: "Fetch the upstream Gateway API `standard-install.yaml` and apply it before Cilium, and set `gatewayAPI.enabled=true` in the Cilium values.",
								Optional:            true,
							},
							"version": schema.StringAttribute{
								MarkdownDescription: "Gateway API release tag for the CRDs (e.g. `v1.3.0`). Defaults to a known-good release.",
								Optional:            true,
							},
						},
					},
				},
			},

			// --- manifests ---
			"manifests": schema.SingleNestedAttribute{
				MarkdownDescription: "Raw Kubernetes manifests the provider applies for you.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"management": schema.ListAttribute{
						MarkdownDescription: "Multi-document YAML strings applied to the management cluster after the providers are initialized and before the generated cluster objects: IPAM pools and their credentials, extra Secrets, anything the cluster objects reference. Applied with server-side apply on every create and update.",
						ElementType:         types.StringType,
						Optional:            true,
					},
				},
			},

			// --- inventory ---
			"inventory": schema.SingleNestedAttribute{
				MarkdownDescription: "Hardware inventory for bare-metal provisioning. With the `tinkerbell` infrastructure provider each machine becomes a Tinkerbell Hardware, a rufio Machine and a BMC credentials Secret in the cluster namespace, applied before the generated cluster objects so CAPT can claim them. The `management.bootstrap.machine` entry is marked provisioned (and excluded from PXE) with `mode = \"in_place\"`, so CAPT adopts the running node; with a pivot it is left out until the bootstrap cluster is gone.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"manage": schema.BoolAttribute{
						MarkdownDescription: "Create the Tinkerbell Hardware, rufio Machine and Secret objects for the inventory. Set `false` when those objects are managed elsewhere.",
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(true),
					},
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
										"address":  schema.StringAttribute{Required: true, MarkdownDescription: "BMC endpoint: `host`, `host:port`, or `scheme://host:port`. A port applies to Redfish and Intel AMT; a scheme applies to Intel AMT (e.g. `https://10.0.0.160:16993` for TLS-only AMT)."},
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

// providerMapAttribute builds one of the six provider maps. Every map shares
// providerNestedObject. Providers are installed once by clusterctl init, so
// any change other than resource_config recreates the cluster.
// providerMapLifecycle is how a provider map reacts to entries coming and going.
type providerMapLifecycle int

const (
	// providerMapStructural: the generated cluster objects follow the names,
	// so adding or removing one replaces the cluster.
	providerMapStructural providerMapLifecycle = iota
	// providerMapConverged: added entries are installed, removed ones deleted.
	providerMapConverged
	// providerMapPinned: there is only one provider; an entry pins its version.
	providerMapPinned
)

func providerMapAttribute(desc string, required bool, lifecycle providerMapLifecycle) schema.MapNestedAttribute {
	const owned = " Entries are owned by this resource unless `management.skip_init` is set: a changed entry (version, fetch config, customizations) is upgraded in place with `clusterctl upgrade apply`"
	var text string
	var modifiers []planmodifier.Map
	switch lifecycle {
	case providerMapStructural:
		text = owned + ". Adding or removing an entry changes the kinds of the generated cluster objects and recreates the cluster."
		modifiers = []planmodifier.Map{providerMapRequiresReplace()}
	case providerMapConverged:
		text = owned + ", an added entry is installed, and a removed entry is deleted from the management cluster (`clusterctl delete`, keeping its CRDs)."
	case providerMapPinned:
		text = owned + "; removing the entry only stops pinning the version."
	}
	return schema.MapNestedAttribute{
		MarkdownDescription: desc + " Each value is a provider object modeled after the cluster-api-operator provider CRDs (`operator.cluster.x-k8s.io/v1alpha2`); customizations are applied natively by wrapping the clusterctl repository factory, so the operator itself is not required." + text,
		Required:            required,
		Optional:            !required,
		NestedObject:        providerNestedObject(),
		PlanModifiers:       modifiers,
	}
}

// addonsAttribute is the shared shape of management.addons and
// management.bootstrap.addons: raw prerequisites, Helm releases, raw manifests.
func addonsAttribute(desc string) schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		MarkdownDescription: desc,
		Optional:            true,
		Attributes: map[string]schema.Attribute{
			"prerequisites": schema.ListAttribute{
				MarkdownDescription: "Raw YAML manifests applied after the `cni` release and before the Helm releases, for objects the releases depend on that need the CNI's CRDs, such as a Cilium load-balancer pool and L2 announcement policy for the Tinkerbell Service.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"helm": schema.ListNestedAttribute{
				MarkdownDescription: "Helm releases installed in order.",
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name":      schema.StringAttribute{MarkdownDescription: "Release name.", Required: true},
						"namespace": schema.StringAttribute{MarkdownDescription: "Release namespace (created if missing).", Required: true},
						"chart": schema.StringAttribute{
							MarkdownDescription: "`oci://` chart reference, or a chart name used with `repository`.",
							Required:            true,
						},
						"repository": schema.StringAttribute{MarkdownDescription: "HTTP chart repository URL. Ignored for `oci://` charts.", Optional: true},
						"version":    schema.StringAttribute{MarkdownDescription: "Chart version. Latest when empty.", Optional: true},
						"values":     schema.StringAttribute{MarkdownDescription: "Chart values as a YAML string.", Optional: true},
						"timeout": schema.StringAttribute{
							MarkdownDescription: "Install timeout as a Go duration.",
							Optional:            true,
							Computed:            true,
							Default:             stringdefault.StaticString("10m"),
						},
					},
				},
			},
			"manifests": schema.ListAttribute{
				MarkdownDescription: "Raw YAML manifests applied after the Helm releases.",
				ElementType:         types.StringType,
				Optional:            true,
			},
		},
	}
}

// providerMapRequiresReplace replaces the cluster when the set of provider
// names in a map changes, since the generated cluster objects depend on
// which providers exist. Changes inside an entry are applied in place: a
// resource_config change re-applies the cluster objects, anything else is a
// provider upgrade.
func providerMapRequiresReplace() planmodifier.Map {
	return mapplanmodifier.RequiresReplaceIf(
		func(ctx context.Context, req planmodifier.MapRequest, resp *mapplanmodifier.RequiresReplaceIfFuncResponse) {
			if req.PlanValue.IsUnknown() || req.StateValue.IsUnknown() {
				resp.RequiresReplace = true
				return
			}
			resp.RequiresReplace = !sameKeys(req.StateValue, req.PlanValue)
		},
		"Adding or removing a provider requires replacing the cluster.",
		"Adding or removing a provider requires replacing the cluster.",
	)
}

// sameKeys reports whether two maps have the same keys.
func sameKeys(a, b types.Map) bool {
	ea, eb := a.Elements(), b.Elements()
	if len(ea) != len(eb) {
		return false
	}
	for k := range ea {
		if _, ok := eb[k]; !ok {
			return false
		}
	}
	return true
}

// withoutResourceConfig returns the provider map with every entry's
// resource_config nulled, for change comparison.
func withoutResourceConfig(ctx context.Context, m types.Map) types.Map {
	elems := map[string]attr.Value{}
	for name, el := range m.Elements() {
		obj, ok := el.(types.Object)
		if !ok {
			elems[name] = el
			continue
		}
		attrs := maps.Clone(obj.Attributes())
		attrs["resource_config"] = types.StringNull()
		stripped, d := types.ObjectValue(obj.AttributeTypes(ctx), attrs)
		if d.HasError() {
			elems[name] = el
			continue
		}
		elems[name] = stripped
	}
	out, d := types.MapValue(m.ElementType(ctx), elems)
	if d.HasError() {
		return m
	}
	return out
}

// providerNestedObject is the provider object shared by every provider map.
func providerNestedObject() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"resource_config": schema.StringAttribute{
				MarkdownDescription: "Provider-specific settings as a YAML or JSON object (typically `yamlencode({ ... })`), deep-merged into the spec of the provider's generated cluster object (RFC 7396: nested maps merge, lists and scalars replace, `null` deletes): `infrastructure` → the `<Infra>Cluster` (or `<Infra>ClusterTemplate` with `cluster_class`), `control_plane` → the `<CP>ControlPlane` (or its template), `bootstrap` → the default worker `<Bootstrap>ConfigTemplate`. Use the CRD's camelCase field names. Not valid on `core`, `ipam` or `addon`. Changes apply in place.",
				Optional:            true,
			},
			"version": schema.StringAttribute{
				MarkdownDescription: "Release tag to install (e.g. `v0.7.9`). Omit for clusterctl's latest release.",
				Optional:            true,
			},
			"fetch_config": schema.SingleNestedAttribute{
				MarkdownDescription: "Where provider components are fetched from. Set `owner` (and `repository` for providers clusterctl does not know) to build `https://github.com/{owner}/{repository}/releases/{version}/{type}-components.yaml`; `url` and `oci` are used verbatim and exclude the other attributes.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"owner": schema.StringAttribute{
						MarkdownDescription: "GitHub owner or organization. Defaults to clusterctl's built-in repository for this provider.",
						Optional:            true,
					},
					"repository": schema.StringAttribute{
						MarkdownDescription: "GitHub repository name. Defaults to clusterctl's built-in repository for this provider; required for providers clusterctl does not know.",
						Optional:            true,
					},
					"url": schema.StringAttribute{
						MarkdownDescription: "Full clusterctl components URL (e.g. `https://github.com/{owner}/{repo}/releases/v1.0.0/infrastructure-components.yaml`).",
						Optional:            true,
					},
					"oci": schema.StringAttribute{
						MarkdownDescription: "OCI artifact reference (e.g. `oci://ghcr.io/org/provider`).",
						Optional:            true,
					},
				},
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
				Sensitive:           true,
			},
			"deployment": schema.SingleNestedAttribute{
				MarkdownDescription: "Deployment customization for the provider controller.",
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
				MarkdownDescription: "Controller manager configuration for the provider.",
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
	logger := log.New(os.Stderr, "[capi-tf] ", log.LstdFlags)
	r.manager = capi.NewManager(
		capi.WithLogger(logger),
		capi.WithAddonInstaller(talos.NewManagementAddonInstaller(logger)),
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

	// Seed any persisted provider secrets (none on a fresh create; present when a
	// tainted resource is recreated) so the bootstrapper can recognize a node it
	// already owns.
	seedSecrets, d := providerSecretsFromDynamic(ctx, data.ProviderSecrets)
	resp.Diagnostics.Append(d...)
	createOpts.ProviderSecrets = seedSecrets

	// Default kubeconfig output path
	if createOpts.KubeconfigOutputPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			createOpts.KubeconfigOutputPath = filepath.Join(home, ".kube", fmt.Sprintf("%s.kubeconfig", data.Name.ValueString()))
		}
	}

	mgr, d := r.managerFor(ctx, &data)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	createOpts.WorkloadAddons = workloadAddonsFor(ctx, &data)
	result, err := mgr.CreateCluster(ctx, *createOpts)
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

	secretsVal, d := providerSecretsToDynamic(ctx, result.ProviderSecrets)
	resp.Diagnostics.Append(d...)
	data.ProviderSecrets = secretsVal

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
	if errors.Is(err, capi.ErrClusterNotFound) {
		// The Cluster object is gone from a reachable management cluster:
		// deleted out of band. Drop it from state so the next apply recreates it.
		tflog.Warn(ctx, "Cluster no longer exists on the management cluster, removing it from state", map[string]interface{}{"error": err.Error()})
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		// An unreachable management cluster is not a deleted cluster: keep
		// the state as it is.
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
	reconcileOpts.SelfManaged = false

	// Provider lifecycle. With skip_init the providers are managed elsewhere;
	// otherwise the management cluster is converged on the provider maps,
	// but only when they changed, since clusterctl init resolves every
	// release again.
	upgrade, removed, added, d := providerChanges(ctx, &state, &plan)
	resp.Diagnostics.Append(d...)
	reconcileOpts.UpgradeProviders = upgrade
	reconcileOpts.RemovedProviders = removed
	if !added && len(upgrade) == 0 && removed.Empty() {
		reconcileOpts.SkipInit = true
	}

	// Seed the secrets persisted from create so the bootstrapper recognizes the
	// node it owns and can reach it over the Talos API.
	seedSecrets, d := providerSecretsFromDynamic(ctx, state.ProviderSecrets)
	resp.Diagnostics.Append(d...)
	reconcileOpts.ProviderSecrets = seedSecrets

	mgr, d := r.managerFor(ctx, &plan)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	reconcileOpts.WorkloadAddons = workloadAddonsFor(ctx, &plan)
	result, err := mgr.CreateCluster(ctx, *reconcileOpts)
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

	// Persist provider secrets: prefer any the bootstrapper surfaced this run,
	// otherwise carry forward what create stored.
	if len(result.ProviderSecrets) > 0 {
		secretsVal, sd := providerSecretsToDynamic(ctx, result.ProviderSecrets)
		resp.Diagnostics.Append(sd...)
		plan.ProviderSecrets = secretsVal
	} else {
		plan.ProviderSecrets = state.ProviderSecrets
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

	deleteSecrets, d := providerSecretsFromDynamic(ctx, data.ProviderSecrets)
	resp.Diagnostics.Append(d...)
	deleteOpts := capi.DeleteClusterOptions{
		Name:                 data.Name.ValueString(),
		Namespace:            namespace,
		ManagementKubeconfig: mgmtKubeconfig,
		ProviderSecrets:      deleteSecrets,
	}
	// The configuration the cluster was created with: a self-managed cluster
	// is deleted through a fresh bootstrap cluster initialised the same way.
	if spec, d := buildCreateOptions(ctx, &data); !d.HasError() {
		spec.ProviderSecrets = deleteSecrets
		deleteOpts.Spec = *spec
		deleteOpts.Wait = spec.Wait
	} else {
		resp.Diagnostics.Append(d...)
		return
	}
	if mgmt != nil {
		externalManagement := !mgmt.Kubeconfig.IsNull() && mgmt.Kubeconfig.ValueString() != ""
		// A pivot only ever happens from a bootstrap cluster of our own.
		deleteOpts.SelfManaged = mgmt.SelfManaged.ValueBool() && !externalManagement
		deleteOpts.UninstallProviders = mgmt.Uninstall.ValueBool() && !mgmt.SkipInit.ValueBool() && externalManagement
	}

	status, _ := extractStatus(ctx, &data)
	if status != nil && !status.BootstrapCluster.IsNull() {
		deleteOpts.DeleteBootstrap = true
		deleteOpts.BootstrapName = status.BootstrapCluster.ValueString()
	}

	// The default manager runs kind bootstrap clusters: it deletes a plain
	// cluster and reverse-pivots a self-managed one. A physical bootstrap
	// node needs its own bootstrapper.
	mgr := r.manager
	switch bootstrapType(ctx, &data) {
	case "talos":
		// A pivoted Talos bootstrap node was reset and released right after
		// the pivot, and CAPT may have reclaimed it into the workload cluster
		// since: never touch it on destroy. An in-place node is the cluster;
		// the whole cluster is torn down node by node through the bootstrapper.
		deleteOpts.DeleteBootstrap = false
		if bs, _ := extractManagementBootstrap(ctx, mgmt); bs != nil && bs.Mode.ValueString() == "in_place" {
			typed, d := r.managerFor(ctx, &data)
			resp.Diagnostics.Append(d...)
			if d.HasError() {
				return
			}
			mgr = typed
			deleteOpts.InPlace = true
			deleteOpts.SelfManaged = false
			deleteOpts.BootstrapMachine = bootstrapMachineHostname(ctx, &data)
			if inv, _ := extractInventory(ctx, &data); inv != nil {
				machines, d := extractInventoryMachines(ctx, inv)
				resp.Diagnostics.Append(d...)
				deleteOpts.Inventory = machines
			}
		}
	case "kairos":
		// A disposable node is powered off right after the pivot. When that
		// failed, status.bootstrap_cluster is still set: power it off now
		// through the Kairos bootstrapper, before the reverse pivot.
		deleteOpts.DeleteBootstrap = false
		if status != nil && !status.BootstrapCluster.IsNull() {
			if typed, d := r.managerFor(ctx, &data); !d.HasError() {
				if err := typed.DeleteBootstrapCluster(ctx, status.BootstrapCluster.ValueString(), deleteSecrets); err != nil {
					resp.Diagnostics.AddWarning("Bootstrap Node Warning", fmt.Sprintf("Could not power off the Kairos bootstrap node: %s", err))
				}
			} else {
				resp.Diagnostics.Append(d...)
			}
		}
	}

	if mgmtKubeconfig == "" && !deleteOpts.InPlace {
		tflog.Warn(ctx, "No management kubeconfig for deletion - manual cleanup may be needed")
		return
	}
	if err := mgr.DeleteCluster(ctx, deleteOpts); err != nil {
		// A deletion that did not finish keeps the resource: the user fixes
		// the cause and destroys again. A plain error, not a warning, or
		// Terraform would forget machines that are still provisioned.
		resp.Diagnostics.AddError("Cluster Deletion Error", fmt.Sprintf("Failed to delete cluster: %s", err))
	}
}

// ImportState imports a cluster by `[kubeconfig:][namespace/]name`: the
// name alone, `namespace/name`, or the management cluster's kubeconfig path
// in front, which lets Read fetch the cluster's status right away. The
// configuration must then set `management.kubeconfig` and `namespace` to
// the same values, since both require replacement when changed.
func (r *ClusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	kubeconfig, namespace, name := parseImportID(req.ID)
	if name == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected [kubeconfig:][namespace/]name, got %q.", req.ID))
		return
	}
	// The id is always the cluster name (see Create), and Read/Update rely on
	// the name attribute being populated, so seed it here too.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	if kubeconfig == "" && namespace == "" {
		return
	}
	mgmt, d := types.ObjectValueFrom(ctx, managementAttrTypes(), ManagementModel{
		Kubeconfig: stringOrNull(kubeconfig), Namespace: stringOrNull(namespace),
		SkipInit: types.BoolNull(), SelfManaged: types.BoolNull(), Uninstall: types.BoolNull(),
		Bootstrap: types.ObjectNull(managementBootstrapAttrTypes()), Addons: types.ObjectNull(bootstrapAddonsAttrTypes()),
	})
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("management"), mgmt)...)
}

// UpgradeState migrates v0 (flat), v1 (nested objects with provider strings)
// and v2 (provider maps with a clusterctl template) state to v3 (generated
// manifests: no template/flavor, cluster_class, network, resource_config).
// Terraform does not chain upgraders, so every entry emits v3 directly.
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
					ClusterClass:      types.BoolValue(false),
					Id:                v0.Id,
					Network:           types.ObjectNull(clusterNetworkAttrTypes()),
					Manifests:         types.ObjectNull(manifestsAttrTypes()),
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
					Bootstrap:   types.ObjectNull(managementBootstrapAttrTypes()),
					Addons:      types.ObjectNull(bootstrapAddonsAttrTypes()),
				}
				mgmtVal, d := types.ObjectValueFrom(ctx, managementAttrTypes(), mgmt)
				resp.Diagnostics.Append(d...)
				v1.Management = mgmtVal

				// Providers: legacy "name:version" strings become one-entry maps.
				v1.Core = types.MapNull(providerMapType().ElemType)
				v1.Bootstrap = types.MapNull(providerMapType().ElemType)
				v1.ControlPlane = types.MapNull(providerMapType().ElemType)
				v1.IPAM = types.MapNull(providerMapType().ElemType)
				v1.Addon = types.MapNull(providerMapType().ElemType)

				v1.Infrastructure, d = providerMapValue(ctx, v0.InfrastructureProvider.ValueString())
				resp.Diagnostics.Append(d...)
				if !v0.BootstrapProvider.IsNull() && v0.BootstrapProvider.ValueString() != "" {
					v1.Bootstrap, d = providerMapValue(ctx, v0.BootstrapProvider.ValueString())
					resp.Diagnostics.Append(d...)
				}
				if !v0.ControlPlaneProvider.IsNull() && v0.ControlPlaneProvider.ValueString() != "" {
					v1.ControlPlane, d = providerMapValue(ctx, v0.ControlPlaneProvider.ValueString())
					resp.Diagnostics.Append(d...)
				}
				if !v0.CoreProvider.IsNull() && v0.CoreProvider.ValueString() != "" {
					v1.Core, d = providerMapValue(ctx, v0.CoreProvider.ValueString())
					resp.Diagnostics.Append(d...)
				}

				// Topology: the two counts become control_plane.replicas and one
				// MachineDeployment.
				v1.Topology, d = legacyTopologyObject(ctx, v0.ControlPlaneMachineCount, v0.WorkerMachineCount)
				resp.Diagnostics.Append(d...)
				v1.CNI = types.ObjectNull(cniAttrTypes())

				v1.Inventory = types.ObjectNull(inventoryAttrTypes())

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
		// No PriorSchema for v1 and v2: the schemas were large and only a
		// handful of attributes change shape, so the upgrades rewrite raw JSON.
		1: rawJSONUpgrader(1, upgradeV1JSON),
		2: rawJSONUpgrader(2, upgradeV2JSON),
	}
}

// rawJSONUpgrader wraps a raw-JSON rewrite as a StateUpgrader.
func rawJSONUpgrader(from int, upgrade func([]byte) ([]byte, error)) resource.StateUpgrader {
	return resource.StateUpgrader{
		StateUpgrader: func(_ context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
			if req.RawState == nil || len(req.RawState.JSON) == 0 {
				resp.Diagnostics.AddError("Unable to upgrade state", fmt.Sprintf("Missing raw state for schema version %d.", from))
				return
			}
			out, err := upgrade(req.RawState.JSON)
			if err != nil {
				resp.Diagnostics.AddError("Unable to upgrade state", fmt.Sprintf("Upgrading capi_cluster state from schema version %d: %s", from, err))
				return
			}
			resp.DynamicValue = &tfprotov6.DynamicValue{JSON: out}
		},
	}
}

// --- Validation ---

// validateWorkerClasses checks that machine deployments sharing a ClusterClass
// worker class (class, or name when unset) agree on their template specs.
func validateWorkerClasses(mds []capi.MachineDeploymentTopology, diags *diag.Diagnostics) {
	first := map[string]capi.MachineDeploymentTopology{}
	for _, md := range mds {
		class := md.Class
		if class == "" {
			class = md.Name
		}
		prev, ok := first[class]
		if !ok {
			first[class] = md
			continue
		}
		if !reflect.DeepEqual(prev.Infrastructure, md.Infrastructure) || !reflect.DeepEqual(prev.Bootstrap, md.Bootstrap) {
			diags.AddError("Conflicting worker class",
				fmt.Sprintf("topology.workers.machine_deployments %q and %q share class %q but differ in infrastructure or bootstrap.", prev.Name, md.Name, class))
		}
	}
}

func (r *ClusterResource) validateLifecycleConfig(ctx context.Context, data *ClusterResourceModel, diags *diag.Diagnostics) {
	// Per-entry rules and name collection for every provider map.
	names := map[capi.ProviderType][]string{}
	for _, pm := range data.providerMaps() {
		entries, d := extractProviders(ctx, pm.Map)
		diags.Append(d...)
		for _, name := range sortedProviderNames(entries) {
			names[pm.Type] = append(names[pm.Type], name)
			validateProviderEntry(ctx, pm.Type, name, entries[name], diags)
		}
	}
	if diags.HasError() || data.Infrastructure.IsUnknown() {
		return
	}

	infraNames := names[capi.ProviderTypeInfrastructure]
	if len(infraNames) != 1 {
		diags.AddError("Invalid infrastructure configuration",
			fmt.Sprintf("infrastructure must contain exactly one provider, got %d.", len(infraNames)))
		return
	}
	provider := strings.ToLower(infraNames[0])

	supportedProviders := map[string]struct{}{
		"aws": {}, "azure": {}, "docker": {}, "openstack": {}, "tinkerbell": {}, "vsphere": {},
	}
	if _, ok := supportedProviders[provider]; !ok {
		diags.AddError(
			"Unsupported infrastructure provider",
			fmt.Sprintf("infrastructure provider %q is not supported. Supported: aws, azure, docker, openstack, tinkerbell, vsphere", provider),
		)
	}

	if provider == "tinkerbell" {
		mgmt, d := extractManagement(ctx, data)
		diags.Append(d...)
		if mgmt == nil || mgmt.SelfManaged.IsNull() || !mgmt.SelfManaged.ValueBool() {
			diags.AddError("Invalid Tinkerbell configuration", "Tinkerbell clusters must have management.self_managed = true.")
		}
		for _, n := range names[capi.ProviderTypeBootstrap] {
			if l := strings.ToLower(n); l != "kubeadm" && l != "talos" {
				diags.AddError("Invalid bootstrap provider for Tinkerbell", fmt.Sprintf("Tinkerbell supports bootstrap providers kubeadm or talos, got %q.", n))
			}
		}
		for _, n := range names[capi.ProviderTypeControlPlane] {
			if l := strings.ToLower(n); l != "kubeadm" && l != "talos" {
				diags.AddError("Invalid control plane provider for Tinkerbell", fmt.Sprintf("Tinkerbell supports control_plane providers kubeadm or talos, got %q.", n))
			}
		}
	}

	// Topology: unique machine deployment names; counts feed inventory validation.
	cpTopo, mds, d := extractTopology(ctx, data)
	diags.Append(d...)
	seen := map[string]bool{}
	var workerCount int64
	for _, md := range mds {
		if seen[md.Name] {
			diags.AddError("Duplicate machine deployment", fmt.Sprintf("topology.workers.machine_deployments name %q appears more than once.", md.Name))
		}
		seen[md.Name] = true
		if md.Replicas != nil {
			workerCount += *md.Replicas
		}
	}
	if data.ClusterClass.ValueBool() {
		validateWorkerClasses(mds, diags)
	}

	// Validate inventory
	inv, d := extractInventory(ctx, data)
	diags.Append(d...)
	if inv != nil {
		var cp int64
		if cpTopo.Replicas != nil {
			cp = *cpTopo.Replicas
		}
		validateInventory(ctx, inv, cp, workerCount, diags)
	}

	// CNI is wired through the Talos machine configs (the bootstrap node's
	// and the CAPI render's) plus a Helm release, so it needs a Talos
	// cluster: a Talos bootstrap node, or a Kairos node pivoting to a Talos
	// control plane.
	if cni, d := extractCNI(ctx, data); cni != nil {
		diags.Append(d...)
		if p := strings.ToLower(cni.Provider.ValueString()); p != "cilium" {
			diags.AddError("Invalid cni configuration", fmt.Sprintf("cni.provider %q is not supported; only \"cilium\".", cni.Provider.ValueString()))
		}
		switch bootstrapType(ctx, data) {
		case "talos":
		case "kairos":
			if _, ok := controlPlaneStrategicPatches(ctx, data, diags); !ok {
				diags.AddError("Invalid cni configuration", "cni with management.bootstrap.type = \"kairos\" requires a control_plane[\"talos\"] entry: the CNI's Talos patch goes into the machine configs CAPI renders.")
			}
		default:
			diags.AddError("Invalid cni configuration", "cni requires management.bootstrap.type = \"talos\" or \"kairos\".")
		}
	}

	// Validate bootstrap cluster configuration and the management addons
	validateManagementBootstrap(ctx, data, diags)
	if mgmt, d := extractManagement(ctx, data); mgmt != nil {
		diags.Append(d...)
		ad, d := extractManagementAddons(ctx, mgmt)
		diags.Append(d...)
		validateAddonsModel(ctx, "management.addons", ad, diags)
	}
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
			Uninstall:   types.BoolValue(false),
			Namespace:   types.StringValue(namespace),
			Bootstrap:   types.ObjectNull(managementBootstrapAttrTypes()),
			Addons:      types.ObjectNull(bootstrapAddonsAttrTypes()),
		}
	} else {
		if mgmt.Namespace.IsNull() || mgmt.Namespace.ValueString() == "" {
			mgmt.Namespace = types.StringValue(namespace)
		}
		if mgmt.Uninstall.IsNull() || mgmt.Uninstall.IsUnknown() {
			mgmt.Uninstall = types.BoolValue(false)
		}
		if mgmt.Bootstrap.IsUnknown() {
			mgmt.Bootstrap = types.ObjectNull(managementBootstrapAttrTypes())
		}
		if mgmt.Addons.IsUnknown() {
			mgmt.Addons = types.ObjectNull(bootstrapAddonsAttrTypes())
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

// validateManagementBootstrap checks management.bootstrap when type = "talos".
func validateManagementBootstrap(ctx context.Context, data *ClusterResourceModel, diags *diag.Diagnostics) {
	const summary = "Invalid bootstrap configuration"

	mgmt, d := extractManagement(ctx, data)
	diags.Append(d...)
	bs, d := extractManagementBootstrap(ctx, mgmt)
	diags.Append(d...)
	if bs == nil {
		return
	}

	typ := "kind"
	if !bs.Type.IsNull() && !bs.Type.IsUnknown() {
		typ = bs.Type.ValueString()
	}
	if !bs.Mode.IsNull() && !bs.Mode.IsUnknown() {
		switch bs.Mode.ValueString() {
		case "", "pivot":
		case "in_place":
			if typ != "talos" {
				diags.AddError(summary, "management.bootstrap.mode = \"in_place\" requires type = \"talos\": only a Talos bootstrap node can be adopted as a cluster member.")
			}
		default:
			diags.AddError(summary, fmt.Sprintf("management.bootstrap.mode %q is not supported. Supported: pivot, in_place.", bs.Mode.ValueString()))
		}
	}
	switch typ {
	case "kind":
		return
	case "talos", "kairos":
	default:
		diags.AddError(summary, fmt.Sprintf("management.bootstrap.type %q is not supported. Supported: kind, talos, kairos.", typ))
		return
	}

	if bs.Machine.IsNull() || bs.Machine.ValueString() == "" {
		diags.AddError(summary, fmt.Sprintf("management.bootstrap.machine is required when type = %q.", typ))
	} else {
		m, d := findInventoryMachine(ctx, data, bs.Machine.ValueString())
		diags.Append(d...)
		if m == nil {
			diags.AddError(summary, fmt.Sprintf("management.bootstrap.machine %q does not match any inventory.machine hostname.", bs.Machine.ValueString()))
		} else {
			if m.BMC.IsNull() || m.BMC.IsUnknown() {
				diags.AddError(summary, fmt.Sprintf("inventory machine %q must define bmc to be used as the bootstrap node.", bs.Machine.ValueString()))
			}
			// A Kairos live node writes nothing to disk.
			if typ == "talos" && (m.Disk.IsNull() || m.Disk.IsUnknown()) {
				diags.AddError(summary, fmt.Sprintf("inventory machine %q must define disk.device to be used as the bootstrap node.", bs.Machine.ValueString()))
			}
			var n NetworkModel
			diags.Append(m.Network.As(ctx, &n, basetypes.ObjectAsOptions{})...)
			if n.IPAddress.IsNull() || n.IPAddress.ValueString() == "" {
				diags.AddError(summary, fmt.Sprintf("inventory machine %q must define network.ip_address to be used as the bootstrap node.", bs.Machine.ValueString()))
			}
		}
	}

	boot, d := extractBoot(ctx, bs)
	diags.Append(d...)
	if boot != nil {
		if !boot.Method.IsNull() && !boot.Method.IsUnknown() {
			switch boot.Method.ValueString() {
			case "auto", "virtual_media":
			case "http":
				if typ == "kairos" {
					diags.AddError(summary, "management.bootstrap.boot.method \"http\" boots a Talos UKI over UEFI HTTP boot; a Kairos live ISO boots from virtual media (auto or virtual_media).")
				}
			default:
				diags.AddError(summary, fmt.Sprintf("management.bootstrap.boot.method %q is not supported. Supported: auto, virtual_media, http.", boot.Method.ValueString()))
			}
		}
		if !boot.Timeout.IsNull() && !boot.Timeout.IsUnknown() {
			if _, err := time.ParseDuration(boot.Timeout.ValueString()); err != nil {
				diags.AddError(summary, fmt.Sprintf("management.bootstrap.boot.timeout %q is not a valid duration: %v", boot.Timeout.ValueString(), err))
			}
		}
		if !boot.Attempts.IsNull() && !boot.Attempts.IsUnknown() && boot.Attempts.ValueInt64() < 1 {
			diags.AddError(summary, "management.bootstrap.boot.attempts must be at least 1.")
		}
	}

	tm, d := extractTalos(ctx, bs)
	diags.Append(d...)
	if typ == "kairos" {
		if tm != nil {
			diags.AddError(summary, "management.bootstrap.talos applies to type = \"talos\"; a Kairos node is configured by management.bootstrap.kairos.")
		}
		validateKairosBootstrap(ctx, data, bs, diags)
		ad, d := extractBootstrapAddons(ctx, bs)
		diags.Append(d...)
		validateAddonsModel(ctx, "management.bootstrap.addons", ad, diags)
		return
	}
	if tm == nil || tm.Version.IsNull() || tm.Version.ValueString() == "" {
		diags.AddError(summary, "management.bootstrap.talos.version is required when type = \"talos\".")
	}
	if tm != nil {
		if !tm.Architecture.IsNull() && !tm.Architecture.IsUnknown() {
			switch tm.Architecture.ValueString() {
			case "amd64", "arm64":
			default:
				diags.AddError(summary, fmt.Sprintf("management.bootstrap.talos.architecture %q is not supported. Supported: amd64, arm64.", tm.Architecture.ValueString()))
			}
		}

		img, d := extractTalosImage(ctx, tm)
		diags.Append(d...)
		if img != nil {
			set := func(s types.String) bool { return !s.IsNull() && !s.IsUnknown() && s.ValueString() != "" }
			nonEmpty := func(l types.List) bool { return !l.IsNull() && !l.IsUnknown() && len(l.Elements()) > 0 }
			hasISO, hasInstaller := set(img.ISO), set(img.Installer)
			hasSchematic := set(img.Schematic)
			hasCustom := nonEmpty(img.Extensions) || nonEmpty(img.KernelArgs)
			if hasISO != hasInstaller {
				diags.AddError(summary, "management.bootstrap.talos.image.iso and installer must be set together.")
			}
			if hasISO && (hasSchematic || hasCustom) {
				diags.AddError(summary, "management.bootstrap.talos.image.iso/installer conflict with schematic, extensions, and kernel_args.")
			}
			if hasSchematic && hasCustom {
				diags.AddError(summary, "management.bootstrap.talos.image.schematic conflicts with extensions and kernel_args.")
			}
		}

		patches, d := stringList(ctx, tm.ConfigPatches)
		diags.Append(d...)
		for i, p := range patches {
			var v any
			if err := yaml.Unmarshal([]byte(p), &v); err != nil {
				diags.AddError(summary, fmt.Sprintf("management.bootstrap.talos.config_patches[%d] is not valid YAML: %v", i, err))
			}
		}
	}

	ad, d := extractBootstrapAddons(ctx, bs)
	diags.Append(d...)
	validateAddonsModel(ctx, "management.bootstrap.addons", ad, diags)
}

// validateKairosBootstrap checks management.bootstrap.kairos and what a
// disposable node requires of the rest of the configuration: the live node is
// powered off after the pivot, so there must be one.
func validateKairosBootstrap(ctx context.Context, data *ClusterResourceModel, bs *ManagementBootstrapModel, diags *diag.Diagnostics) {
	const summary = "Invalid bootstrap configuration"
	mgmt, d := extractManagement(ctx, data)
	diags.Append(d...)
	if mgmt == nil || (!mgmt.SelfManaged.IsUnknown() && !mgmt.SelfManaged.ValueBool()) {
		diags.AddError(summary, "management.bootstrap.type = \"kairos\" requires management.self_managed = true: the live node is disposable, so CAPI must pivot to the workload cluster before it is powered off.")
	}
	k, d := extractKairos(ctx, bs)
	diags.Append(d...)
	if k == nil {
		diags.AddError(summary, "management.bootstrap.kairos is required when type = \"kairos\".")
		return
	}
	if !k.ISO.IsUnknown() && k.ISO.ValueString() == "" {
		diags.AddError(summary, "management.bootstrap.kairos.iso is required.")
	}
	s, d := extractKairosSSH(ctx, k)
	diags.Append(d...)
	if s == nil {
		diags.AddError(summary, "management.bootstrap.kairos.ssh is required.")
		return
	}
	if !s.PrivateKey.IsUnknown() && s.PrivateKey.ValueString() == "" {
		diags.AddError(summary, "management.bootstrap.kairos.ssh.private_key is required.")
	}
	if !s.Port.IsNull() && !s.Port.IsUnknown() && (s.Port.ValueInt64() < 1 || s.Port.ValueInt64() > 65535) {
		diags.AddError(summary, "management.bootstrap.kairos.ssh.port must be between 1 and 65535.")
	}
}

// validateAddonsModel checks an addons block: unique releases, parseable
// timeouts, YAML values and manifests.
func validateAddonsModel(ctx context.Context, where string, ad *BootstrapAddonsModel, diags *diag.Diagnostics) {
	const summary = "Invalid addons configuration"
	if ad == nil {
		return
	}
	var rels []HelmReleaseModel
	if !ad.Helm.IsNull() && !ad.Helm.IsUnknown() {
		diags.Append(ad.Helm.ElementsAs(ctx, &rels, false)...)
	}
	seen := map[string]bool{}
	for i, rel := range rels {
		key := rel.Namespace.ValueString() + "/" + rel.Name.ValueString()
		if seen[key] {
			diags.AddError(summary, fmt.Sprintf("%s.helm[%d] is a duplicate release %q.", where, i, key))
		}
		seen[key] = true
		if !rel.Timeout.IsNull() && !rel.Timeout.IsUnknown() && rel.Timeout.ValueString() != "" {
			if _, err := time.ParseDuration(rel.Timeout.ValueString()); err != nil {
				diags.AddError(summary, fmt.Sprintf("%s.helm[%d].timeout %q is not a valid duration: %v", where, i, rel.Timeout.ValueString(), err))
			}
		}
		if !rel.Values.IsNull() && !rel.Values.IsUnknown() && rel.Values.ValueString() != "" {
			var v any
			if err := yaml.Unmarshal([]byte(rel.Values.ValueString()), &v); err != nil {
				diags.AddError(summary, fmt.Sprintf("%s.helm[%d].values is not valid YAML: %v", where, i, err))
			}
		}
	}
	for _, list := range []struct {
		name string
		l    types.List
	}{{"prerequisites", ad.Prerequisites}, {"manifests", ad.Manifests}} {
		docs, d := stringList(ctx, list.l)
		diags.Append(d...)
		for i, doc := range docs {
			var v any
			if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
				diags.AddError(summary, fmt.Sprintf("%s.%s[%d] is not valid YAML: %v", where, list.name, i, err))
			}
		}
	}
}
