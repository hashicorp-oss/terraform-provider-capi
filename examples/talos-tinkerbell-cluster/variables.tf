variable "actions" {
  description = "Registry and tag of the tinkerbell-community actions (taloscmdline, talosmeta) the staged install workflow runs after image2disk. The tag is a commit SHA so the rendered workflow is reproducible."
  type = object({
    repository = optional(string, "ghcr.io/tinkerbell-community/actions")
    tag        = optional(string, "5e3a4da0e403460d628ade7088536535c0a9e1e6")
  })
  default  = {}
  nullable = false
}

variable "cluster_name" {
  description = "Name of the Cluster, its ClusterClass and the prefix of every template revision."
  type        = string
  nullable    = false
}

variable "namespace" {
  description = "Namespace of the Cluster, its templates, Hardware and IPAM objects. CAPT looks for Hardware here and the IPAM claim's poolRef carries no namespace, so everything shares it."
  type        = string
  nullable    = false
}

# Same shape as modules/cluster in cluster-bootstrap, so callers pass the same
# values. Object type constraints drop undeclared attributes, so a caller's
# operating_systems lookup, ipxe_binary and bmc.provider_options are accepted and
# ignored: capi_hardware has no equivalent yet (README.md, "Gaps").
variable "control_plane_nodes" {
  description = "Control-plane machines. The first one already runs the cluster (it was stood up outside CAPI) and is adopted rather than imaged."
  type = list(object({
    name         = string
    machine_type = string
    arch         = string
    interfaces = list(object({
      mac = string
      ip  = optional(string)
    }))
    disks = list(object({
      device = string
    }))
    bmc = object({
      host = string
      port = number
      auth = optional(object({
        username = string
        password = string
      }))
    })
    operating_system_lookup_key = optional(string)
  }))
  nullable = false
}

variable "worker_nodes" {
  description = "Worker machines, same shape as control_plane_nodes. Their count per variant is a deployment's default replica count."
  type = list(object({
    name         = string
    machine_type = string
    arch         = string
    interfaces = list(object({
      mac = string
      ip  = optional(string)
    }))
    disks = list(object({
      device = string
    }))
    bmc = object({
      host = string
      port = number
      auth = optional(object({
        username = string
        password = string
      }))
    })
    operating_system_lookup_key = optional(string)
  }))
  default  = []
  nullable = false
}

variable "gateway_ip" {
  description = "Gateway of the node network."
  type        = string
  nullable    = false
}

variable "netmask" {
  description = "Netmask of the node network, dotted decimal."
  type        = string
  default     = "255.255.0.0"
  nullable    = false
}

variable "node_ip_range" {
  description = "Inclusive range the IPAM pool hands to nodes; every pinned node IP must fall inside it."
  type = object({
    start = string
    end   = string
  })
  nullable = false
}

variable "virtual_ip" {
  description = <<-EOT
    The cluster endpoint (Cluster.spec.controlPlaneEndpoint.host). With
    bgp.enabled it is also the API anycast address: every control plane whose
    apiserver answers /readyz binds it and originates it to the router, so the
    endpoint answers from the first control plane on, before Cilium or any
    LoadBalancer Service exists. Talos adds the endpoint to the apiserver
    certificate itself.
  EOT
  type        = string
  nullable    = false
}

variable "node_networks" {
  description = "CIDRs of the node network (cluster-bootstrap's `networks`). With bgp.enabled etcd advertises only from these, because the VIP and veth links add node addresses it must not pick. Must not contain virtual_ip."
  type        = list(string)
  default     = []
  nullable    = false
}

variable "bgp" {
  description = <<-EOT
    Talos native BGP (1.14) on the control planes, as cluster-bootstrap's
    config-patches module renders it. The Talos `fabric` instance peers with the
    router (`upstream_address`, default gateway_ip) and originates
    `virtual_ip` while talos-vip-watcher holds it. Cilium peers with a
    VRF-bound `workload` instance on its own node over a veth pair, and the
    fabric re-originates what Cilium advertises: the pod and service CIDRs plus
    `service_lb_cidrs`.

    ASNs: `upstream_asn` is the router's, `local_asn` the fabric's (what the
    router's peer group expects), `workload_asn` and `cilium_asn` the two ends
    of the veth session. All four must differ.

    `cilium_peering` also delivers Cilium's side (CiliumBGPClusterConfig,
    CiliumBGPPeerConfig, CiliumBGPAdvertisement) to the cluster as a
    ClusterResourceSet. Leave it off where something else owns those objects;
    cluster-bootstrap's base chart renders them on talos.

    Do not also pass the BGP documents in strategic_patches: Talos appends
    lists when it merges two patches of one document, so neighbours, addresses
    and the watcher's args would be doubled.
  EOT
  type = object({
    enabled              = optional(bool, true)
    upstream_address     = optional(string)
    upstream_asn         = optional(number, 65000)
    local_asn            = optional(number, 65001)
    workload_asn         = optional(number, 4200000002)
    cilium_asn           = optional(number, 4200000003)
    hold_time            = optional(string, "90s")
    service_lb_cidrs     = optional(list(string), [])
    upstream_link        = optional(string, "eth0")
    vip_watcher_image    = optional(string, "ghcr.io/sidero-community/talos-vip-watcher:sha-ae905f0@sha256:a2a5595e68c036bdca9c9b87e9c6ad835cdec85daf578d1d0cc56e3686b75932")
    vip_watcher_interval = optional(string, "2s")
    workload_router_id   = optional(string, "169.254.0.1")
    vip_link_name        = optional(string, "vip-api")
    veth_name            = optional(string, "veth-workload")
    veth_peer_name       = optional(string, "veth-router")
    veth_prefix          = optional(string, "fda1:b2c3:d4e5:1::/127")
    vrf_name             = optional(string, "vrf-workload")
    vrf_table            = optional(number, 88)
    fabric_instance      = optional(string, "fabric")
    workload_instance    = optional(string, "workload")
    cilium_peering       = optional(bool, false)
    advertisement_labels = optional(map(string), { advertise = "bgp" })
  })
  default  = {}
  nullable = false

  validation {
    condition     = length(distinct([var.bgp.upstream_asn, var.bgp.local_asn, var.bgp.workload_asn, var.bgp.cilium_asn])) == 4
    error_message = "bgp.upstream_asn, local_asn, workload_asn and cilium_asn must all differ."
  }
}

