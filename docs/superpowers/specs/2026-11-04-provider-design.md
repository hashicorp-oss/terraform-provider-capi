# terraform-provider-capi: target design

Date: 2026-11-04. Status: proposed. Supersedes the single-resource shape in
`.claude/CLAUDE.md` once implemented; the composite `capi_cluster` of today is
the migration starting point (§12).

## 1. Principles

The design follows the shape of `aws_eks_cluster` and takes its workflow
patterns from `azurerm_kubernetes_cluster`, after reading both.

**From EKS (the shape):**

- One resource per API object the management cluster holds. The cluster, each
  node group, each template, each addon declaration, each hardware entry.
- Dependents reference the cluster by identity, never by copying its settings.
  What they need from it (namespace, version, provider kinds, failure domains,
  worker classes) is read from the live object at plan or apply time.
- Credentials never enter state. `aws_eks_cluster_auth` became ephemeral; so
  is `capi_kubeconfig`.
- Create-only inputs for things that exist only during Create
  (`bootstrap_self_managed_addons`, RDS `snapshot_identifier`): the bootstrap
  cluster and the seed inventory.
- `version` updates in place. Delete waits.

**From AKS (the workflow patterns), used only where the API offers no single
operation:**

- A multi-step workflow runs inside one resource, with a *named* scaffold
  (`temporary_name_for_rotation`; ours is `<name>-bootstrap`).
- Every step checks what exists before acting, so a rerun is idempotent.
- State can represent a half-finished workflow and the next apply resumes
  instead of replacing (`FindDefaultNodePool` falling back to the temporary
  pool, and the `ForceNewIf` that recognises it).
- Transition rules that the API enforces are encoded as plan logic, so an
  impossible change is refused at plan, not at apply.
- Controller-rewritten values (version aliases, autoscaler-owned counts) are
  suppressed as diffs, not fought.

**What not to copy:** inline sub-objects with their own lifecycle (AKS's
`default_node_pool`, the sole reason the rotation workflow exists), long-lived
credentials in state (`kube_config_raw`), blind retries without backoff.

**Terraform constructs used:** managed resources for objects, ephemeral
resources for credentials, data sources for reads, actions (Terraform 1.14)
for one-shot clusterctl verbs, write-only or create-only arguments for
scaffolding. Everything is importable.

## 2. The management connection

The provider block is the management cluster, the way the aws provider block
is the AWS API:

```hcl
provider "capi" {
  kubeconfig = ephemeral.capi_kubeconfig.prod.raw   # or kubeconfig_path / context
}
```

A self-managed cluster has no external authority. Its root credential is the
cluster's own secrets bundle, captured from the management cluster once the
pivot completes (the `<name>-talos` bundle CABPT generated, or the `<name>-ca`
key pair for kubeadm) and persisted as `provider_secrets`. Nothing else about
its connection is stored. `capi_cluster` derives a
short-lived client certificate from that bundle whenever it needs to Read
itself, and `capi_kubeconfig` does the same for everything downstream:

```hcl
ephemeral "capi_kubeconfig" "prod" {
  cluster = capi_cluster.prod.id            # external management: reads the <name>-kubeconfig Secret
  secrets = capi_cluster.prod.provider_secrets   # self-managed: derives from the bundle; omit otherwise
  ttl     = "1h"                            # renewed by Terraform while the run lasts
}

provider "capi"       { alias = "prod"; kubeconfig = ephemeral.capi_kubeconfig.prod.raw }
provider "helm"       { kubernetes = { host = ephemeral.capi_kubeconfig.prod.host, ... } }
provider "kubectl"    { ... }
```

Two provider instances therefore appear in a self-managed configuration: the
default one, used only by the `capi_cluster` that stands the cluster up, and
`capi.prod`, used by every dependent after the pivot. With an external
management cluster there is one.

## 3. Resource catalog

