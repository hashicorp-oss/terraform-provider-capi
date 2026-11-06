# Cluster API IPAM for node addresses, backed by the UniFi controller.
#
# CAPT claims each machine's address from the IPPool named in the machine
# templates' addressFromPool and writes it into the Hardware's DHCP
# reservation; the IPAM provider reserves the same address as a fixed-IP
# client on the UniFi network containing the pool. All three objects live in
# the machines' namespace: the claim's poolRef carries no namespace, and the
# provider reads the credentials Secret from the pool's.

resource "kubernetes_secret_v1" "unifi_credentials" {
  metadata {
    name      = "unifi-credentials"
    namespace = var.namespace
  }

  # The provider rejects a Secret without "apiKey".
  data = { apiKey = var.unifi_api_key }
  type = "Opaque"
}

resource "capi_object" "unifi_instance" {
  api_version = "unifi.ipam.cluster.x-k8s.io/v1beta2"
  kind        = "Instance"
  name        = "unifi"
  namespace   = var.namespace

  spec = {
    host           = var.unifi.api_url
    credentialsRef = { name = kubernetes_secret_v1.unifi_credentials.metadata[0].name }
    site           = var.unifi.site
    insecure       = var.unifi.allow_insecure
  }
}

resource "capi_object" "ip_pool" {
  api_version = "unifi.ipam.cluster.x-k8s.io/v1beta2"
  kind        = "IPPool"
  name        = local.ip_pool_name
  namespace   = var.namespace
  labels      = { "cluster.x-k8s.io/cluster-name" = var.cluster_name }

  spec = {
    instanceRef = {
      name      = capi_object.unifi_instance.name
      namespace = var.namespace
    }
    gateway = var.gateway_ip
    prefix  = local.node_prefix
    # networkId is left unset: the provider finds the UniFi network whose
    # range contains start/prefix.
    subnets = [{
      start = var.node_ip_range.start
      end   = var.node_ip_range.end
    }]
    # Pins each node that carries an address to it: the claim is named after
    # the Hardware, not the machine, so a replaced machine on the same board
    # keeps its address. Nodes without one draw from the range.
    preAllocations = { for name, node in local.pinned_nodes : "${name}-${local.ip_pool_name}" => node.interfaces[0].ip }
  }

  lifecycle {
    precondition {
      condition = alltrue([
        for node in local.pinned_nodes :
        local.ipv4_int[node.interfaces[0].ip] >= local.ipv4_int[var.node_ip_range.start] &&
        local.ipv4_int[node.interfaces[0].ip] <= local.ipv4_int[var.node_ip_range.end]
      ])
      error_message = "Every pinned node IP must fall inside node_ip_range: the IPAM provider refuses a preAllocation outside the pool's subnets, and the machine would never get an address."
    }
  }
}
