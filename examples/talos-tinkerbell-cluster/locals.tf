# Nodes
locals {
  # Every node of either role, keyed by its name, which is also the Hardware's.
  # CAPT names a machine's IPAddressClaim "<hardware>-<pool>", so the same key
  # pins the node's address in the IPPool (ipam.tf).
  nodes = { for node in concat(var.control_plane_nodes, var.worker_nodes) : node.name => node }

  # The tinkerbell.org/variant label: the Image Factory variant the node runs,
  # which the worker classes' hardwareAffinity selects on.
  node_variants = { for name, node in local.nodes : name => coalesce(node.operating_system_lookup_key, node.arch) }

  # Nodes with an address on their primary interface; the rest draw one from
  # the pool when claimed.
  pinned_nodes = { for name, node in local.nodes : name => node if node.interfaces[0].ip != null }

  # The first control-plane node already runs the cluster: its Hardware is
  # adopted (provisioned, off PXE) instead of imaged.
  bootstrap_node = try(var.control_plane_nodes[0].name, null)
}

# Versions and Talos configuration
locals {
  talos_version      = format("v%s", trimprefix(var.talos_version, "v"))
  kubernetes_version = format("v%s", trimprefix(var.kubernetes_version, "v"))

  # yamlencode quotes every key; the Talos config bundle wants bare ones.
  strategic_patches = [
    for patch in var.strategic_patches : replace(replace(yamlencode(patch), "/\"([a-zA-Z0-9_-]+)\":/", "$1:"), "<<~EOT\n", "")
  ]

  # imageFactory as CABPT's CRD spells it, unset parts left out so the
  # template matches the live object field for field.
  image_factory_specs = {
    for name, spec in merge({ controlplane = var.image_factory }, { for n, d in var.worker_deployments : n => d.image_factory }) :
    name => merge(
      length(spec.extensions) > 0 ? { extensions = spec.extensions } : {},
      length(spec.extra_kernel_args) > 0 ? { extraKernelArgs = spec.extra_kernel_args } : {},
      spec.overlay != null ? { overlay = spec.overlay } : {},
      spec.bootloader != null ? { bootloader = spec.bootloader } : {},
    )
  }
}

# Install workflows (the tink Template both machine templates inline). Only the
# deploy-level values are rendered here; the {{ .Hardware.* }} directives are
# the workflow controller's.
locals {
  talos_config_url = "http://${var.tinkerbell_ip}:7080/2009-04-04/user-data"

  install_templates = {
    staged = templatefile("${path.module}/templates/template-data.yaml", {
      actions_repository = var.actions.repository
      actions_tag        = var.actions.tag
      talos_config_url   = local.talos_config_url
    })
    talos2disk = templatefile("${path.module}/templates/template-data-talos2disk.yaml", {
      talos_config_url = local.talos_config_url
    })
  }
}

# IPAM
locals {
  ip_pool_name = "${var.cluster_name}-nodes"

  # The IPAM contract wants a prefix length; var.netmask is dotted decimal.
  node_prefix = index([for i in range(33) : cidrnetmask("0.0.0.0/${i}")], var.netmask)

  # IPv4 addresses as integers, so the pool bounds can be checked at plan.
  ipv4_int = {
    for ip in distinct(concat([var.node_ip_range.start, var.node_ip_range.end, var.virtual_ip], [for node in local.pinned_nodes : node.interfaces[0].ip])) :
    ip => sum([for i, octet in split(".", ip) : tonumber(octet) * pow(256, 3 - i)])
  }
}

# Worker groups
locals {
  # One worker class and one capi_machine_deployment per entry. replicas
  # defaults to the worker nodes of the variant; autoscaling bounds default
  # the same way and capi_machine_deployment renders the autoscaler
  # annotations and leaves replicas to the autoscaler.
  worker_deployments = {
    for name, d in var.worker_deployments : name => {
      variant          = coalesce(d.variant, name)
      allow_discovered = d.allow_discovered
      install_workflow = d.install_workflow
      image_factory    = local.image_factory_specs[name]
      # yamlencode quotes every key; the Talos config bundle wants bare ones.
      strategic_patches = [for patch in d.strategic_patches : replace(replace(patch, "/\"([a-zA-Z0-9_-]+)\":/", "$1:"), "<<~EOT\n", "")]
      replicas = coalesce(d.replicas, length([
        for node in var.worker_nodes : node if coalesce(node.operating_system_lookup_key, node.arch) == coalesce(d.variant, name)
      ]))
      autoscaling = d.autoscaling.enabled ? {
        min = d.autoscaling.min_size
        max = coalesce(d.autoscaling.max_size, length([
          for node in var.worker_nodes : node if coalesce(node.operating_system_lookup_key, node.arch) == coalesce(d.variant, name)
        ]))
      } : null
    }
  }
}
