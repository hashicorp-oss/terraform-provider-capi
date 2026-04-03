---
description: Terraform plugin framework instructions for implementing the CAPI cluster resource, including schema design, lifecycle management, and best practices. Load when modifying cluster_resource.go or any related files under internal/provider/.
applyTo: 'internal/provider/**'
---

# Terraform Plugin Framework: `capi_cluster` Implementation Guide

This document defines the schema conventions, model structures, lifecycle patterns, and implementation rules for the `capi_cluster` resource using the Terraform Plugin Framework.

---

## 1. Schema Design Philosophy

### 1.1 Core Nesting Rules

The schema follows a **nested-first** design. Every concept that groups multiple related attributes MUST be expressed as a `schema.SingleNestedAttribute` (or `schema.ListNestedAttribute` / `schema.SetNestedAttribute` for collections).

**Rule 1 — Multi-word attributes become nested objects:**

Any attribute name composed of two or more words where the first word is a *domain* and the second is a *property* MUST be split into a nested object.

```
✗  infrastructure_provider    →  ✓  infrastructure { provider }
✗  bootstrap_provider         →  ✓  bootstrap { provider }
✗  management_kubeconfig      →  ✓  management { kubeconfig }
✗  worker_machine_count       →  ✓  workers { machine_count }
✗  kubeconfig_path            →  ✓  output { kubeconfig_path }
```

**Rule 2 — Single-subject compound words stay as ONE nested object:**

Compound words that describe a single concept become a top-level nested object name, not a further split.

```
✓  control_plane { provider, machine_count }   ← "control_plane" is one subject
✓  machine_count                                ← inside its parent, this is a simple property
✗  control { plane { provider } }              ← wrong: splits a single subject
```

**Rule 3 — Flat attributes are the exception, not the rule:**

Only truly atomic, cluster-level identity properties remain flat at the top level:

```
✓  name                ← cluster identity (single word)
✓  kubernetes_version   ← cluster-wide version (single property, no domain grouping)
✓  flavor              ← template flavor (single word)
✓  id                  ← standard Terraform computed identifier
```

Everything else MUST be nested under a domain object.

**Rule 4 — Computed outputs go in `status`:**

All provider-computed values (endpoint, kubeconfig, CA cert, description) MUST be grouped under a single computed-only `status` nested attribute. This cleanly separates user inputs from provider outputs.

**Rule 5 — Use `schema.SingleNestedAttribute` for singular objects, `schema.ListNestedAttribute` for ordered collections:**

| Pattern | Attribute Type | When |
|---------|---------------|------|
| One instance | `schema.SingleNestedAttribute` | `infrastructure`, `control_plane`, `management`, `status` |
| Ordered collection | `schema.ListNestedAttribute` | `inventory.machine` (list of machines) |
| Unordered unique collection | `schema.SetNestedAttribute` | Worker node groups (future) |
| Key-value pairs | `schema.MapAttribute` | `labels`, `annotations` |

**Rule 6 — Never use blocks for new schemas:**

Per HashiCorp's official guidance: *"Use nested attribute types instead of block types for new schema implementations. Block support is mainly for migrating legacy SDK-based providers."*

All nested structures MUST use `schema.SingleNestedAttribute`, `schema.ListNestedAttribute`, or `schema.SetNestedAttribute` — never `schema.SingleNestedBlock`, `schema.ListNestedBlock`, or `schema.SetNestedBlock`.

### 1.2 Naming Conventions

| Convention | Example | Rule |
|-----------|---------|------|
| Snake case for all attributes | `machine_count`, `ip_address` | Standard Terraform convention |
| Nested object names are domain nouns | `infrastructure`, `control_plane`, `workers` | Not verbs, not adjectives |
| Boolean attributes use positive form | `enabled`, `self_managed` | Avoid double negatives like `skip_disabled` |
| Computed outputs never appear in config | `status { endpoint }` | Prevent user confusion on what's configurable |
| Sensitive attributes are marked | `bmc { password }`, `status { kubeconfig }` | Framework enforces masking in output |

---

## 2. Target HCL Experience

### 2.1 Minimal Configuration (Docker Provider)

```hcl
resource "capi_cluster" "dev" {
  name               = "dev-cluster"
  kubernetes_version = "v1.31.0"

  infrastructure {
    provider = "docker"
  }
}
```

### 2.2 Full Configuration (Tinkerbell Bare-Metal)

