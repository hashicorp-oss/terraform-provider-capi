output "node_variants" {
  description = "Variant label per node: the key each worker class selects boards by."
  value       = local.node_variants
}

output "worker_deployments" {
  description = "Derived worker groups: variant, replicas or autoscaling bounds, schematic and install workflow."
  value       = local.worker_deployments
}

output "template_names" {
  description = "Current revision of every template the ClusterClass references (<name>-<8 hex of the spec>)."
  value = {
    cluster               = capi_machine_template.cluster.object_name
    control_plane         = capi_control_plane_template.this.object_name
    control_plane_machine = capi_machine_template.control_plane.object_name
    worker_machine        = { for name, t in capi_machine_template.worker : name => t.object_name }
    worker_config         = { for name, t in capi_bootstrap_template.worker : name => t.object_name }
  }
}

output "cluster" {
  description = "The Cluster: id (namespace/name), endpoint and conditions."
  value = {
    id         = capi_cluster.this.id
    endpoint   = capi_cluster.this.status.endpoint
    conditions = capi_cluster.this.status.conditions
  }
}

output "machine_deployments" {
  description = "Realised MachineDeployment per worker group, with its replica counts."
  value = {
    for name, md in capi_machine_deployment.worker : name => {
      object_name = md.object_name
      status      = md.status
    }
  }
}

output "bgp" {
  description = "The BGP endpoints, for whoever configures the other speaker (Cilium's chart values, the router's peer group): the address and interface Cilium peers over, both ASNs of that session, and the VIP the control planes originate."
  value = var.bgp.enabled ? {
    talos_address    = local.bgp_veth_talos
    cilium_address   = local.bgp_veth_cilium
    source_interface = var.bgp.veth_name
    talos_asn        = var.bgp.workload_asn
    cilium_asn       = var.bgp.cilium_asn
    fabric_asn       = var.bgp.local_asn
    upstream_address = local.bgp_upstream_address
    api_vip          = var.virtual_ip
  } : null
}
