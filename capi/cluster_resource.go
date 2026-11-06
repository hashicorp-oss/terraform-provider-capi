// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/dynamicplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/tinkerbell-community/terraform-provider-capi/capi/validators"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/addons"
	"sigs.k8s.io/yaml"
)

// Ensure provider defined types fully satisfy framework interfaces.
// Ensure provider defined types fully satisfy framework interfaces.
var (
	_ resource.Resource                   = &clusterResource{}
	_ resource.ResourceWithIdentity       = &clusterResource{}
	_ resource.ResourceWithImportState    = &clusterResource{}
	_ resource.ResourceWithUpgradeState   = &clusterResource{}
	_ resource.ResourceWithModifyPlan     = &clusterResource{}
	_ resource.ResourceWithValidateConfig = &clusterResource{}
)

func NewClusterResource() resource.Resource {
	return &clusterResource{}
}

// clusterResource defines the resource implementation.
type clusterResource struct {
	pd      *providerData
	manager *capi.Manager
}

func (r *clusterResource) Metadata(
	ctx context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

// IdentitySchema implements [resource.ResourceWithIdentity].
func (r *clusterResource) IdentitySchema(
	_ context.Context,
	_ resource.IdentitySchemaRequest,
	resp *resource.IdentitySchemaResponse,
) {
	resp.IdentitySchema = namespacedIdentitySchema()
}

// Defaults the schema applies.
const (
	defaultWaitCondition = "Available"
	onFailureResume      = "resume"
	onFailureFail        = "fail"
)

func (r *clusterResource) Schema(
	ctx context.Context,
	req resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Version:             5,
		MarkdownDescription: "A Cluster API cluster, the way `aws_eks_cluster` is an EKS cluster: identity, version, network, the control plane, and, for a cluster that manages itself, the create-only bootstrap that stands it up. On the management cluster the provider block names, create applies the `Cluster`, the `<Infra>Cluster` and the `<CP>ControlPlane` (or a `Cluster` with a topology on `class`) and waits. With `management.self_managed`, create runs the pivot: a bootstrap cluster (kind on the runner, or a disposable Kairos live node), `management.providers`, the seed `inventory` and `manifests.management`, the first control plane, the Tinkerbell handover, `clusterctl move`, and the bootstrap's teardown; progress is recorded in `status.phase` (and, in finer detail, in private state) and a failed run resumes from it, skipping what must not run twice. Node groups are `capi_machine_deployment`, templates `capi_machine_template`, addons `capi_helm_chart_proxy` and `capi_cluster_resource_set`, machines `capi_hardware`. Import with `[namespace/]name`.",

		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				MarkdownDescription: "The name of the cluster. Must be a valid DNS-1123 subdomain.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"namespace": schema.StringAttribute{
				MarkdownDescription: "Namespace on the management cluster where the cluster's objects live. Default `default`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("default"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"kubernetes_version": schema.StringAttribute{
				MarkdownDescription: "Kubernetes version of the cluster (`v1.34.0`): the control plane's `version`, or `spec.topology.version` with a class, where it is required. Changed in place.",
				Optional:            true,
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
								Validators: []validator.List{
									listvalidator.ValueStringsAre(validators.CIDRValidator()),
								},
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
								Validators: []validator.List{
									listvalidator.ValueStringsAre(validators.CIDRValidator()),
								},
							},
						},
					},
					"service_domain": schema.StringAttribute{
						MarkdownDescription: "Cluster DNS domain (`clusterNetwork.serviceDomain`).",
						Optional:            true,
					},
				},
			},

			// --- direct mode: the infrastructure object and the control plane ---
			"infrastructure": schema.SingleNestedAttribute{
				MarkdownDescription: "The cluster's `<Infra>Cluster` (direct mode; exclusive with `class`). The kind comes from the provider's CRDs.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"provider": schema.StringAttribute{
						MarkdownDescription: "Infrastructure provider name as clusterctl knows it (`tinkerbell`, `docker`). Changing it replaces the cluster.",
						Required:            true,
						PlanModifiers: []planmodifier.String{
							stringplanmodifier.RequiresReplace(),
						},
					},
					"spec": schema.DynamicAttribute{
						MarkdownDescription: "The `<Infra>Cluster` spec as an object with the CRD's camelCase keys (Tinkerbell `templateInline`), merged over the provider's defaults and, for a self-managed cluster, over `management.providers.infrastructure.<name>.resource_config` (RFC 7396: nested maps merge, lists and scalars replace, `null` deletes). Applied in place.",
						Optional:            true,
					},
				},
			},
			"control_plane": schema.SingleNestedAttribute{
				MarkdownDescription: "The control plane. With a `class` only `replicas` applies; otherwise `provider` and `infrastructure` are required and the `<CP>ControlPlane` is rendered from them.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"provider": schema.StringAttribute{
						MarkdownDescription: "Control-plane provider name (`talos`, `kubeadm`). Changing it replaces the cluster.",
						Optional:            true,
						PlanModifiers: []planmodifier.String{
							stringplanmodifier.RequiresReplace(),
						},
					},
					"replicas": schema.Int64Attribute{
						MarkdownDescription: "Number of control plane machines. Use an odd number for HA (1, 3, 5). Scaled in place.",
						Optional:            true,
					},
					"infrastructure": templateRefOptional(
						"The control-plane machines' `<Infra>MachineTemplate`, as a `capi_machine_template` ref. A new revision rolls the control plane. Exclusive with `machine_spec`.",
					),
					"machine_spec": schema.DynamicAttribute{
						MarkdownDescription: "The control-plane `<Infra>MachineTemplate` spec (Tinkerbell `hardwareAffinity`) as an object with the CRD's camelCase keys, for a cluster that renders its own template: a self-managed cluster, whose CRDs exist only once it is bootstrapped, so a `capi_machine_template` cannot precede it. The template is named `<name>-control-plane-<hash of the spec>`; a change is a new template and a control-plane rollout. Exclusive with `infrastructure`.",
						Optional:            true,
					},
					"spec": schema.DynamicAttribute{
						MarkdownDescription: "The `<CP>ControlPlane` spec as an object with the CRD's camelCase keys (Talos `controlPlaneConfig`, kubeadm `kubeadmConfigSpec`), merged over the provider's defaults and `management.providers.control_plane.<name>.resource_config`. `version`, `replicas` and the machine template ref are set by the provider. Applied in place.",
						Optional:            true,
					},
				},
			},
			"class": schema.SingleNestedAttribute{
				MarkdownDescription: "Topology mode: the ClusterClass the cluster instantiates, as a `capi_cluster_class` ref (exclusive with `infrastructure`). `kubernetes_version` is required. Switching between a class and a direct control plane replaces the cluster.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{
						MarkdownDescription: "ClusterClass name.",
						Required:            true,
					},
					"namespace": schema.StringAttribute{
						MarkdownDescription: "ClusterClass namespace, when not the cluster's.",
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
				MarkdownDescription: "The root credential of a self-managed cluster, captured after the pivot (the Talos machine-secrets bundle, or the kubeadm CA): what later runs derive their connection from, and what `ephemeral.capi_kubeconfig` takes as `secrets`. Computed and sensitive; never set by practitioners.",
				Computed:            true,
				Sensitive:           true,
				PlanModifiers: []planmodifier.Dynamic{
					dynamicplanmodifier.UseNonNullStateForUnknown(),
				},
			},

			// --- management ---
			"management": schema.SingleNestedAttribute{
				MarkdownDescription: "How the cluster is managed. Leave it out for a cluster on the management cluster the provider block names (its providers are `capi_provider` resources). Set `self_managed` for a cluster that becomes its own management cluster: everything under `bootstrap` and `providers` is used at create and never diffed afterwards.",
				Optional:            true,
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"self_managed": schema.BoolAttribute{
						MarkdownDescription: "Stand the cluster up through a bootstrap cluster and pivot CAPI management into it (`clusterctl move`). Required for Tinkerbell. Immutable.",
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(false),
						PlanModifiers: []planmodifier.Bool{
							boolplanmodifier.RequiresReplace(),
						},
					},
					"providers": schema.SingleNestedAttribute{
						MarkdownDescription: "The CAPI providers of a self-managed cluster, installed on the bootstrap cluster and again on the cluster before the move (`clusterctl init`), keyed by provider name per type. Changes after create are applied in place (`clusterctl upgrade apply`, init, delete). On an external management cluster use `capi_provider` instead.",
						Optional:            true,
						Attributes: map[string]schema.Attribute{
							"core": providerMapAttribute(
								"Core CAPI provider, keyed by name (`cluster-api`). Omit to let clusterctl install the default.",
								providerMapPinned,
							),
							"infrastructure": providerMapAttribute(
								"Infrastructure provider, keyed by name (`docker`, `tinkerbell`). Exactly one entry; it is the cluster's `infrastructure.provider` by default.",
								providerMapStructural,
							),
							"bootstrap": providerMapAttribute(
								"Bootstrap providers, keyed by name (`kubeadm`, `talos`).",
								providerMapStructural,
							),
							"control_plane": providerMapAttribute(
								"Control plane providers, keyed by name (`kubeadm`, `talos`).",
								providerMapStructural,
							),
							"ipam": providerMapAttribute(
								"IPAM providers, keyed by name (`in-cluster`, `unifi`).",
								providerMapConverged,
							),
							"addon": providerMapAttribute(
								"Addon providers, keyed by name (`helm`). An empty object installs the default release.",
								providerMapConverged,
							),
						},
					},

					"bootstrap": schema.SingleNestedAttribute{
						MarkdownDescription: "Transient bootstrap cluster configuration, used once at create. `type = \"kind\"` (default) creates a kind cluster on the machine running Terraform. `type = \"kairos\"` boots one `inventory.machine` entry from a user-built Kairos live ISO that runs k3s from RAM (see `kairos`); nothing is written to its disk, and after the self-managed pivot it is powered off and provisioned by CAPT as an ordinary cluster member. Beyond `status.bootstrap_cluster` while it is in use, only the progress record in private state remembers the bootstrap cluster (its type, name and, for kind, the machine it runs on).",
						Optional:            true,
						PlanModifiers: []planmodifier.Object{
							objectplanmodifier.RequiresReplace(),
						},
						Attributes: map[string]schema.Attribute{
							"type": schema.StringAttribute{
								MarkdownDescription: "Bootstrap cluster type: `kind` or `kairos`.",
								Optional:            true,
								Computed:            true,
								Default:             stringdefault.StaticString("kind"),
								Validators: []validator.String{
									stringvalidator.OneOf("kind", "kairos"),
								},
							},
							"machine": schema.StringAttribute{
								MarkdownDescription: "Hostname of the `inventory.machine` entry to boot the live image on. Its `bmc` and `network.ip_address` are used; a live node writes nothing to disk and needs no `disk`. Required when `type = \"kairos\"`.",
								Optional:            true,
							},
							"on_failure": schema.StringAttribute{
								MarkdownDescription: "What a failed create does. `resume` (default): the resource is saved with the phase reached in `status.phase` and the error in `status.error`, nothing is torn down, and the next apply continues from there; `destroy` tears everything down at any phase. `fail`: the apply fails, Terraform taints the resource, and the next apply replaces it (destroy, then create from scratch). `resume` is for an operator at the keyboard with hardware that takes long to come up; `fail` is for CI. Once CAPI management has moved to the cluster, a failure is always kept for the resume whatever this says: replacing the cluster then would provision its machines a second time. A resume or destroy that needs a kind bootstrap cluster stops at plan time when run on another machine than the one it lives on.",
								Optional:            true,
								Computed:            true,
								Default:             stringdefault.StaticString(onFailureResume),
								Validators: []validator.String{
									stringvalidator.OneOf(onFailureResume, onFailureFail),
								},
							},
							"boot": schema.SingleNestedAttribute{
								MarkdownDescription: "How the node is booted into the live image.",
								Optional:            true,
								Attributes: map[string]schema.Attribute{
									"method": schema.StringAttribute{
										MarkdownDescription: "`auto` or `virtual_media` attach the ISO through the BMC (Intel AMT storage redirection, or bmclib virtual media plus a one-shot boot override) and power the node on. `manual` drives no BMC: the node is booted by hand (a USB stick and a power button) and only waited for, and powered off by hand after the pivot.",
										Optional:            true,
										Computed:            true,
										Default:             stringdefault.StaticString("auto"),
										Validators: []validator.String{
											stringvalidator.OneOf(
												"auto",
												"virtual_media",
												"manual",
											),
										},
									},
									"timeout": schema.StringAttribute{
										MarkdownDescription: "Per-attempt wait for the node to answer over SSH after a boot, as a Go duration (e.g. `15m`).",
										Optional:            true,
										Computed:            true,
										Default:             stringdefault.StaticString("15m"),
										Validators: []validator.String{
											validators.DurationValidator(),
										},
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
							"kairos": schema.SingleNestedAttribute{
								MarkdownDescription: "Disposable live node for `type = \"kairos\"`: a user-built Kairos ISO (AuroraBoot's `kairos.iso.custom.iso` with the cloud-config embedded: `k3s.enabled`, the SSH key, the node's static address, `kairos.ram` on the live command line) booted from virtual media through the machine's BMC, or by hand with `boot.method = \"manual\"`. The provider reads the k3s kubeconfig over SSH, uses the node as the management cluster until the pivot, then ejects the media and powers it off. Requires `management.self_managed = true`.",
								Optional:            true,
								Attributes: map[string]schema.Attribute{
									"iso": schema.StringAttribute{
										MarkdownDescription: "The live image: an `http(s)` URL the BMC fetches as virtual media, or with Intel AMT storage redirection also a local path. Informational with `boot.method = \"manual\"`.",
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
												Default: stringdefault.StaticString(
													"kairos",
												),
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
							"addons": addonsAttribute(
								"The bootstrap cluster's own stack, installed right after its CAPI providers are initialized (`prerequisites`, `helm`, `manifests`, in that order) and uninstalled before management moves to the workload cluster: a Tinkerbell on host networking bound to the node's address, so the first control-plane machine can netboot. The workload cluster's own addons (its CNI, its Tinkerbell) are `HelmChartProxy` and `ClusterResourceSet` objects declared in `manifests.management`; CAAPH installs them once the control plane is initialized and they move with the cluster.",
							),
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
						MarkdownDescription: "Multi-document YAML strings applied to the management cluster before the cluster objects: IPAM pools and their credentials, extra Secrets, anything the cluster objects reference. Applied with server-side apply on every create and update, and again on the workload cluster after a self-managed pivot. With a pivot this is also where the workload cluster's addons go, as CAAPH `HelmChartProxy` and `ClusterResourceSet` objects (the `helm` addon provider installs CAAPH): CAAPH brings them up on the workload cluster as soon as its control plane is initialized, and the pivot waits for them before management moves. On an external management cluster, prefer `capi_helm_chart_proxy` and `capi_cluster_resource_set`.",
						ElementType:         types.StringType,
						Optional:            true,
					},
				},
			},

			// --- inventory ---
			"inventory": schema.SingleNestedAttribute{
				MarkdownDescription: "The seed hardware inventory of a self-managed Tinkerbell cluster: each machine becomes a Tinkerbell Hardware, a rufio Machine and a BMC credentials Secret in the cluster namespace, applied to the bootstrap cluster before the cluster objects and again to the cluster after the pivot. The `management.bootstrap.machine` entry is left out while it hosts the bootstrap cluster and registered as a netboot target after the pivot. Machines added later are `capi_hardware` resources.",
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
										"ip_address": schema.StringAttribute{
											Required:            true,
											MarkdownDescription: "Primary IP address.",
										},
										"netmask": schema.StringAttribute{
											Required:            true,
											MarkdownDescription: "Network mask.",
										},
										"gateway": schema.StringAttribute{
											Required:            true,
											MarkdownDescription: "Default gateway.",
										},
										"mac_address": schema.StringAttribute{
											Required:            true,
											MarkdownDescription: "Primary NIC MAC address.",
										},
										"nameservers": schema.ListAttribute{
											Optional:            true,
											ElementType:         types.StringType,
											MarkdownDescription: "DNS nameservers.",
										},
										"vlan_id": schema.StringAttribute{
											Optional:            true,
											MarkdownDescription: "VLAN ID.",
										},
									},
								},
								"disk": schema.SingleNestedAttribute{
									MarkdownDescription: "Boot disk configuration.",
									Optional:            true,
									Attributes: map[string]schema.Attribute{
										"device": schema.StringAttribute{
											Required:            true,
											MarkdownDescription: "Disk device path.",
										},
									},
								},
								"bmc": schema.SingleNestedAttribute{
									MarkdownDescription: "BMC configuration.",
									Optional:            true,
									Attributes: map[string]schema.Attribute{
										"address": schema.StringAttribute{
											Required:            true,
											MarkdownDescription: "BMC endpoint: `host`, `host:port`, or `scheme://host:port`. A port applies to Redfish and Intel AMT; a scheme applies to Intel AMT (e.g. `https://10.0.0.160:16993` for TLS-only AMT).",
										},
										"username": schema.StringAttribute{
											Required:            true,
											MarkdownDescription: "BMC username.",
										},
										"password": schema.StringAttribute{
											Required:            true,
											Sensitive:           true,
											MarkdownDescription: "BMC password.",
										},
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
			"timeouts": timeouts.Attributes(
				ctx,
				timeouts.Opts{Create: true, Update: true, Delete: true},
			),
			"wait": schema.SingleNestedAttribute{
				MarkdownDescription: "What to wait for after an apply, within the operation's `timeouts` (create `2h`, update `60m`, delete `2h` by default).",
				Optional:            true,
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"conditions": schema.ListAttribute{
						MarkdownDescription: "Cluster conditions that must be True (`Available` is `Ready` on the legacy contract; `ControlPlaneInitialized`, `InfrastructureReady`, ...). Default `[\"Available\"]`; an empty list waits for nothing. A self-managed create always waits for the control plane before the pivot.",
						ElementType:         types.StringType,
						Optional:            true,
						Computed:            true,
						Default: listdefault.StaticValue(
							types.ListValueMust(
								types.StringType,
								[]attr.Value{types.StringValue(defaultWaitCondition)},
							),
						),
					},
				},
			},

			// --- status ---
			"status": schema.SingleNestedAttribute{
				MarkdownDescription: "Computed cluster status.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"phase": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "How far the create workflow came: `bootstrapping`, `initialized`, `control_plane_initialized`, `paused`, `handed_over`, `moved` (the cluster holds its own CAPI objects; the bootstrap is still to be torn down), `pivoted`, `ready`. Anything but `ready` is resumed by the next apply. The finer progress a resume needs (which steps must not run twice, the bootstrap cluster's identity and the machine a kind bootstrap runs on, a kubeconfig for the cluster until its root credential is captured) is kept in the resource's private state, never shown in plans.",
					},
					"error": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "The error that stopped the last create, when `status.phase` is not `ready`.",
					},
					"endpoint": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "API server URL.",
					},
					"ca_certificate": schema.StringAttribute{
						Computed:            true,
						Sensitive:           true,
						MarkdownDescription: "CA certificate (PEM).",
					},
					"description": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "The cluster's object tree with conditions, as `clusterctl describe` renders it.",
					},
					"bootstrap_cluster": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "Name of the bootstrap cluster while it is in use (a self-managed create that has not pivoted yet).",
					},
					"conditions": schema.ListNestedAttribute{
						MarkdownDescription: "The Cluster's conditions.",
						Computed:            true,
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"type":    schema.StringAttribute{Computed: true},
								"status":  schema.StringAttribute{Computed: true},
								"reason":  schema.StringAttribute{Computed: true},
								"message": schema.StringAttribute{Computed: true},
							},
						},
					},
				},
			},
		},
	}
}

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

