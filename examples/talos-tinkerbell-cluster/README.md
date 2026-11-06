# Talos on Tinkerbell, topology cluster

This example module does what `modules/cluster` in cluster-bootstrap does, but
with this provider's resources in place of `kubectl_manifest`. It manages a Talos
ClusterClass on Tinkerbell, the Cluster that instantiates it, one worker group
per Image Factory variant, UniFi-backed IPAM for node addresses, and the
Hardware inventory with BMCs.

The control planes run Talos native BGP, as cluster-bootstrap's config-patches
module renders it, so the cluster endpoint answers from the first control
plane CAPI brings up (see "The endpoint").

It runs against an existing management cluster: the one the `capi` provider
block names. In cluster-bootstrap that is the cluster itself, stood up by
`modules/bootstrap` with its providers installed by `modules/core`.

```hcl
provider "capi" {
  kubernetes = { config_path = "~/.kube/talos" }
}

provider "kubernetes" {
  config_path = "~/.kube/talos"
}

module "cluster" {
  source = "./examples/talos-tinkerbell-cluster"

  cluster_name        = "talos"
  namespace           = "tinkerbell"
  kubernetes_version  = "1.36.4"
  talos_version       = "v1.13.10"
  virtual_ip          = "10.1.40.100" # the endpoint and the BGP anycast VIP
  node_networks       = ["10.1.40.0/26"]
  tinkerbell_ip       = "10.1.1.111"
  gateway_ip          = "10.1.0.1"
  pod_cidr            = "10.42.0.0/16"
  service_cidr        = "10.43.0.0/16"
  node_ip_range       = { start = "10.1.40.30", end = "10.1.40.60" }
  image_factory       = { extensions = ["siderolabs/iscsi-tools"] }
  bgp                 = { service_lb_cidrs = ["10.1.1.64/26", "104.191.234.24/29"] }
  control_plane_nodes = local.control_plane_nodes
  worker_nodes        = local.worker_nodes
  worker_deployments  = local.worker_deployments
  unifi               = { api_url = "https://10.0.0.1" }
  unifi_api_key       = var.unifi_api_key
}
```

The node and deployment inputs have the same shape as the original's, so the
same locals can be passed. Attributes the module does not declare are dropped
by the type constraint (see Gaps).

The module renders the BGP documents itself. Pass `strategic_patches` without
them (cluster-bootstrap's `module.data_lookup.documents` includes them when its
`bgp.enabled` is set): when Talos merges two patches of the same document it
appends lists, so neighbours, addresses and the watcher's arguments would be
doubled.

## The endpoint

`virtual_ip` is both `Cluster.spec.controlPlaneEndpoint` and an anycast /32
that each control plane originates itself:

1. talos-vip-watcher binds the VIP to the `vip-api` dummy link while the local
   apiserver answers `/readyz`, and removes it when it stops.
2. The Talos `fabric` BGP instance advertises that link and the node's own
   link to the router (`bgp.upstream_address`, default `gateway_ip`, AS
   65000), as AS 65001.
3. The router ECMPs the endpoint across whichever control planes hold it.

None of this needs Kubernetes, Cilium or a LoadBalancer Service. The first
control plane CAPI images answers on the endpoint as soon as its apiserver is
ready, so CAPI's connection to the new cluster, and `capi_cluster`'s wait, go
through the VIP from the start. A Cilium-announced VIP is not reachable at
that point, because nothing runs yet to announce it. Talos puts the endpoint
in the apiserver certificate itself.

Cilium peers with a `workload` BGP instance on its own node instead of with
the router. The instance is bound to a VRF and reached over a veth pair:

- Peering over the veth keeps two BGP speakers from contending for TCP/179.
- The fabric re-originates what Cilium advertises: the pod and service CIDRs
  and `bgp.service_lb_cidrs`.

`bgp.cilium_peering = true` also delivers Cilium's three objects
(CiliumBGPClusterConfig, CiliumBGPPeerConfig, CiliumBGPAdvertisement) to the
cluster as a `capi_cluster_resource_set`. Turn it on for a cluster whose Cilium
comes from a `capi_helm_chart_proxy`. Leave it off on talos, where
cluster-bootstrap's base chart owns those objects.

The documents and the Cilium objects are the ones in cluster-bootstrap's
`clustarr/talos-bgp.yaml` and `clustarr/cilium-bgp.yaml`. That file's
`certExtraSANs` addition is not needed here, because the VIP is the endpoint.

Plan fails, with a precondition on the control-plane template, when:

- the VIP lies inside `node_ip_range`, where IPAM could hand it to a node;
- the VIP lies inside `node_networks`, where etcd is pinned;
- the VIP lies inside one of `bgp.service_lb_cidrs`, where a Service could
  claim it and two speakers would advertise one /32;
- a control-plane address (any pool address, for an unpinned control plane)
  does not sort below the VIP. Talos takes the fabric's router-id from the
  lowest IPv4 across its advertised links; above the VIP, every control plane
  would share the VIP as its router-id.

The router needs a peer group that accepts AS 65001 from the control-plane
addresses. The original Cilium peering used the same AS, so a router set up
for that needs no change. With `bgp.enabled = false`, something else must
answer on `virtual_ip` before the first control plane can be reached.

## Where each object went

