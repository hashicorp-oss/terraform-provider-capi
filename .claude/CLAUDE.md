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
✗  infrastructure_provider    →  ✓  infrastructure = { <name> = { version } }
✗  bootstrap_provider         →  ✓  bootstrap = { <name> = { version } }
✗  management_kubeconfig      →  ✓  management { kubeconfig }
✗  worker_machine_count       →  ✓  topology { workers { machine_deployments } }
✗  kubeconfig_path            →  ✓  output { kubeconfig_path }
✗  control_plane_endpoint →  ✓  network { control_plane_endpoint { host, port } }
```

**Rule 2 — Single-subject compound words stay as ONE nested object:**

Compound words that describe a single concept become a top-level nested object name, not a further split.

```
✓  control_plane = { talos = { version } }      ← "control_plane" is one subject
✓  machine_count                                ← inside its parent, this is a simple property
✗  control { plane { provider } }              ← wrong: splits a single subject
```

**Rule 3 — Flat attributes are the exception, not the rule:**

Only truly atomic, cluster-level identity properties remain flat at the top level:

```
✓  name                ← cluster identity (single word)
✓  kubernetes_version   ← cluster-wide version (single property, no domain grouping)
✓  cluster_class       ← generation mode switch (single subject)
✓  id                  ← standard Terraform computed identifier
```

Everything else MUST be nested under a domain object.

**Rule 4 — Computed outputs go in `status`:**

All provider-computed values (endpoint, kubeconfig, CA cert, description) MUST be grouped under a single computed-only `status` nested attribute. This cleanly separates user inputs from provider outputs.

**Rule 5 — Use `schema.SingleNestedAttribute` for singular objects, `schema.ListNestedAttribute` for ordered collections:**

| Pattern | Attribute Type | When |
|---------|---------------|------|
| One instance | `schema.SingleNestedAttribute` | `management`, `topology`, `status` |
| Named instances | `schema.MapNestedAttribute` | Provider maps: `infrastructure`, `bootstrap`, `control_plane`, `core`, `ipam`, `addon` (keyed by provider name) |
| Ordered collection | `schema.ListNestedAttribute` | `inventory.machine`, `topology.workers.machine_deployments` |
| Unordered unique collection | `schema.SetNestedAttribute` | Worker node groups (future) |
| Key-value pairs | `schema.MapAttribute` | `labels`, `annotations` |

**Rule 6 — Never use blocks for new schemas:**

Per HashiCorp's official guidance: *"Use nested attribute types instead of block types for new schema implementations. Block support is mainly for migrating legacy SDK-based providers."*

All nested structures MUST use `schema.SingleNestedAttribute`, `schema.ListNestedAttribute`, or `schema.SetNestedAttribute` — never `schema.SingleNestedBlock`, `schema.ListNestedBlock`, or `schema.SetNestedBlock`.

### 1.2 Naming Conventions

| Convention | Example | Rule |
|-----------|---------|------|
| Snake case for all attributes | `machine_count`, `ip_address` | Standard Terraform convention |
| Nested object names are domain nouns | `infrastructure`, `control_plane`, `topology` | Not verbs, not adjectives |
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

  infrastructure = { docker = {} }
}
```

### 2.2 Full Configuration (Tinkerbell Bare-Metal)

Providers are maps keyed by provider name, matching the cluster-api-operator
Helm values layout. `fetch_config.owner` alone is enough to point at a fork of
a provider clusterctl already knows; the repository name and components file
are inferred and the pinned `version` is embedded in the release URL. The
provider generates the CAPI objects itself (§3.6); each entry's
`resource_config` (a YAML/JSON object, typically `yamlencode({...})`) is merged
into the spec of that provider's object.