| Resource | API object | Analogue |
|---|---|---|
| `capi_cluster` | `Cluster` (+ control plane object, inline: no independent lifecycle) | `aws_eks_cluster` |
| `capi_machine_deployment` | `MachineDeployment`, or one entry of `Cluster.spec.topology.workers.machineDeployments` | `aws_eks_node_group` |
| `capi_machine_template` | `<Infra>MachineTemplate`, revisioned | `aws_launch_template` |
| `capi_bootstrap_template` | `<Bootstrap>ConfigTemplate`, revisioned | `aws_launch_template` (user data half) |
| `capi_cluster_class` | `ClusterClass` | none; closest is a launch template of templates |
| `capi_machine_health_check` | `MachineHealthCheck` | none |
| `capi_helm_chart_proxy` | `HelmChartProxy` (+ its `HelmReleaseProxy` status) | `aws_eks_addon` |
| `capi_cluster_resource_set` | `ClusterResourceSet` + its ConfigMaps/Secrets | `aws_eks_addon` with configuration |
| `capi_hardware` | Tinkerbell `Hardware` + rufio `Machine` + BMC `Secret` | `aws_ec2_host` |
| `capi_provider` | clusterctl `Provider` inventory object (init / upgrade apply / delete) | `aws_eks_addon` for the service itself |
| `capi_object` | any CR of an installed provider, kind resolved by provider label | `kubernetes_manifest` |
| ephemeral `capi_kubeconfig` | `<name>-kubeconfig` Secret or derived from secrets | `aws_eks_cluster_auth` |
| data `capi_cluster` | describe: conditions, object tree | `data.aws_eks_cluster` |
| data `capi_provider_components` | kinds and API versions a provider release ships | none |
| actions `capi_move`, `capi_rollout_restart`, `capi_pause`, `capi_resume`, `capi_remediate_machine` | clusterctl verbs | `aws_lambda_invoke` |

Not resources: the bootstrap cluster, the Kairos node standup, waits.

## 4. `capi_cluster`

### 4.1 Schema

```hcl
resource "capi_cluster" "prod" {
  name               = "prod"
  namespace          = "capi"                       # RequiresReplace
  kubernetes_version = "v1.34.0"                    # in place: Cluster.spec.topology.version or the CP object

  network = { control_plane_endpoint = { host = "10.0.0.160" }, pods = {...}, services = {...} }

  # One of the two:
  class = capi_cluster_class.main.ref               # topology mode
  control_plane = {                                 # direct mode (and replicas in both modes)
    replicas       = 3
    provider       = "talos"                        # control-plane provider name; kinds resolved from CRDs
    infrastructure = capi_machine_template.cp.ref
    spec           = { controlPlaneConfig = { controlplane = { talosVersion = "v1.13", strategicPatches = local.cp } } }
  }

  management = {
    # External management cluster: nothing here; the provider block is it.
    # Self-managed: everything below is create-only (write-only where the framework allows it).
    self_managed = true
    bootstrap = {
      type       = "kairos" | "kind"
      machine    = "nuc"                            # inventory hostname (kairos)
      boot       = { method = "auto" | "virtual_media" | "manual", device = "usb", timeout = "10m", attempts = 3 }
      kairos     = { iso = "...", address = "10.1.40.50", ssh = { user = "kairos", private_key = ... } }
      addons     = { helm = [local.tinkerbell_host_network], manifests = [...] }   # the bootstrap node's own stack, §4.3
      on_failure = "resume" | "fail"                # §4.3
    }
    providers = {                                   # only for self-managed; external uses capi_provider
      core = { cluster-api = {...} }, infrastructure = { tinkerbell = {...} }, bootstrap = {...}, control_plane = {...}, ipam = {...}, addon = { helm = {} }
    }
    uninstall = false
  }

  inventory = { machine = [...] }                   # seed, self-managed only; later additions are capi_hardware
  manifests = { management = [...] }                # on the management cluster before the Cluster; re-applied after a move

  wait = { conditions = ["ControlPlaneInitialized", "Available"], timeout = "60m" }

  # status (computed): phase, endpoint, ca_certificate, conditions, bootstrap_cluster, description
  # provider_secrets (computed, sensitive): the root credential of a self-managed cluster
}
```

Why these stay inline: the control plane object has no lifecycle apart from
the cluster (EKS hides it entirely); `management.providers`, the seed
`inventory` and `manifests.management` must exist on a management cluster
that does not exist before this resource creates it. With an external
management cluster they are empty and `capi_provider`, `capi_hardware` carry
the same things as separate resources.