```hcl
resource "capi_cluster" "production" {
  name               = "prod-cluster"
  kubernetes_version = "v1.31.0"
  flavor             = "default"

  management {
    self_managed = true
    namespace    = "capi-system"
  }

  infrastructure {
    provider = "tinkerbell:v0.5.4"
  }

  bootstrap {
    provider = "kubeadm:v1.12.2"
  }

  control_plane {
    provider      = "kubeadm:v1.12.2"
    machine_count = 3
  }

  core {
    provider = "cluster-api:v1.12.2"
  }

  workers {
    machine_count = 5
  }

  inventory {
    machine = [
      {
        hostname = "cp-1"
        network = {
          ip_address  = "192.168.1.10"
          netmask     = "255.255.255.0"
          gateway     = "192.168.1.1"
          mac_address = "aa:bb:cc:dd:ee:01"
          nameservers = ["8.8.8.8", "8.8.4.4"]
          vlan_id     = "100"
        }
        disk = {
          device = "/dev/sda"
        }
        bmc = {
          address  = "192.168.2.10"
          username = "admin"
          password = var.bmc_passwords["cp-1"]
        }
        labels = { "type" = "cp" }
      },
      {
        hostname = "worker-1"
        network = {
          ip_address  = "192.168.1.20"
          netmask     = "255.255.255.0"
          gateway     = "192.168.1.1"
          mac_address = "aa:bb:cc:dd:ee:11"
          nameservers = ["8.8.8.8"]
        }
        disk = {
          device = "/dev/sda"
        }
        bmc = {
          address  = "192.168.2.20"
          username = "admin"
          password = var.bmc_passwords["worker-1"]
        }
        labels = { "type" = "worker" }
      }
    ]
  }

  wait {
    enabled = true
    timeout = "60m"
  }

  output {
    kubeconfig_path = "~/.kube/prod-cluster.kubeconfig"
  }
}

# Outputs from computed status block:
output "cluster_endpoint" {
  value = capi_cluster.production.status.endpoint
}

output "kubeconfig" {
  value     = capi_cluster.production.status.kubeconfig
  sensitive = true
}
```

### 2.3 File-Based Inventory (EKS-A CSV Compatibility)

```hcl
resource "capi_cluster" "bare_metal" {
  name               = "bm-cluster"
  kubernetes_version = "v1.31.0"

  infrastructure {
    provider = "tinkerbell:v0.5.4"
  }

  management {
    self_managed = true
  }

  control_plane {
    machine_count = 3
  }

  workers {
    machine_count = 3
  }

  inventory {
    source = "${path.module}/hardware.csv"
  }
}
```

---

## 3. Complete Schema Definition

### 3.1 Top-Level Attributes

```go
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
```

### 3.2 `management` — Cluster Management Configuration

Groups all attributes related to HOW the cluster is managed: management cluster identity, initialization, self-management pivot, and namespace scoping.

```go
"management": schema.SingleNestedAttribute{
    MarkdownDescription: "Management cluster configuration. Controls how the CAPI lifecycle is managed.",
    Optional:            true,
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
```

**Mutability:**
- `kubeconfig` — immutable (RequiresReplace)
- `self_managed` — immutable (RequiresReplace)
- `namespace` — immutable (RequiresReplace)
- `skip_init` — mutable (only affects initialization behavior)

### 3.3 `infrastructure` — Infrastructure Provider

```go
"infrastructure": schema.SingleNestedAttribute{
    MarkdownDescription: "Infrastructure provider configuration. Defines which CAPI infrastructure provider manages the cluster's machines.",
    Required:            true,
    PlanModifiers: []planmodifier.Object{
        objectplanmodifier.RequiresReplace(),
    },
    Attributes: map[string]schema.Attribute{
        "provider": schema.StringAttribute{
            MarkdownDescription: "Infrastructure provider name and optional version (e.g., `docker`, `tinkerbell:v0.5.4`, `aws:v2.7.1`).",
            Required:            true,
        },
    },
},
```

**Mutability:** Entire block is immutable. Changing infrastructure provider requires cluster recreation.

### 3.4 `bootstrap` — Bootstrap Provider

```go
"bootstrap": schema.SingleNestedAttribute{
    MarkdownDescription: "Bootstrap provider configuration. Defines how node bootstrapping is handled (e.g., kubeadm, talos).",
    Optional:            true,
    PlanModifiers: []planmodifier.Object{
        objectplanmodifier.RequiresReplace(),
    },
    Attributes: map[string]schema.Attribute{
        "provider": schema.StringAttribute{
            MarkdownDescription: "Bootstrap provider name and optional version (e.g., `kubeadm:v1.12.2`).",
            Required:            true,
        },
    },
},
```

**Mutability:** Entire block is immutable.

### 3.5 `control_plane` — Control Plane Configuration

`control_plane` is a single subject (not "control" + "plane"). Groups control plane provider and replica configuration.