```hcl
resource "capi_cluster" "production" {
  name               = "prod-cluster"
  kubernetes_version = "v1.31.0"

  network = {
    control_plane_endpoint = { host = "192.168.1.100", port = 6443 }
    pods                   = { cidr_blocks = ["10.244.0.0/16"] }
    services               = { cidr_blocks = ["10.96.0.0/12"] }
  }

  management = {
    self_managed = true
    namespace    = "capi-system"
  }

  core = {
    cluster-api = {
      manager = {
        feature_gates = { ClusterTopology = true, MachinePool = true }
      }
    }
  }

  infrastructure = {
    tinkerbell = {
      version         = "v0.7.9"
      fetch_config    = { owner = "tinkerbell-community" }
      manager         = { feature_gates = { ClusterTopology = true } }
      resource_config = yamlencode({ templateInline = file("${path.module}/talos-workflow.yaml") }) # TinkerbellCluster.spec
    }
  }

  bootstrap = {
    talos = {
      version         = "v0.8.2"
      fetch_config    = { owner = "sidero-community" }
      resource_config = yamlencode({ talosVersion = "v1.13" }) # default worker TalosConfigTemplate.spec
    }
  }

  control_plane = {
    talos = {
      version      = "v0.7.1"
      fetch_config = { owner = "sidero-community" }
      resource_config = yamlencode({ # TalosControlPlane.spec
        controlPlaneConfig = { controlplane = { talosVersion = "v1.13", strategicPatches = [local.vip_patch] } }
      })
    }
  }

  ipam = {
    unifi = {
      version      = "v0.4.1"
      fetch_config = { owner = "ubiquiti-community", repository = "cluster-api-ipam-provider-unifi" }
    }
  }

  addon = { helm = {} }

  topology = {
    control_plane = {
      replicas       = 3
      infrastructure = yamlencode({ hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "cp" } } }] } })
    }
    workers = {
      machine_deployments = [
        {
          name           = "md-0"
          replicas       = 5
          metadata       = { labels = { tier = "worker" } }
          infrastructure = yamlencode({ hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker" } } }] } })
        }
      ]
    }
  }

  inventory = {
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
        disk = { device = "/dev/sda" }
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
        disk = { device = "/dev/sda" }
        bmc = {
          address  = "192.168.2.20"
          username = "admin"
          password = var.bmc_passwords["worker-1"]
        }
        labels = { "type" = "worker" }
      }
    ]
  }

  wait = {
    enabled = true
    timeout = "60m"
  }

  output = {
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

  infrastructure = { tinkerbell = { version = "v0.7.9" } }

  management = {
    self_managed = true
  }

  topology = {
    control_plane = { replicas = 3 }
    workers = {
      machine_deployments = [{ name = "md-0", replicas = 3 }]
    }
  }

  inventory = {
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
"cluster_class": schema.BoolAttribute{
    MarkdownDescription: "Generate a ClusterClass and a topology Cluster instead of direct objects.",
    Optional:            true,
    Computed:            true,
    Default:             booldefault.StaticBool(false),
    PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
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

**`management.bootstrap` — transient bootstrap cluster:**

```go
"bootstrap": schema.SingleNestedAttribute{
    Optional: true,
    PlanModifiers: []planmodifier.Object{objectplanmodifier.RequiresReplace()},
    Attributes: map[string]schema.Attribute{
        "type":    // "kind" (default) | "talos" | "kairos"
        "machine": // inventory.machine[].hostname; required for talos and kairos
        "boot":    // { method: auto|virtual_media|http, timeout: "15m", attempts: 3 }
        "mode":    // "pivot" (default) | "in_place"
        "talos":   // { version, architecture, endpoint, image { factory, schematic, extensions, kernel_args, iso, installer }, config_patches }
        "kairos":  // { iso, address, ssh { user, port, private_key, host_key } }
        "addons":  // { prerequisites [yaml], helm [ { name, namespace, chart, repository, version, values, timeout } ], manifests [yaml] }
    },
},
```

The Talos bootstrapper (`internal/capi/talos`) implements `capi.Bootstrapper`. It is an
observed-state reconciler over bmclib and the Talos machinery API; the machine-secrets
bundle is the only thing kept (`provider_secrets` in state plus a resume cache). See
`docs/superpowers/specs/2026-09-12-talos-bmc-bootstrapper-design.md`.

Addon install order on the bootstrap node is fixed (`talos.Addons`): Gateway API CRD URLs,
the `cni` release, `addons.prerequisites`, `addons.helm`, `addons.manifests`. The CNI goes
first because nothing schedules without it; `prerequisites` exist for objects the Helm
releases wait on that need the CNI's CRDs (a Cilium load-balancer pool for the Tinkerbell
Service).

**In-place adoption** (`mode = "in_place"`, Talos only, needs `control_plane["talos"]`):
the bootstrap node becomes the first control-plane Machine of the cluster it hosts. The
contract (`docs/superpowers/specs/2026-11-03-bootstrap-topologies-design.md` §4) is
implemented in `applyInPlaceContract` (`internal/provider/talos_bootstrap.go`) and the
Talos package:

- the node's machine config is `control_plane["talos"].resource_config`
  `controlPlaneConfig.controlplane.strategicPatches` with `PROVIDER_ID` substituted to
  `tinkerbell://<namespace>/<hostname>`, plus a kubelet `provider-id` patch when none sets
  one, then the user's `config_patches`; a warning is raised when the patches do not set
  `cluster.allowSchedulingOnControlPlanes` (the node allows it; the CAPI render must agree);
- `talos.endpoint` defaults to `network.control_plane_endpoint`; different hosts are an error;
- once the node is ours and etcd is bootstrapped, `ActionPinNetwork` writes hostname, address,
  route and resolvers to Talos META key `0xa` (`internal/capi/talos/meta.go`, the `talosmeta`
  action's document shape) when `inventory.machine[].network.netmask` is set, so an in-place
  config update that drops the static network cannot move the node; best effort, logged;
- the inventory Hardware for the bootstrap machine carries
  `v1alpha1.tinkerbell.org/provisioned: "true"` and `allowPXE: false`, so CAPT adopts it;
- destroy goes through `capi.InPlaceTeardowner` (`Bootstrapper.TeardownInPlace`): every other
  inventory machine that answers the cluster talosconfig is reset and powered off through its
  BMC, then the bootstrap node; the Cluster object is never deleted from inside itself.

A DHCP reservation for the bootstrap machine's MAC remains an external prerequisite (Talos
maintenance mode is DHCP-only). With `mode = "pivot"` the bootstrap machine is left out of
the inventory while it hosts the management cluster.

**Disposable Kairos node** (`type = "kairos"`, `internal/capi/kairos`, always `mode = "pivot"`
and `self_managed = true`): the bootstrap machine is booted from virtual media through its BMC
(`talos.MediaBoot`, the boot layer shared with the Talos reconciler: power off if on, eject,
attach the ISO by AMT storage redirection or bmclib virtual media plus a one-shot boot override,
power on) with a user-built Kairos live ISO (AuroraBoot `kairos.iso.custom.iso` embedding the
cloud-config: `k3s.enabled`, `users[].ssh_authorized_keys`; a GRUB config whose default
entry carries `kairos.ram`, since AuroraBoot's live template only offers that in a submenu; see
`examples/resources/capi_cluster/kairos-cloud-config.yaml`). The bootstrapper waits for SSH on
`kairos.address` (default: the machine's `ip_address`), keeps the media attached (AMT
redirection session included) for the life of the node (AuroraBoot puts the cloud-config at the
ISO root as `config.yaml`, read at every stage, and without `kairos.ram` the ISO is the root
filesystem too), reads
`/etc/rancher/k3s/k3s.yaml` (plain read, then `sudo -n`), rewrites its server to the node
address, waits for the API and a Ready node, installs `bootstrap.addons`, and returns the
cluster; a node that already answers over SSH is reused. `Delete` ejects the media and powers
the node off: nothing was written to disk, there is no reset and no `provider_secrets`.
`boot.method` must be `auto` or `virtual_media`; `disk` is not required on the machine;
`management.bootstrap.talos` is an error. The pivot sequence below is unchanged; the node's
Hardware is added to the inventory as a netboot target after the move, like any pivoted
bootstrap machine. Model: `KairosModel{ISO, Address, SSH}` / `KairosSSHModel{User, Port,
PrivateKey, HostKey}` (`kairosAttrTypes`, `kairosSSHAttrTypes`); wiring in
`buildKairosBootstrapConfig` (`internal/provider/kairos_bootstrap.go`); `managerFor` picks the
bootstrapper by `bootstrapType`.

