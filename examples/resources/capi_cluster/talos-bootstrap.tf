# A Tinkerbell cluster whose bootstrap (management) cluster is a single
# bare-metal machine provisioned with Talos through its BMC instead of kind.
# After the self-managed pivot the node is reset, powered off, and free for
# CAPT to reclaim into the workload cluster.
#
# The provider generates the CAPI objects itself (Cluster, TinkerbellCluster,
# TalosControlPlane, MachineDeployments and their templates) from the
# configured providers; each provider's resource_config is merged into the
# spec of its object. Kinds and API versions come from the CRDs installed on
# the management cluster, so nothing here names an apiVersion.
locals {
  talos_version = "v1.13"

  # Talos metal raw image the Tinkerbell workflow streams to disk. Use an
  # Image Factory schematic whose extraKernelArgs include
  # talos.config=http://<hegel>:50061/2009-04-04/user-data so the node fetches
  # the CAPI machine config (CAPT writes it to the Hardware userdata).
  talos_image_url = "https://factory.talos.dev/image/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba/v1.13.10/metal-amd64.raw.xz"

  # Talos machine-config patch shared by every node. CAPT replaces the literal
  # PROVIDER_ID in the bootstrap data with the Tinkerbell provider ID.
  talos_common_patch = yamlencode({
    machine = {
      install = { disk = "/dev/nvme0n1" }
      kubelet = { extraArgs = { provider-id = "PROVIDER_ID" } }
    }
  })
}

resource "capi_cluster" "bare_metal" {
  name               = "bm-cluster"
  kubernetes_version = "v1.34.0"

  # Cluster.spec.clusterNetwork and controlPlaneEndpoint. The VIP is served by
  # Talos (see the control plane strategic patch below).
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
      version      = "v0.8.2"
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
      version      = "v0.7.1"
      fetch_config = { owner = "sidero-community" }

      # TalosControlPlane.spec (generateType = "controlplane", version,
      # replicas and the machine template ref are set by the provider).
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

  # Providers clusterctl does not know need the repository name too.
  ipam = {
    unifi = {
      version      = "v0.4.1"
      fetch_config = { owner = "ubiquiti-community", repository = "cluster-api-ipam-provider-unifi" }
    }
  }

  addon = { helm = {} }

  # Mirrors Cluster.spec.topology. infrastructure is the TinkerbellMachineTemplate
  # spec of each group: pin control plane and worker machines to hardware by
  # the inventory labels.
  topology = {
    control_plane = {
      replicas       = 3
      infrastructure = yamlencode({ hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "cp" } } }] } })
    }
    workers = {
      machine_deployments = [
        {
          name           = "md-0"
          replicas       = 2
          infrastructure = yamlencode({ hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker" } } }] } })
        }
      ]
    }
  }

  management = {
    self_managed = true

    bootstrap = {
      type    = "talos"
      machine = "cp-1"

      boot = {
        method   = "auto" # virtual media first, then UEFI HTTP boot
        timeout  = "15m"
        attempts = 3
      }

      talos = {
        version      = "v1.13.6"
        architecture = "amd64"

        image = {
          extensions  = ["siderolabs/iscsi-tools"]
          kernel_args = ["net.ifnames=0"]
        }

      }
    }
  }

  # First-class CNI: replaces Talos's built-in flannel with Cilium. This
  # disables the built-in CNI and kube-proxy (KubePrism serves the API server)
  # and installs the Cilium Helm release with kube-proxy replacement. Extra
  # values are deep-merged over those defaults.
  cni = {
    provider = "cilium"
    version  = "1.20.1"

    # Install the Gateway API CRDs before Cilium and enable gatewayAPI in the
    # Cilium values. Omit this block to keep Gateway API off.
    gateway_api = {
      enabled = true
      version = "v1.3.0"
    }

    # The provider already sets the Talos-required defaults (KubePrism
    # kube-proxy replacement, Kubernetes IPAM, cgroup and securityContext).
    # Add anything beyond that here; it is deep-merged over the defaults.
    values = yamlencode({
      hubble = { relay = { enabled = true }, ui = { enabled = true } }
    })

    # For a fuller bare-metal setup (native routing, BGP, L2, Gateway API),
    # see cilium-values.yaml and use:
    #   values = file("${path.module}/cilium-values.yaml")
  }

  inventory = {
    machine = [
      {
        hostname = "cp-1"
        network = {
          ip_address  = "192.168.1.10"
          netmask     = "255.255.255.0"
          gateway     = "192.168.1.1"
          mac_address = "aa:bb:cc:dd:ee:01"
        }
        disk = { device = "/dev/nvme0n1" }
        bmc = {
          address  = "192.168.2.10"
          username = "admin"
          password = var.bmc_password
        }
        labels = { type = "cp" }
      },
    ]
  }
}

variable "bmc_password" {
  type      = string
  sensitive = true
}