// providerMapAttribute builds one of the six provider maps. Every map shares
// providerNestedObject.
func providerMapAttribute(desc string, lifecycle providerMapLifecycle) schema.MapNestedAttribute {
	const owned = " Entries are owned by this resource: a changed entry (version, fetch config, customizations) is upgraded in place with `clusterctl upgrade apply`"
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
		Optional:            true,
		NestedObject:        providerNestedObject(),
		PlanModifiers:       modifiers,
	}
}

// addonsAttribute is the shape of management.bootstrap.addons: raw
// prerequisites, Helm releases, raw manifests.
func addonsAttribute(desc string) schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		MarkdownDescription: desc,
		Optional:            true,
		Attributes: map[string]schema.Attribute{
			"prerequisites": schema.ListAttribute{
				MarkdownDescription: "Raw YAML manifests applied before the Helm releases, for objects the releases depend on.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"helm": schema.ListNestedAttribute{
				MarkdownDescription: "Helm releases installed in order.",
				Optional:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							MarkdownDescription: "Release name.",
							Required:            true,
						},
						"namespace": schema.StringAttribute{
							MarkdownDescription: "Release namespace (created if missing).",
							Required:            true,
						},
						"chart": schema.StringAttribute{
							MarkdownDescription: "`oci://` chart reference, or a chart name used with `repository`.",
							Required:            true,
						},
						"repository": schema.StringAttribute{
							MarkdownDescription: "HTTP chart repository URL. Ignored for `oci://` charts.",
							Optional:            true,
						},
						"version": schema.StringAttribute{
							MarkdownDescription: "Chart version. Latest when empty.",
							Optional:            true,
						},
						"values": schema.StringAttribute{
							MarkdownDescription: "Chart values as a YAML string.",
							Optional:            true,
						},
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
								"group": schema.StringAttribute{
									Optional:            true,
									MarkdownDescription: "API group of the target.",
								},
								"version": schema.StringAttribute{
									Optional:            true,
									MarkdownDescription: "API version of the target.",
								},
								"kind": schema.StringAttribute{
									Optional:            true,
									MarkdownDescription: "Kind of the target.",
								},
								"name": schema.StringAttribute{
									Optional:            true,
									MarkdownDescription: "Name of the target.",
								},
								"namespace": schema.StringAttribute{
									Optional:            true,
									MarkdownDescription: "Namespace of the target.",
								},
								"label_selector": schema.StringAttribute{
									Optional:            true,
									MarkdownDescription: "Label selector expression.",
								},
							},
						},
					},
				},
			},
		},
	}
}