```go
"control_plane": schema.SingleNestedAttribute{
    MarkdownDescription: "Control plane configuration. Defines the control plane provider and machine count.",
    Optional:            true,
    Attributes: map[string]schema.Attribute{
        "provider": schema.StringAttribute{
            MarkdownDescription: "Control plane provider name and optional version (e.g., `kubeadm:v1.12.2`).",
            Optional:            true,
            PlanModifiers: []planmodifier.String{
                stringplanmodifier.RequiresReplace(),
            },
        },
        "machine_count": schema.Int64Attribute{
            MarkdownDescription: "Number of control plane machines. Must be an odd number for HA (1, 3, 5).",
            Optional:            true,
        },
    },
},
```

**Mutability:**
- `provider` — immutable (RequiresReplace)
- `machine_count` — mutable (in-place scale via CAPI)

### 3.6 `core` — Core CAPI Provider

```go
"core": schema.SingleNestedAttribute{
    MarkdownDescription: "Core CAPI provider configuration. Specifies the cluster-api core provider version.",
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
```

**Mutability:** Entire block is immutable. Version changes trigger cluster recreation (future: provider upgrade).

### 3.7 `workers` — Worker Node Configuration

```go
"workers": schema.SingleNestedAttribute{
    MarkdownDescription: "Worker node configuration. Defines worker machine count and future named node groups.",
    Optional:            true,
    Attributes: map[string]schema.Attribute{
        "machine_count": schema.Int64Attribute{
            MarkdownDescription: "Number of worker machines.",
            Optional:            true,
        },
    },
},
```

**Mutability:** `machine_count` is mutable (in-place scale via CAPI).

**Future expansion** — named worker node groups will use `schema.ListNestedAttribute`:

```go
// Future: multiple worker groups
"groups": schema.ListNestedAttribute{
    Optional: true,
    NestedObject: schema.NestedAttributeObject{
        Attributes: map[string]schema.Attribute{
            "name":          schema.StringAttribute{Required: true},
            "machine_count": schema.Int64Attribute{Required: true},
            "labels":        schema.MapAttribute{ElementType: types.StringType, Optional: true},
        },
    },
},
```

### 3.8 `inventory` — Hardware Inventory

The most complex nested attribute. Supports file-based (EKS-A CSV) or inline machine definitions.

```go
"inventory": schema.SingleNestedAttribute{
    MarkdownDescription: "Hardware inventory for bare-metal provisioning. Supports file-based (CSV/YAML) or inline machine definitions. Required for Tinkerbell infrastructure provider.",
    Optional:            true,
    Attributes: map[string]schema.Attribute{
        "source": schema.StringAttribute{
            MarkdownDescription: "Path to a hardware inventory file. Supports EKS Anywhere CSV format or YAML manifests containing Hardware, Machine, and Secret resources.",
            Optional:            true,
        },
        "machine": schema.ListNestedAttribute{
            MarkdownDescription: "Inline machine definitions. Each machine maps to a Tinkerbell Hardware + optional BMC (Rufio Machine) + Secret.",
            Optional:            true,
            NestedObject: schema.NestedAttributeObject{
                Attributes: map[string]schema.Attribute{
                    "hostname": schema.StringAttribute{
                        MarkdownDescription: "Machine hostname. Must be unique across all inventory machines.",
                        Required:            true,
                    },
                    "network": schema.SingleNestedAttribute{
                        MarkdownDescription: "Network configuration for the machine's primary interface.",
                        Required:            true,
                        Attributes: map[string]schema.Attribute{
                            "ip_address": schema.StringAttribute{
                                MarkdownDescription: "Primary IP address.",
                                Required:            true,
                            },
                            "netmask": schema.StringAttribute{
                                MarkdownDescription: "Network mask (e.g., `255.255.255.0`).",
                                Required:            true,
                            },
                            "gateway": schema.StringAttribute{
                                MarkdownDescription: "Default gateway address.",
                                Required:            true,
                            },
                            "mac_address": schema.StringAttribute{
                                MarkdownDescription: "Primary NIC MAC address (e.g., `aa:bb:cc:dd:ee:01`). Must be unique.",
                                Required:            true,
                            },
                            "nameservers": schema.ListAttribute{
                                MarkdownDescription: "DNS nameserver addresses.",
                                ElementType:         types.StringType,
                                Optional:            true,
                            },
                            "vlan_id": schema.StringAttribute{
                                MarkdownDescription: "VLAN ID for the interface (0-4096).",
                                Optional:            true,
                            },
                        },
                    },
                    "disk": schema.SingleNestedAttribute{
                        MarkdownDescription: "Boot disk configuration.",
                        Optional:            true,
                        Attributes: map[string]schema.Attribute{
                            "device": schema.StringAttribute{
                                MarkdownDescription: "Disk device path (e.g., `/dev/sda`).",
                                Required:            true,
                            },
                        },
                    },
                    "bmc": schema.SingleNestedAttribute{
                        MarkdownDescription: "BMC (Baseboard Management Controller) configuration for out-of-band management. Maps to a Rufio Machine + Secret.",
                        Optional:            true,
                        Attributes: map[string]schema.Attribute{
                            "address": schema.StringAttribute{
                                MarkdownDescription: "BMC endpoint address (IP or hostname).",
                                Required:            true,
                            },
                            "username": schema.StringAttribute{
                                MarkdownDescription: "BMC username for authentication.",
                                Required:            true,
                            },
                            "password": schema.StringAttribute{
                                MarkdownDescription: "BMC password for authentication.",
                                Required:            true,
                                Sensitive:           true,
                            },
                        },
                    },
                    "labels": schema.MapAttribute{
                        MarkdownDescription: "Labels applied to the Hardware resource. Use `type=cp` for control plane and `type=worker` for worker nodes.",
                        ElementType:         types.StringType,
                        Optional:            true,
                    },
                },
            },
        },
    },
},
```

