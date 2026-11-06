# Plan-only checks of what the module derives, against mocked providers.
# Computed attributes (refs, revision names) are unknown at plan; the
# provider's own tests cover how they are computed.

mock_provider "capi" {}
mock_provider "kubernetes" {}

variables {
  cluster_name       = "talos"
  namespace          = "tinkerbell"
  gateway_ip         = "10.1.0.1"
  kubernetes_version = "1.36.4"
  talos_version      = "v1.13.10"
  tinkerbell_ip      = "10.1.1.111"
  virtual_ip         = "10.1.40.100"
  node_networks      = ["10.1.40.0/26"]
  pod_cidr           = "10.42.0.0/16"
  service_cidr       = "10.43.0.0/16"
  node_ip_range      = { start = "10.1.40.30", end = "10.1.40.60" }
  unifi              = { api_url = "https://10.0.0.1", site = "default", allow_insecure = true }
  unifi_api_key      = "test-key"
  strategic_patches  = [{ machine = { network = { hostname = "cp" } } }]
  bgp                = { service_lb_cidrs = ["10.1.1.64/26", "104.191.234.24/29"] }

  image_factory = {
    extensions        = ["siderolabs/iscsi-tools"]
    extra_kernel_args = ["console=tty0", "net.ifnames=0"]
    bootloader        = "sd-boot"
  }

  # The cluster-bootstrap fixtures as they are: attributes this module does
  # not declare (insecure_tls, ipxe_binary, ...) are dropped by the type.
  control_plane_nodes = [
    {
      name       = "talos-10-1-40-32", machine_type = "controlplane", arch = "arm64"
      interfaces = [{ mac = "88:a2:9e:87:76:6c", ip = "10.1.40.32" }]
      disks      = [{ device = "/dev/nvme0n1" }]
      bmc        = { host = "10.0.80.1", insecure_tls = true, port = 443, auth = { username = "admin", password = "admin" } }
    },
    {
      name       = "talos-10-1-40-33", machine_type = "controlplane", arch = "arm64"
      interfaces = [{ mac = "88:a2:9e:a6:3f:b1", ip = "10.1.40.33" }]
      disks      = [{ device = "/dev/nvme0n1" }]
      bmc        = { host = "10.0.80.2", insecure_tls = true, port = 443, auth = { username = "admin", password = "admin" } }
    },
  ]

  worker_nodes = [
    {
      name       = "talos-b8-ae-ed-76-79-61", machine_type = "worker", arch = "amd64"
      interfaces = [{ mac = "b8:ae:ed:76:79:61" }]
      disks      = [{ device = "/dev/sda" }]
      bmc        = { host = "10.0.197.82", insecure_tls = true, port = 443, auth = { username = "admin", password = "admin" } }
    },
    {
      name       = "talos-f4-4d-30-6f-a7-93", machine_type = "worker", arch = "amd64"
      interfaces = [{ mac = "f4:4d:30:6f:a7:93" }]
      disks      = [{ device = "/dev/sda" }]
      bmc        = { host = "10.0.199.135", insecure_tls = true, port = 443, auth = { username = "admin", password = "admin" } }
    },
    {
      name       = "talos-pi-worker", machine_type = "worker", arch = "arm64"
      interfaces = [{ mac = "88:a2:9e:00:00:01" }]
      disks      = [{ device = "/dev/nvme0n1" }]
      bmc        = { host = "10.0.80.9", insecure_tls = true, port = 443 }
    },
  ]

  worker_deployments = {
    amd64 = {
      allow_discovered  = true
      image_factory     = { extensions = ["siderolabs/nvme-cli"], extra_kernel_args = ["console=tty0", "console=ttyACM0", "net.ifnames=0"] }
      strategic_patches = ["machine:\n  kubelet:\n    extraArgs:\n      node-labels: pool=amd64\n"]
      autoscaling       = { enabled = true, min_size = 1, max_size = 2 }
    }
    arm64 = {
      image_factory    = { extensions = ["siderolabs/iscsi-tools"], extra_kernel_args = ["console=tty0", "net.ifnames=0"], bootloader = "sd-boot" }
      install_workflow = "talos2disk"
    }
  }
}

