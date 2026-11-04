# A Tinkerbell cluster whose first control-plane node is provisioned with
# Talos through its BMC and then adopted in place by Cluster API: the node
# becomes the management cluster, the provider registers every inventory
# machine with Tinkerbell, and the Talos control-plane provider adopts the
# running node as the first Machine. No pivot, no re-image.
#
# The provider generates the CAPI objects itself (Cluster, TinkerbellCluster,
# TalosControlPlane, MachineDeployments and their templates) from the
# configured providers; each provider's resource_config is merged into the
# spec of its object. Kinds and API versions come from the CRDs installed on
# the management cluster, so nothing here names an apiVersion.
#
# In-place contract (see docs/superpowers/specs/2026-11-03-bootstrap-topologies-design.md):
# the bootstrap node runs what CAPI renders for a control-plane machine, i.e.
# control_plane.talos.resource_config strategic patches with PROVIDER_ID
# substituted, plus install-time settings. Its hostname and address are also
# written to Talos META so an in-place config update cannot move it. The
# bootstrap machine's MAC needs a DHCP reservation for its ip_address: Talos
# maintenance mode is DHCP-only.
locals {
  talos_version = "v1.13"
  cluster_name  = "bm-cluster"
  namespace     = "tinkerbell"
  tinkerbell_ip = "192.168.1.111" # Cilium L2 VIP the Tinkerbell Service takes

  # Talos metal raw image the Tinkerbell workflow streams to disk. Use an
  # Image Factory schematic whose extraKernelArgs include
  # talos.config=http://<tinkerbell_ip>:7080/2009-04-04/user-data so the node
  # fetches the CAPI machine config (CAPT writes it to the Hardware userdata).
  talos_image_url = "https://factory.talos.dev/image/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba/v1.13.10/metal-amd64.raw.xz"

  # Talos machine-config patches shared by every node. CAPT replaces the
  # literal PROVIDER_ID in the bootstrap data with the Tinkerbell provider ID;
  # the provider does the same for the bootstrap node.
  talos_common_patch = yamlencode({
    machine = {
      install = { disk = "/dev/nvme0n1" }
      kubelet = { extraArgs = { provider-id = "PROVIDER_ID" } }
    }
  })

  # The bootstrap node must schedule the platform pods on its own, and the
  # CAPI render has to agree or the first in-place update taints it.
  talos_scheduling_patch = yamlencode({
    cluster = { allowSchedulingOnControlPlanes = true }
  })
}