func (r *clusterResource) Configure(
	ctx context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}

	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData),
		)
		return
	}

	r.pd = pd
	logger := log.New(os.Stderr, "[capi-tf] ", log.LstdFlags)
	r.manager = capi.NewManager(
		capi.WithLogger(logger),
		capi.WithAddonInstaller(addons.NewInstaller(logger)),
	)
}

// --- plan-time rules ---

// ValidateConfig refuses the combinations the schema cannot express.
func (r *clusterResource) ValidateConfig(
	ctx context.Context,
	req resource.ValidateConfigRequest,
	resp *resource.ValidateConfigResponse,
) {
	var data clusterResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hasClass := !data.Class.IsNull()
	if hasClass && !data.Infrastructure.IsNull() {
		resp.Diagnostics.AddAttributeError(
			path.Root("infrastructure"),
			"Invalid cluster configuration",
			"class and infrastructure are exclusive: a ClusterClass brings its own infrastructure template.",
		)
	}
	if hasClass && data.KubernetesVersion.IsNull() {
		resp.Diagnostics.AddAttributeError(
			path.Root("kubernetes_version"),
			"Invalid cluster configuration",
			"kubernetes_version is required with a class (Cluster.spec.topology.version).",
		)
	}
	if !hasClass && !data.Class.IsUnknown() && data.Infrastructure.IsNull() {
		resp.Diagnostics.AddAttributeError(
			path.Root("infrastructure"),
			"Invalid cluster configuration",
			"One of infrastructure (direct mode) or class (topology mode) is required.",
		)
	}
	cp, d := extractControlPlane(ctx, &data)
	resp.Diagnostics.Append(d...)
	if cp != nil {
		if hasClass {
			if !cp.Provider.IsNull() || !cp.Infrastructure.IsNull() || !cp.MachineSpec.IsNull() ||
				!cp.Spec.IsNull() {
				resp.Diagnostics.AddAttributeError(
					path.Root("control_plane"),
					"Invalid cluster configuration",
					"With a class, control_plane takes only replicas: the ClusterClass defines the control plane.",
				)
			}
		} else if !data.Class.IsUnknown() {
			if cp.Provider.IsNull() && !cp.Provider.IsUnknown() {
				resp.Diagnostics.AddAttributeError(
					path.Root("control_plane").AtName("provider"),
					"Invalid cluster configuration",
					"control_plane.provider is required without a class.",
				)
			}
			if cp.Infrastructure.IsNull() && !cp.Infrastructure.IsUnknown() &&
				cp.MachineSpec.IsNull() &&
				!cp.MachineSpec.IsUnknown() {
				resp.Diagnostics.AddAttributeError(
					path.Root("control_plane").AtName("infrastructure"),
					"Invalid cluster configuration",
					"control_plane.infrastructure (a capi_machine_template ref) or control_plane.machine_spec is required without a class.",
				)
			}
			if !cp.Infrastructure.IsNull() && !cp.MachineSpec.IsNull() {
				resp.Diagnostics.AddAttributeError(
					path.Root("control_plane").AtName("machine_spec"),
					"Invalid cluster configuration",
					"control_plane.infrastructure and control_plane.machine_spec are exclusive.",
				)
			}
		}
	} else if !hasClass && !data.Class.IsUnknown() && !data.ControlPlane.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("control_plane"),
			"Invalid cluster configuration",
			"control_plane is required without a class.",
		)
	}
	mgmt, d := extractManagement(ctx, &data)
	resp.Diagnostics.Append(d...)
	if mgmt != nil && !mgmt.Providers.IsNull() && !mgmt.SelfManaged.IsUnknown() &&
		!mgmt.SelfManaged.ValueBool() {
		resp.Diagnostics.AddAttributeError(
			path.Root("management").AtName("providers"),
			"Invalid cluster configuration",
			"management.providers is for a self-managed cluster; on the provider block's management cluster declare providers with capi_provider.",
		)
	}
	if mgmt != nil && !mgmt.Bootstrap.IsNull() && !mgmt.SelfManaged.IsUnknown() &&
		!mgmt.SelfManaged.ValueBool() {
		resp.Diagnostics.AddAttributeError(
			path.Root("management").AtName("bootstrap"),
			"Invalid cluster configuration",
			"management.bootstrap needs management.self_managed = true: a bootstrap cluster only exists to pivot from.",
		)
	}
}

