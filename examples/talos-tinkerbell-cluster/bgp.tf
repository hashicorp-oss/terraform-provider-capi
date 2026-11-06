# Talos native BGP on the control planes, so the cluster endpoint answers from
# the first control plane CAPI brings up.
#
# The endpoint (virtual_ip) is an anycast /32 that every control plane
# originates while its own apiserver is ready: talos-vip-watcher binds it to a
# dummy link when https://127.0.0.1:6443/readyz answers and removes it
# otherwise, and the Talos `fabric` instance advertises that link to the
# router. Nothing in that path needs Kubernetes, Cilium or a LoadBalancer
# Service, so CAPI reaches the new cluster through its endpoint as soon as the
# first apiserver is up, and the router ECMPs the API across the healthy
# control planes after that.
#
# Cilium does not peer with the router. Two BGP speakers cannot both hold
# TCP/179 in one routing domain, so it peers with a `workload` instance bound
# to a VRF on its own node, reached over a veth pair, and the fabric
# re-originates what Cilium advertises (pod CIDRs, ClusterIPs, LoadBalancer
# and external IPs). `installRoutes: false` keeps those routes in the
# instances' RIBs and out of the host's main table, where a service VIP would
# blackhole on nodes without a backend. This is the topology Talos' own
# integration test runs (internal/integration/api/bgp_cilium.go), and what
# cluster-bootstrap's modules/data-lookup/modules/config-patches/bgp.tf
# renders.

locals {
  bgp_upstream_address = coalesce(var.bgp.upstream_address, var.gateway_ip)

  # A /127 from a ULA prefix, as in the Talos test (a /126 would make Linux
  # join the subnet-router anycast ::0 on the VRF end too). The ::0 end
  # (veth_name) stays in the default namespace and is what Cilium sources its
  # session from; the ::1 end (veth_peer_name) moves into the VRF and is the
  # address Cilium dials.
  bgp_veth_cilium        = cidrhost(var.bgp.veth_prefix, 0)
  bgp_veth_talos         = cidrhost(var.bgp.veth_prefix, 1)
  bgp_veth_prefix_length = split("/", var.bgp.veth_prefix)[1]

  # What the fabric takes from Cilium and sends upstream. Import selectors may
  # not overlap, so the pools are listed separately, never as a supernet.
  bgp_import_prefixes = concat([var.pod_cidr, var.service_cidr], var.bgp.service_lb_cidrs)

  bgp_documents = [
    {
      apiVersion = "v1alpha1"
      kind       = "VethConfig"
      name       = var.bgp.veth_name
      up         = true
      addresses  = [{ address = "${local.bgp_veth_cilium}/${local.bgp_veth_prefix_length}" }]
      peer = {
        name      = var.bgp.veth_peer_name
        up        = true
        addresses = [{ address = "${local.bgp_veth_talos}/${local.bgp_veth_prefix_length}" }]
      }
    },
    {
      apiVersion = "v1alpha1"
      kind       = "VRFConfig"
      name       = var.bgp.vrf_name
      up         = true
      links      = [var.bgp.veth_peer_name]
      table      = tostring(var.bgp.vrf_table)
    },

    # Talos' side of the Cilium session: VRF-bound, passive (Cilium dials),
    # RIB-only. Its router-id never leaves the VRF, so one value fits every node.
    {
      apiVersion    = "v1alpha1"
      kind          = "BGPInstanceConfig"
      name          = var.bgp.workload_instance
      vrf           = var.bgp.vrf_name
      localASN      = var.bgp.workload_asn
      routerID      = var.bgp.workload_router_id
      installRoutes = false
      neighbors = [{
        address = local.bgp_veth_cilium
        peerASN = var.bgp.cilium_asn
        passive = true
      }]
    },

    # The router-facing instance. No routerID on purpose: Talos takes the
    # lowest IPv4 across the advertised links, which is the node's own address
    # only while it sorts below the VIP (the precondition on
    # capi_control_plane_template.this). Advertising the upstream link also
    # originates the node network's connected prefix, which the router's own
    # connected route outranks.
    {
      apiVersion    = "v1alpha1"
      kind          = "BGPInstanceConfig"
      name          = var.bgp.fabric_instance
      localASN      = var.bgp.local_asn
      installRoutes = false
      advertise     = [var.bgp.upstream_link, var.bgp.vip_link_name]
      importRoutes = [{
        bgpInstance = var.bgp.workload_instance
        prefixes    = local.bgp_import_prefixes
      }]
      neighbors = [{
        address  = local.bgp_upstream_address
        peerASN  = var.bgp.upstream_asn
        holdTime = var.bgp.hold_time
      }]
    },

    # The dummy and veth links add node addresses, and etcd advertises from
    # all of them by default; its peer URLs stay on the node network.
    { cluster = { etcd = { advertisedSubnets = var.node_networks } } },

    # Binds the VIP while the local apiserver is ready. Root with NET_ADMIN;
    # the watcher's README warns against runAs (the capability would not
    # survive the exec into a non-root process).
    {
      apiVersion = "v1alpha1"
      kind       = "ContainerConfig"
      name       = "talos-vip-watcher"
      image      = var.bgp.vip_watcher_image
      args = [
        "--vip=${var.virtual_ip}/32",
        "--iface=${var.bgp.vip_link_name}",
        "--url=https://127.0.0.1:6443/readyz",
        "--interval=${var.bgp.vip_watcher_interval}",
      ]
      network   = { mode = "host" }
      security  = { capabilities = { add = ["NET_ADMIN"] } }
      dependsOn = { networks = ["addresses"] }
    },
  ]

  # One patch per document, keys unquoted as for strategic_patches. Filtered
  # with a `for` because a conditional's branches would have to unify an empty
  # tuple with a six-element one.
  bgp_patches = [
    for document in local.bgp_documents :
    replace(replace(yamlencode(document), "/\"([a-zA-Z0-9_-]+)\":/", "$1:"), "<<~EOT\n", "")
    if var.bgp.enabled
  ]
}

