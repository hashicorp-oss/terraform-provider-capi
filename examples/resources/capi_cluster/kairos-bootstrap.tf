# A Tinkerbell cluster bootstrapped from a disposable node: a spare machine (a
# NUC, say) is booted through its BMC from a Kairos live ISO that runs k3s from
# RAM. The provider uses that node as the management cluster only until Cluster
# API has pivoted to the new control plane, then powers it off and ejects the
# media. Nothing is written to its disk, and afterwards it is an ordinary netboot
# target like every other inventory machine.
#
# Build the ISO once from kairos-cloud-config.yaml (its header has the AuroraBoot
# command): management.bootstrap.kairos.iso is the ISO it produces, served over
# HTTP for BMC virtual media (an Intel AMT machine can also take a local path,
# streamed over storage redirection). The media stays attached until the node is
# powered off.
#
# Sequence (docs/superpowers/specs/2026-11-03-bootstrap-topologies-design.md §6):
# boot the live node -> clusterctl init + management.addons (Tinkerbell) there ->
# inventory and cluster objects -> first control-plane machine -> the cni release
# on the new cluster -> no Workflow in flight -> clusterctl init + management.addons
# on the new cluster -> clusterctl move (Hardware and BMC Machines move; their
# Secrets are re-applied) -> live node powered off -> cluster Ready.
locals {
  talos_version = "v1.13"
  cluster_name  = "bm-cluster"
  namespace     = "tinkerbell"

  # Where the Tinkerbell stack answers. On the live node its Service takes the
  # node's own address through k3s ServiceLB; on the cluster it takes a Cilium
  # L2 VIP. A machine reads talos.config once, on its first boot, from whichever
  # stack provisions it, so the install image names the stack by a DNS name:
  # point it at the live node while the cluster bootstraps and at the VIP after
  # the pivot. The provider does not manage DNS.
  bootstrap_ip    = "192.168.1.50"
  tinkerbell_ip   = "192.168.1.111"
  tinkerbell_host = "tinkerbell.lab.internal"

  # Talos metal raw image the Tinkerbell workflow streams to disk, from an Image
  # Factory schematic whose extraKernelArgs include
  # talos.config=http://${local.tinkerbell_host}:7080/2009-04-04/user-data.
  talos_image_url = "https://factory.talos.dev/image/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba/v1.13.10/metal-amd64.raw.xz"

  # Shared Talos machine-config patch; CAPT substitutes PROVIDER_ID.
  talos_common_patch = yamlencode({
    machine = {
      install = { disk = "/dev/nvme0n1" }
      kubelet = { extraArgs = { provider-id = "PROVIDER_ID" } }
    }
  })
}

