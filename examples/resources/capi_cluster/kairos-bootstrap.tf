# A Tinkerbell cluster bootstrapped from a disposable node: a spare machine (a
# NUC, say) is booted from a Kairos live ISO that runs k3s from RAM, either
# through its BMC or by hand. The provider uses that node as the management
# cluster only until Cluster API has pivoted to the new control plane, then
# powers it off and ejects the media. Nothing is written to its disk, and
# afterwards it is an ordinary netboot target like every other inventory
# machine.
#
# Build the ISO once from kairos-cloud-config.yaml (its header has the AuroraBoot
# command): management.bootstrap.kairos.iso is the ISO it produces, served over
# HTTP for BMC virtual media (an Intel AMT machine can also take a local path,
# streamed over storage redirection). The media stays attached until the node is
# powered off.
#
# Sequence (docs/superpowers/specs/2026-11-04-provider-design.md §4.3): boot the
# live node -> clusterctl init + bootstrap.addons there (a Tinkerbell on host
# networking) -> inventory, manifests.management and the cluster objects -> the
# first control-plane machine netboots from the live node -> the Cluster is
# paused, in-flight Workflows finish, the live node's Tinkerbell is uninstalled
# -> CAAPH brings up the HelmChartProxies below on the new cluster (Cilium, the
# load-balancer pool, the cluster's own Tinkerbell) -> clusterctl move -> the
# live node is powered off and netboots as a member. At most one Tinkerbell
# answers DHCP at any step.
locals {
  talos_version = "v1.13"
  cluster_name  = "bm-cluster"
  namespace     = "tinkerbell"

  # Where the Tinkerbell stack answers. On the live node it binds the node's
  # own address (host networking); on the cluster its Service takes a Cilium
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

  # Talos ships flannel and kube-proxy; Cilium replaces both, so every machine
  # config CAPI renders carries this patch (cni-none.yaml) and Cilium is
  # installed with the Talos-required values in cilium-values.yaml.
  talos_cni_patch = file("${path.module}/cni-none.yaml")

  cluster_selector = { matchLabels = { "cluster.x-k8s.io/cluster-name" = local.cluster_name } }

  # The cluster's own addons, as CAAPH objects on the management cluster. They
  # are declared on the live node, installed on the new cluster as soon as its
  # control plane is initialized, waited for before the move, and moved with
  # the cluster. The helm addon provider below installs CAAPH.
  cilium_proxy = yamlencode({
    apiVersion = "addons.cluster.x-k8s.io/v1alpha1"
    kind       = "HelmChartProxy"
    metadata   = { name = "cilium", namespace = local.namespace }
    spec = {
      clusterSelector   = local.cluster_selector
      repoURL           = "oci://quay.io/cilium/charts"
      chartName         = "cilium"
      version           = "1.20.1" # a tag that exists on the registry; quay keeps only recent patch releases
      releaseName       = "cilium"
      namespace         = "kube-system"
      reconcileStrategy = "Continuous"
      valuesTemplate    = file("${path.module}/cilium-values.yaml")
    }
  })

  # Objects the cluster's Tinkerbell depends on that need Cilium's CRDs: its
  # load-balancer pool and the L2 announcement of the VIP.
  tinkerbell_network_crs = yamlencode({
    apiVersion = "v1"
    kind       = "ConfigMap"
    metadata   = { name = "tinkerbell-network", namespace = local.namespace }
    data = {
      "pool.yaml" = yamlencode({
        apiVersion = "cilium.io/v2"
        kind       = "CiliumLoadBalancerIPPool"
        metadata   = { name = "tinkerbell" }
        spec       = { blocks = [{ cidr = "${local.tinkerbell_ip}/32" }] }
      })
      "l2.yaml" = yamlencode({
        apiVersion = "cilium.io/v2alpha1"
        kind       = "CiliumL2AnnouncementPolicy"
        metadata   = { name = "tinkerbell" }
        spec       = { loadBalancerIPs = true, interfaces = ["^eth[0-9]+"] }
      })
    }
  })
  tinkerbell_network_set = yamlencode({
    apiVersion = "addons.cluster.x-k8s.io/v1beta2"
    kind       = "ClusterResourceSet"
    metadata   = { name = "tinkerbell-network", namespace = local.namespace }
    spec = {
      clusterSelector = local.cluster_selector
      strategy        = "Reconcile"
      resources       = [{ kind = "ConfigMap", name = "tinkerbell-network" }]
    }
  })

  tinkerbell_proxy = yamlencode({
    apiVersion = "addons.cluster.x-k8s.io/v1alpha1"
    kind       = "HelmChartProxy"
    metadata   = { name = "tinkerbell", namespace = local.namespace }
    spec = {
      clusterSelector   = local.cluster_selector
      repoURL           = "oci://ghcr.io/tinkerbell-community/charts"
      chartName         = "tinkerbell"
      version           = "0.23.4"
      releaseName       = "tinkerbell"
      namespace         = local.namespace
      reconcileStrategy = "Continuous"
      # The cluster's instance, customized for the cluster: a LoadBalancer
      # Service on the VIP, smee as a proxy DHCP server. Use the keys of your
      # chart version's values.yaml.
      valuesTemplate = yamlencode({
        publicIP = local.tinkerbell_ip
        service  = { type = "LoadBalancer", loadBalancerIP = local.tinkerbell_ip }
        deployment = {
          envs = {
            dhcp = { mode = "auto-proxy", tftpIp = local.tinkerbell_host, syslogIp = local.tinkerbell_host, ipxeHttpBinaryHost = local.tinkerbell_host, ipxeHttpScriptHost = local.tinkerbell_host }
            ipxe = { httpScriptOsieUrl = "http://${local.tinkerbell_host}:7080/images", scriptTinkServerAddrPort = "${local.tinkerbell_host}:42113" }
          }
        }
      })
    }
  })
}