**`management.addons`** (same shape as `management.bootstrap.addons`: `prerequisites`,
`helm`, `manifests`) are management-plane addons, the Tinkerbell stack above all. The
manager installs them with the provider's own Helm and apply clients
(`capi.AddonInstaller`, `talos.NewManagementAddonInstaller`) on every cluster that takes
the management role right after `clusterctl init` there. `prerequisites` are applied only
on the final management cluster (a transient bootstrap cluster may lack their CRDs).

**Pivot sequence** (`mode = "pivot"`, `Manager.CreateCluster`): init bootstrap →
`management.addons` without prerequisites → inventory (bootstrap machine excluded) and
`manifests.management` → cluster objects → phase one: `ControlPlaneInitialized` → the
`cni` release and its Gateway API CRDs on the workload cluster (`WorkloadAddons`) →
Tinkerbell quiescence on the bootstrap cluster (`capi.Quiescer`, no Workflow
PREPARING/PENDING/RUNNING/POST; Workflows do not move and a mid-flight agent reports to the
tink-server that booted it) → init workload → `management.addons` with prerequisites →
`clusterctl move` (carries Hardware and rufio Machines: their CRDs are labelled for move) →
re-apply the inventory (bootstrap machine now a netboot target; BMC Secrets do not move) and
`manifests.management` on the workload → delete the bootstrap cluster → phase two:
Cluster `Available`/`Ready` on the new management cluster. A pivot without a workload
kubeconfig is an error, never a bootstrap deletion.

**`cni` reaches the CAPI render.** `injectCNIPatch` (`internal/provider/cni.go`) prepends
the Talos CNI patch to `control_plane["talos"].resource_config` strategic patches, to
`bootstrap["talos"].resource_config` and to every machine deployment's `bootstrap` spec, so
nodes CAPI renders (and the in-place node after an in-place update) run without flannel. It
applies to Talos and Kairos bootstraps (`talosClusterBootstrap`); with Kairos the cluster must
have a `control_plane["talos"]` entry. `workloadAddonsFor` builds the workload cluster's cni
release from the block itself (`cniOptionsFor` + `cniRelease`), so a pivot installs it on the
workload cluster whichever node bootstrapped it.

### 3.3 Provider Maps — `core`, `infrastructure`, `bootstrap`, `control_plane`, `ipam`, `addon`

Every CAPI provider category is a `schema.MapNestedAttribute` keyed by the
provider name clusterctl uses (`cluster-api`, `docker`, `tinkerbell`, `kubeadm`,
`talos`, `helm`, `unifi`). All six maps share one nested object built by
`providerNestedObject()` and are created through `providerMapAttribute(desc, required)`:

```go
"infrastructure": providerMapAttribute("Infrastructure provider, keyed by name ...", true),
"bootstrap":      providerMapAttribute("Bootstrap providers, keyed by name ...", false),
// core, control_plane, ipam, addon likewise
```

The shared provider object (modeled after the cluster-api-operator provider CRDs):

| Attribute | Type | Purpose |
|---|---|---|
| `version` | String Optional | Release tag (`v0.7.9`); omitted means clusterctl's `latest` |
| `fetch_config` | SingleNested Optional | `{ owner, repository, url, oci }` — see 3.4 |
| `config_variables` | Map(String) Optional | `${VAR}` substitution in component YAML |
| `secret_config_variables` | Map(String) Optional, Sensitive | Same, for secrets |
| `deployment` | SingleNested Optional | replicas, node_selector, service_account_name, containers[] |
| `manager` | SingleNested Optional | profiler_address, max_concurrent_reconciles, verbosity, `feature_gates` (Map(Bool)), additional_args |
| `additional_manifests` | String Optional | Extra YAML applied with the components |
| `manifest_patches` | List(String) Optional | RFC 7396 merge patches (exclusive with `patches`) |
| `patches` | ListNested Optional | Strategic merge / RFC 6902 patches with `target` selectors |
| `resource_config` | String Optional | YAML/JSON object (`yamlencode({...})`) merged into the spec of the provider's generated cluster object (§3.6); only on `infrastructure`, `control_plane`, `bootstrap` |

An empty object (`helm = {}`) installs clusterctl's default release.

**Mutability:** every provider map carries `providerMapRequiresReplace()`, a
`mapplanmodifier.RequiresReplaceIf` that compares state and plan with each
entry's `resource_config` blanked: providers are installed once by
`clusterctl init`, so any other change replaces the cluster, while a
`resource_config` change re-applies the generated objects in place (Update
runs with `SkipInit`).

**Cardinality:** `infrastructure` is Required and must contain exactly one
entry. The other five are Optional and may hold several entries; a missing
`control_plane` or `bootstrap` entry defaults to `kubeadm` for generation,
exactly as `clusterctl init` installs the kubeadm providers by default.