**Validation (custom validator on `inventory`):**
- `source` and `machine` are mutually exclusive: `objectvalidator.AtLeastOneOf()` + `objectvalidator.ConflictsWith()` equivalent logic
- When `machine` is provided, validate uniqueness of hostnames, IP addresses, and MAC addresses
- When BMC fields are partially set, require all three (`address`, `username`, `password`)

### 3.9 `wait` — Readiness Wait Configuration

```go
"wait": schema.SingleNestedAttribute{
    MarkdownDescription: "Readiness wait configuration. Controls whether and how long to wait for the workload cluster to become ready.",
    Optional:            true,
    Attributes: map[string]schema.Attribute{
        "enabled": schema.BoolAttribute{
            MarkdownDescription: "Wait for the workload cluster to become ready before returning.",
            Optional:            true,
            Computed:            true,
            Default:             booldefault.StaticBool(true),
        },
        "timeout": schema.StringAttribute{
            MarkdownDescription: "Maximum time to wait for readiness (Go duration string, e.g., `30m`, `1h`). Default: `60m`.",
            Optional:            true,
            Computed:            true,
        },
    },
},
```

### 3.10 `output` — Output Configuration

```go
"output": schema.SingleNestedAttribute{
    MarkdownDescription: "Output configuration. Controls where cluster credentials and artifacts are written.",
    Optional:            true,
    Attributes: map[string]schema.Attribute{
        "kubeconfig_path": schema.StringAttribute{
            MarkdownDescription: "File path where the workload cluster kubeconfig is written. Defaults to `~/.kube/<name>.kubeconfig`.",
            Optional:            true,
            Computed:            true,
        },
    },
},
```

### 3.11 `status` — Computed Cluster Status

All computed outputs are grouped here. This block is entirely computed — practitioners never configure it.

```go
"status": schema.SingleNestedAttribute{
    MarkdownDescription: "Computed cluster status. Contains the cluster endpoint, kubeconfig, CA certificate, and description after creation.",
    Computed:            true,
    Attributes: map[string]schema.Attribute{
        "endpoint": schema.StringAttribute{
            MarkdownDescription: "Cluster API server endpoint URL.",
            Computed:            true,
        },
        "kubeconfig": schema.StringAttribute{
            MarkdownDescription: "Kubeconfig content for accessing the workload cluster.",
            Computed:            true,
            Sensitive:           true,
        },
        "ca_certificate": schema.StringAttribute{
            MarkdownDescription: "Cluster CA certificate (PEM-encoded).",
            Computed:            true,
            Sensitive:           true,
        },
        "description": schema.StringAttribute{
            MarkdownDescription: "Cluster status description from `clusterctl describe cluster`.",
            Computed:            true,
        },
        "bootstrap_cluster": schema.StringAttribute{
            MarkdownDescription: "Name of the bootstrap cluster that was created during provisioning.",
            Computed:            true,
        },
    },
},
```

### 3.12 `addons` — Addon Configuration (Future)

```go
"addons": schema.SingleNestedAttribute{
    MarkdownDescription: "Addon configuration. ClusterResourceSets, Helm releases, or other post-provisioning add-ons.",
    Optional:            true,
    Attributes: map[string]schema.Attribute{
        // Future: ClusterResourceSet bindings, Helm chart references, etc.
    },
},
```

---

## 4. Go Model Structs

### 4.1 Naming Convention

Every nested attribute has a corresponding Go model struct. Struct names follow the pattern `<Domain>Model`.

```go
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
    Inventory      types.Object `tfsdk:"inventory"`
    Wait           types.Object `tfsdk:"wait"`
    Output         types.Object `tfsdk:"output"`
    Status         types.Object `tfsdk:"status"`
    Addons         types.Object `tfsdk:"addons"`
}
```

### 4.2 Nested Model Structs