run "ip_pool_is_named_after_the_cluster" {
  command = plan

  assert {
    condition     = capi_object.ip_pool.name == "talos-nodes"
    error_message = "the IPPool must keep its <cluster>-nodes name; every machine template draws from it"
  }

  assert {
    condition     = capi_object.ip_pool.spec.prefix == 16 && capi_object.ip_pool.spec.preAllocations["talos-10-1-40-32-talos-nodes"] == "10.1.40.32"
    error_message = "the pool carries the prefix length and pins every addressed node by its claim name"
  }
}

run "hardware_carries_role_variant_and_adoption" {
  command = plan

  assert {
    condition     = output.node_variants["talos-10-1-40-32"] == "arm64" && output.node_variants["talos-b8-ae-ed-76-79-61"] == "amd64"
    error_message = "every node must resolve a variant from operating_system_lookup_key, else arch"
  }

  assert {
    condition     = capi_hardware.node["talos-10-1-40-32"].adopted && !capi_hardware.node["talos-10-1-40-33"].adopted
    error_message = "only the node already running the cluster is adopted"
  }

  assert {
    condition     = capi_hardware.node["talos-b8-ae-ed-76-79-61"].labels["tinkerbell.org/variant"] == "amd64" && capi_hardware.node["talos-b8-ae-ed-76-79-61"].labels["tinkerbell.org/role"] == "worker"
    error_message = "worker classes select Hardware by role and variant labels"
  }

  assert {
    condition     = capi_hardware.node["talos-10-1-40-32"].bmc.address == "10.0.80.1:443" && capi_hardware.node["talos-pi-worker"].bmc == null
    error_message = "a BMC with credentials becomes host:port; one without gets no rufio Machine"
  }

  assert {
    condition     = capi_hardware.node["talos-b8-ae-ed-76-79-61"].network.ip_address == null
    error_message = "an unpinned node leaves its address to the IPAM claim"
  }
}

run "rejects_an_unknown_variant" {
  command = plan

  variables {
    worker_deployments = {
      gpu = {
        variant       = "riscv"
        image_factory = { extensions = [] }
      }
    }
  }

  expect_failures = [var.worker_deployments]
}

run "rejects_a_bad_bootloader_on_a_deployment" {
  command = plan

  variables {
    worker_deployments = {
      amd64 = {
        image_factory = { bootloader = "syslinux" }
      }
    }
  }

  expect_failures = [var.worker_deployments]
}

run "rejects_an_unpinned_talos_version" {
  command = plan

  variables {
    talos_version = "latest"
  }

  expect_failures = [var.talos_version]
}

run "rejects_a_pinned_ip_outside_the_pool" {
  command = plan

  variables {
    node_ip_range = { start = "10.1.40.40", end = "10.1.40.60" }
  }

  expect_failures = [capi_object.ip_pool]
}

run "groups_default_replicas_from_matching_nodes" {
  command = plan

  assert {
    condition     = output.worker_deployments["arm64"].replicas == 1 && output.worker_deployments["amd64"].replicas == 2
    error_message = "replicas must default to the count of worker nodes whose variant matches the group"
  }

  assert {
    condition     = capi_machine_deployment.worker["amd64"].replicas == null && capi_machine_deployment.worker["amd64"].autoscaling.max == 2
    error_message = "an autoscaled group leaves replicas to the autoscaler"
  }

  assert {
    condition     = capi_machine_deployment.worker["arm64"].replicas == 1 && capi_machine_deployment.worker["arm64"].autoscaling == null
    error_message = "a fixed group sets replicas and no autoscaling"
  }

  assert {
    condition     = capi_machine_deployment.worker["amd64"].class.name == "amd64" && capi_machine_deployment.worker["amd64"].rollout.max_surge == 0
    error_message = "each group instantiates its own worker class and never surges"
  }
}