`resource_config` and the topology `infrastructure`/`bootstrap` specs are
strings, not objects: terraform-plugin-framework does not support dynamic
attributes inside collection nested attributes (the provider maps and the
machine-deployment list), so these take YAML or JSON text and the provider
decodes it (`yamlObject`; integral numbers → `int64`).

**Backend:** entries become `capi.ProviderConfig{Name, Type, Version, FetchConfig, ...}`
grouped in a `capi.ProviderSet` (`internal/capi/types.go`). Customizations are
keyed by `ProviderKey{Type, Name}`, so bootstrap `talos` and control-plane
`talos` are configured independently (`internal/capi/customize.go`).

### 3.4 `fetch_config` — Friendlier Component Source

```go
"fetch_config": schema.SingleNestedAttribute{
    Optional: true,
    Attributes: map[string]schema.Attribute{
        "owner":      schema.StringAttribute{Optional: true}, // GitHub owner/org
        "repository": schema.StringAttribute{Optional: true}, // GitHub repository
        "url":        schema.StringAttribute{Optional: true}, // verbatim clusterctl URL
        "oci":        schema.StringAttribute{Optional: true}, // verbatim OCI reference
    },
},
```

Resolution (`capi.ResolveFetchURL`, `internal/capi/fetch.go`):

1. `url` or `oci` set → used verbatim (they exclude `owner`/`repository`).
2. Otherwise `https://github.com/{owner}/{repository}/releases/{version|latest}/{type}-components.yaml`,
   where `owner`/`repository` default from clusterctl's built-in entry for the
   provider (including the `<name>-<name>` aliases such as `tinkerbell-tinkerbell`)
   and `{type}` is `core`, `infrastructure`, `bootstrap`, `control-plane`, `ipam`, or `addon`.
3. A provider clusterctl does not know needs `repository` (or `url`/`oci`);
   otherwise create fails with `ErrUnknownProviderRepository`.

Pinning `version` in the URL avoids clusterctl's GitHub API call for `latest`.
Resolved URLs are registered with clusterctl through an isolated viper-backed
`config.Reader` (`internal/capi/clusterctl_config.go`) that still loads the
user's `clusterctl.yaml` and environment.

### 3.5 `topology` — Machine Counts (mirrors `Cluster.spec.topology`)

```go
"topology": schema.SingleNestedAttribute{
    Optional: true,
    Attributes: map[string]schema.Attribute{
        "control_plane": schema.SingleNestedAttribute{
            Optional: true,
            Attributes: map[string]schema.Attribute{
                "replicas":       schema.Int64Attribute{Optional: true},
                "infrastructure": schema.StringAttribute{Optional: true}, // YAML/JSON control plane <Infra>MachineTemplate spec
            },
        },
        "workers": schema.SingleNestedAttribute{
            Optional: true,
            Attributes: map[string]schema.Attribute{
                "machine_deployments": schema.ListNestedAttribute{ // Cluster.spec.topology.workers.machineDeployments[]
                    Optional: true,
                    NestedObject: schema.NestedAttributeObject{
                        Attributes: map[string]schema.Attribute{
                            "name":           schema.StringAttribute{Required: true},
                            "class":          schema.StringAttribute{Optional: true},
                            "replicas":       schema.Int64Attribute{Optional: true},
                            "failure_domain": schema.StringAttribute{Optional: true},
                            "metadata":       // { labels, annotations } Map(String)
                            "infrastructure": schema.StringAttribute{Optional: true}, // YAML/JSON <Infra>MachineTemplate spec
                            "bootstrap":      schema.StringAttribute{Optional: true}, // YAML/JSON <Bootstrap>ConfigTemplate spec
                        },
                    },
                },
            },
        },
    },
},
```

**Mutability:** everything under `topology` is mutable (in-place scale).

**Backend:** `control_plane` → `CreateClusterOptions.ControlPlane capi.ControlPlaneTopology`
(`Replicas`, `Infrastructure`); the list → `CreateClusterOptions.MachineDeployments
[]capi.MachineDeploymentTopology` (with `Infrastructure` and `Bootstrap`). Each entry
becomes one MachineDeployment with its own machine template and bootstrap config
template (or, with `cluster_class`, a worker class named `class` — default `name` —
plus a topology entry; deployments sharing a class must agree on both specs).
Inventory validation counts workers as the sum of all replicas; names must be unique.

### 3.6 `cni` — Container Network Interface (Talos bootstrap only)

Replaces Talos's built-in flannel. `SingleNestedAttribute`, Optional; omitting
it keeps flannel.

```go
"cni": schema.SingleNestedAttribute{
    Optional: true,
    Attributes: map[string]schema.Attribute{
        "provider": schema.StringAttribute{Required: true}, // only "cilium" today
        "version":  schema.StringAttribute{Optional: true}, // Helm chart version
        "values":   schema.StringAttribute{Optional: true}, // extra Helm values (YAML), deep-merged over defaults
    },
},
```

Only valid with `management.bootstrap.type = "talos"`, or `"kairos"` with a
`control_plane["talos"]` entry; anything else is a validation error, because
the knob is sugar over Talos `config_patches`/strategic patches plus a Helm
release (`internal/provider/cni.go`, `applyCNI`, `cniRelease`). When set to
`cilium`, `buildTalosBootstrapConfig` prepends a Talos machine-config patch
(`cluster.network.cni.name: none`, `cluster.proxy.disabled: true`,
`machine.features.kubePrism` on 7445) and appends a Cilium Helm release
(`oci://quay.io/cilium/charts/cilium`, `kube-system`) whose values are the
Talos-required defaults deep-merged with the block's `values`. The defaults
come from the official Talos+Cilium guide and are needed for Cilium to start
on Talos: `kubeProxyReplacement`, KubePrism `k8sServiceHost`/`k8sServicePort`,
Kubernetes IPAM, `cgroup.autoMount.enabled=false` + `cgroup.hostRoot`, and the
restricted `securityContext.capabilities` lists (`ciliumAgent`,
`cleanCiliumState`). Environment-specific values (BGP, native routing, Gateway
API, Hubble, resource limits) go in `values`; see
`examples/resources/capi_cluster/cilium-values.yaml`. `cni` was added in schema version 2 without a version bump; the upgraders
emit `cni: null`.