resource "capi_cluster" "kairos" {
  name               = local.cluster_name
  kubernetes_version = "v1.34.0"

  network = {
    control_plane_endpoint = { host = "192.168.1.100" }
    pods                   = { cidr_blocks = ["10.244.0.0/16"] }
    services               = { cidr_blocks = ["10.96.0.0/12"] }
  }

  infrastructure = {
    tinkerbell = {
      version      = "v0.7.9"
      fetch_config = { owner = "tinkerbell-community" }

      # TinkerbellCluster.spec: stream the Talos image to disk and reboot.
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
      version      = "v0.8.2"
      fetch_config = { owner = "sidero-community" }
      resource_config = yamlencode({
        talosVersion     = local.talos_version
        strategicPatches = [local.talos_common_patch]
      })
    }
  }

  # cni below injects its Talos patch into these strategic patches too, so the
  # machines CAPI renders run without flannel.
  control_plane = {
    talos = {
      version      = "v0.8.0"
      fetch_config = { owner = "sidero-community" }
      resource_config = yamlencode({
        controlPlaneConfig = {
          controlplane = {
            talosVersion = local.talos_version
            strategicPatches = [
              local.talos_common_patch,
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

  topology = {
    control_plane = {
      replicas       = 3
      infrastructure = yamlencode({ hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "cp" } } }] } })
    }
    workers = {
      machine_deployments = [
        {
          name           = "md-0"
          replicas       = 1
          infrastructure = yamlencode({ hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker" } } }] } })
        }
      ]
    }
  }

  management = {
    self_managed = true # required: the live node is disposable, CAPI must move off it
    namespace    = local.namespace

    bootstrap = {
      type    = "kairos"
      machine = "nuc" # inventory.machine[].hostname: its bmc boots the ISO; no disk is needed

      boot = {
        method   = "virtual_media" # a live ISO boots from virtual media; "http" is for a Talos UKI
        device   = "usb"           # BMCs that present virtual media as a USB device (NanoKVM, Intel AMT USB-R)
        timeout  = "10m"           # per attempt, until the node answers over SSH
        attempts = 3
      }

      kairos = {
        iso     = "http://files.lab.internal/kairos/kairos.iso.custom.iso"
        address = local.bootstrap_ip # the static address the cloud-config gives the node
        ssh = {
          user        = "kairos"
          private_key = file("~/.ssh/kairos-bootstrap")
          # host_key = file("${path.module}/kairos_host_key.pub") # only if the ISO embeds a host key
        }
      }

      # Releases only the live node needs; the Tinkerbell stack belongs in
      # management.addons so the cluster gets it too.
    }

    # Installed by the provider right after the CAPI providers on both the live
    # node and the cluster. k3s ServiceLB gives the Tinkerbell LoadBalancer
    # Service the live node's address; on the cluster Cilium LB-IPAM gives it
    # the VIP, so the pool and the announcement policy are prerequisites, which
    # are applied only on the final management cluster.
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
            # smee answers as a proxy DHCP server, so the stack's own address
            # only has to be reachable by the machines it boots.
            deployment = {
              envs = {
                dhcp = { mode = "auto-proxy", tftpIp = local.tinkerbell_host, syslogIp = local.tinkerbell_host, ipxeHttpBinaryHost = local.tinkerbell_host, ipxeHttpScriptHost = local.tinkerbell_host }
                ipxe = { httpScriptOsieUrl = "http://${local.tinkerbell_host}:7080/images", scriptTinkServerAddrPort = "${local.tinkerbell_host}:42113" }
              }
            }
          })
        },
      ]
    }
  }

  # The cluster's CNI. With a Kairos bootstrap the live node keeps k3s's own
  # networking; this release is installed on the new cluster as soon as its
  # control plane initializes, and its Talos patch goes into every machine
  # config CAPI renders. Requires control_plane["talos"].
  cni = {
    provider = "cilium"
    version  = "1.20.1"
    values = yamlencode({
      l2announcements = { enabled = true }
    })
  }

  # Every machine becomes a Tinkerbell Hardware, a rufio Machine and a BMC
  # Secret on whichever cluster manages. The bootstrap machine is left out
  # while it hosts the management cluster and added after the pivot.
  inventory = {
    machine = [
      {
        hostname = "nuc"
        network = {
          ip_address  = local.bootstrap_ip
          netmask     = "255.255.255.0"
          gateway     = "192.168.1.1"
          mac_address = "aa:bb:cc:dd:ee:50"
        }
        bmc    = { address = "192.168.2.50", username = "admin", password = var.bmc_passwords["nuc"] }
        labels = { type = "worker" }
      },
      {
        hostname = "cp-1"
        network  = { ip_address = "192.168.1.10", netmask = "255.255.255.0", gateway = "192.168.1.1", mac_address = "aa:bb:cc:dd:ee:01" }
        disk     = { device = "/dev/nvme0n1" }
        bmc      = { address = "192.168.2.10", username = "admin", password = var.bmc_passwords["cp-1"] }
        labels   = { type = "cp" }
      },
      {
        hostname = "cp-2"
        network  = { ip_address = "192.168.1.11", netmask = "255.255.255.0", gateway = "192.168.1.1", mac_address = "aa:bb:cc:dd:ee:02" }
        disk     = { device = "/dev/nvme0n1" }
        bmc      = { address = "192.168.2.11", username = "admin", password = var.bmc_passwords["cp-2"] }
        labels   = { type = "cp" }
      },
      {
        hostname = "cp-3"
        network  = { ip_address = "192.168.1.12", netmask = "255.255.255.0", gateway = "192.168.1.1", mac_address = "aa:bb:cc:dd:ee:03" }
        disk     = { device = "/dev/nvme0n1" }
        bmc      = { address = "192.168.2.12", username = "admin", password = var.bmc_passwords["cp-3"] }
        labels   = { type = "cp" }
      },
    ]
  }

  wait = {
    enabled = true
    timeout = "90m"
  }

  output = {
    kubeconfig_path = "~/.kube/${local.cluster_name}.kubeconfig"
  }
}

variable "bmc_passwords" {
  type      = map(string)
  sensitive = true
}
