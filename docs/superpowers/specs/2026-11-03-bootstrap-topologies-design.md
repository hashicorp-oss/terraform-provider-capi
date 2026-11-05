# Bootstrap Topologies: In-Place Adoption and the Disposable ISO Node

> Partly superseded 2026-11-04: §4 (in-place adoption) and the Talos bootstrap node were
> removed; the Kairos disposable node (§6) is the only bare-metal bootstrap. See
> `docs/superpowers/specs/2026-11-04-provider-design.md` §4.3. Kept as history.

Date: 2026-11-03
Status: approved for implementation (in-place half); design only (Kairos half)

## 1. Goal

`capi_cluster` must be able to stand up a self-managed bare-metal Talos cluster the way
`tfc/cluster-bootstrap` does today, and it must offer a second, standards-shaped path in which the
management plane starts on a disposable node booted from an ISO and disappears after the pivot.
Both paths stay. This document compares them, fixes the contract that makes in-place adoption
sound, and designs the disposable path so it can be built later without reopening the comparison.

## 2. What the workspace does today

Confirmed from the workspace state on 2026-11-03: the first control-plane node is stood up outside
CAPI, then adopted by it. `Cluster.spec.topology.controlPlane.replicas` counts it, its Hardware
carries `v1alpha1.tinkerbell.org/provisioned: "true"` and `netboot.allowPXE: false`, and the
cluster's identity secrets are pre-seeded from the same machine secrets the node was installed with.

The adoption works on upstream behaviour only:

| Step | Component | Behaviour |
| --- | --- | --- |
| Machine ready without a workflow | CAPT (annotation since v0.6.0) | `reconcile` short-circuits on the annotation: `Status.Ready`, `Initialization.Provisioned`, provider ID `tinkerbell://<ns>/<hardware>` |
| Machine address | CAPT | `Status.Addresses` from the Hardware's first `dhcp.ip.address` |
| Node link | kubelet | `--provider-id` equal to the CAPT provider ID, patched into the machine config |
| Secrets | CABPT | `getSecretsBundle` loads `<cluster>-talos` by name; derives `-ca` and `-talosconfig` |
| Kubeconfig | CACPPT | `reconcileKubeconfig` keeps an existing `<cluster>-kubeconfig`; rotates only near expiry |
| etcd | CACPPT | `bootstrapCluster` skips when any machine has `/var/lib/etcd/member` |

The flakiness is in the Terraform implementation, not the model: a fixed one-minute sleep before
Helm, one-shot Redfish actions that must never re-trigger, a manual cut-over runbook with `removed`
blocks, and 1Password as the only durable copy of the cluster identity. The provider's reconciler
already replaces all of that.

## 3. Comparison

### 3.1 In-place adoption (exists; finished by this spec)

The bootstrap node is installed through its BMC, becomes a single-node cluster, runs the CAPI
providers, and is then adopted as the first control-plane Machine of the cluster it hosts.

- **Hardware**: one machine. No spare, no re-image, no second boot.
- **Time**: one install.
- **Standards**: adoption relies on pre-setting an annotation CAPT normally writes itself. Every
  other step is the providers' documented behaviour.
- **Risk**: configuration convergence (section 4). The node's running config must be what CAPI
  would render for it, because the control-plane provider's in-place update path replaces the
  whole config on the first TalosControlPlane change.
- **Destroy**: the management plane is inside the cluster being destroyed; teardown has to be
  ordered by the provider, not by the controllers.

### 3.2 Disposable ISO node (designed here, built: §6)

A machine with a BMC that can mount virtual media boots a Kairos ISO into RAM, runs k3s with the
Tinkerbell stack and the CAPI providers, provisions the real cluster, pivots, and is powered off.
Nothing is written to its disk, so ejecting the media ends the bootstrap cluster.

- **Hardware**: one extra machine for the duration of the bootstrap, reclaimable afterwards.
- **Time**: one netboot install per real control-plane node plus the pivot.
- **Standards**: this is the clusterctl and EKS Anywhere shape. No adoption, no shared identity,
  no self-delete.
- **Risk**: Hardware and rufio Machines move with `clusterctl move` (their CRDs carry the
  `clusterctl.cluster.x-k8s.io/move` label), but Workflows do not, and a Tinkerbell agent mid-
  workflow reports to the tink-server that booted it. The move therefore has to wait for
  Tinkerbell quiescence, and only one smee may answer DHCP on the node network while machines
  boot. The Tinkerbell address itself does not need to survive the pivot (see 6.3).