resource "capi_cluster" "kairos" {
  name               = local.cluster_name
  namespace          = local.namespace
  kubernetes_version = "v1.34.0"

  network = {
    control_plane_endpoint = { host = "192.168.1.100" }
    pods                   = { cidr_blocks = ["10.244.0.0/16"] }
    services               = { cidr_blocks = ["10.96.0.0/12"] }
  }

  # TinkerbellCluster.spec: stream the Talos image to disk and reboot.
  infrastructure = {
    provider = "tinkerbell"
    spec = {
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
    }
  }

  # TalosControlPlane.spec.
  control_plane = {
    provider = "talos"
    replicas = 3
    # The cluster renders its own control-plane machine template: a
    # capi_machine_template cannot precede the cluster whose bootstrap installs
    # the CRDs. A change is a new hash-named template and a rollout.
    machine_spec = { hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "cp" } } }] } }
    spec = {
      controlPlaneConfig = {
        controlplane = {
          talosVersion = local.talos_version
          strategicPatches = [
            local.talos_cni_patch,
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
    }
  }

  management = {
    self_managed = true # required: the live node is disposable, CAPI must move off it

    # The providers of a self-managed cluster: installed on the live node and
    # again on the cluster before the move. On an external management cluster
    # these are capi_provider resources instead.
    providers = {
      infrastructure = {
        tinkerbell = {
          version      = "v0.7.9"
          fetch_config = { owner = "tinkerbell-community" }
        }
      }
      bootstrap = {
        talos = {
          version      = "v0.8.2"
          fetch_config = { owner = "sidero-community" }
          # Default worker TalosConfigTemplate.spec for capi_bootstrap_template
          # resources that leave it to the provider.
          resource_config = yamlencode({
            talosVersion     = local.talos_version
            strategicPatches = [local.talos_cni_patch, local.talos_common_patch]
          })
        }
      }
      control_plane = {
        talos = {
          version      = "v0.8.0"
          fetch_config = { owner = "sidero-community" }
        }
      }
      # CAAPH, which installs the HelmChartProxies in manifests.management.
      addon = { helm = {} }
    }

    bootstrap = {
      type       = "kairos"
      machine    = "nuc"    # inventory.machine[].hostname: its bmc boots the ISO; no disk is needed
      on_failure = "resume" # keep what exists on a failure and continue from status.phase next apply

      boot = {
        method   = "virtual_media" # or "manual": boot the node from a USB stick yourself; no BMC is driven
        device   = "usb"           # BMCs that present virtual media as a USB device (NanoKVM, Intel AMT USB-R)
        timeout  = "10m"           # per attempt, until the node answers over SSH
        attempts = 3
      }

      kairos = {
        iso     = "http://files.lab.internal/kairos/kairos-bootstrap.iso"
        address = local.bootstrap_ip # the static address the cloud-config gives the node
        ssh = {
          user        = "kairos"
          private_key = file("~/.ssh/kairos-bootstrap")
          # host_key = file("${path.module}/kairos_host_key.pub") # only if the ISO embeds a host key
        }
      }

      # The live node's own Tinkerbell, on host networking so it answers on the
      # node's address with no load balancer. It netboots the first
      # control-plane machine and is uninstalled before the move; from then on
      # the cluster's own instance (tinkerbell_proxy above) is the only one.
      addons = {
        helm = [
          {
            name      = "tinkerbell"
            namespace = local.namespace
            chart     = "oci://ghcr.io/tinkerbell-community/charts/tinkerbell"
            version   = "0.23.4"
            timeout   = "15m"
            # Use the keys of your chart version's values.yaml.
            values = yamlencode({
              publicIP   = local.bootstrap_ip
              deployment = { hostNetwork = true }
              service    = { type = "ClusterIP" }
            })
          },
        ]
      }
    }
  }

  # Applied to the live node before the cluster objects and again to the
  # cluster after the move: the cluster's addon declarations (CAAPH brings
  # them up on the cluster once its control plane is initialized), IPAM pools,
  # anything the cluster objects reference. After the pivot, more addons are
  # capi_helm_chart_proxy and capi_cluster_resource_set resources.
  manifests = {
    management = [
      local.cilium_proxy,
      local.tinkerbell_network_crs,
      local.tinkerbell_network_set,
      local.tinkerbell_proxy,
    ]
  }

  # The seed inventory: every machine becomes a Tinkerbell Hardware, a rufio
  # Machine and a BMC Secret on whichever cluster manages. The bootstrap
  # machine is left out while it hosts the management cluster and added after
  # the pivot. Machines added later are capi_hardware resources.
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
    conditions = ["Available"]
  }

  # The whole pivot: bootstrap node, providers, first control plane,
  # handover, move and readiness.
  timeouts = {
    create = "3h"
    delete = "2h"
  }
}