The workload cluster's own addons, its CNI and its customized Tinkerbell
included, are `capi_helm_chart_proxy` and `capi_cluster_resource_set`
resources (§8). They are declared against the bootstrap cluster, installed by
CAAPH, and travel with the cluster. Nothing about them lives on
`capi_cluster`; there is no `cni` block. The Talos machine-config patch that
turns the built-in flannel off belongs in `control_plane.spec` with the rest
of the control-plane configuration.

### 4.2 Plan

- Kinds and API versions come from the installed CRDs (`KindCatalog`), the
  core version from the `Cluster` CRD's storage version, ref shapes from the
  contract. With `management.providers` set and no cluster yet, they come from
  the release artifacts (`capi_provider_components` logic), so the first plan
  is complete, and Create verifies the live CRDs match before applying.
- Transition rules, refused at plan: `self_managed` cannot change; `class`
  cannot change to `control_plane` or back; `namespace` and `name` replace;
  `kubernetes_version` must be a full `vX.Y.Z` in topology mode; a pivoted
  cluster cannot be un-pivoted.
- Diff suppression: `kubernetes_version` compared without the `v` prefix;
  `control_plane.replicas` ignored while the cluster autoscaler owns it;
  `status` never diffs.

### 4.3 Create: the workflow, resumable

Create is one of two workflows, chosen by `management`:

- **External management:** apply the rendered objects, wait. One step.
- **Self-managed pivot:** the sequence below. There is no in-place mode. A
  node adopted into the cluster it bootstrapped is rewritten by CABPT at the
  first control-plane change and drifts toward replacement, so the bootstrap
  node is disposable by design and joins the cluster afterwards as an
  ordinary CAPT-provisioned machine.

The pivot, with the Tinkerbell handover that keeps at most one DHCP responder
on the network at any time:

1. **Bootstrap node.** Boot the Kairos live ISO on `bootstrap.machine`
   (§4.7), wait for SSH and k3s, run `clusterctl init` with
   `management.providers`, install `bootstrap.addons`: Tinkerbell on host
   networking, bound to the node's own address, no load balancer.
2. **Objects.** Inventory without the bootstrap machine, `manifests.management`
   (IPAM pools, credentials, and the workload's addon declarations until the
   addon resources exist), then the cluster objects.
3. **First control plane.** Wait for `ControlPlaneInitialized`. The first
   control-plane machine was netbooted by the bootstrap Tinkerbell.
4. **Pause.** Set `spec.paused` on the Cluster, so CAPT creates no further
   Machines or Workflows.
5. **Quiesce.** Wait until no Workflow is pending or running. A machine whose
   Workflow completes here boots from disk and needs nothing more from the
   bootstrap node.
6. **Stop the bootstrap Tinkerbell.** Uninstall the `bootstrap.addons` Helm
   releases. From here no DHCP responder exists until the workload's is up.
7. **Workload addons.** The HelmChartProxy and ClusterResourceSet objects for
   the workload (Cilium, its load-balancer pool, the customized Tinkerbell)
   were declared on the bootstrap cluster in step 2; CAAPH installs them now
   that the control plane is initialized. Wait for their `HelmReleaseProxy`
   status, Tinkerbell last. The workload Tinkerbell is now the only responder.
8. **Move.** `clusterctl init` on the workload, `clusterctl move` (which
   carries Hardware, rufio Machines, Workflows, Templates and the proxies,
   and resumes the Cluster on the target), then re-apply the inventory (the
   bootstrap machine is now a netboot target) and `manifests.management` on
   the workload, since BMC Secrets and anything unlabelled do not move.
9. **Bootstrap teardown.** Eject the media, power the node off. CAPT
   provisions it as a cluster member from the workload Tinkerbell, along with
   the remaining control-plane replicas and the workers.
10. **Root credential.** Capture the cluster's secrets bundle from the
    workload cluster into `provider_secrets` (§2), then wait for `Available`.

Each step is check-before-act: a node that already answers over SSH is
reused, a provider at the requested version is skipped by clusterctl, objects
are server-side applied, a kubeconfig Secret is waited for before the move.
The workflow records its progress in `status.phase` (`bootstrapping`,
`initialized`, `control_plane_initialized`, `paused`, `handed_over`,
`pivoted`, `ready`).

On a failure, `management.bootstrap.on_failure` decides:

- `resume` (default, the AKS model): Create returns the partial state with a
  **warning** carrying the step and error; nothing is torn down. `ModifyPlan`
  turns any non-terminal `status.phase` into a planned in-place update, so the
  next plan reads "phase bootstrapping → (known after apply)" with the stored
  error, and Update continues from that phase. `terraform destroy` at any
  phase tears everything down (§4.5). Hardware runs are long and the scaffold
  is expensive; this is what an operator at the keyboard wants.
- `fail`: Create returns an error, Terraform taints the resource, the next
  apply replaces it: Delete tears the partial cluster down, Create starts over.
  This is what CI wants, because the apply exits non-zero.

### 4.4 Update

Update is per concern and in a fixed order, the AKS way: providers
(`clusterctl upgrade apply` / init / delete, only when `management.providers`
changed), inventory seed and manifests (re-applied), the control plane object
or topology (version, replicas, template refs), then waits. `bootstrap`,
`bootstrap.addons` and `boot` are create-only and never diffed. A resumed
Create (§4.3) is an Update whose first step is the recorded phase.

### 4.5 Delete

Delete the `Cluster` object and wait for it to disappear, which is when CAPI
has released every machine. Already-gone is not an error; a timeout is an
error, never a warning, so Terraform does not forget provisioned hardware.