| cluster-bootstrap (`kubectl_manifest` unless noted) | Here |
| --- | --- |
| `TinkerbellClusterTemplate` | `capi_machine_template.cluster` (`role = "cluster"`) |
| `TalosControlPlaneTemplate`, hash-named in locals | `capi_control_plane_template.this` |
| control-plane `TinkerbellMachineTemplate` | `capi_machine_template.control_plane` |
| worker `TinkerbellMachineTemplate` per deployment | `capi_machine_template.worker[*]` |
| worker `TalosConfigTemplate` per deployment | `capi_bootstrap_template.worker[*]` |
| `ClusterClass` with 5 variables and 5 patches | `capi_cluster_class.this`, no variables or patches |
| `Cluster` with `spec.topology` and inline `workers.machineDeployments` | `capi_cluster.this` and `capi_machine_deployment.worker[*]` |
| UniFi `Secret` | `kubernetes_secret_v1.unifi_credentials` |
| UniFi `Instance`, `IPPool` | `capi_object.unifi_instance`, `capi_object.ip_pool` |
| `module.node`: `Hardware`, rufio `Machine`, BMC `Secret`, `kubernetes_labels.variant` | `capi_hardware.node[*]` |
| Control-plane BGP documents (data-lookup's config-patches, passed in `strategic_patches`) | `local.bgp_documents`, appended to the control-plane template only (bgp.tf) |
| Cilium BGP objects (core base chart) | `capi_cluster_resource_set.cilium_bgp`, opt-in |

## What behaves differently

- **No class variables.** The original patches `talosVersion`, `imageFactory`,
  `strategicPatches`, `workerStrategicPatches` and `ipPool` into one shared
  template per kind. Here every template is already per class and
  hash-named, so the values are written into the templates directly. Changing
  one creates a new revision, moves the class's ref, and CAPI rolls the
  affected machines, as a changed variable did before. The configuration that
  reaches the machines is the same. `capi_cluster` cannot set
  `spec.topology.variables`, so a class that requires variables can't be used
  with it.
- **Revision names come from the provider.** Each revision is
  `<name>-<8 hex of the spec>`, computed at plan. Unreferenced old revisions
  are deleted beyond `revision_history`, and referenced ones never are. The
  `create_before_destroy` blocks and the hash locals are gone.
- **Node groups are their own resources.** Each topology worker entry is
  applied under its own field manager, so adding a group never re-applies the
  Cluster. `autoscaling` writes the autoscaler min/max annotations and leaves
  `replicas` out. When a rollout gets stuck, the wait reports each machine's
  failing conditions.
- **Waits.** The original applied with 1-minute timeouts and did not wait. Here
  `capi_cluster` waits up to 90 minutes for the Cluster's `Available` condition,
  and each group waits up to 60 minutes for `Available` and `MachinesUpToDate`.
- **Ordering.** The original ordered nodes after the Cluster. Here the Cluster
  depends on the Hardware: machines have something to claim at create, and on
  destroy the Cluster goes first, so CAPT releases each Hardware before it is
  deleted. `capi_hardware` refuses to delete Hardware a machine still owns
  unless `force` is set.
- **Labels.** `capi_hardware` applies only the label keys it sets, under its
  own field manager. The owner labels CAPT stamps are left alone and a changed
  variant label is applied in place, so `kubernetes_labels.variant` is not
  needed.
- **`talos_version` is required and must be pinned.** This is checked on the
  variable instead of by a precondition on the Cluster. There is no `latest`
  default.

## Gaps

These are not covered by the provider yet:

- **Hardware fields.** `capi_hardware` does not render
  `metadata.instance.operating_system`, DHCP `arch`, the classless static
  route to the VIP, `iface_name`, `netboot.ipxe.binary`,
  `auto.enrollmentEnabled` or `userData`.
  - The staged workflow reads `.Hardware.Metadata.Instance.OperatingSystem`,
    so a node using it depends on the talos-image-resolver filling that block
    when the Hardware is claimed. The `talos2disk` workflow does not read it.
  - Nodes that need an `snp-*` iPXE build, such as the X550 boards, can't be
    described yet.
- **BMC options.** The rufio Machine always gets `insecureTLS = true` and
  either Redfish with basic auth or, on ports 16992/16993, Intel AMT. Custom
  `provider_options` (`rpc`, another `preferredOrder`) are not supported.
  Nothing waits for the Machine's `Contactable` condition. A BMC without
  credentials gets no rufio Machine.
- **Secrets.** `capi_object` writes only an object's `spec`, so the UniFi
  credentials Secret needs the `kubernetes` provider.
- **`topology_objects` output.** The template resources export refs and
  revision names, not manifests, so the multi-document YAML used as input to
  `clusterctl alpha topology plan` is not produced.

## Moving a live cluster to this module

This module is not a drop-in replacement for objects that already exist:

- Every template gets a new revision name, because the hash covers different
  content.
- The Cluster's topology worker entries move from the `terraform` field
  manager to one manager per group.

Plan the move on a non-production cluster first. Compare the rendered
templates with the live ones before switching the class over.

## Tests

```sh
terraform init -backend=false
terraform test
```

`tests/cluster.tftest.hcl` is a port of the original's suite. It runs plan-only
against mocked providers and checks the derived values: variants, adoption,
replicas and autoscaling, template specs, pool pinning, and input validation.
It also checks the BGP documents (control planes only), the Cilium objects,
and each endpoint precondition.