`cni.version` must be a tag that exists on `oci://quay.io/cilium/charts/cilium`.
quay retains only recent patch releases and has no floating `latest`, so an old
or omitted version fails at apply with a `FetchReference ... not found` error
(the cluster provisions first, then the addon install fails). List tags with
`curl -s https://quay.io/v2/cilium/charts/cilium/tags/list`.

**`cni.gateway_api`** installs the Gateway API CRDs before Cilium (Cilium does
not bundle them; enabling `gatewayAPI` without them fails the chart with "no
matches for kind GatewayClass"):

```go
"gateway_api": schema.SingleNestedAttribute{
    Optional: true,
    Attributes: map[string]schema.Attribute{
        "enabled": schema.BoolAttribute{Optional: true},
        "version": schema.StringAttribute{Optional: true}, // Gateway API release; default defaultGatewayAPIVersion
    },
},
```

When `enabled`, `applyCNI` appends the upstream `standard-install.yaml` URL for
the release to `talos.Addons.PreHelmManifestURLs` and sets `gatewayAPI.enabled=true`
in the Cilium values. The Talos addon installer fetches and applies every
`PreHelmManifestURLs` entry BEFORE the Helm releases (`internal/capi/talos/addons.go`),
so the CRDs exist when Cilium's GatewayClass is rendered. Do not set
`gatewayAPI.enabled` in `cni.values` yourself; the block owns it (extra
`gatewayAPI.*` keys still deep-merge).

For a pivoted CAPT workload cluster (not the Talos bootstrap path), CNI is a
HelmChartProxy concern and out of scope here.

### 3.6 `network`, `cluster_class` and generated manifests

There is no cluster template. `internal/capi/generate.go` builds the CAPI
objects from the provider set (`ManifestGenerator`, default
`NewManifestGenerator(NewCRDKindResolver())`), and `Manager.CreateCluster`
server-side-applies them where it used to apply the clusterctl template.

```go
"network": schema.SingleNestedAttribute{ // Cluster.spec.clusterNetwork + controlPlaneEndpoint; mutable
    Optional: true,
    Attributes: map[string]schema.Attribute{
        "control_plane_endpoint": // { host (Required), port (Optional; the generator defaults 6443) }
        "pods":                   // { cidr_blocks List(String) }
        "services":               // { cidr_blocks List(String) }
        "service_domain":         schema.StringAttribute{Optional: true},
    },
},
```

**Kinds and versions are discovered, never hardcoded** (`internal/capi/kinds.go`).
`KindCatalog` lists the CRDs labelled `cluster.x-k8s.io/provider=<label>`
(clusterctl's manifest label: `cluster-api`, `infrastructure-tinkerbell`,
`control-plane-talos`, ...) and resolves a kind by suffix (`Cluster`,
`ClusterTemplate`, `MachineTemplate`, `ControlPlane`, `ControlPlaneTemplate`,
`ConfigTemplate`; shortest match wins, ties are an error). The API version is
the newest served version named by the CRD's contract label for the core
contract in use (`cluster.x-k8s.io/v1beta2: v1beta1` on Talos CRDs), else the
storage version. The control-plane CRD's OpenAPI schema decides the
machine-template ref shape (`machineTemplate.spec.infrastructureRef{apiGroup}`
vs the legacy `machineTemplate.infrastructureRef{apiVersion}`); core refs
follow the core version. Missing kinds fail with `ErrKindNotFound` naming the
label and suffix.

**Direct mode** (`cluster_class = false`): `Cluster` `<name>` (network settings
+ refs), `<Infra>Cluster` `<name>`, `<CP>ControlPlane` `<name>-control-plane`
(`version`, `replicas`, machine template ref), and per machine deployment a
`MachineDeployment` `<name>-<md>` with `<Infra>MachineTemplate` and
`<Bootstrap>ConfigTemplate`. **ClusterClass mode** (`cluster_class = true`):
`<Infra>ClusterTemplate`, `<CP>ControlPlaneTemplate`, per-class machine and
config templates, a `ClusterClass` `<name>` and a `Cluster` whose
`spec.topology` comes from `topology`; init gets `CLUSTER_TOPOLOGY=true` (the
gate is Alpha/off in cluster-api v1.14), `kubernetes_version` is required and
the v1beta2 core contract is required.

**Spec assembly** for every provider object: `providerDefaults` (required-field
minimums: Talos `generateType` per role, kubeadm `kubeadmConfigSpec: {}`) ←
`resource_config` (RFC 7396: nested maps merge, lists/scalars replace, `null`
deletes) ← generator-owned wiring set last (refs, names, `version`,
`replicas`, `clusterName`, labels). Immutable templates are named
`<base>-<8 hex of sha256(spec JSON)>`, so a spec change is a new object plus a
moved ref (CAPI rolls it out); stale templates stay until cluster deletion.
An existing template-created cluster therefore sees one control-plane rollout
on its first apply after upgrading, because its machine template is renamed.

The waiter (`internal/capi/waiter.go`) watches the core `Cluster`'s
`ControlPlaneInitialized` and `Available`/`Ready` conditions at the discovered
core version, so `wait.enabled` works for any control-plane provider.

Golden manifests live in `internal/capi/testdata/golden/`; CRD fixtures in
`internal/capi/testdata/crds/` (real CAPT/CACPPT/CABPT CRDs trimmed to kinds,
labels, versions and the `machineTemplate` schema slice, plus synthetic
core/docker/kubeadm sets and a v1beta1-contract set).

### 3.7 Removed Attributes

`infrastructure.provider`, `bootstrap.provider`, `control_plane.provider`,
`control_plane.machine_count`, `core.provider`, `workers`, and the `addons`
list were replaced by the provider maps and `topology` in schema version 2.
`template` (`source`, `variables`) and `flavor` were removed in schema
version 3 when manifest generation replaced clusterctl templates.

### 3.8 `inventory` — Hardware Inventory

The most complex nested attribute. Supports file-based (EKS-A CSV) or inline machine definitions.

With the `tinkerbell` infrastructure provider and `manage = true` (default) every
`machine` becomes, in the cluster namespace, a basic-auth Secret `<hostname>-bmc-auth`, a
rufio `Machine <hostname>` (Redfish with basic auth by default, IntelAMT when the BMC port
is 16992/16993) and a Tinkerbell `Hardware <hostname>` (labels from `labels`, `dhcp.ip`
from `network`, `disks`, `bmcRef`). `capi.InventoryManifests` (`internal/capi/inventory.go`,
golden `testdata/golden/inventory-tinkerbell.yaml`) renders them and the manager applies
them before the generated cluster objects, after the caller's `manifests.management`.

```go
"inventory": schema.SingleNestedAttribute{
    MarkdownDescription: "Hardware inventory for bare-metal provisioning. Supports file-based (CSV/YAML) or inline machine definitions. Required for Tinkerbell infrastructure provider.",
    Optional:            true,
    Attributes: map[string]schema.Attribute{
        "manage": schema.BoolAttribute{ // create the Tinkerbell/rufio objects; default true
            Optional: true, Computed: true, Default: booldefault.StaticBool(true),
        },
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

### 3.12 `addon` — Addon Providers

Addon providers (e.g. `helm`) are one of the provider maps described in 3.3.

### 3.13 `manifests` — Raw Manifests

```go
"manifests": schema.SingleNestedAttribute{
    Optional: true,
    Attributes: map[string]schema.Attribute{
        "management": schema.ListAttribute{ElementType: types.StringType, Optional: true}, // multi-doc YAML strings
    },
},
```

`manifests.management` is server-side-applied to the management cluster after provider
init and before the generated cluster objects (`CreateClusterOptions.ManagementManifests`):
IPAM pools and credentials, extra Secrets, anything the cluster objects reference.

---

## 4. Go Model Structs

### 4.1 Naming Convention

Every nested attribute has a corresponding Go model struct. Struct names follow the pattern `<Domain>Model`.

```go
type ClusterResourceModel struct {
    Name              types.String `tfsdk:"name"`
    KubernetesVersion types.String `tfsdk:"kubernetes_version"`
    ClusterClass      types.Bool   `tfsdk:"cluster_class"`
    Id                types.String `tfsdk:"id"`

    Management     types.Object `tfsdk:"management"`
    Core           types.Map    `tfsdk:"core"`           // map[string]ProviderModel
    Infrastructure types.Map    `tfsdk:"infrastructure"` // exactly one entry
    Bootstrap      types.Map    `tfsdk:"bootstrap"`
    ControlPlane   types.Map    `tfsdk:"control_plane"`
    IPAM           types.Map    `tfsdk:"ipam"`
    Addon          types.Map    `tfsdk:"addon"`
    Topology       types.Object `tfsdk:"topology"`
    Network        types.Object `tfsdk:"network"`   // ClusterNetworkModel {control_plane_endpoint, pods, services, service_domain}
    CNI            types.Object `tfsdk:"cni"`       // CNIModel {provider, version, values}
    Manifests      types.Object `tfsdk:"manifests"` // ManifestsModel {management}
    Inventory      types.Object `tfsdk:"inventory"`
    Wait           types.Object `tfsdk:"wait"`
    Output         types.Object `tfsdk:"output"`
    Status         types.Object `tfsdk:"status"`
}
```

### 4.2 Nested Model Structs

```go
type ManagementModel struct {
    Kubeconfig  types.String `tfsdk:"kubeconfig"`
    SkipInit    types.Bool   `tfsdk:"skip_init"`
    SelfManaged types.Bool   `tfsdk:"self_managed"`
    Namespace   types.String `tfsdk:"namespace"`
    Bootstrap   types.Object `tfsdk:"bootstrap"` // ManagementBootstrapModel
    Addons      types.Object `tfsdk:"addons"`    // BootstrapAddonsModel (management-plane addons)
}

// Shared by all six provider maps; one providerAttrTypes()/providerMapType().
type ProviderModel struct {
    Version               types.String `tfsdk:"version"`
    FetchConfig           types.Object `tfsdk:"fetch_config"` // FetchConfigModel
    ConfigVariables       types.Map    `tfsdk:"config_variables"`
    SecretConfigVariables types.Map    `tfsdk:"secret_config_variables"`
    Deployment            types.Object `tfsdk:"deployment"` // DeploymentModel
    Manager               types.Object `tfsdk:"manager"`    // ManagerModel
    AdditionalManifests   types.String `tfsdk:"additional_manifests"`
    ManifestPatches       types.List   `tfsdk:"manifest_patches"`
    Patches               types.List   `tfsdk:"patches"` // []PatchModel
    ResourceConfig        types.String `tfsdk:"resource_config"` // YAML/JSON → capi.ProviderConfig.ResourceConfig
}

type FetchConfigModel struct {
    Owner      types.String `tfsdk:"owner"`
    Repository types.String `tfsdk:"repository"`
    URL        types.String `tfsdk:"url"`
    OCI        types.String `tfsdk:"oci"`
}

type TopologyModel struct {
    ControlPlane types.Object `tfsdk:"control_plane"` // ControlPlaneTopologyModel
    Workers      types.Object `tfsdk:"workers"`       // WorkersTopologyModel
}

type ControlPlaneTopologyModel struct {
    Replicas       types.Int64  `tfsdk:"replicas"`
    Infrastructure types.String `tfsdk:"infrastructure"` // YAML/JSON
}

type WorkersTopologyModel struct {
    MachineDeployments types.List `tfsdk:"machine_deployments"` // []MachineDeploymentModel
}

type MachineDeploymentModel struct {
    Name           types.String `tfsdk:"name"`
    Class          types.String `tfsdk:"class"`
    Replicas       types.Int64  `tfsdk:"replicas"`
    FailureDomain  types.String `tfsdk:"failure_domain"`
    Metadata       types.Object `tfsdk:"metadata"` // TopologyMetadataModel {labels, annotations}
    Infrastructure types.String `tfsdk:"infrastructure"` // YAML/JSON
    Bootstrap      types.String `tfsdk:"bootstrap"`      // YAML/JSON
}

type ClusterNetworkModel struct {
    ControlPlaneEndpoint types.Object `tfsdk:"control_plane_endpoint"` // EndpointModel {host, port}
    Pods                 types.Object `tfsdk:"pods"`                   // NetworkRangesModel {cidr_blocks}
    Services             types.Object `tfsdk:"services"`               // NetworkRangesModel
    ServiceDomain        types.String `tfsdk:"service_domain"`
}

type InventoryModel struct {
    Manage  types.Bool   `tfsdk:"manage"`
    Source  types.String `tfsdk:"source"`
    Machine types.List   `tfsdk:"machine"` // []MachineModel
}

type ManifestsModel struct {
    Management types.List `tfsdk:"management"` // []types.String
}

type BootstrapAddonsModel struct {
    Prerequisites types.List `tfsdk:"prerequisites"`
    Helm          types.List `tfsdk:"helm"` // []HelmReleaseModel
    Manifests     types.List `tfsdk:"manifests"`
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

For the provider maps, apply `mapplanmodifier.RequiresReplace()` on the map
itself (see `providerMapAttribute`). Mutable settings such as `topology`
replica counts live in their own nested attribute with no plan modifier, so a
scale operation never touches an immutable map:

```go
"topology": schema.SingleNestedAttribute{
    Optional: true, // no RequiresReplace — replicas scale in place
    // ...
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

// On topology.control_plane.replicas — must be odd for HA
"replicas": schema.Int64Attribute{
    Validators: []validator.Int64{
        int64validator.AtLeast(1),
    },
},
```

### 6.2 Custom Validation in CRUD

Provider-specific validation logic (e.g., Tinkerbell requires `self_managed=true`) belongs in `validateLifecycleConfig()`, called at the top of Create and Update:

```go
func (r *ClusterResource) validateLifecycleConfig(ctx context.Context, data *ClusterResourceModel, diags *diag.Diagnostics) {
    // 1. Per-entry rules (fetch_config exclusivity, patches) for every provider map.
    names := map[capi.ProviderType][]string{}
    for _, pm := range data.providerMaps() {
        entries, d := extractProviders(ctx, pm.Map)
        diags.Append(d...)
        for _, name := range sortedProviderNames(entries) {
            names[pm.Type] = append(names[pm.Type], name)
            validateProviderEntry(ctx, pm.Type, name, entries[name], diags)
        }
    }

    // 2. Exactly one infrastructure provider; it must be supported.
    infraNames := names[capi.ProviderTypeInfrastructure]
    if len(infraNames) != 1 {
        diags.AddError("Invalid infrastructure configuration", "infrastructure must contain exactly one provider.")
        return
    }

    // 3. Provider-specific rules, e.g. Tinkerbell needs self_managed and kubeadm/talos.
    if strings.ToLower(infraNames[0]) == "tinkerbell" {
        mgmt, d := extractManagement(ctx, data)
        diags.Append(d...)
        if mgmt == nil || !mgmt.SelfManaged.ValueBool() {
            diags.AddError("Invalid Tinkerbell configuration", "Tinkerbell clusters must have management.self_managed = true.")
        }
    }

    // 4. Topology (unique machine deployment names) feeds inventory validation.
    cpCount, mds, d := extractTopology(ctx, data)
    // ...
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

    // Management (kubeconfig, skip_init, self_managed, namespace, bootstrap mode)
    mgmt, d := extractManagement(ctx, data)
    diags.Append(d...)
    // ...

    // Providers: all six maps flattened into a ProviderSet. Each entry is
    // converted by buildProviderConfig(ctx, name, type, ProviderModel), which
    // handles version, fetch_config, config variables, deployment, manager,
    // manifests and patches. Map keys are visited in sorted order.
    providers, d := extractProviderSet(ctx, data)
    diags.Append(d...)
    opts.Providers = providers

    // Topology
    cpCount, mds, d := extractTopology(ctx, data)
    diags.Append(d...)
    opts.ControlPlaneMachineCount = cpCount
    opts.MachineDeployments = mds

    // Wait, Output ...
    return opts, diags
}
```

Helpers: `extractProviders(ctx, types.Map) (map[string]ProviderModel, diag.Diagnostics)`
returns nil for a null map; `data.providerMaps()` lists the six maps with their
`capi.ProviderType`; `providerMapValue(ctx, "name:version")` builds a one-entry
map for the state upgraders.

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

    // Map infrastructure: the legacy "name:version" string becomes a one-entry map
    v1.Infrastructure, _ = providerMapValue(ctx, v0.InfrastructureProvider.ValueString())

    // Counts become topology (worker count -> one machine deployment "md-0")
    v1.Topology, _ = legacyTopologyObject(ctx, v0.ControlPlaneMachineCount, v0.WorkerMachineCount)

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
| 0 | Flat attributes | N/A (initial) |
| 1 | Nested objects + inventory | All provider/count fields moved into nested objects |
| 2 | Provider maps, `fetch_config` owner/repository, `topology`, `ipam` | `provider = "name:version"` strings become map keys + `version`; `workers` and `addons` removed; counts move to `topology` (`replicas`) |
| 3 | Generated manifests: `resource_config`, `network`, `cluster_class`, topology specs (current) | `template` and `flavor` removed |

Every upgrader emits v3 directly (Terraform does not chain them). The v1 and
v2 upgraders rewrite the raw state JSON (`cluster_resource_upgrade.go`,
`upgradeV1JSON` = v1→v2→v3, `upgradeV2JSON` = v2→v3); a legacy worker count
becomes one machine deployment named `md-0`, and v3 adds `cluster_class: false`,
`network: null`, `resource_config: null` on every provider entry and null
`infrastructure`/`bootstrap` on the topology entries. `manifests`,
`inventory.manage`, `management.addons`, `management.bootstrap.addons.prerequisites` and
`management.bootstrap.kairos` joined v3 without a bump (`upgradeInventoryAndAddonsJSON`
fills them; a v3 state that predates them decodes with nulls).

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
| `flavor` | *(removed in v3)* | — | — |
| *(new)* | `cluster_class` | `BoolAttribute` Optional | Immutable |
| *(new)* | `network.control_plane_endpoint.{host,port}` / `network.{pods,services}.cidr_blocks` / `network.service_domain` | `SingleNestedAttribute` Optional | **Mutable** |
| `id` | `id` | `StringAttribute` Computed | Computed |
| `management_kubeconfig` | `management.kubeconfig` | `StringAttribute` Optional | Immutable |
| `skip_init` | `management.skip_init` | `BoolAttribute` Optional | **Mutable** |
| `self_managed` | `management.self_managed` | `BoolAttribute` Optional | Immutable |
| `target_namespace` | `management.namespace` | `StringAttribute` Optional | Immutable |
| `infrastructure_provider` | `infrastructure.<name>` (+ `.version`) | `MapNestedAttribute` Required | Immutable |
| `bootstrap_provider` | `bootstrap.<name>` | `MapNestedAttribute` Optional | Immutable |
| `control_plane_provider` | `control_plane.<name>` | `MapNestedAttribute` Optional | Immutable |
| `core_provider` | `core.<name>` | `MapNestedAttribute` Optional | Immutable |
| *(new)* | `ipam.<name>` | `MapNestedAttribute` Optional | Immutable |
| *(was `addons[]`)* | `addon.<name>` | `MapNestedAttribute` Optional | Immutable |
| *(new)* | `<map>.<name>.fetch_config.{owner,repository,url,oci}` | `SingleNestedAttribute` Optional | Immutable |
| *(new)* | `<map>.<name>.resource_config` | `StringAttribute` (YAML/JSON) Optional | **Mutable** (in-place re-apply) |
| *(new)* | `cni.provider` / `cni.version` / `cni.values` | `SingleNestedAttribute` Optional | **Mutable** (Talos bootstrap only) |
| `control_plane_machine_count` | `topology.control_plane.replicas` | `Int64Attribute` Optional | **Mutable** |
| *(new)* | `topology.control_plane.infrastructure` | `StringAttribute` (YAML/JSON) Optional | **Mutable** (template rotation) |
| `worker_machine_count` | `topology.workers.machine_deployments[].replicas` | `ListNestedAttribute` Optional | **Mutable** |
| *(new)* | `topology.workers.machine_deployments[].{infrastructure,bootstrap}` | `StringAttribute` (YAML/JSON) Optional | **Mutable** (template rotation) |
| *(new)* | `manifests.management` | `ListAttribute` Optional | **Mutable** (re-applied) |
| *(new)* | `inventory.manage` | `BoolAttribute` Optional, default true | **Mutable** |
| *(new)* | `inventory.source` | `StringAttribute` Optional | **Mutable** |
| *(new)* | `inventory.machine[]` | `ListNestedAttribute` Optional | **Mutable** (re-applied as Hardware/Machine/Secret) |
| *(new)* | `management.bootstrap.mode` | `StringAttribute` Optional, `pivot`/`in_place` | Immutable |
| *(new)* | `management.bootstrap.kairos.{iso,address,ssh}` | `SingleNestedAttribute` Optional | Immutable |
| *(new)* | `management.addons.{prerequisites,helm,manifests}` | `SingleNestedAttribute` Optional | **Mutable** (re-installed on update) |
| *(new)* | `management.bootstrap.addons.prerequisites` | `ListAttribute` Optional | Immutable |
| `wait_for_ready` | `wait.enabled` | `BoolAttribute` Optional | **Mutable** |
| *(new)* | `wait.timeout` | `StringAttribute` Optional | **Mutable** |
| `kubeconfig_path` | `output.kubeconfig_path` | `StringAttribute` Optional | **Mutable** |
| `endpoint` | `status.endpoint` | `StringAttribute` Computed | Computed |
| `kubeconfig` | `status.kubeconfig` | `StringAttribute` Computed+Sensitive | Computed |
| `cluster_ca_certificate` | `status.ca_certificate` | `StringAttribute` Computed+Sensitive | Computed |
| `cluster_description` | `status.description` | `StringAttribute` Computed | Computed |
| `bootstrap_cluster_name` | `status.bootstrap_cluster` | `StringAttribute` Computed | Computed |