resource "capi_cluster" "bare_metal" {
  name               = local.cluster_name
  kubernetes_version = "v1.34.0"

  # Cluster.spec.clusterNetwork and controlPlaneEndpoint. The VIP is served by
  # Talos (see the control plane strategic patch below). In place, the
  # bootstrap node's endpoint defaults to this host.
  network = {
    control_plane_endpoint = { host = "192.168.1.100", port = 6443 }
    pods                   = { cidr_blocks = ["10.244.0.0/16"] }
    services               = { cidr_blocks = ["10.96.0.0/12"] }
  }

  # Providers are keyed by name, like the cluster-api-operator Helm values.
  # fetch_config.owner switches to a fork; repository and the components file
  # are inferred from clusterctl's built-in entry, and the version is pinned in
  # the resulting release URL.
  core = {
    cluster-api = {
      manager = {
        feature_gates = { ClusterTopology = true, MachinePool = true }
      }
    }
  }

  infrastructure = {
    tinkerbell = {
      version      = "v0.7.9"
      fetch_config = { owner = "tinkerbell-community" }
      manager      = { feature_gates = { ClusterTopology = true } }

      # TinkerbellCluster.spec: the cluster-wide Tinkerbell workflow streams
      # the Talos metal image to the first disk and reboots into it. Talos then
      # fetches its machine config from the Tinkerbell metadata service.
      resource_config = yamlencode({
        templateInline = <<-EOT
          version: "0.1"
          name: talos
          global_timeout: 6000
          tasks:
            - name: talos
              worker: "{{.device_1}}"
              volumes:
                - /dev:/dev
                - /dev/console:/dev/console
                - /lib/firmware:/lib/firmware:ro
              actions:
                - name: stream-talos-image
                  image: quay.io/tinkerbell/actions/image2disk
                  timeout: 1200
                  environment:
                    IMG_URL: ${local.talos_image_url}
                    DEST_DISK: {{ index .Hardware.Disks 0 }}
                    COMPRESSED: true
                - name: reboot
                  image: ghcr.io/jacobweinstock/waitdaemon:latest
                  timeout: 90
                  pid: host
                  environment:
                    IMAGE: quay.io/tinkerbell/actions/reboot
                    WAIT_SECONDS: 5
                  volumes:
                    - /var/run/docker.sock:/var/run/docker.sock
        EOT
      })
    }
  }

  bootstrap = {
    talos = {
      version      = "v0.9.0"
      fetch_config = { owner = "sidero-community" }

      # Default TalosConfigTemplate.spec for every machine deployment
      # (generateType = "worker" is set by the provider).
      resource_config = yamlencode({
        talosVersion     = local.talos_version
        strategicPatches = [local.talos_common_patch]
      })
    }
  }

  control_plane = {
    talos = {
      version      = "v0.8.0"
      fetch_config = { owner = "sidero-community" }

      # TalosControlPlane.spec (generateType = "controlplane", version,
      # replicas and the machine template ref are set by the provider). In
      # place, these strategic patches are also what the bootstrap node runs.
      resource_config = yamlencode({
        controlPlaneConfig = {
          controlplane = {
            talosVersion = local.talos_version
            strategicPatches = [
              local.talos_common_patch,
              local.talos_scheduling_patch,
              yamlencode({
                machine = {
                  network = {
                    interfaces = [{
                      deviceSelector = { physical = true }
                      dhcp           = true
                      vip            = { ip = "192.168.1.100" }
                    }]
                  }
                }
              }),
            ]
          }
        }
      })
    }
  }

  # Providers clusterctl does not know need the repository name too.
  ipam = {
    unifi = {
      version      = "v0.4.1"
      fetch_config = { owner = "ubiquiti-community", repository = "cluster-api-ipam-provider-unifi" }
    }
  }

  addon = { helm = {} }

  # Objects the generated cluster references, applied to the management
  # cluster after the providers are initialized: here the UniFi IPAM instance
  # and the pool the machine templates draw node addresses from. The
  # bootstrap node's address is pre-allocated so its claim resolves to the
  # address it already runs on.
  manifests = {
    management = [
      yamlencode({
        apiVersion = "v1"
        kind       = "Secret"
        metadata   = { name = "unifi-credentials", namespace = local.namespace }
        stringData = { apiKey = var.unifi_api_key }
      }),
      yamlencode({
        apiVersion = "unifi.ipam.cluster.x-k8s.io/v1beta2"
        kind       = "Instance"
        metadata   = { name = "unifi", namespace = local.namespace }
        spec       = { host = "https://192.168.1.1", credentialsRef = { name = "unifi-credentials" }, site = "default", insecure = true }
      }),
      yamlencode({
        apiVersion = "unifi.ipam.cluster.x-k8s.io/v1beta2"
        kind       = "IPPool"
        metadata   = { name = "${local.cluster_name}-nodes", namespace = local.namespace }
        spec = {
          instanceRef    = { name = "unifi", namespace = local.namespace }
          gateway        = "192.168.1.1"
          prefix         = 24
          subnets        = [{ start = "192.168.1.10", end = "192.168.1.40" }]
          preAllocations = { "cp-1-${local.cluster_name}-nodes" = "192.168.1.10" }
        }
      }),
    ]
  }

  # Mirrors Cluster.spec.topology. infrastructure is the TinkerbellMachineTemplate
  # spec of each group: pin control plane and worker machines to hardware by
  # the inventory labels and draw their addresses from the IPAM pool.
  topology = {
    control_plane = {
      replicas = 3
      infrastructure = yamlencode({
        hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "cp" } } }] }
        addressFromPool  = { apiGroup = "unifi.ipam.cluster.x-k8s.io", kind = "IPPool", name = "${local.cluster_name}-nodes" }
      })
    }
    workers = {
      machine_deployments = [
        {
          name     = "md-0"
          replicas = 2
          infrastructure = yamlencode({
            hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker" } } }] }
            addressFromPool  = { apiGroup = "unifi.ipam.cluster.x-k8s.io", kind = "IPPool", name = "${local.cluster_name}-nodes" }
          })
        }
      ]
    }
  }

  management = {
    self_managed = true
    namespace    = local.namespace

    bootstrap = {
      type    = "talos"
      mode    = "in_place" # the node is adopted; "pivot" moves CAPI off it and resets it instead
      machine = "cp-1"

      boot = {
        method   = "auto" # virtual media first, then UEFI HTTP boot
        timeout  = "15m"
        attempts = 3
      }

      talos = {
        version      = "v1.13.10"
        architecture = "amd64"

        image = {
          extensions  = ["siderolabs/iscsi-tools"]
          kernel_args = ["net.ifnames=0"]
        }
      }

      # Releases only the bootstrap node needs go in bootstrap.addons; the
      # Tinkerbell stack belongs in management.addons below, which follows the
      # management role (in place: this node; pivot: the workload cluster too).
    }

    # Management-plane addons, installed by the provider right after the CAPI
    # providers on every cluster that manages, in order: prerequisites, helm,
    # manifests. The Tinkerbell Service takes a Cilium L2 VIP, so the pool and
    # the announcement policy are prerequisites (they need Cilium's CRDs and
    # must exist before Helm waits for the Service to get an address).
    addons = {
      prerequisites = [
        yamlencode({
          apiVersion = "cilium.io/v2"
          kind       = "CiliumLoadBalancerIPPool"
          metadata   = { name = "tinkerbell" }
          spec       = { blocks = [{ cidr = "${local.tinkerbell_ip}/32" }] }
        }),
        yamlencode({
          apiVersion = "cilium.io/v2alpha1"
          kind       = "CiliumL2AnnouncementPolicy"
          metadata   = { name = "tinkerbell" }
          spec       = { loadBalancerIPs = true, interfaces = ["^eth[0-9]+"] }
        }),
      ]
      helm = [
        {
          name      = "tinkerbell"
          namespace = local.namespace
          chart     = "oci://ghcr.io/tinkerbell-community/charts/tinkerbell"
          version   = "0.23.4"
          timeout   = "15m"
          values = yamlencode({
            service = {
              lbClass     = "io.cilium/l2-announcer"
              annotations = { "lbipam.cilium.io/ips" = local.tinkerbell_ip }
            }
            deployment = {
              envs = {
                dhcp = { mode = "auto-proxy", tftpIp = local.tinkerbell_ip, syslogIp = local.tinkerbell_ip, ipxeHttpBinaryHost = local.tinkerbell_ip, ipxeHttpScriptHost = local.tinkerbell_ip }
                ipxe = { httpScriptOsieUrl = "http://${local.tinkerbell_ip}:7080/images", scriptTinkServerAddrPort = "${local.tinkerbell_ip}:42113" }
              }
            }
          })
        },
      ]
    }
  }

  # First-class CNI: replaces Talos's built-in flannel with Cilium. This
  # disables the built-in CNI and kube-proxy (KubePrism serves the API server)
  # and installs the Cilium Helm release with kube-proxy replacement. Extra
  # values are deep-merged over those defaults; L2 announcements carry the
  # Tinkerbell VIP.
  cni = {
    provider = "cilium"
    version  = "1.20.1"

    # Install the Gateway API CRDs before Cilium and enable gatewayAPI in the
    # Cilium values. Omit this block to keep Gateway API off.
    gateway_api = {
      enabled = true
      version = "v1.3.0"
    }

    values = yamlencode({
      l2announcements = { enabled = true }
      hubble          = { relay = { enabled = true }, ui = { enabled = true } }
    })

    # For a fuller bare-metal setup (native routing, BGP, Gateway API), see
    # cilium-values.yaml and use:
    #   values = file("${path.module}/cilium-values.yaml")
  }

  # Every machine becomes a Tinkerbell Hardware, a rufio Machine and a BMC
  # Secret in the cluster namespace (inventory.manage defaults to true). The
  # bootstrap machine's Hardware is marked provisioned with PXE off so CAPT
  # adopts the running node; the others are netboot targets.
  inventory = {
    machine = [
      {
        hostname = "cp-1"
        network = {
          ip_address  = "192.168.1.10"
          netmask     = "255.255.255.0"
          gateway     = "192.168.1.1"
          mac_address = "aa:bb:cc:dd:ee:01"
          nameservers = ["192.168.1.1"]
        }
        disk = { device = "/dev/nvme0n1" }
        bmc = {
          address  = "192.168.2.10"
          username = "admin"
          password = var.bmc_password
        }
        labels = { type = "cp" }
      },
      {
        hostname = "cp-2"
        network  = { ip_address = "192.168.1.11", netmask = "255.255.255.0", gateway = "192.168.1.1", mac_address = "aa:bb:cc:dd:ee:02" }
        disk     = { device = "/dev/nvme0n1" }
        bmc      = { address = "192.168.2.11", username = "admin", password = var.bmc_password }
        labels   = { type = "cp" }
      },
      {
        hostname = "cp-3"
        network  = { ip_address = "192.168.1.12", netmask = "255.255.255.0", gateway = "192.168.1.1", mac_address = "aa:bb:cc:dd:ee:03" }
        disk     = { device = "/dev/nvme0n1" }
        bmc      = { address = "192.168.2.12", username = "admin", password = var.bmc_password }
        labels   = { type = "cp" }
      },
      {
        hostname = "worker-1"
        network  = { ip_address = "192.168.1.20", netmask = "255.255.255.0", gateway = "192.168.1.1", mac_address = "aa:bb:cc:dd:ee:11" }
        disk     = { device = "/dev/sda" }
        bmc      = { address = "https://192.168.2.20", username = "admin", password = var.bmc_password }
        labels   = { type = "worker" }
      },
      {
        hostname = "worker-2"
        network  = { ip_address = "192.168.1.21", netmask = "255.255.255.0", gateway = "192.168.1.1", mac_address = "aa:bb:cc:dd:ee:12" }
        disk     = { device = "/dev/nvme0n1" }
        # Intel AMT: the port selects rufio's IntelAMT provider.
        bmc    = { address = "https://192.168.1.21:16993", username = "admin", password = var.amt_password }
        labels = { type = "worker" }
      },
    ]
  }
}

variable "bmc_password" {
  type      = string
  sensitive = true
}

variable "amt_password" {
  type      = string
  sensitive = true
}

variable "unifi_api_key" {
  type      = string
  sensitive = true
}