- **Self-managed (pivoted):** reverse the pivot through a fresh kind cluster:
  init, `bootstrap.addons` (Tinkerbell's host networking stays inside the
  kind node's namespace; only rufio matters here, to power machines off), the
  full inventory, manifests, pause, quiesce, move back, delete, wait, delete
  kind. A failure after the move leaves the kind cluster; the next destroy
  finds the Cluster gone from the workload, finds `<name>-bootstrap`, and
  resumes there.
- **Incomplete create (`status.phase` not terminal):** tear down whatever the
  phase says exists, in reverse: the Cluster on whichever side holds it, then
  the bootstrap node (eject, power off).
- `management.uninstall` runs `clusterctl delete --all` afterwards on an
  external management cluster this resource initialised.

### 4.6 Import

`[kubeconfig:][namespace/]name`. A self-managed cluster imports with its
kubeconfig and then needs `provider_secrets` supplied once through a
write-only `management.import_secrets` argument, after which Read derives the
connection as usual. How the cluster was bootstrapped does not matter to
import, so a cluster stood up by other tooling is managed the same way.

### 4.7 The bootstrap node

`type = "kairos"` boots a user-built Kairos live ISO on `bootstrap.machine`:
the stock `standard` image (k3s included), AuroraBoot with the cloud-config
embedded, `kairos.ram` so the ISO is not the root filesystem. The cloud-config
carries the SSH key and k3s settings; Kairos bundles add anything else the
node needs. Tinkerbell on host networking comes from `bootstrap.addons` or
from k3s auto-deploy manifests in the cloud-config, whichever the user
prefers; the provider waits for SSH and a Ready node either way.

`boot.method` is `auto` or `virtual_media` to attach the ISO through the BMC
with a one-shot boot override, or `manual`, which drives no BMC at all and
waits for SSH. A node that is booted once and thrown away can be started from
a USB stick and a power button, so the BMC is a convenience, not a dependency.
The media stays attached for the life of the node. Nothing is written to
disk, so teardown is eject and power off, there is no reset, and the node's
Hardware is a normal netboot target after the move.

`type = "kind"` runs the same workflow on a kind cluster on the runner, for
providers that need no bare-metal engine (Docker, cloud providers) and for
the reverse pivot on delete.

## 5. Machine templates (the launch template pattern)

CAPI templates are immutable. A change is a new object, the consumer moves its
ref, CAPI rolls out, the old object stays until nothing references it. That is
`aws_launch_template` versions with the housekeeping done by the provider.

```hcl
resource "capi_machine_template" "worker_amd64" {
  infrastructure = "tinkerbell"              # provider name; kind <Infra>MachineTemplate resolved from the CRD label
  name           = "prod-worker-amd64"       # logical name, stable
  namespace      = "capi"
  spec = {                                   # the inner spec (spec.template.spec); provider-specific, dynamic
    hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker", arch = "amd64" } } }] }
  }
  revision_history = 2                       # unreferenced older revisions kept, beyond the referenced ones
}

resource "capi_bootstrap_template" "worker" {
  bootstrap = "talos"                        # kind <Bootstrap>ConfigTemplate
  name      = "prod-worker"
  spec      = { talosVersion = "v1.13", strategicPatches = local.worker_patches }
}
```

Exported:

| Attribute | Meaning |
|---|---|
| `ref` | `{ api_group, api_version, kind, name, namespace }` of the **current** revision; consumers take this |
| `object_name` | `<name>-<8 hex of sha256(rendered spec)>` |
| `revision` | the hash |
| `revisions` | every revision object still present, with whether it is referenced |

Behaviour:

- **Plan:** the manifest is rendered from `spec` plus provider defaults (Talos
  `generateType`, kubeadm `kubeadmConfigSpec: {}`), with `apiVersion`, `kind`,
  the `template.spec` wrapper, labels and namespace owned by the provider. A
  spec change shows as `object_name` and `ref.name` changing, and every
  consumer shows its ref moving, in the same plan.
- **Apply:** server-side apply the new revision; never mutate an existing one.
- **Garbage collection:** on every apply and Read, list revisions by the
  `capi.tf/template=<name>` label, find references by scanning
  MachineDeployments, MachineSets, control planes and ClusterClasses in the
  namespace (MachineSets hold the old ref during a rollout), delete revisions
  that are unreferenced and beyond `revision_history`. A referenced revision
  is never deleted.
- **Delete:** refuse while any revision is referenced, naming the referrer.
- **Import:** `infrastructure:namespace/object_name`; the logical name and
  revision are recovered from labels and the hash.

The control plane uses `capi_machine_template` through
`capi_cluster.control_plane.infrastructure`, and `capi_cluster_class`
references both kinds.

## 6. `capi_machine_deployment` (the node group)

```hcl
resource "capi_machine_deployment" "workers" {
  provider = capi.prod
  cluster  = capi_cluster.prod.id            # "capi/prod": namespace and identity
  name     = "md-0"

  replicas    = 3                            # exclusive with autoscaling
  autoscaling = { min = 2, max = 10 }        # annotations; replicas left to the autoscaler, never diffed
  version     = "v1.34.0"                    # default: the cluster's; skew validated

  # Direct mode:
  template = {
    infrastructure = capi_machine_template.worker_amd64.ref
    bootstrap      = capi_bootstrap_template.worker.ref
  }
  # Topology mode (the cluster has a class):
  class = { name = "worker-amd64", variables = { imageFactory = "..." } }

  failure_domain = "rack-1"                  # validated against Cluster.status.failureDomains
  metadata       = { labels = {...}, annotations = {...} }
  rollout        = { strategy = "RollingUpdate", max_surge = 0, max_unavailable = 1 }
  timeouts       = { node_drain = "5m", node_volume_detach = "5m", node_deletion = "10m" }
  wait           = { rollout = true, timeout = "30m" }
}
```

What the user writes versus what Go fills, which is the abstraction asked for:

| Filled by the provider | Source |
|---|---|
| `apiVersion` | core `Cluster` CRD storage version on the management cluster |
| `kind`, `metadata.namespace`, `spec.clusterName`, cluster-name and deployment-name labels, `selector` | the `cluster` reference |
| `template.spec.version` | the cluster's version when `version` is omitted; a minor-version skew check otherwise |
| `infrastructureRef`, `bootstrap.configRef` and their shape (`apiGroup` for v1beta2, `apiVersion` for v1beta1) | the template refs plus the contract |
| failure domain validity | `Cluster.status.failureDomains` |
| worker class validity and variable names | the referenced `ClusterClass` |
| autoscaler annotations, replicas omitted from the object | `autoscaling` |
| v1beta2 rollout and deletion fields | `rollout`, `timeouts` |

Topology mode writes one entry of `Cluster.spec.topology.workers.machineDeployments`.
That list is `listType=map` keyed by `name` in v1beta2, so each resource owns
its entry under its own field manager with server-side apply, and
`capi_cluster` owns the rest of the Cluster object. Read reads the entry back
from the Cluster and the realised `MachineDeployment` for status. The mode is
read from the Cluster, so `template` and `class` are validated against it at
plan.

Waits and remediation: `wait.rollout` waits for `Available` and
`MachinesUpToDate` with desired counts; on failure the diagnostic carries the
failing Machine's condition (drain timeout, infrastructure failure, Hardware
not found). Delete deletes the object and waits for its Machines to be gone;
with Tinkerbell it then confirms the Hardware is released. A scale to zero
before delete is available as `delete = { scale_to_zero = true }` for
providers that drain slowly. Import: `namespace/name`.