- **Why Kairos**: it has a documented in-RAM workflow (`kairos.ram` on the kernel command line),
  takes its cloud-config from `kairos.config_url`, a metadata source, or a config embedded with
  AuroraBoot, and ships k3s in its standard images. k3s brings ServiceLB, so a `LoadBalancer`
  Service for Tinkerbell gets the node's own address with no CNI work. Talos cannot play this role:
  it has no in-RAM Kubernetes mode.

### 3.3 Permanent management node (reference)

The bootstrap node stays a one-node management cluster forever and the workload cluster is a
separate cluster. Zero adoption and zero pivot, at the cost of a dedicated machine. The provider
supports it as soon as the Tinkerbell-requires-`self_managed` validation is lifted; nothing else
in this document is needed for it.

### 3.4 Decision

Finish in-place now; design the disposable path so it shares every building block with it; keep
the existing `pivot` mode as the general self-managed path. The permanent-management variant is
documented, not built.

## 4. The in-place contract

Adoption is sound only under these rules. The provider enforces or documents each one.

1. **One config source.** The bootstrap node's machine config is composed from the same inputs
   CAPI renders for a control-plane machine: `control_plane.talos.resource_config` →
   `controlPlaneConfig.controlplane.strategicPatches`, with `PROVIDER_ID` substituted to
   `tinkerbell://<namespace>/<hostname>` exactly as CAPT substitutes it in Hardware userData. The
   bootstrap-only additions are install-time settings (installer image, install disk, wipe) and
   the identity pins in rule 3. User `config_patches` still apply last.
2. **Provider ID.** When no composed patch sets the kubelet `provider-id`, the provider adds it.
   Without it the adopted Machine never gets a `nodeRef`.
3. **Identity survives an in-place apply.** Hostname and static addressing are pinned in the
   machine config for the install and additionally written to Talos META key `0xa` as platform
   network configuration once the node is ours, the same mechanism the `talosmeta` Tinkerbell
   action uses for CAPT-provisioned nodes. Talos layers platform configuration under the machine
   config, so a later config without the addresses keeps the node on its address and name.
4. **Endpoint convergence.** `management.bootstrap.talos.endpoint` defaults to
   `network.control_plane_endpoint` when the latter is set; setting both to different hosts is an
   error. The derived `<cluster>-kubeconfig` and the API server certificate SANs then name the
   same endpoint CAPI uses.
5. **Scheduling.** The bootstrap node allows scheduling on control planes so the platform pods
   can run on a single node. A warning is raised when the composed patches do not set
   `cluster.allowSchedulingOnControlPlanes`, because the first in-place update would taint the node.
6. **Hardware is the provider's to create.** Each `inventory.machine` becomes a Tinkerbell
   Hardware, a rufio Machine, and a basic-auth Secret in the cluster namespace, applied before the
   generated cluster objects. The bootstrap machine's Hardware carries the provisioned annotation
   and `allowPXE: false`; every other machine is a normal netboot target.
7. **Destroy is ordered by the provider.** Deleting the Cluster from inside itself would power
   off the node that hosts the controllers. In-place destroy resets every other inventory machine
   over the Talos API (the bundle authenticates to all of them), powers them off through their
   BMCs, and resets the bootstrap node last.

The DHCP reservation for the bootstrap machine's MAC remains an external prerequisite: Talos
maintenance mode is DHCP-only, so the node must land on its inventory address before it has a
config at all.

## 5. Schema additions

```hcl
inventory = {
  manage  = true            # create Hardware, rufio Machine and Secret objects (default true)
  machine = [ ... ]
}

manifests = {
  management = [ yamlencode({...}) ]   # applied to the management cluster after provider init,
}                                      # before the generated cluster objects (IPAM pools, extra Secrets)

management = {
  addons = {                  # management-plane addons (the Tinkerbell stack), installed by the
    prerequisites = [ ... ]   #   provider on every cluster that takes the management role; the
    helm          = [ ... ]   #   prerequisites only on the final one (see 6.1)
    manifests     = [ ... ]
  }
  bootstrap = {
    addons = {
      prerequisites = [ ... ]   # manifests applied after the CNI release, before `helm`
      helm          = [ ... ]
      manifests     = [ ... ]   # applied after `helm` (unchanged)
    }
  }
}
```