// ModifyPlan refuses the class/direct switch, turns a create that did not
// finish into a planned update so the next apply resumes it, and stops a
// plan at once when the run would need a kind bootstrap cluster that lives
// on another machine (resume or destroy alike).
func (r *clusterResource) ModifyPlan(
	ctx context.Context,
	req resource.ModifyPlanRequest,
	resp *resource.ModifyPlanResponse,
) {
	if req.State.Raw.IsNull() {
		return
	}
	var state clusterResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	cp, d := loadCheckpoint(ctx, req.Private)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	mgmt, _ := extractManagement(ctx, &state)
	selfManaged := mgmt != nil && mgmt.SelfManaged.ValueBool()
	status := extractStatus(ctx, &state)
	name := state.Name.ValueString()

	if req.Plan.Raw.IsNull() {
		// Destroy: an unfinished create is torn down through its bootstrap
		// cluster, an interrupted deletion continues on the one it moved to.
		if !selfManaged {
			return
		}
		if !onItself(status, cp) {
			if err := foreignKind(
				cp.Bootstrap,
				fmt.Sprintf("The unfinished create of cluster %s", name),
			); err != nil {
				resp.Diagnostics.AddError("Bootstrap cluster on another machine", err.Error())
			}
			return
		}
		dcp, d := loadDeleteCheckpoint(ctx, req.Private)
		resp.Diagnostics.Append(d...)
		if err := foreignKind(
			dcp.MovedTo,
			fmt.Sprintf("The interrupted deletion of cluster %s", name),
		); err != nil {
			resp.Diagnostics.AddError("Bootstrap cluster on another machine", err.Error())
		}
		return
	}

	var plan clusterResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.Class.IsNull() != state.Class.IsNull() && !plan.Class.IsUnknown() {
		resp.RequiresReplace = append(resp.RequiresReplace, path.Root("class"))
	}
	if status != nil && !status.Phase.IsNull() &&
		status.Phase.ValueString() != string(capi.PhaseReady) {
		where := "This apply continues from there."
		if cp.Done(capi.StepMoved) && !pivotComplete(status) {
			where = "Management already moved to the cluster; this apply continues there."
		}
		resp.Diagnostics.AddWarning(
			"Resuming cluster creation",
			fmt.Sprintf(
				"The last create of cluster %s stopped at phase %s: %s.%s %s",
				name,
				status.Phase.ValueString(),
				status.Error.ValueString(),
				resumeSummary(cp, time.Now()),
				where,
			),
		)
		resp.Diagnostics.Append(
			resp.Plan.SetAttribute(
				ctx,
				path.Root("status"),
				types.ObjectUnknown(statusAttrTypes()),
			)...)
		if selfManaged && !onItself(status, cp) {
			if err := foreignKind(
				cp.Bootstrap,
				fmt.Sprintf("The unfinished create of cluster %s", name),
			); err != nil {
				resp.Diagnostics.AddError("Bootstrap cluster on another machine", err.Error())
			}
		}
	}
}

// --- CRUD ---