run "templates_render_their_specs" {
  command = plan

  assert {
    condition     = nonsensitive(capi_control_plane_template.this.spec.rolloutStrategy.rollingUpdate.maxSurge) == 0
    error_message = "the control-plane template must forbid surging"
  }

  assert {
    condition     = nonsensitive(capi_control_plane_template.this.spec.controlPlaneConfig.controlplane.talosVersion) == "v1.13.10"
    error_message = "the control plane carries the pinned Talos version (the talosVersion variable of the original)"
  }

  assert {
    condition     = strcontains(nonsensitive(capi_control_plane_template.this.spec.controlPlaneConfig.controlplane.strategicPatches[0]), "hostname: \"cp\"")
    error_message = "the control plane carries the cluster's strategic patches with bare keys"
  }

  assert {
    condition     = nonsensitive(capi_control_plane_template.this.spec.controlPlaneConfig.controlplane.imageFactory.bootloader) == "sd-boot"
    error_message = "the control plane carries its schematic"
  }

  assert {
    condition     = capi_machine_template.control_plane.spec.addressFromPool.name == "talos-nodes"
    error_message = "control-plane machines draw addresses from the cluster pool"
  }

  assert {
    condition     = capi_machine_template.worker["amd64"].spec.hardwareAffinity.required[0].labelSelector.matchLabels["tinkerbell.org/variant"] == "amd64"
    error_message = "a worker class selects boards by its variant"
  }

  assert {
    condition     = length(capi_machine_template.worker["amd64"].spec.hardwareAffinity.required) == 2 && length(capi_machine_template.worker["arm64"].spec.hardwareAffinity.required) == 1
    error_message = "only allow_discovered classes carry the auto-discovered affinity term"
  }

  assert {
    condition     = strcontains(capi_machine_template.worker["arm64"].spec.templateInline, "talos2disk") && strcontains(capi_machine_template.worker["amd64"].spec.templateInline, "image2disk")
    error_message = "each class inlines the install workflow it asked for"
  }

  assert {
    condition     = capi_bootstrap_template.worker["arm64"].spec.generateType == "worker" && capi_bootstrap_template.worker["arm64"].spec.talosVersion == "v1.13.10"
    error_message = "worker bootstrap templates generate worker configs for the pinned Talos version"
  }

  assert {
    condition     = capi_bootstrap_template.worker["amd64"].spec.imageFactory.extraKernelArgs[1] == "console=ttyACM0" && length(capi_bootstrap_template.worker["arm64"].spec.strategicPatches) == 0
    error_message = "each group carries its own schematic and patches (the variable overrides of the original)"
  }

  assert {
    condition     = capi_machine_template.cluster.role == "cluster" && capi_machine_template.cluster.spec == {}
    error_message = "the cluster template carries no endpoint; CAPT reads it from the Cluster"
  }
}

run "class_and_cluster" {
  command = plan

  assert {
    condition     = toset(keys(capi_cluster_class.this.workers)) == toset(["amd64", "arm64"])
    error_message = "one worker class per group"
  }

  assert {
    condition     = capi_cluster.this.kubernetes_version == "v1.36.4" && capi_cluster.this.control_plane.replicas == 2
    error_message = "the topology version carries a v prefix and control-plane replicas follow control_plane_nodes"
  }

  assert {
    condition     = capi_cluster.this.network.control_plane_endpoint.host == "10.1.40.100" && capi_cluster.this.network.pods.cidr_blocks[0] == "10.42.0.0/16"
    error_message = "the VIP and the pod CIDR stay on the Cluster"
  }
}

