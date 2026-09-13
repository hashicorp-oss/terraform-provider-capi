# A Tinkerbell cluster whose bootstrap (management) cluster is a single
# bare-metal machine provisioned with Talos through its BMC instead of kind.
# After the self-managed pivot the node is reset, powered off, and free for
# CAPT to reclaim into the workload cluster.
resource "capi_cluster" "bare_metal" {
  name               = "bm-cluster"
  kubernetes_version = "v1.34.0"

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
    }
  }

  bootstrap = {
    talos = {
      version      = "v0.8.2"
      fetch_config = { owner = "sidero-community" }
    }
  }

  control_plane = {
    talos = {
      version      = "v0.7.1"
      fetch_config = { owner = "sidero-community" }
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

  # Mirrors Cluster.spec.topology.
  topology = {
    control_plane = { replicas = 3 }
    workers = {
      machine_deployments = [
        { name = "md-0", replicas = 2 }
      ]
    }
  }

  # Provider releases ship a kubeadm template only. For Talos, supply a
  # custom template (local path, file:// or https:// URL) and the clusterctl
  # variables it substitutes.
  template = {
    source = "${path.module}/cluster-template-talos.yaml"
    variables = {
      CONTROL_PLANE_VIP  = "192.168.1.100"
      TALOS_VERSION      = "v1.13"
      TALOS_INSTALL_DISK = "/dev/sda"
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

        config_patches = [file("${path.module}/cni-none.yaml")]
      }

      addons = {
        helm = [
          {
            name      = "cilium"
            namespace = "kube-system"
            chart     = "oci://quay.io/cilium/charts/cilium"
            version   = "1.18.0"
            values = yamlencode({
              kubeProxyReplacement = true
              k8sServiceHost       = "localhost"
              k8sServicePort       = 7445
              ipam                 = { mode = "kubernetes" }
            })
          }
        ]
      }
    }
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
