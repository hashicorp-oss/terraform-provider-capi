# One Tinkerbell Hardware per node, with its rufio Machine and BMC Secret
# when the node has BMC credentials (modules/node in cluster-bootstrap).
#
# capi_hardware applies with server-side apply under its own field manager,
# so the labels CAPT stamps on a claimed Hardware (ownerName, ownerNamespace)
# are never fought over and a changed variant label is applied in place: no
# separate kubernetes_labels resource is needed. Deleting claimed Hardware is
# refused (set force to power the board off and delete anyway).
resource "capi_hardware" "node" {
  for_each = local.nodes

  name      = each.key
  namespace = var.namespace
  cluster   = var.cluster_name

  labels = {
    "tinkerbell.org/role"    = each.value.machine_type
    "tinkerbell.org/variant" = local.node_variants[each.key]
  }

  # The node already running the cluster: marked provisioned and kept off
  # PXE, so CAPT adopts it instead of re-imaging it.
  adopted = each.key == local.bootstrap_node

  network = {
    mac_address = each.value.interfaces[0].mac
    # Only a pinned address is seeded; without one CAPT writes the
    # reservation from the IPAM claim once a machine binds the Hardware.
    ip_address  = each.value.interfaces[0].ip
    gateway     = var.gateway_ip
    netmask     = var.netmask
    nameservers = [var.gateway_ip, "1.1.1.1"]
  }

  disk = { device = each.value.disks[0].device }

  # Redfish with basic auth, or Intel AMT on ports 16992/16993.
  bmc = each.value.bmc.auth == null ? null : {
    address  = format("%s:%d", each.value.bmc.host, each.value.bmc.port)
    username = each.value.bmc.auth.username
    password = each.value.bmc.auth.password
  }
}