```go
type ManagementModel struct {
    Kubeconfig  types.String `tfsdk:"kubeconfig"`
    SkipInit    types.Bool   `tfsdk:"skip_init"`
    SelfManaged types.Bool   `tfsdk:"self_managed"`
    Namespace   types.String `tfsdk:"namespace"`
}

type InfrastructureModel struct {
    Provider types.String `tfsdk:"provider"`
}

type BootstrapModel struct {
    Provider types.String `tfsdk:"provider"`
}

type ControlPlaneModel struct {
    Provider     types.String `tfsdk:"provider"`
    MachineCount types.Int64  `tfsdk:"machine_count"`
}

type CoreModel struct {
    Provider types.String `tfsdk:"provider"`
}

type WorkersModel struct {
    MachineCount types.Int64 `tfsdk:"machine_count"`
}

type InventoryModel struct {
    Source  types.String `tfsdk:"source"`
    Machine types.List   `tfsdk:"machine"` // []MachineModel
}

type MachineModel struct {
    Hostname types.String `tfsdk:"hostname"`
    Network  types.Object `tfsdk:"network"` // NetworkModel
    Disk     types.Object `tfsdk:"disk"`    // DiskModel
    BMC      types.Object `tfsdk:"bmc"`     // BMCModel
    Labels   types.Map    `tfsdk:"labels"`
}

type NetworkModel struct {
    IPAddress   types.String `tfsdk:"ip_address"`
    Netmask     types.String `tfsdk:"netmask"`
    Gateway     types.String `tfsdk:"gateway"`
    MACAddress  types.String `tfsdk:"mac_address"`
    Nameservers types.List   `tfsdk:"nameservers"` // []types.String
    VLANID      types.String `tfsdk:"vlan_id"`
}

type DiskModel struct {
    Device types.String `tfsdk:"device"`
}

type BMCModel struct {
    Address  types.String `tfsdk:"address"`
    Username types.String `tfsdk:"username"`
    Password types.String `tfsdk:"password"`
}

type WaitModel struct {
    Enabled types.Bool   `tfsdk:"enabled"`
    Timeout types.String `tfsdk:"timeout"`
}

type OutputModel struct {
    KubeconfigPath types.String `tfsdk:"kubeconfig_path"`
}

type StatusModel struct {
    Endpoint         types.String `tfsdk:"endpoint"`
    Kubeconfig       types.String `tfsdk:"kubeconfig"`
    CACertificate    types.String `tfsdk:"ca_certificate"`
    Description      types.String `tfsdk:"description"`
    BootstrapCluster types.String `tfsdk:"bootstrap_cluster"`
}
```

### 4.3 Accessing Nested Models

Use `types.Object` with `As()` for type-safe access:

```go
func extractManagement(ctx context.Context, data *ClusterResourceModel) (*ManagementModel, diag.Diagnostics) {
    if data.Management.IsNull() || data.Management.IsUnknown() {
        return nil, nil
    }
    var mgmt ManagementModel
    diags := data.Management.As(ctx, &mgmt, basetypes.ObjectAsOptions{})
    return &mgmt, diags
}
```

### 4.4 Setting Nested Values

Use `types.ObjectValueFrom()` to construct nested objects for state:

```go
func setStatus(ctx context.Context, data *ClusterResourceModel, result *capi.ClusterResult) diag.Diagnostics {
    status := StatusModel{
        Endpoint:         stringOrNull(result.Endpoint),
        Kubeconfig:       stringOrNull(result.Kubeconfig),
        CACertificate:    stringOrNull(result.CACertificate),
        Description:      stringOrNull(result.ClusterDescription),
        BootstrapCluster: bootstrapClusterName(result),
    }
    val, diags := types.ObjectValueFrom(ctx, statusAttrTypes(), status)
    if diags.HasError() {
        return diags
    }
    data.Status = val
    return nil
}

func stringOrNull(s string) types.String {
    if s == "" {
        return types.StringNull()
    }
    return types.StringValue(s)
}
```

### 4.5 Attribute Type Registration

Each nested model requires an `attrTypes()` helper for `types.ObjectValueFrom()` and `types.ObjectNull()`:

```go
func statusAttrTypes() map[string]attr.Type {
    return map[string]attr.Type{
        "endpoint":          types.StringType,
        "kubeconfig":        types.StringType,
        "ca_certificate":    types.StringType,
        "description":       types.StringType,
        "bootstrap_cluster": types.StringType,
    }
}

func managementAttrTypes() map[string]attr.Type {
    return map[string]attr.Type{
        "kubeconfig":   types.StringType,
        "skip_init":    types.BoolType,
        "self_managed": types.BoolType,
        "namespace":    types.StringType,
    }
}

// Pattern: one *AttrTypes() function per nested model struct.
```

---

## 5. Plan Modifiers