`capi_machine_health_check` is a separate resource (own object, own lifecycle)
taking `cluster` and a selector or a `machine_deployment` reference.

## 7. `capi_cluster_class`

```hcl
resource "capi_cluster_class" "main" {
  name           = "prod"
  infrastructure = capi_machine_template.cluster.ref      # a ClusterTemplate: role = "cluster" on capi_machine_template
  control_plane  = { template = capi_control_plane_template.main.ref, infrastructure = capi_machine_template.cp.ref }
  workers = {
    worker-amd64 = { infrastructure = capi_machine_template.worker_amd64.ref, bootstrap = capi_bootstrap_template.worker.ref }
  }
  variables = [...]
  patches   = [...]
}
```

`capi_machine_template` gains `role = "cluster" | "machine" | "control_plane"`
for the three template kinds an infrastructure or control-plane provider
ships, so one resource covers `ClusterTemplate`, `MachineTemplate` and
`ControlPlaneTemplate`. The class exports `ref` and the worker class names
`capi_machine_deployment.class` validates against. A ref change in a template
flows through the class to the cluster in one plan.

## 8. Addon resources

### 8.1 `capi_helm_chart_proxy`

```hcl
resource "capi_helm_chart_proxy" "cert_manager" {
  provider         = capi.prod
  name             = "cert-manager"
  cluster_selector = { "cluster.x-k8s.io/cluster-name" = capi_cluster.prod.name }
  chart            = { name = "cert-manager", repository = "https://charts.jetstack.io", version = "v1.17.0" }
  release          = { name = "cert-manager", namespace = "cert-manager", values = file("values.yaml") }
  strategy         = "Continuous" | "InstallOnce"
  options          = { wait = true, timeout = "10m", install = { create_namespace = true } }
  wait             = { timeout = "15m" }
}
```

Applied to the management cluster; CAAPH pushes into every matching workload
cluster. `wait` watches the `HelmReleaseProxy` per matched cluster and the
diagnostic quotes Helm's failure message and revision, which is more than
`helm_release` reports. Status exports per-cluster release state. Remediation:
when the matched Cluster no longer exists, CAAPH's uninstall finalizer cannot
reach the workload and hangs; Delete detects that and removes the finalizer.
Requires `capi_provider.helm` or `management.providers.addon.helm`.

### 8.2 `capi_cluster_resource_set`

```hcl
resource "capi_cluster_resource_set" "gateway_crds" {
  provider         = capi.prod
  name             = "gateway-api"
  cluster_selector = { "cluster.x-k8s.io/cluster-name" = capi_cluster.prod.name }
  strategy         = "Reconcile" | "ApplyOnce"
  manifests        = [file("gateway-api.yaml")]       # provider creates the ConfigMaps/Secrets the CRS references
  prune            = true
  wait             = { applied = true, timeout = "10m" }
}
```

Two things the controller does not do are done in Go: **prune**, because a CRS
never deletes objects that leave the set (the resource keeps the applied
object list in state and deletes removals on the workload through the
kubeconfig Secret), and **ready waits** on the applied objects' conditions,
since the `ClusterResourceSetBinding` only reports "applied". The 1 MiB
ConfigMap limit is validated at plan.

## 9. `capi_hardware` and `capi_provider`

`capi_hardware` is one machine: Tinkerbell `Hardware`, rufio `Machine` and
the BMC `Secret`, one resource because none has a life without the others
(the same justification as `aws_eks_addon` carrying its configuration).
Attributes match today's `inventory.machine[]`, plus `adopted` (provisioned,
PXE off) and `cluster` for the label. Delete refuses while a Machine references
the Hardware, naming it; `force` powers the machine off first.

`capi_provider` is one clusterctl provider on an external management cluster:
Create is init, Update is `upgrade apply` (or a re-render at the installed
version when only customizations changed), Delete is `clusterctl delete`
keeping CRDs. It exports `ref` with the installed version, contract and
`kinds`, which `capi_machine_template` and `capi_object` can take instead of a
bare provider name. On a self-managed cluster the same entries live in
`capi_cluster.management.providers`; declaring a provider in both places is an
error detected through the inventory object's labels.