run "control_planes_originate_the_endpoint" {
  command = plan

  assert {
    condition     = length(nonsensitive(capi_control_plane_template.this.spec.controlPlaneConfig.controlplane.strategicPatches)) == 7
    error_message = "the control plane carries the caller's patch and the six BGP documents, one patch each"
  }

  assert {
    condition = anytrue([
      for p in nonsensitive(capi_control_plane_template.this.spec.controlPlaneConfig.controlplane.strategicPatches) :
      strcontains(p, "talos-vip-watcher") && strcontains(p, "--vip=10.1.40.100/32") && strcontains(p, "--iface=vip-api")
    ])
    error_message = "talos-vip-watcher binds the cluster endpoint to the VIP link"
  }

  assert {
    condition = anytrue([
      for p in nonsensitive(capi_control_plane_template.this.spec.controlPlaneConfig.controlplane.strategicPatches) :
      strcontains(p, "name: \"fabric\"") && strcontains(p, "- \"vip-api\"") && strcontains(p, "10.1.1.64/26") && strcontains(p, "address: \"10.1.0.1\"") && !strcontains(p, "routerID")
    ])
    error_message = "the fabric advertises the VIP link, imports the LoadBalancer pools, peers with the gateway and derives its router-id"
  }

  assert {
    condition = anytrue([
      for p in nonsensitive(capi_control_plane_template.this.spec.controlPlaneConfig.controlplane.strategicPatches) :
      strcontains(p, "name: \"workload\"") && strcontains(p, "passive: true") && strcontains(p, "localASN: 4200000002") && strcontains(p, "peerASN: 4200000003")
    ])
    error_message = "the workload instance waits for Cilium on the veth, with 4-byte ASNs rendered as integers"
  }

  assert {
    condition = anytrue([
      for p in nonsensitive(capi_control_plane_template.this.spec.controlPlaneConfig.controlplane.strategicPatches) :
      strcontains(p, "advertisedSubnets") && strcontains(p, "10.1.40.0/26")
    ])
    error_message = "etcd is pinned to the node network"
  }

  assert {
    condition     = alltrue([for name, t in capi_bootstrap_template.worker : !anytrue([for p in t.spec.strategicPatches : strcontains(p, "BGPInstanceConfig")])])
    error_message = "workers carry no BGP documents"
  }

  assert {
    condition     = output.bgp.talos_address == "fda1:b2c3:d4e5:1::1" && output.bgp.cilium_address == "fda1:b2c3:d4e5:1::" && output.bgp.api_vip == "10.1.40.100"
    error_message = "Cilium dials the VRF end of the veth and sources from its own end"
  }

  assert {
    condition     = length(capi_cluster_resource_set.cilium_bgp) == 0
    error_message = "Cilium's objects are not delivered unless asked for"
  }
}

run "bgp_off_renders_no_documents" {
  command = plan

  variables {
    bgp = { enabled = false }
  }

  assert {
    condition     = length(nonsensitive(capi_control_plane_template.this.spec.controlPlaneConfig.controlplane.strategicPatches)) == 1
    error_message = "without BGP the control plane carries only the caller's patches"
  }

  assert {
    condition     = output.bgp == null
    error_message = "no BGP endpoints without BGP"
  }
}

run "cilium_peering_delivers_the_cilium_objects" {
  command = plan

  variables {
    bgp = { service_lb_cidrs = ["10.1.1.64/26"], cilium_peering = true }
  }

  assert {
    condition     = capi_cluster_resource_set.cilium_bgp[0].strategy == "Reconcile" && capi_cluster_resource_set.cilium_bgp[0].cluster == "talos"
    error_message = "the set targets the cluster and follows changes"
  }

  assert {
    condition = alltrue([
      for s in ["\"kind\": \"CiliumBGPClusterConfig\"", "\"kind\": \"CiliumBGPPeerConfig\"", "\"kind\": \"CiliumBGPAdvertisement\"", "\"peerAddress\": \"fda1:b2c3:d4e5:1::1\"", "\"localASN\": 4200000003", "\"sourceInterface\": \"veth-workload\""] :
      strcontains(capi_cluster_resource_set.cilium_bgp[0].manifests[0], s)
    ])
    error_message = "Cilium peers with the local workload instance from its veth end"
  }
}

run "rejects_a_vip_inside_the_node_pool" {
  command = plan

  variables {
    node_ip_range = { start = "10.1.40.30", end = "10.1.40.120" }
  }

  expect_failures = [capi_control_plane_template.this]
}

run "rejects_a_vip_below_a_control_plane" {
  command = plan

  variables {
    virtual_ip = "10.1.40.20"
  }

  expect_failures = [capi_control_plane_template.this]
}

run "rejects_a_vip_inside_the_node_network" {
  command = plan

  variables {
    node_networks = ["10.1.40.0/24"]
  }

  expect_failures = [capi_control_plane_template.this]
}

run "rejects_a_vip_inside_a_load_balancer_pool" {
  command = plan

  variables {
    bgp = { service_lb_cidrs = ["10.1.40.96/27"] }
  }

  expect_failures = [capi_control_plane_template.this]
}

run "rejects_shared_asns" {
  command = plan

  variables {
    bgp = { cilium_asn = 65001 }
  }

  expect_failures = [var.bgp]
}