### 5.1 RequiresReplace on Nested Attributes

For immutable nested attributes where the *entire block* triggers replacement, apply `objectplanmodifier.RequiresReplace()` at the nested attribute level:

```go
"infrastructure": schema.SingleNestedAttribute{
    PlanModifiers: []planmodifier.Object{
        objectplanmodifier.RequiresReplace(),
    },
    // ...
}
```

For nested attributes where *some child attributes* trigger replacement and others are mutable, apply `RequiresReplace()` on the individual child attributes:

```go
"control_plane": schema.SingleNestedAttribute{
    Attributes: map[string]schema.Attribute{
        "provider": schema.StringAttribute{
            PlanModifiers: []planmodifier.String{
                stringplanmodifier.RequiresReplace(),  // immutable
            },
        },
        "machine_count": schema.Int64Attribute{
            // no RequiresReplace — mutable (scale in place)
        },
    },
}
```

### 5.2 UseStateForUnknown on Computed Attributes

Apply to computed values that persist across updates:

```go
"id": schema.StringAttribute{
    Computed: true,
    PlanModifiers: []planmodifier.String{
        stringplanmodifier.UseStateForUnknown(),
    },
},
```

For `status` (computed block), do NOT use `UseStateForUnknown` on child attributes — they should refresh on every read/update.

### 5.3 Defaults

Use `Default` for optional attributes with sensible defaults:

```go
"enabled": schema.BoolAttribute{
    Optional: true,
    Computed: true,
    Default:  booldefault.StaticBool(true),
},
```

---

## 6. Validation

### 6.1 Framework Validators

Use `terraform-plugin-framework-validators` for declarative validation:

```go
import (
    "github.com/hashicorp/terraform-plugin-framework-validators/objectvalidator"
    "github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
    "github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
    "github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
)

// On inventory.source — mutually exclusive with inventory.machine
"source": schema.StringAttribute{
    Validators: []validator.String{
        stringvalidator.ConflictsWith(path.MatchRelative().AtParent().AtName("machine")),
    },
},

// On control_plane.machine_count — must be odd for HA
"machine_count": schema.Int64Attribute{
    Validators: []validator.Int64{
        int64validator.AtLeast(1),
    },
},
```

### 6.2 Custom Validation in CRUD

Provider-specific validation logic (e.g., Tinkerbell requires `self_managed=true`) belongs in `validateLifecycleConfig()`, called at the top of Create and Update:

```go
func (r *ClusterResource) validateLifecycleConfig(ctx context.Context, data *ClusterResourceModel, diags *diag.Diagnostics) {
    infra, d := extractInfrastructure(ctx, data)
    diags.Append(d...)
    if diags.HasError() || infra == nil {
        return
    }

    provider := strings.Split(infra.Provider.ValueString(), ":")[0]
    provider = strings.ToLower(provider)

    if provider == "tinkerbell" {
        mgmt, d := extractManagement(ctx, data)
        diags.Append(d...)
        if mgmt == nil || mgmt.SelfManaged.IsNull() || !mgmt.SelfManaged.ValueBool() {
            diags.AddError(
                "Invalid Tinkerbell configuration",
                "Tinkerbell clusters must have management.self_managed = true.",
            )
        }
    }
}
```

### 6.3 Inventory Validation

```go
func validateInventory(ctx context.Context, inv *InventoryModel, cpCount, workerCount int64, diags *diag.Diagnostics) {
    if inv == nil {
        return
    }

    // source and machine are mutually exclusive
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

    // Uniqueness checks
    hostnames := map[string]bool{}
    ips := map[string]bool{}
    macs := map[string]bool{}
    for _, m := range machines {
        h := m.Hostname.ValueString()
        if hostnames[h] {
            diags.AddError("Duplicate hostname", fmt.Sprintf("hostname %q appears more than once", h))
        }
        hostnames[h] = true

        // Extract network and check IP/MAC uniqueness
        var net NetworkModel
        diags.Append(m.Network.As(ctx, &net, basetypes.ObjectAsOptions{})...)
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
```

---

## 7. CRUD Implementation Patterns

### 7.1 Building CreateClusterOptions from Nested Schema