The pivot sequence of 6.3, the `management.addons` block and the `cni` patch injection into
the CAPI render are implemented in the manager and the provider (they serve the Talos `pivot`
mode today); the Kairos bootstrapper itself is not.

Addon order on the bootstrap node becomes: Gateway API CRDs → CNI release → `prerequisites` →
`helm` → `manifests`. The CNI release was previously appended after the user's releases, which
left a Tinkerbell release waiting for pods that could not schedule.

No schema version bump: the new attributes are optional and the upgraders emit them as null.

## 6. Disposable ISO node design (`management.bootstrap.type = "kairos"`)

Implemented 2026-11-03 in `internal/capi/kairos` and the provider (`kairos_bootstrap.go`);
nothing here changes the in-place path. §6.4 lists what is still unverified on hardware.

### 6.1 Shape

```hcl
management = {
  self_managed = true
  bootstrap = {
    type    = "kairos"
    machine = "nuc-15"                       # inventory.machine[].hostname: BMC, MAC, disk
    boot    = { method = "virtual_media", device = "usb", timeout = "10m", attempts = 3 }
    kairos = {
      iso      = "${path.module}/build/iso/kairos.iso.custom.iso"   # AuroraBoot output, or a URL
      address  = "10.1.40.50"                 # the live node's address (SSH and API); its Tinkerbell Service takes it via ServiceLB
      ssh      = { user = "kairos", private_key = file("~/.ssh/bootstrap") }   # host_key optional
    }
    addons = { ... }                         # bootstrap-node-only releases, same block as the Talos path
  }
  addons = {                                 # management-plane addons: installed by the provider's Helm
    prerequisites = [ ... ]                  #   client on every cluster that takes the management role
    helm          = [ { name = "tinkerbell", ... } ]
    manifests     = [ ... ]
  }
}
```

`management.addons` is the home of the Tinkerbell stack. The provider installs it on the
bootstrap cluster right after `clusterctl init` there, and again on the workload cluster right
after `clusterctl init` for the pivot, with the same releases and values. `prerequisites` are
applied only on the final management cluster, because they are objects such as a Cilium
load-balancer pool whose CRDs the disposable node does not have; the Tinkerbell Service stays a
plain `LoadBalancer` so k3s ServiceLB and Cilium LB-IPAM both serve it. In place and with a
permanent management node the block is installed once. `management.bootstrap.addons` remains
for releases only the bootstrap node needs.

The user builds the ISO once with AuroraBoot from a documented cloud-config template: the SSH
public key under `users[].ssh_authorized_keys`, `k3s.enabled: true`, `k3s.args` with
`--tls-san=<address>` and `--write-kubeconfig-mode=644`, the static address, and
`kairos.ram` on the live command line (`--extend-live-cmdline`). Embedding the config at build
time is the only delivery that needs no network dependency; `kairos.config_url` is the
alternative when the provider can serve HTTP on the node network. Building the ISO inside the
provider is a later enhancement; it would need an ISO writer.

### 6.2 Bootstrapper (as built)

`internal/capi/kairos` implements `capi.Bootstrapper` with the Talos package's BMC layer. The
boot step is `talos.MediaBoot`, factored out of the reconciler's `attachMedia` so both paths
issue the same BMC call sequence (power off a running node, eject, attach by AMT storage
redirection or bmclib virtual media plus a one-shot EFI boot override, power on or cycle):

