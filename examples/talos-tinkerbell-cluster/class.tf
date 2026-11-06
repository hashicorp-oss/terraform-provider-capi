# The ClusterClass and its templates.
#
# Each template is a launch-template-style resource: the object is named
# <name>-<8 hex of the spec> and a spec change creates a new revision, moves
# `ref`, and through the class rolls the machines that use it. Older
# revisions are kept while anything refers to them.
#
# cluster-bootstrap's modules/cluster sets five class variables on the
# Cluster (talosVersion, imageFactory, strategicPatches,
# workerStrategicPatches, ipPool) and patches them into one shared template
# per kind. Here every template is already per class, so the values are
# written into the templates themselves and the class carries no variables
# or patches. The rendered machine configuration is the same.

locals {
  cluster_labels = { "cluster.x-k8s.io/cluster-name" = var.cluster_name }

  # Referencing the pool resource orders every machine template after it.
  address_from_pool = {
    apiGroup = "unifi.ipam.cluster.x-k8s.io"
    kind     = "IPPool"
    name     = capi_object.ip_pool.name
  }
}

# TinkerbellClusterTemplate. No endpoint: CAPT falls back to
# Cluster.spec.controlPlaneEndpoint, which capi_cluster sets from the VIP.
resource "capi_machine_template" "cluster" {
  name           = var.cluster_name
  namespace      = var.namespace
  infrastructure = "tinkerbell"
  role           = "cluster"
  labels         = local.cluster_labels
  spec           = {}
}

# TalosControlPlaneTemplate. The control planes alone carry the BGP
# documents (bgp.tf): they are the nodes that answer for the API VIP and that
# Cilium peers from.
resource "capi_control_plane_template" "this" {
  name          = "${var.cluster_name}-control-plane"
  namespace     = var.namespace
  control_plane = "talos"
  labels        = local.cluster_labels

  spec = {
    controlPlaneConfig = {
      controlplane = {
        generateType     = "controlplane"
        talosVersion     = local.talos_version
        imageFactory     = local.image_factory_specs.controlplane
        strategicPatches = concat(local.strategic_patches, local.bgp_patches)
      }
    }
    # A single-VIP control plane cannot surge.
    rolloutStrategy = { type = "RollingUpdate", rollingUpdate = { maxSurge = 0 } }
  }

  lifecycle {
    precondition {
      condition     = !var.bgp.enabled || !local.vip_in_node_pool
      error_message = "virtual_ip lies inside node_ip_range: the IPAM pool could hand the API VIP to a node."
    }
    precondition {
      condition     = !var.bgp.enabled || local.bgp_router_ids_unique
      error_message = "Every control-plane address, and with an unpinned control plane every pool address, must sort below virtual_ip. Talos takes the fabric instance's router-id from the lowest IPv4 across its advertised links; above the VIP every control plane would share the VIP as its id, and each bind or withdraw would reset the sessions."
    }
    precondition {
      condition     = !var.bgp.enabled || (length(var.node_networks) > 0 && !anytrue(values(local.vip_in_node_networks)))
      error_message = "With bgp.enabled, node_networks must name the node network and must not contain virtual_ip: etcd is pinned to it so it never advertises from the VIP link."
    }
    precondition {
      condition     = !var.bgp.enabled || !anytrue(values(local.vip_in_lb_pools))
      error_message = "virtual_ip lies inside one of bgp.service_lb_cidrs: a Service could claim it, and two speakers would advertise the same /32."
    }
  }
}

# TinkerbellMachineTemplate of the control-plane machines.
resource "capi_machine_template" "control_plane" {
  name           = "${var.cluster_name}-control-plane"
  namespace      = var.namespace
  infrastructure = "tinkerbell"
  labels         = local.cluster_labels

  spec = {
    templateInline   = local.install_templates.staged
    bootOptions      = { bootMode = "netboot" }
    hardwareAffinity = { required = [{ labelSelector = { matchLabels = { "tinkerbell.org/role" = "controlplane" } } }] }
    addressFromPool  = local.address_from_pool
  }
}

# TinkerbellMachineTemplate per worker class: boards of the class's variant,
# plus auto-discovered boards without a role when allow_discovered is set.
resource "capi_machine_template" "worker" {
  for_each = local.worker_deployments

  name           = "${var.cluster_name}-workers-${replace(each.key, "_", "-")}"
  namespace      = var.namespace
  infrastructure = "tinkerbell"
  labels         = local.cluster_labels

  spec = {
    templateInline = local.install_templates[each.value.install_workflow]
    bootOptions    = { bootMode = "netboot" }
    hardwareAffinity = {
      required = concat(
        [{ labelSelector = { matchLabels = { "tinkerbell.org/role" = "worker", "tinkerbell.org/variant" = each.value.variant } } }],
        each.value.allow_discovered ? [{ labelSelector = { matchExpressions = [
          { key = "tinkerbell.org/auto-discovered", operator = "In", values = ["true"] },
          { key = "tinkerbell.org/role", operator = "DoesNotExist" },
        ] } }] : [],
      )
      preferred = [{
        weight               = 100
        hardwareAffinityTerm = { labelSelector = { matchLabels = { "tinkerbell.org/role" = "worker" } } }
      }]
    }
    addressFromPool = local.address_from_pool
  }
}

# TalosConfigTemplate per worker class, carrying the group's own schematic
# and patches (the class variable overrides of the original).
resource "capi_bootstrap_template" "worker" {
  for_each = local.worker_deployments

  name      = "${var.cluster_name}-workers-${replace(each.key, "_", "-")}"
  namespace = var.namespace
  bootstrap = "talos"
  labels    = local.cluster_labels

  spec = {
    generateType     = "worker"
    talosVersion     = local.talos_version
    imageFactory     = each.value.image_factory
    strategicPatches = each.value.strategic_patches
  }
}

resource "capi_cluster_class" "this" {
  name      = var.cluster_name
  namespace = var.namespace

  infrastructure = capi_machine_template.cluster.ref

  control_plane = {
    template       = capi_control_plane_template.this.ref
    infrastructure = capi_machine_template.control_plane.ref
  }

  workers = {
    for name in keys(local.worker_deployments) : name => {
      infrastructure = capi_machine_template.worker[name].ref
      bootstrap      = capi_bootstrap_template.worker[name].ref
    }
  }
}