```go
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
    if mgmt, d := extractManagement(ctx, data); mgmt != nil {
        diags.Append(d...)
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
    if bs, d := extractBootstrap(ctx, data); bs != nil {
        diags.Append(d...)
        opts.BootstrapProvider = bs.Provider.ValueString()
    }

    // Control Plane
    if cp, d := extractControlPlane(ctx, data); cp != nil {
        diags.Append(d...)
        if !cp.Provider.IsNull() {
            opts.ControlPlaneProvider = cp.Provider.ValueString()
        }
        if !cp.MachineCount.IsNull() {
            count := cp.MachineCount.ValueInt64()
            opts.ControlPlaneMachineCount = &count
        }
    }

    // Core
    if core, d := extractCore(ctx, data); core != nil {
        diags.Append(d...)
        opts.CoreProvider = core.Provider.ValueString()
    }

    // Workers
    if w, d := extractWorkers(ctx, data); w != nil {
        diags.Append(d...)
        if !w.MachineCount.IsNull() {
            count := w.MachineCount.ValueInt64()
            opts.WorkerMachineCount = &count
        }
    }

    // Wait
    if wait, d := extractWait(ctx, data); wait != nil {
        diags.Append(d...)
        opts.WaitForReady = wait.Enabled.ValueBool()
        // Parse timeout if provided
    }

    // Output
    if out, d := extractOutput(ctx, data); out != nil {
        diags.Append(d...)
        if !out.KubeconfigPath.IsNull() {
            opts.KubeconfigOutputPath = out.KubeconfigPath.ValueString()
        }
    }

    return opts, diags
}
```

### 7.2 Populating State from ClusterResult

After CreateCluster/UpdateCluster returns a `ClusterResult`, populate the model:

```go
func populateStateFromResult(ctx context.Context, data *ClusterResourceModel, result *capi.ClusterResult) diag.Diagnostics {
    var diags diag.Diagnostics

    data.Id = types.StringValue(data.Name.ValueString())

    // Set status block from result
    diags.Append(setStatus(ctx, data, result)...)

    // Set computed output.kubeconfig_path if not already set
    if !data.Output.IsNull() {
        // preserve user-provided output config
    }

    return diags
}
```

### 7.3 Null Handling for Optional Nested Attributes

When an optional nested attribute is not configured, its value is `types.ObjectNull(attrTypes)`. Always check before extracting:

```go
if data.Management.IsNull() || data.Management.IsUnknown() {
    // Use defaults: no management kubeconfig, self_managed=false, etc.
}
```

When setting computed nested attributes (like `status`) that have no prior value, construct a full object:

```go
// For the initial state when status has no value yet:
data.Status, diags = types.ObjectValueFrom(ctx, statusAttrTypes(), StatusModel{
    Endpoint:         types.StringNull(),
    Kubeconfig:       types.StringNull(),
    CACertificate:    types.StringNull(),
    Description:      types.StringNull(),
    BootstrapCluster: types.StringNull(),
})
```

---

## 8. Migration Path

### 8.1 From Flat Schema to Nested Schema

The current `cluster_resource.go` uses flat attributes. The migration to nested attributes requires a state migration function using `resource.ResourceWithUpgradeState`.

**Step 1 — Implement `UpgradeState()`:**

```go
func (r *ClusterResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
    return map[int64]resource.StateUpgrader{
        0: {
            PriorSchema:   &clusterResourceSchemaV0(), // flat schema
            StateUpgrader: upgradeClusterResourceStateV0ToV1,
        },
    }
}
```

**Step 2 — Map flat fields to nested structure:**

```go
func upgradeClusterResourceStateV0ToV1(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
    // Read flat v0 state
    var v0 ClusterResourceModelV0
    resp.Diagnostics.Append(req.State.Get(ctx, &v0)...)

    // Build nested v1 state
    v1 := ClusterResourceModel{
        Name:              v0.Name,
        KubernetesVersion: v0.KubernetesVersion,
        Flavor:            v0.Flavor,
        Id:                v0.Id,
    }

    // Map management fields
    mgmt := ManagementModel{
        Kubeconfig:  v0.ManagementKubeconfig,
        SkipInit:    v0.SkipInit,
        SelfManaged: v0.SelfManaged,
        Namespace:   v0.TargetNamespace,
    }
    v1.Management, _ = types.ObjectValueFrom(ctx, managementAttrTypes(), mgmt)

    // Map infrastructure
    infra := InfrastructureModel{Provider: v0.InfrastructureProvider}
    v1.Infrastructure, _ = types.ObjectValueFrom(ctx, infrastructureAttrTypes(), infra)

    // ... repeat for all nested blocks ...

    resp.Diagnostics.Append(resp.State.Set(ctx, v1)...)
}
```

**Step 3 — Version the schema:**

```go
func (r *ClusterResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
    resp.Schema = schema.Schema{
        Version: 1, // increment from 0
        // ... nested schema definition ...
    }
}
```

### 8.2 Schema Version History

| Version | Description | Breaking Changes |
|---------|-------------|-----------------|
| 0 | Flat attributes (current) | N/A (initial) |
| 1 | Nested objects + inventory | All provider/count fields moved into nested objects |

---

## 9. Testing Patterns

### 9.1 Schema Validation Tests