1. if the node already answers over SSH (a retried apply), reuse it; otherwise boot the ISO,
   up to `boot.attempts` times, each attempt waiting `boot.timeout` for SSH on
   `kairos.address` (default: the machine's `ip_address`); a BMC error is final, a silent boot
   is retried;
2. once SSH answers, leave the media attached (the AMT redirection session included) until
   `Delete`: AuroraBoot places the cloud-config at the ISO root as `config.yaml`, which the
   live system reads at every stage, and without `kairos.ram` the ISO is the root filesystem
   as well. `kairos.ram` still matters for robustness (a dropped redirection session no
   longer kills the node) and is the entry the ISO's GRUB must boot by default: AuroraBoot's
   live template (`pkg/constants/grub_live_bios.cfg`) offers it only in a submenu,
   `--extend-live-cmdline` is a `build-uki` flag, and `--overlay-iso` is applied before the
   template is written, so the GRUB config is replaced after the build (see the example);
3. read `/etc/rancher/k3s/k3s.yaml` (plain read, then `sudo -n`) until k3s has written it,
   rewrite the server to `https://<address>:6443`, write it to the temp dir;
4. wait for the API and a Ready node (k3s brings its own CNI);
5. install `bootstrap.addons` through the Talos addon installer (k3s ServiceLB gives a
   LoadBalancer Service the node's address, so no pool work);
6. return the cluster. No `provider_secrets`: nothing survives the node.

`Delete` ejects the media and powers the machine off. There is no reset: nothing was written to
disk. `Exists` is an SSH probe. SSH authenticates with `ssh.private_key`; the host key is
checked only when `ssh.host_key` is set, because a RAM-booted image generates a fresh one at
every boot. `boot.method` must be `auto` or `virtual_media` (`http` boots a Talos UKI, not a
live ISO); `mode` must be `pivot` and `management.self_managed` true.

### 6.3 Pivot sequencing

Tinkerbell is not a CAPI provider, so `clusterctl init` and `clusterctl move` carry only the
CAPI managers, CRDs and objects. Each management cluster runs its own Tinkerbell stack from
`management.addons`, installed by the provider's Helm client, and the CAPT manager uses the
stack co-located with it: the Hardware and Workflow objects in its own API. The provider's
sequence, which the existing Talos `pivot` mode shares:

1. **Phase one.** Wait for `ControlPlaneInitialized`, the control plane's ready replicas, and
   Tinkerbell quiescence on the bootstrap cluster: no Workflow pending or running. Workflows do
   not move, and an agent mid-workflow reports to the tink-server that booted it, so an
   in-flight provision cannot be handed over. Machines that need the disposable node's hardware
   are still pending here.
2. **Init the workload cluster** with `clusterctl init`, then install `management.addons` on it:
   `prerequisites`, then the Helm releases, then `manifests`. The workload stack now answers
   DHCP on its own address alongside the bootstrap stack, but nothing boots during the window.
3. **Move.** `clusterctl move` carries the CAPI objects plus Hardware and rufio Machines, whose
   CRDs are labelled for move, with their provisioned annotations and owner labels, so CAPT on
   the workload adopts the machines it already provisioned.
4. **Re-apply what does not move.** The BMC auth Secrets (clusterctl soft-owns Secrets only by
   CAPI purpose suffix), the disposable machine's inventory objects as a normal netboot target,
   `manifests.management`, and the generated cluster objects with the metadata endpoint token
   resolved to the workload stack's address.
5. **Delete the bootstrap cluster.** The node powers off and the media is ejected; its smee
   stops answering, leaving one responder on the segment.
6. **Phase two.** Wait for the Cluster `Available`/`Ready` conditions on the new management
   cluster; the machines that needed the disposable hardware complete now.

The workload cluster's CNI is a prerequisite of phase one, not of this sequence: CAPI reports
nodes Ready only with a CNI, so in pivot mode the `cni` block must apply to the workload cluster
too, installed by the provider at `ControlPlaneInitialized` with its Talos patch merged into the
control-plane and worker strategic patches. That generalisation is part of the pivot work.

The Tinkerbell address need not survive the pivot. CAPT has no address coupling (no
`TINKERBELL_IP`), smee answers as a proxy DHCP server so machines find it at boot, and a Talos
machine reads its `talos.config` metadata URL only on first boot before its config exists in
STATE. The one persistent reference is that URL in the install template. Name the metadata
endpoint by DNS, or let the provider substitute a token with the stack's current address when it
re-applies the generated objects in step 4.

### 6.4 Open verification items

- A Kairos ISO booted over AMT USB-R with `kairos.ram` on the NUC 15 Pro (Secure Boot off),
  and that a redirection session held for the whole bootstrap is harmless now that the
  phantom-floppy USB reset storm is fixed in the iamt fork (the Talos path still closes its
  session at maintenance mode).
- k3s publishing `/etc/rancher/k3s/k3s.yaml` readable by the SSH user (the template sets
  `--write-kubeconfig-mode=644`; `sudo -n` is the fallback).
- k3s ServiceLB binding the Tinkerbell `LoadBalancer` Service to the static address.
- CAPT re-claiming moved Hardware on the workload cluster, and the BMC auth Secrets being
  re-applied before rufio needs them.

## 7. Out of scope

- Building the Kairos ISO inside the provider.
- MachineHealthCheck and remediation.
- The runtime-extensions components (`talos-image-resolver`, janitor, upgrade coordinator); the
  provider must not depend on them.