variable "pod_cidr" {
  description = "Pod CIDR."
  type        = string
  nullable    = false
}

variable "service_cidr" {
  description = "Service CIDR."
  type        = string
  nullable    = false
}

variable "tinkerbell_ip" {
  description = "IP of the Tinkerbell stack; the install workflows point talos.config at tootles on it."
  type        = string
  nullable    = false
}

variable "kubernetes_version" {
  description = "Kubernetes version of the cluster, with or without the v prefix."
  type        = string
  nullable    = false
}

variable "talos_version" {
  description = "Talos version every TalosConfig is generated for (vX.Y or vX.Y.Z). CAPT only resolves Image Factory schematics when the bootstrap config carries a full vX.Y.Z."
  type        = string
  nullable    = false

  validation {
    condition     = can(regex("^v?[0-9]+\\.[0-9]+(\\.[0-9]+)?$", var.talos_version))
    error_message = "talos_version must be pinned (vX.Y or vX.Y.Z); CABPT and CACPPT would otherwise pick their own default."
  }
}

variable "image_factory" {
  description = "Image Factory schematic of the control-plane machines (TalosControlPlane controlPlaneConfig.controlplane.imageFactory). CABPT registers it and renders machine.install.image from it."
  type = object({
    extensions        = optional(list(string), [])
    extra_kernel_args = optional(list(string), [])
    overlay = optional(object({
      name  = string
      image = string
    }))
    bootloader = optional(string)
  })
  nullable = false

  validation {
    condition     = var.image_factory.bootloader == null || contains(["auto", "dual-boot", "grub", "sd-boot"], var.image_factory.bootloader)
    error_message = "image_factory.bootloader must be one of auto, dual-boot, grub or sd-boot."
  }
}

variable "strategic_patches" {
  description = "Talos configuration patches for the control plane, as objects (yamlencoded here)."
  type        = any
  default     = []
  nullable    = false
  sensitive   = true
}

variable "worker_deployments" {
  description = <<-EOT
    Worker node groups, keyed by name. Each entry is a worker class of the
    ClusterClass and one capi_machine_deployment. `variant` (default: the key)
    is the tinkerbell.org/variant Hardware label its machines claim;
    `allow_discovered` also admits auto-discovered boards without a role.
    `image_factory` and `strategic_patches` are this group's own (YAML
    documents rendered by the caller). `replicas` defaults to the worker nodes
    of the variant; with `autoscaling.enabled` the cluster autoscaler owns the
    count. `install_workflow` is `staged` (image2disk, taloscmdline,
    talosmeta) or `talos2disk` (needed when the schematic adds extensions).
  EOT
  type = map(object({
    variant          = optional(string)
    allow_discovered = optional(bool, false)
    replicas         = optional(number)
    image_factory = object({
      extensions        = optional(list(string), [])
      extra_kernel_args = optional(list(string), [])
      overlay = optional(object({
        name  = string
        image = string
      }))
      bootloader = optional(string)
    })
    strategic_patches = optional(list(string), [])
    install_workflow  = optional(string, "staged")
    autoscaling = optional(object({
      enabled  = optional(bool, false)
      min_size = optional(number, 1)
      max_size = optional(number)
    }), {})
  }))
  default  = {}
  nullable = false

  validation {
    condition = alltrue([
      for name, d in var.worker_deployments :
      contains(["amd64", "amd64_amd", "amd64_intel", "arm64", "arm64_rpi_5", "arm64_rpi_generic"], coalesce(d.variant, name))
    ])
    error_message = "worker_deployments[*].variant (or the key) must be an Image Factory schematic variant: amd64, amd64_amd, amd64_intel, arm64, arm64_rpi_5 or arm64_rpi_generic."
  }

  validation {
    condition = alltrue([
      for name, d in var.worker_deployments :
      d.image_factory.bootloader == null || contains(["auto", "dual-boot", "grub", "sd-boot"], d.image_factory.bootloader)
    ])
    error_message = "worker_deployments[*].image_factory.bootloader must be one of auto, dual-boot, grub or sd-boot."
  }

  validation {
    condition     = alltrue([for d in values(var.worker_deployments) : contains(["staged", "talos2disk"], d.install_workflow)])
    error_message = "worker_deployments[*].install_workflow must be staged or talos2disk."
  }
}

variable "unifi" {
  description = "UniFi controller the IPAM provider reserves node addresses on."
  type = object({
    api_url        = string
    site           = optional(string, "default")
    allow_insecure = optional(bool, false)
  })
  nullable = false
}

variable "unifi_api_key" {
  description = "API key the IPAM provider authenticates to the UniFi controller with."
  type        = string
  nullable    = false
  sensitive   = true
}