# Plan-time checks the preconditions on capi_control_plane_template.this read.
locals {
  vip_int = local.ipv4_int[var.virtual_ip]

  vip_in_node_pool = local.vip_int >= local.ipv4_int[var.node_ip_range.start] && local.vip_int <= local.ipv4_int[var.node_ip_range.end]

  # A pinned control plane's router-id candidate is its own address; one that
  # draws from the pool could get any address in it, the highest included.
  bgp_router_ids_unique = alltrue([
    for node in var.control_plane_nodes :
    (node.interfaces[0].ip != null ? local.ipv4_int[node.interfaces[0].ip] : local.ipv4_int[var.node_ip_range.end]) < local.vip_int
  ])

  # IPv4 containment by masking the VIP to each CIDR's length (cidrsubnet
  # normalises host bits away). IPv6 CIDRs cannot contain an IPv4 VIP.
  vip_in_node_networks = {
    for cidr in var.node_networks : cidr => cidrsubnet("${var.virtual_ip}/${split("/", cidr)[1]}", 0, 0) == cidrsubnet(cidr, 0, 0)
    if !strcontains(cidr, ":")
  }
  vip_in_lb_pools = {
    for cidr in var.bgp.service_lb_cidrs : cidr => cidrsubnet("${var.virtual_ip}/${split("/", cidr)[1]}", 0, 0) == cidrsubnet(cidr, 0, 0)
    if !strcontains(cidr, ":")
  }
}

# Cilium's side of the peering, for a cluster whose Cilium nothing else
# configures (cluster-bootstrap's base chart renders these objects on talos;
# leave bgp.cilium_peering off there so they keep one owner). Applied through
# the cluster's kubeconfig Secret once Cilium's CRDs are on the cluster: install
# Cilium first (a capi_helm_chart_proxy) and add it to depends_on.
locals {
  cilium_bgp_objects = [
    {
      apiVersion = "cilium.io/v2"
      kind       = "CiliumBGPAdvertisement"
      metadata   = { name = "bgp-advertisements", labels = var.bgp.advertisement_labels }
      spec = {
        advertisements = [
          { advertisementType = "PodCIDR" },
          {
            advertisementType = "Service"
            service           = { addresses = ["ClusterIP", "ExternalIP", "LoadBalancerIP"] }
            selector          = { matchExpressions = [{ key = "cilium-lb-disabled", operator = "DoesNotExist" }] }
          },
        ]
      }
    },
    # Cilium runs where the Talos instances are: the control planes.
    {
      apiVersion = "cilium.io/v2"
      kind       = "CiliumBGPClusterConfig"
      metadata   = { name = "cilium-bgp" }
      spec = {
        nodeSelector = { matchExpressions = [{ key = "node-role.kubernetes.io/control-plane", operator = "Exists" }] }
        bgpInstances = [{
          name     = "talos-workload"
          localASN = var.bgp.cilium_asn
          peers = [{
            name          = "talos-workload"
            peerASN       = var.bgp.workload_asn
            peerAddress   = local.bgp_veth_talos
            peerConfigRef = { name = "cilium-peer" }
          }]
        }]
      }
    },
    # A directly connected veth peer: no multihop. The session runs over IPv6
    # and carries IPv4 prefixes (RFC 8950 extended next-hop).
    {
      apiVersion = "cilium.io/v2"
      kind       = "CiliumBGPPeerConfig"
      metadata   = { name = "cilium-peer" }
      spec = {
        gracefulRestart = { enabled = true, restartTimeSeconds = 15 }
        transport       = { peerPort = 179, sourceInterface = var.bgp.veth_name }
        families = [{
          afi            = "ipv4"
          safi           = "unicast"
          advertisements = { matchLabels = var.bgp.advertisement_labels }
        }]
      }
    },
  ]
}

resource "capi_cluster_resource_set" "cilium_bgp" {
  count = var.bgp.enabled && var.bgp.cilium_peering ? 1 : 0

  name      = "${var.cluster_name}-cilium-bgp"
  namespace = var.namespace
  cluster   = capi_cluster.this.name
  strategy  = "Reconcile"
  manifests = [join("---\n", [for object in local.cilium_bgp_objects : yamlencode(object)])]
  prune     = true
  timeouts  = { create = "30m", update = "30m" }
}