func (r *clusterResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var data clusterResourceModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	createTimeout, timeoutDiags := data.Timeouts.Create(ctx, defaultClusterCreateTimeout)
	resp.Diagnostics.Append(timeoutDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()

	r.validateLifecycleConfig(ctx, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Info(ctx, "Creating CAPI cluster", map[string]any{"name": data.Name.ValueString()})

	// Seed any persisted provider secrets (none on a fresh create; present
	// when a tainted resource is recreated) so the bootstrapper can recognize
	// a node it already owns.
	seedSecrets, d := providerSecretsFromDynamic(ctx, data.ProviderSecrets)
	resp.Diagnostics.Append(d...)
	cp := &capi.Checkpoint{}
	r.run(ctx, &data, nil, seedSecrets, cp, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(saveCheckpoint(ctx, resp.Private, keptCheckpoint(cp))...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	resp.Diagnostics.Append(
		setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)
}

func (r *clusterResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var plan, state clusterResourceModel

	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	updateTimeout, timeoutDiags := plan.Timeouts.Update(ctx, defaultClusterUpdateTimeout)
	resp.Diagnostics.Append(timeoutDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	r.validateLifecycleConfig(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	seedSecrets, d := providerSecretsFromDynamic(ctx, state.ProviderSecrets)
	resp.Diagnostics.Append(d...)
	cp, d := loadCheckpoint(ctx, req.Private)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.run(ctx, &plan, &state, seedSecrets, cp, &resp.Diagnostics)
	// Recorded whatever the outcome: a failed run's progress is what the
	// next one resumes from.
	resp.Diagnostics.Append(saveCheckpoint(ctx, resp.Private, keptCheckpoint(cp))...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.ProviderSecrets.IsNull() || plan.ProviderSecrets.IsUnknown() {
		plan.ProviderSecrets = state.ProviderSecrets
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	resp.Diagnostics.Append(
		setIdentity(ctx, resp.Identity, namespacedIdentity(plan.Namespace, plan.Name))...)
}

// run is the create workflow and the update that follows it: against the
// provider block's management cluster for an ordinary cluster; through a
// bootstrap cluster for a self-managed one that has not pivoted yet
// (a fresh create, or one being resumed); against the cluster itself once
// it has. state is nil on create.
func (r *clusterResource) run(
	ctx context.Context,
	data, state *clusterResourceModel,
	seedSecrets map[string]string,
	cp *capi.Checkpoint,
	diags *diag.Diagnostics,
) {
	opts, d := buildCreateOptions(ctx, data)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	opts.ProviderSecrets = seedSecrets

	mgmt, _ := extractManagement(ctx, data)
	selfManaged := mgmt != nil && mgmt.SelfManaged.ValueBool()
	var prevStatus *statusModel
	if state != nil {
		prevStatus = extractStatus(ctx, state)
	}
	// complete: the cluster manages itself (or never pivots); a resume
	// that only has the pivot's tail left goes through the manager, which
	// finishes it on the cluster.
	complete := state != nil && pivotComplete(prevStatus)
	resuming := state != nil && !complete
	onFailure := bootstrapOnFailure(ctx, data)
	bootstrapKind := bootstrapType(ctx, data)
	if bootstrapKind == "" {
		bootstrapKind = "kind" // management.bootstrap left out
	}

	var cleanup func()
	switch {
	case !selfManaged:
		// The provider block's management cluster; its providers are
		// capi_provider resources, never touched here.
		conn, d := r.pd.managementOrDiag(ctx)
		diags.Append(d...)
		if d.HasError() {
			return
		}
		opts.ManagementKubeconfig = conn.KubeconfigPath
		opts.SkipInit = true
		opts.SelfManaged = false
	case complete:
		// The cluster manages itself: reach it through the credential the
		// pivot captured, and converge its providers on management.providers.
		kubeconfig, done, err := r.selfKubeconfig(ctx, state, cp)
		if err != nil {
			diags.AddError("Cluster connection", err.Error())
			return
		}
		cleanup = done
		opts.ManagementKubeconfig = kubeconfig
		opts.SelfManaged = false
		upgrade, removed, added, d := providerChanges(ctx, state, data)
		diags.Append(d...)
		opts.UpgradeProviders = upgrade
		opts.RemovedProviders = removed
		opts.SkipInit = !added && len(upgrade) == 0 && removed.Empty()
	default:
		// A fresh self-managed create, or one resumed from the bootstrap
		// cluster the last run left in place (or, after the move, from the
		// cluster itself): the manager skips what the checkpoint records.
		what := fmt.Sprintf("The unfinished create of cluster %s", data.Name.ValueString())
		if err := foreignKind(cp.Bootstrap, what); err != nil {
			if !cp.Done(capi.StepMoved) {
				diags.AddError("Bootstrap cluster on another machine", err.Error())
				return
			}
			// After the move the bootstrap holds nothing: finish here and
			// leave the kind cluster to the machine it runs on.
			if !cp.Done(capi.StepBootstrapDeleted) {
				cp.Mark(capi.StepBootstrapDeleted)
				diags.AddWarning(
					"Bootstrap cluster left behind",
					fmt.Sprintf(
						"Kind cluster %s on host %s holds nothing since the move and is not deleted from here; delete it there with `kind delete cluster --name %s`.",
						cp.Bootstrap.Name,
						cp.Bootstrap.Host,
						cp.Bootstrap.Name,
					),
				)
			}
		}
		opts.ManagementKubeconfig = ""
		opts.SelfManaged = true
		opts.KeepBootstrapOnFailure = onFailure == onFailureResume
		opts.Checkpoint = cp
		startAttempt(cp, time.Now())
	}
	if cleanup != nil {
		defer cleanup()
	}

	var phase capi.Phase
	opts.OnPhase = func(p capi.Phase) { phase = p }

	mgr, d := r.managerFor(ctx, data)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	result, err := mgr.CreateCluster(ctx, *opts)
	if opts.Checkpoint != nil {
		stampBootstrap(cp, bootstrapKind)
	}
	if err != nil {
		var pe *capi.PhaseError
		moved := cp.Done(capi.StepMoved)
		// Once management moved, a failure is always kept for the resume:
		// replacing the resource would provision the machines a second time
		// while the cluster already manages them.
		if selfManaged && !complete && (onFailure == onFailureResume || moved) &&
			errors.As(err, &pe) {
			cp.LastError = pe.Err.Error()
			// Keep what exists: the phase, the error and the bootstrap
			// cluster, so the next apply continues from here.
			data.Id = types.StringValue(data.Name.ValueString())
			r.ensureManagementComputed(ctx, data)
			r.ensureWaitComputed(ctx, data)
			diags.Append(
				setPartialStatus(ctx, data, pe, prevStatus, cp.Done(capi.StepBootstrapDeleted))...)
			switch {
			case pe.Result != nil && len(pe.Result.ProviderSecrets) > 0:
				secretsVal, sd := providerSecretsToDynamic(ctx, pe.Result.ProviderSecrets)
				diags.Append(sd...)
				data.ProviderSecrets = secretsVal
			case state != nil && !state.ProviderSecrets.IsUnknown():
				data.ProviderSecrets = state.ProviderSecrets
			case data.ProviderSecrets.IsUnknown():
				data.ProviderSecrets = types.DynamicNull()
			}
			verb := "create"
			if resuming {
				verb = "resume"
			}
			diags.AddWarning(
				"Cluster creation incomplete",
				fmt.Sprintf(
					"Cluster %s stopped at phase %s during %s: %s. Nothing was torn down; the next apply resumes from this phase, and destroy tears down what exists.",
					data.Name.ValueString(),
					pe.Phase,
					verb,
					pe.Err,
				),
			)
			if onFailure != onFailureResume {
				diags.AddWarning(
					"Failure kept for resume",
					fmt.Sprintf(
						"management.bootstrap.on_failure is %q, but CAPI management of cluster %s already moved to the cluster itself: the failure is recorded for the next apply to resume instead of replacing the cluster, which would provision its machines a second time.",
						onFailure,
						data.Name.ValueString(),
					),
				)
			}
			return
		}
		cp.LastError = err.Error()
		diags.AddError("Cluster Creation Error", fmt.Sprintf("Failed to create cluster: %s", err))
		return
	}
	if selfManaged {
		// Finished: drop the progress. Keep the kubeconfig the move recorded
		// only for a cluster whose providers keep no root credential, as its
		// one way back in.
		keep := ""
		if len(result.ProviderSecrets) == 0 &&
			(state == nil || state.ProviderSecrets.IsNull() || state.ProviderSecrets.IsUnknown()) {
			keep = cp.WorkloadKubeconfig
		}
		*cp = capi.Checkpoint{WorkloadKubeconfig: keep}
	}

	data.Id = types.StringValue(data.Name.ValueString())
	r.ensureManagementComputed(ctx, data)
	r.ensureWaitComputed(ctx, data)
	if phase == "" {
		phase = capi.PhaseReady
	}
	diags.Append(setStatusWithFallback(ctx, data, result, prevStatus, capi.PhaseReady)...)
	if diags.HasError() {
		return
	}
	switch {
	case len(result.ProviderSecrets) > 0:
		secretsVal, sd := providerSecretsToDynamic(ctx, result.ProviderSecrets)
		diags.Append(sd...)
		data.ProviderSecrets = secretsVal
	case state != nil:
		data.ProviderSecrets = state.ProviderSecrets
	default:
		data.ProviderSecrets = types.DynamicNull()
	}
}

func (r *clusterResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var data clusterResourceModel

	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The identity follows from the state, whatever the read finds.
	resp.Diagnostics.Append(
		setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)

	cp, d := loadCheckpoint(ctx, req.Private)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	kubeconfig, cleanup, err := r.managementKubeconfig(ctx, &data, cp)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		// An unreachable management cluster is not a deleted cluster: keep
		// the state as it is.
		tflog.Warn(
			ctx,
			"Unable to reach the management cluster",
			map[string]any{"error": err.Error()},
		)
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		resp.Diagnostics.Append(
			setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)
		return
	}

	result, err := r.manager.GetClusterInfo(
		ctx,
		kubeconfig,
		data.Name.ValueString(),
		data.Namespace.ValueString(),
	)
	if errors.Is(err, capi.ErrClusterNotFound) {
		// The Cluster object is gone from a reachable management cluster:
		// deleted out of band. Drop it from state so the next apply recreates it.
		tflog.Warn(
			ctx,
			"Cluster no longer exists on the management cluster, removing it from state",
			map[string]any{"error": err.Error()},
		)
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		tflog.Warn(ctx, "Unable to read cluster info", map[string]any{"error": err.Error()})
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		resp.Diagnostics.Append(
			setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)
		return
	}

	prevStatus := extractStatus(ctx, &data)
	phase := capi.PhaseReady
	if prevStatus != nil && !prevStatus.Phase.IsNull() && prevStatus.Phase.ValueString() != "" {
		phase = capi.Phase(prevStatus.Phase.ValueString())
	}
	resp.Diagnostics.Append(setStatusWithFallback(ctx, &data, result, prevStatus, phase)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	resp.Diagnostics.Append(
		setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)
}

func (r *clusterResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var data clusterResourceModel

	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	deleteTimeout, timeoutDiags := data.Timeouts.Delete(ctx, defaultClusterDeleteTimeout)
	resp.Diagnostics.Append(timeoutDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()

	cp, d := loadCheckpoint(ctx, req.Private)
	resp.Diagnostics.Append(d...)
	dcp, d := loadDeleteCheckpoint(ctx, req.Private)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	mgmt, _ := extractManagement(ctx, &data)
	selfManaged := mgmt != nil && mgmt.SelfManaged.ValueBool()
	status := extractStatus(ctx, &data)
	bootstrapName := ""
	if status != nil && !status.BootstrapCluster.IsNull() {
		bootstrapName = status.BootstrapCluster.ValueString()
	}

	deleteSecrets, d := providerSecretsFromDynamic(ctx, data.ProviderSecrets)
	resp.Diagnostics.Append(d...)
	deleteOpts := capi.DeleteClusterOptions{
		Name:            data.Name.ValueString(),
		Namespace:       data.Namespace.ValueString(),
		ProviderSecrets: deleteSecrets,
	}
	// The configuration the cluster was created with: a self-managed cluster
	// is deleted through a fresh bootstrap cluster initialised the same way.
	spec, d := buildCreateOptions(ctx, &data)
	if d.HasError() {
		resp.Diagnostics.Append(d...)
		return
	}
	spec.ProviderSecrets = deleteSecrets
	deleteOpts.Spec = *spec
	deleteOpts.Wait = spec.Wait

	// A create that never moved management: whatever exists is on the
	// bootstrap cluster. A kind cluster is reachable and deleted through
	// (from the machine it runs on); a Kairos node is powered off, which
	// takes its objects with it.
	if selfManaged && !onItself(status, cp) {
		if bootstrapName == "" {
			return
		}
		if err := foreignKind(
			cp.Bootstrap,
			fmt.Sprintf("The unfinished create of cluster %s", data.Name.ValueString()),
		); err != nil {
			resp.Diagnostics.AddError("Bootstrap cluster on another machine", err.Error())
			return
		}
		if bootstrapType(ctx, &data) == "kairos" {
			if typed, d := r.managerFor(ctx, &data); !d.HasError() {
				if err := typed.DeleteBootstrapCluster(
					ctx,
					bootstrapName,
					deleteSecrets,
				); err != nil {
					resp.Diagnostics.AddError(
						"Bootstrap Node Error",
						fmt.Sprintf(
							"Could not power off the Kairos bootstrap node %s: %s",
							bootstrapName,
							err,
						),
					)
				}
			} else {
				resp.Diagnostics.Append(d...)
			}
			return
		}
		deleteOpts.ManagementKubeconfig = kindKubeconfigPath(bootstrapName)
		deleteOpts.DeleteBootstrap = true
		deleteOpts.BootstrapName = bootstrapName
		if err := r.manager.DeleteCluster(ctx, deleteOpts); err != nil {
			resp.Diagnostics.AddError(
				"Cluster Deletion Error",
				fmt.Sprintf("Failed to delete cluster: %s", err),
			)
		}
		return
	}

	if selfManaged {
		if err := foreignKind(
			dcp.MovedTo,
			fmt.Sprintf("The interrupted deletion of cluster %s", data.Name.ValueString()),
		); err != nil {
			resp.Diagnostics.AddError("Bootstrap cluster on another machine", err.Error())
			return
		}
	}
	kubeconfig, cleanup, err := r.managementKubeconfig(ctx, &data, cp)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		resp.Diagnostics.AddError(
			"Cluster Deletion Error",
			fmt.Sprintf(
				"Cannot reach the management cluster of %s: %s",
				data.Name.ValueString(),
				err,
			),
		)
		return
	}
	deleteOpts.ManagementKubeconfig = kubeconfig
	deleteOpts.SelfManaged = selfManaged
	deleteOpts.Checkpoint = dcp

	// The default manager runs kind bootstrap clusters: it deletes a plain
	// cluster and reverse-pivots a self-managed one.
	mgr := r.manager
	if selfManaged && bootstrapType(ctx, &data) == "kairos" && bootstrapName != "" &&
		!cp.Done(capi.StepBootstrapDeleted) {
		// A disposable node is powered off right after the pivot. When that
		// failed, status.bootstrap_cluster is still set: power it off now
		// through the Kairos bootstrapper, before the reverse pivot.
		if typed, d := r.managerFor(ctx, &data); !d.HasError() {
			if err := typed.DeleteBootstrapCluster(ctx, bootstrapName, deleteSecrets); err != nil {
				resp.Diagnostics.AddWarning(
					"Bootstrap Node Warning",
					fmt.Sprintf("Could not power off the Kairos bootstrap node: %s", err),
				)
			}
		} else {
			resp.Diagnostics.Append(d...)
		}
	}
	if err := mgr.DeleteCluster(ctx, deleteOpts); err != nil {
		// A deletion that did not finish keeps the resource: the user fixes
		// the cause and destroys again. A plain error, not a warning, or
		// Terraform would forget machines that are still provisioned. Where
		// the objects went is recorded so the next destroy finds them (the
		// reverse pivot always runs a kind bootstrap on this machine).
		if dcp.MovedTo != nil && dcp.MovedTo.Type == "" {
			dcp.MovedTo.Type, dcp.MovedTo.Host = "kind", runnerHost()
		}
		resp.Diagnostics.Append(saveDeleteCheckpoint(ctx, resp.Private, dcp)...)
		resp.Diagnostics.AddError(
			"Cluster Deletion Error",
			fmt.Sprintf("Failed to delete cluster: %s", err),
		)
	}
}

// ImportState imports a cluster by `[namespace/]name` from the management
// cluster the provider block names. A self-managed cluster is imported the
// same way through its own kubeconfig in the provider block; later reads
// derive the connection once `provider_secrets` is known, so a cluster stood
// up by other tooling is managed like any other.
func (r *clusterResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	importedID, d := importID(ctx, req, namespacedImportID)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := strings.TrimSpace(importedID)
	namespace, name := "default", id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		namespace, name = strings.TrimSpace(id[:i]), strings.TrimSpace(id[i+1:])
	}
	if name == "" || strings.Contains(id, ":") {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected [namespace/]name, got %q.", importedID),
		)
		return
	}
	if namespace == "" {
		namespace = "default"
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("namespace"), namespace)...)
	resp.Diagnostics.Append(
		resp.State.SetAttribute(
			ctx,
			path.Root("status"),
			types.ObjectValueMust(statusAttrTypes(), map[string]attr.Value{
				"phase": types.StringValue(
					string(capi.PhaseReady),
				),
				"error":             types.StringNull(),
				"endpoint":          types.StringNull(),
				"ca_certificate":    types.StringNull(),
				"description":       types.StringNull(),
				"bootstrap_cluster": types.StringNull(),
				"conditions": types.ListNull(
					types.ObjectType{AttrTypes: conditionAttrTypes()},
				),
			}),
		)...)
	resp.Diagnostics.Append(
		setIdentity(
			ctx,
			resp.Identity,
			namespacedIdentity(types.StringValue(namespace), types.StringValue(name)),
		)...)
}

// --- connections ---

// managementKubeconfig is where the cluster's objects live: the provider
// block's management cluster, the bootstrap cluster while a self-managed
// create is under way, or the cluster itself once it manages itself, reached
// through a credential derived from provider_secrets. The cleanup, when set,
// removes the derived kubeconfig file.
func (r *clusterResource) managementKubeconfig(
	ctx context.Context,
	data *clusterResourceModel,
	cp *capi.Checkpoint,
) (string, func(), error) {
	mgmt, _ := extractManagement(ctx, data)
	if mgmt == nil || !mgmt.SelfManaged.ValueBool() {
		conn, err := r.pd.management(ctx)
		if err != nil {
			return "", nil, err
		}
		return conn.KubeconfigPath, nil, nil
	}
	status := extractStatus(ctx, data)
	if !onItself(status, cp) {
		if status.BootstrapCluster.IsNull() || bootstrapType(ctx, data) == "kairos" {
			return "", nil, fmt.Errorf(
				"cluster %s has not pivoted yet (phase %s); its objects are on the bootstrap cluster",
				data.Name.ValueString(),
				status.Phase.ValueString(),
			)
		}
		if err := foreignKind(
			cp.Bootstrap,
			fmt.Sprintf("The unfinished create of cluster %s", data.Name.ValueString()),
		); err != nil {
			return "", nil, err
		}
		return kindKubeconfigPath(status.BootstrapCluster.ValueString()), nil, nil
	}
	return r.selfKubeconfig(ctx, data, cp)
}

// selfKubeconfig reaches a cluster that manages itself: through a kubeconfig
// derived from provider_secrets, or else through the one the move recorded
// in the checkpoint (until the root credential is captured, and for clusters
// whose providers keep none).
func (r *clusterResource) selfKubeconfig(
	ctx context.Context,
	data *clusterResourceModel,
	cp *capi.Checkpoint,
) (string, func(), error) {
	secrets, d := providerSecretsFromDynamic(ctx, data.ProviderSecrets)
	if (d.HasError() || len(secrets) == 0) && cp != nil && cp.WorkloadKubeconfig != "" {
		dir, err := os.MkdirTemp("", "capi-tf-"+data.Name.ValueString()+"-")
		if err != nil {
			return "", nil, err
		}
		p := filepath.Join(dir, "kubeconfig")
		if err := os.WriteFile(p, []byte(cp.WorkloadKubeconfig), 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", nil, err
		}
		return p, func() { _ = os.RemoveAll(dir) }, nil
	}
	return r.derivedKubeconfig(ctx, data)
}

// derivedKubeconfig writes a short-lived admin kubeconfig for a self-managed
// cluster from provider_secrets and status.endpoint.
func (r *clusterResource) derivedKubeconfig(
	ctx context.Context,
	data *clusterResourceModel,
) (string, func(), error) {
	secrets, d := providerSecretsFromDynamic(ctx, data.ProviderSecrets)
	if d.HasError() || len(secrets) == 0 {
		return "", nil, fmt.Errorf(
			"cluster %s manages itself but provider_secrets holds no root credential; import it through its kubeconfig in the provider block",
			data.Name.ValueString(),
		)
	}
	status := extractStatus(ctx, data)
	endpoint := ""
	if status != nil && !status.Endpoint.IsNull() {
		endpoint = status.Endpoint.ValueString()
	}
	if endpoint == "" {
		return "", nil, fmt.Errorf(
			"cluster %s manages itself but status.endpoint is unknown",
			data.Name.ValueString(),
		)
	}
	root, err := capi.ParseRootSecrets(data.Name.ValueString(), secrets)
	if err != nil {
		return "", nil, err
	}
	raw, err := capi.DeriveKubeconfig(
		root,
		endpoint,
		data.Name.ValueString(),
		derivedCredentialTTL(ctx),
		time.Now(),
	)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "capi-tf-"+data.Name.ValueString()+"-")
	if err != nil {
		return "", nil, err
	}
	p := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return p, func() { _ = os.RemoveAll(dir) }, nil
}

// kindKubeconfigPath is where the kind bootstrapper keeps a cluster's kubeconfig.
func kindKubeconfigPath(name string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("kind-%s-kubeconfig", name))
}

// bootstrapOnFailure is management.bootstrap.on_failure, resume by default.
func bootstrapOnFailure(ctx context.Context, data *clusterResourceModel) string {
	mgmt, _ := extractManagement(ctx, data)
	bs, _ := extractManagementBootstrap(ctx, mgmt)
	if bs == nil || bs.OnFailure.IsNull() || bs.OnFailure.IsUnknown() ||
		bs.OnFailure.ValueString() == "" {
		return onFailureResume
	}
	return bs.OnFailure.ValueString()
}

// --- Computed Field Helpers ---

func (r *clusterResource) ensureManagementComputed(
	ctx context.Context,
	data *clusterResourceModel,
) {
	mgmt, _ := extractManagement(ctx, data)
	if mgmt == nil {
		mgmt = &managementModel{
			SelfManaged: types.BoolValue(false),
			Providers:   types.ObjectNull(providersAttrTypes()),
			Bootstrap:   types.ObjectNull(managementBootstrapAttrTypes()),
		}
	}
	if mgmt.Bootstrap.IsUnknown() {
		mgmt.Bootstrap = types.ObjectNull(managementBootstrapAttrTypes())
	}
	if mgmt.Providers.IsUnknown() {
		mgmt.Providers = types.ObjectNull(providersAttrTypes())
	}
	val, _ := types.ObjectValueFrom(ctx, managementAttrTypes(), mgmt)
	data.Management = val
}

func (r *clusterResource) ensureWaitComputed(ctx context.Context, data *clusterResourceModel) {
	wait, _ := extractWait(ctx, data)
	if wait == nil {
		wait = &waitModel{
			Conditions: types.ListValueMust(
				types.StringType,
				[]attr.Value{types.StringValue(defaultWaitCondition)},
			),
		}
	}
	if wait.Conditions.IsUnknown() {
		wait.Conditions = types.ListValueMust(
			types.StringType,
			[]attr.Value{types.StringValue(defaultWaitCondition)},
		)
	}
	val, _ := types.ObjectValueFrom(ctx, waitAttrTypes(), wait)
	data.Wait = val
}

// --- state upgrades ---

// UpgradeState migrates every earlier schema version to v5. Terraform does
// not chain upgraders, so each entry rewrites the raw state JSON through
// every step to the current shape.
func (r *clusterResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
	return map[int64]resource.StateUpgrader{
		0: rawJSONUpgrader(0, upgradeV0JSON),
		1: rawJSONUpgrader(1, upgradeV1JSON),
		2: rawJSONUpgrader(2, upgradeV2JSON),
		3: rawJSONUpgrader(3, upgradeV3JSON),
		4: rawJSONUpgrader(4, upgradeV4JSON),
	}
}

func rawJSONUpgrader(from int, upgrade func([]byte) ([]byte, error)) resource.StateUpgrader {
	return resource.StateUpgrader{
		StateUpgrader: func(_ context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
			if req.RawState == nil || len(req.RawState.JSON) == 0 {
				resp.Diagnostics.AddError(
					"Unable to upgrade state",
					fmt.Sprintf("Missing raw state for schema version %d.", from),
				)
				return
			}
			out, err := upgrade(req.RawState.JSON)
			if err != nil {
				resp.Diagnostics.AddError(
					"Unable to upgrade state",
					fmt.Sprintf(
						"Upgrading capi_cluster state from schema version %d: %s",
						from,
						err,
					),
				)
				return
			}
			resp.DynamicValue = &tfprotov6.DynamicValue{JSON: out}
		},
	}
}

// --- Validation ---

// validateWorkerClasses checks that machine deployments sharing a ClusterClass
// worker class (class, or name when unset) agree on their template specs.
// --- Validation ---

// supportedInfrastructure lists the infrastructure providers the resource
// renders objects for.
var supportedInfrastructure = map[string]struct{}{
	"aws": {}, "azure": {}, "docker": {}, "openstack": {}, "tinkerbell": {}, "vsphere": {},
}

// validateLifecycleConfig checks what the schema cannot: the provider maps
// of a self-managed cluster, the infrastructure provider, the inventory
// against the control plane size, and the bootstrap configuration.
func (r *clusterResource) validateLifecycleConfig(
	ctx context.Context,
	data *clusterResourceModel,
	diags *diag.Diagnostics,
) {
	names := map[capi.ProviderType][]string{}
	for _, pm := range data.providerMaps(ctx) {
		entries, d := extractProviders(ctx, pm.Map)
		diags.Append(d...)
		for _, name := range sortedProviderNames(entries) {
			names[pm.Type] = append(names[pm.Type], name)
			validateProviderEntry(ctx, pm.Type, name, entries[name], diags)
		}
	}
	if diags.HasError() {
		return
	}
	mgmt, d := extractManagement(ctx, data)
	diags.Append(d...)
	selfManaged := mgmt != nil && mgmt.SelfManaged.ValueBool()
	if selfManaged && data.Class.IsNull() && len(names[capi.ProviderTypeInfrastructure]) != 1 {
		diags.AddError(
			"Invalid management configuration",
			fmt.Sprintf(
				"management.providers.infrastructure must contain exactly one provider for a self-managed cluster, got %d.",
				len(names[capi.ProviderTypeInfrastructure]),
			),
		)
		return
	}

	infra := ""
	if spec, _ := extractInfrastructure(
		ctx,
		data,
	); spec != nil && !spec.Provider.IsNull() &&
		!spec.Provider.IsUnknown() {
		infra = strings.ToLower(spec.Provider.ValueString())
	} else if n := names[capi.ProviderTypeInfrastructure]; len(
		n,
	) == 1 {
		infra = strings.ToLower(n[0])
	}
	if infra != "" {
		if _, ok := supportedInfrastructure[infra]; !ok {
			diags.AddError(
				"Unsupported infrastructure provider",
				fmt.Sprintf(
					"infrastructure provider %q is not supported. Supported: aws, azure, docker, openstack, tinkerbell, vsphere",
					infra,
				),
			)
		}
		if n := names[capi.ProviderTypeInfrastructure]; len(n) == 1 &&
			!strings.EqualFold(n[0], infra) {
			diags.AddError(
				"Invalid infrastructure configuration",
				fmt.Sprintf(
					"infrastructure.provider %q is not the provider management.providers.infrastructure installs (%q).",
					infra,
					n[0],
				),
			)
		}
	}
	cp, d := extractControlPlane(ctx, data)
	diags.Append(d...)
	if cp != nil && !cp.Provider.IsNull() && !cp.Provider.IsUnknown() {
		if n := names[capi.ProviderTypeControlPlane]; len(n) > 0 &&
			!contains(n, cp.Provider.ValueString()) {
			diags.AddError(
				"Invalid control plane configuration",
				fmt.Sprintf(
					"control_plane.provider %q is not one of the providers management.providers.control_plane installs (%s).",
					cp.Provider.ValueString(),
					strings.Join(n, ", "),
				),
			)
		}
	}
	if infra == "tinkerbell" {
		for _, n := range names[capi.ProviderTypeBootstrap] {
			if l := strings.ToLower(n); l != "kubeadm" && l != "talos" {
				diags.AddError(
					"Invalid bootstrap provider for Tinkerbell",
					fmt.Sprintf(
						"Tinkerbell supports bootstrap providers kubeadm or talos, got %q.",
						n,
					),
				)
			}
		}
		for _, n := range names[capi.ProviderTypeControlPlane] {
			if l := strings.ToLower(n); l != "kubeadm" && l != "talos" {
				diags.AddError(
					"Invalid control plane provider for Tinkerbell",
					fmt.Sprintf(
						"Tinkerbell supports control_plane providers kubeadm or talos, got %q.",
						n,
					),
				)
			}
		}
	}

	// The inventory must seat the control plane; node groups are checked
	// against the hardware by CAPT as they come.
	inv, d := extractInventory(ctx, data)
	diags.Append(d...)
	if inv != nil {
		var replicas int64
		if cp != nil && !cp.Replicas.IsNull() && !cp.Replicas.IsUnknown() {
			replicas = cp.Replicas.ValueInt64()
		}
		validateInventory(ctx, inv, replicas, 0, diags)
	}

	// Validate the bootstrap cluster configuration
	validateManagementBootstrap(ctx, data, diags)
}

// validateManagementBootstrap checks management.bootstrap: the type, the
// machine a Kairos node boots on, the boot settings, the kairos block and the
// bootstrap addons.
func validateManagementBootstrap(
	ctx context.Context,
	data *clusterResourceModel,
	diags *diag.Diagnostics,
) {
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
	switch typ {
	case "kind":
		return
	case "kairos":
	default:
		diags.AddError(
			summary,
			fmt.Sprintf(
				"management.bootstrap.type %q is not supported. Supported: kind, kairos.",
				typ,
			),
		)
		return
	}

	boot, d := extractBoot(ctx, bs)
	diags.Append(d...)
	manual := false
	if boot != nil {
		if !boot.Method.IsNull() && !boot.Method.IsUnknown() {
			switch boot.Method.ValueString() {
			case "auto", "virtual_media":
			case "manual":
				manual = true
			case "http":
				diags.AddError(
					summary,
					"management.bootstrap.boot.method \"http\" boots a UKI over UEFI HTTP boot; a live ISO boots from virtual media (auto or virtual_media) or by hand (manual).",
				)
			default:
				diags.AddError(
					summary,
					fmt.Sprintf(
						"management.bootstrap.boot.method %q is not supported. Supported: auto, virtual_media, manual.",
						boot.Method.ValueString(),
					),
				)
			}
		}
		if !boot.Timeout.IsNull() && !boot.Timeout.IsUnknown() {
			if _, err := time.ParseDuration(boot.Timeout.ValueString()); err != nil {
				diags.AddError(
					summary,
					fmt.Sprintf(
						"management.bootstrap.boot.timeout %q is not a valid duration: %v",
						boot.Timeout.ValueString(),
						err,
					),
				)
			}
		}
		if !boot.Attempts.IsNull() && !boot.Attempts.IsUnknown() && boot.Attempts.ValueInt64() < 1 {
			diags.AddError(summary, "management.bootstrap.boot.attempts must be at least 1.")
		}
	}

	if bs.Machine.IsNull() || bs.Machine.ValueString() == "" {
		diags.AddError(
			summary,
			fmt.Sprintf("management.bootstrap.machine is required when type = %q.", typ),
		)
	} else {
		m, d := findInventoryMachine(ctx, data, bs.Machine.ValueString())
		diags.Append(d...)
		if m == nil {
			diags.AddError(
				summary,
				fmt.Sprintf(
					"management.bootstrap.machine %q does not match any inventory.machine hostname.",
					bs.Machine.ValueString(),
				),
			)
		} else {
			if !manual && (m.BMC.IsNull() || m.BMC.IsUnknown()) {
				diags.AddError(
					summary,
					fmt.Sprintf(
						"inventory machine %q must define bmc to be used as the bootstrap node (or set boot.method = \"manual\").",
						bs.Machine.ValueString(),
					),
				)
			}
			var n networkModel
			diags.Append(m.Network.As(ctx, &n, basetypes.ObjectAsOptions{})...)
			if n.IPAddress.IsNull() || n.IPAddress.ValueString() == "" {
				diags.AddError(
					summary,
					fmt.Sprintf(
						"inventory machine %q must define network.ip_address to be used as the bootstrap node.",
						bs.Machine.ValueString(),
					),
				)
			}
		}
	}

	validateKairosBootstrap(ctx, data, bs, diags)
	ad, d := extractBootstrapAddons(ctx, bs)
	diags.Append(d...)
	validateAddonsModel(ctx, "management.bootstrap.addons", ad, diags)
}

// validateKairosBootstrap checks management.bootstrap.kairos and what a
// disposable node requires of the rest of the configuration: the live node is
// powered off after the pivot, so there must be one.
func validateKairosBootstrap(
	ctx context.Context,
	data *clusterResourceModel,
	bs *managementBootstrapModel,
	diags *diag.Diagnostics,
) {
	const summary = "Invalid bootstrap configuration"
	mgmt, d := extractManagement(ctx, data)
	diags.Append(d...)
	if mgmt == nil || (!mgmt.SelfManaged.IsUnknown() && !mgmt.SelfManaged.ValueBool()) {
		diags.AddError(
			summary,
			"management.bootstrap.type = \"kairos\" requires management.self_managed = true: the live node is disposable, so CAPI must pivot to the workload cluster before it is powered off.",
		)
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
	if !s.Port.IsNull() && !s.Port.IsUnknown() &&
		(s.Port.ValueInt64() < 1 || s.Port.ValueInt64() > 65535) {
		diags.AddError(summary, "management.bootstrap.kairos.ssh.port must be between 1 and 65535.")
	}
}

// validateAddonsModel checks an addons block: unique releases, parseable
// timeouts, YAML values and manifests.
func validateAddonsModel(
	ctx context.Context,
	where string,
	ad *bootstrapAddonsModel,
	diags *diag.Diagnostics,
) {
	const summary = "Invalid addons configuration"
	if ad == nil {
		return
	}
	var rels []helmReleaseModel
	if !ad.Helm.IsNull() && !ad.Helm.IsUnknown() {
		diags.Append(ad.Helm.ElementsAs(ctx, &rels, false)...)
	}
	seen := map[string]bool{}
	for i, rel := range rels {
		key := rel.Namespace.ValueString() + "/" + rel.Name.ValueString()
		if seen[key] {
			diags.AddError(
				summary,
				fmt.Sprintf("%s.helm[%d] is a duplicate release %q.", where, i, key),
			)
		}
		seen[key] = true
		if !rel.Timeout.IsNull() && !rel.Timeout.IsUnknown() && rel.Timeout.ValueString() != "" {
			if _, err := time.ParseDuration(rel.Timeout.ValueString()); err != nil {
				diags.AddError(
					summary,
					fmt.Sprintf(
						"%s.helm[%d].timeout %q is not a valid duration: %v",
						where,
						i,
						rel.Timeout.ValueString(),
						err,
					),
				)
			}
		}
		if !rel.Values.IsNull() && !rel.Values.IsUnknown() && rel.Values.ValueString() != "" {
			var v any
			if err := yaml.Unmarshal([]byte(rel.Values.ValueString()), &v); err != nil {
				diags.AddError(
					summary,
					fmt.Sprintf("%s.helm[%d].values is not valid YAML: %v", where, i, err),
				)
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
				diags.AddError(
					summary,
					fmt.Sprintf("%s.%s[%d] is not valid YAML: %v", where, list.name, i, err),
				)
			}
		}
	}
}