# Workers join after the pivot as node groups on the cluster, which now
# manages itself: point the provider block at it through the kubeconfig
# ephemeral.capi_kubeconfig derives from provider_secrets.
resource "capi_machine_template" "worker" {
  name           = "${local.cluster_name}-worker"
  namespace      = local.namespace
  infrastructure = "tinkerbell"
  api_version    = "infrastructure.cluster.x-k8s.io/v1beta1"
  kind           = "TinkerbellMachineTemplate"
  spec           = { hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker" } } }] } }
  depends_on     = [capi_cluster.kairos]
}

resource "capi_bootstrap_template" "worker" {
  name        = "${local.cluster_name}-worker"
  namespace   = local.namespace
  bootstrap   = "talos"
  api_version = "bootstrap.cluster.x-k8s.io/v1alpha3"
  kind        = "TalosConfigTemplate"
  spec = {
    generateType     = "worker"
    talosVersion     = local.talos_version
    strategicPatches = [local.talos_cni_patch, local.talos_common_patch]
  }
  depends_on = [capi_cluster.kairos]
}

resource "capi_machine_deployment" "md_0" {
  cluster   = capi_cluster.kairos.name
  namespace = local.namespace
  name      = "md-0"
  replicas  = 1
  version   = "v1.34.0"
  template = {
    infrastructure = capi_machine_template.worker.ref
    bootstrap      = capi_bootstrap_template.worker.ref
  }
}

variable "bmc_passwords" {
  type      = map(string)
  sensitive = true
}