## 10. Ephemeral, data sources, actions

- ephemeral `capi_kubeconfig`: §2. Open reads the Secret or derives from the
  bundle, Renew re-derives before `ttl`, Close does nothing.
- data `capi_cluster`: conditions, phase, endpoint, the rendered describe tree.
- data `capi_provider_components`: for a provider name, version and fetch
  config, the components URL, contract and `kinds`. Reads the release artifact
  at plan when its inputs are literal, so templates render on the first plan.
- actions: `capi_move { from, to, namespace }`, `capi_rollout_restart { kind,
  name }`, `capi_pause` / `capi_resume { cluster }`, `capi_remediate_machine {
  machine }` (the delete-machine annotation). One-shot, stateless, the way the
  module already treats Redfish steps.

## 11. Remediation catalogue (what Go does so HCL does not have to)

| Situation | Behaviour |
|---|---|
| Bootstrap node does not come up | media boot retried with attempts and backoff; a node already answering over SSH is reused; `boot.method = manual` takes the BMC out of the path entirely |
| Two DHCP responders on the network | the handover order in §4.3 never has both Tinkerbells running |
| Provider installed at another version | `Init` upgrades it first (clusterctl refuses a second instance) |
| Provider customizations dropped by `upgrade apply` | customizing repository factory injected into the cluster client too |
| CRDs do not match what templates were rendered against | Create fails before applying, naming the kind |
| Server-side apply conflict | reported with the competing field manager; force only for fields the resource owns |
| Kubeconfig Secret not yet written | waited for before the pivot and before `capi_kubeconfig` returns |
| Provisioning mid-flight at move time | Cluster paused, Workflows drained, bootstrap Tinkerbell uninstalled, all before the move |
| Transient move failure | retried with backoff |
| Create fails midway | `on_failure = resume`: phase persisted, next apply continues; `fail`: taint and replace |
| Cluster deleted out of band | Read removes it from state; any other Read error keeps state |
| Delete stuck on a finalizer | the holding object is named; `force` on the addon resources removes a finalizer whose cluster is gone |
| Self-managed delete fails after the move | kind cluster retained; next destroy resumes from it |
| Machine rollout stuck | wait reports the Machine's condition and the drain or volume-detach timeout that applies |
| Template still referenced | never deleted; the referrer is named |
| Autoscaler or topology controller rewrites a field | not a diff |
| Impossible transition | refused at plan with the rule that applies |

## 12. Migration from the current resource

The Talos bootstrapper, in-place mode, the `cni` block and `management.addons`
are removed first; that is its own plan
(`docs/superpowers/plans/2026-11-04-remove-talos-bootstrap.md`) and lands as
schema version 4. `capi_cluster` then keeps `name`, `kubernetes_version`,
`network`, `topology.control_plane` (as `control_plane.replicas`),
`management` (bootstrap `kind` or `kairos`, pivot only), `inventory`,
`manifests`, `wait`, `status`, `provider_secrets`. The six provider maps move
under `management.providers`; `cluster_class` becomes the presence of
`class`; `topology.workers.machine_deployments` and the topology specs become
`capi_machine_deployment` and the template resources; `output.kubeconfig_path`
goes (the ephemeral replaces it). That reshape is schema version 5 with an
upgrader for state. A `terraform state mv` is not possible across resource
types, so machine deployments are imported after the upgrade.

Build order, each step usable alone against the existing cluster with
`skip_init`:

0. The removal plan above.
1. ephemeral `capi_kubeconfig`, data `capi_provider_components`.
2. `capi_machine_template`, `capi_bootstrap_template` with rotation and GC.
3. `capi_machine_deployment` in both modes, `capi_machine_health_check`.
4. `capi_helm_chart_proxy`, `capi_cluster_resource_set`.
5. `capi_hardware`, `capi_provider`, `capi_cluster_class`, `capi_object`.
6. `capi_cluster` reshaped: `management.providers`, `class`, `control_plane`,
   phases and `on_failure`, derived connection, schema v5.
7. Actions.

The internals carry over: catalog, applier, waiter, describe renderer,
inventory renderer, installer, the Kairos bootstrapper, the BMC media boot,
the addon installer, the pivot and reverse pivot. The generator becomes
per-kind renderers called from ModifyPlan.