```go
func TestClusterResource_Schema(t *testing.T) {
    ctx := context.Background()
    r := NewClusterResource()
    req := resource.SchemaRequest{}
    resp := &resource.SchemaResponse{}
    r.Schema(ctx, req, resp)

    assert.False(t, resp.Diagnostics.HasError())

    // Verify nested attributes exist
    infraAttr, ok := resp.Schema.Attributes["infrastructure"]
    assert.True(t, ok, "infrastructure nested attribute must exist")
    _, isSingleNested := infraAttr.(schema.SingleNestedAttribute)
    assert.True(t, isSingleNested, "infrastructure must be SingleNestedAttribute")
}
```

### 9.2 Model Extraction Tests

```go
func TestExtractManagement_Null(t *testing.T) {
    ctx := context.Background()
    data := &ClusterResourceModel{
        Management: types.ObjectNull(managementAttrTypes()),
    }
    mgmt, diags := extractManagement(ctx, data)
    assert.Nil(t, mgmt)
    assert.False(t, diags.HasError())
}

func TestExtractManagement_Populated(t *testing.T) {
    ctx := context.Background()
    mgmtVal, _ := types.ObjectValueFrom(ctx, managementAttrTypes(), ManagementModel{
        Kubeconfig:  types.StringValue("/path/to/kubeconfig"),
        SkipInit:    types.BoolValue(false),
        SelfManaged: types.BoolValue(true),
        Namespace:   types.StringValue("capi-system"),
    })
    data := &ClusterResourceModel{Management: mgmtVal}
    mgmt, diags := extractManagement(ctx, data)
    assert.NotNil(t, mgmt)
    assert.False(t, diags.HasError())
    assert.Equal(t, "/path/to/kubeconfig", mgmt.Kubeconfig.ValueString())
}
```

### 9.3 Inventory Validation Tests

```go
func TestValidateInventory_DuplicateHostnames(t *testing.T) {
    // Build inventory with two machines sharing a hostname
    // Assert error diagnostic about duplicate hostname
}

func TestValidateInventory_InsufficientMachines(t *testing.T) {
    // Build inventory with 2 CP machines but cpCount=3
    // Assert error diagnostic about insufficient
}

func TestValidateInventory_SourceAndMachineMutuallyExclusive(t *testing.T) {
    // Build inventory with both source and machine populated
    // Assert error diagnostic
}
```

---

## 10. Summary: Attribute Location Map

Quick reference for where every attribute lives in the schema:

| Old (Flat) | New (Nested) | Type | Mutability |
|---|---|---|---|
| `name` | `name` | `StringAttribute` Required | Immutable (RequiresReplace) |
| `kubernetes_version` | `kubernetes_version` | `StringAttribute` Optional | **Mutable** |
| `flavor` | `flavor` | `StringAttribute` Optional | **Mutable** |
| `id` | `id` | `StringAttribute` Computed | Computed |
| `management_kubeconfig` | `management.kubeconfig` | `StringAttribute` Optional | Immutable |
| `skip_init` | `management.skip_init` | `BoolAttribute` Optional | **Mutable** |
| `self_managed` | `management.self_managed` | `BoolAttribute` Optional | Immutable |
| `target_namespace` | `management.namespace` | `StringAttribute` Optional | Immutable |
| `infrastructure_provider` | `infrastructure.provider` | `StringAttribute` Required | Immutable |
| `bootstrap_provider` | `bootstrap.provider` | `StringAttribute` Optional | Immutable |
| `control_plane_provider` | `control_plane.provider` | `StringAttribute` Optional | Immutable |
| `control_plane_machine_count` | `control_plane.machine_count` | `Int64Attribute` Optional | **Mutable** |
| `core_provider` | `core.provider` | `StringAttribute` Optional | Immutable |
| `worker_machine_count` | `workers.machine_count` | `Int64Attribute` Optional | **Mutable** |
| *(new)* | `inventory.source` | `StringAttribute` Optional | **Mutable** |
| *(new)* | `inventory.machine[]` | `ListNestedAttribute` Optional | **Mutable** |
| `wait_for_ready` | `wait.enabled` | `BoolAttribute` Optional | **Mutable** |
| *(new)* | `wait.timeout` | `StringAttribute` Optional | **Mutable** |
| `kubeconfig_path` | `output.kubeconfig_path` | `StringAttribute` Optional | **Mutable** |
| `endpoint` | `status.endpoint` | `StringAttribute` Computed | Computed |
| `kubeconfig` | `status.kubeconfig` | `StringAttribute` Computed+Sensitive | Computed |
| `cluster_ca_certificate` | `status.ca_certificate` | `StringAttribute` Computed+Sensitive | Computed |
| `cluster_description` | `status.description` | `StringAttribute` Computed | Computed |
| `bootstrap_cluster_name` | `status.bootstrap_cluster` | `StringAttribute` Computed | Computed |

