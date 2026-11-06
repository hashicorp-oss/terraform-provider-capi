# terraform-provider-capi: implementation guide

The provider block is the management cluster (as the `aws` provider block is
the AWS API), every separable Cluster API object is its own resource, and
`capi_cluster` keeps what `aws_eks_cluster` would: identity, version, network,
the control plane, the create-only bootstrap, waits, status. Design:
`docs/superpowers/specs/2026-11-04-provider-design.md`; build plan:
`docs/superpowers/plans/2026-11-05-eks-shaped-resources.md`.

---

## 1. Schema conventions

**Nested-first.** Every concept that groups attributes is a
`schema.SingleNestedAttribute` (`MapNestedAttribute` for named instances,
`ListNestedAttribute` for ordered collections, `MapAttribute` for key/value
pairs). Never blocks. Multi-word attributes become nested objects
(`control_plane_endpoint` → `network { control_plane_endpoint { host, port } }`);
single-subject compounds stay one object (`control_plane`). Computed outputs
live under `status`. Snake case; nested object names are domain nouns;
booleans positive; sensitive values marked.

**Dynamic attributes** (`schema.DynamicAttribute`) carry object specs
(`spec`, `machine_spec`, `variables`, `patches`). terraform-plugin-framework
refuses a dynamic attribute *inside a collection nested attribute* at provider
start ("Dynamic types inside of collections are not currently supported"),
which no Go test catches: the provider map entries' `resource_config` and the
machine-deployment list in older schemas are therefore YAML/JSON strings
(`yamlObject`). Run `.test` (`go install ./... && terraform validate`) after
any schema change.

**Kinds and API versions are discovered, never hardcoded**
(`internal/capi/kinds.go`, `KindCatalog`: CRDs labelled
`cluster.x-k8s.io/provider=<label>`, resolved by kind suffix, version by the
contract label for the core contract in use; `ByKind` for a kind name;
`KindsOf` for a provider's kinds). Ref shapes follow the contract: v1beta2
`{apiGroup, kind, name}`, legacy `{apiVersion, kind, name, namespace}`.

---

## 2. Resources

| Resource | Object(s) | Cloud analogue |
| --- | --- | --- |
| `capi_cluster` | `Cluster`, `<Infra>Cluster`, `<CP>ControlPlane` (+ its own machine template with `machine_spec`), or a topology `Cluster` on a class; the bootstrap/pivot | `aws_eks_cluster` |
| `capi_machine_template` (`role` machine/cluster), `capi_control_plane_template`, `capi_bootstrap_template` | hash-named immutable revisions `<name>-<8 hex sha256(spec)>`, `ref`, GC beyond `revision_history` | `aws_launch_template` |
| `capi_machine_deployment` | `MachineDeployment` (direct) or one topology entry under its own field manager | `aws_eks_node_group` |
| `capi_machine_health_check` | `MachineHealthCheck` (v1beta2 `checks`/`remediation` and legacy shapes) | — |
| `capi_cluster_class` | `ClusterClass` from template refs; exports `ref`, `classes` | — |
| `capi_helm_chart_proxy` | CAAPH `HelmChartProxy` + per-cluster `HelmReleaseProxy` wait; `force_delete` | `aws_eks_addon` |
| `capi_cluster_resource_set` | `ClusterResourceSet` + its ConfigMaps/Secrets; `prune`, `wait.applied`/`ready` | `aws_eks_addon` |
| `capi_hardware` | Tinkerbell `Hardware`, rufio `Machine`, BMC `Secret`; `adopted`, `force` | `aws_ec2_host` |
| `capi_provider` | one clusterctl provider: init / upgrade apply / delete; `kinds`, `ref` | `aws_eks_addon` |
| `capi_object` | any CR of an installed provider | `kubernetes_manifest` |
| ephemeral `capi_kubeconfig` | `<name>-kubeconfig` Secret, or derived from `provider_secrets` | `aws_eks_cluster_auth` |
| data `capi_cluster`, `capi_provider_components` | describe; kinds a release ships | — |

Shared machinery: `internal/capi/objects.go` (`ObjectClient`: server-side
apply with a field manager, get, list, delete, patch, condition and deletion
waits, `WithFieldManager`, `WorkloadClient` through a cluster's kubeconfig
Secret), `capi/object_engine.go` (render, apply-and-wait, subset
drift detection, `wait` and metadata attributes, `attr.Value` ↔ Go for
dynamic specs), `capi/connection.go` (the provider block's
`kubeconfig` content or `kubernetes` block, materialised lazily as a file).

---

## 3. `capi_cluster` (schema version 5)

### 3.1 Target HCL

```hcl
# On the management cluster the provider block names (providers are capi_provider resources).
resource "capi_cluster" "dev" {
  name               = "dev"
  kubernetes_version = "v1.31.0"
  infrastructure     = { provider = "docker" }
  control_plane      = { provider = "kubeadm", replicas = 1, infrastructure = capi_machine_template.cp.ref }
}

# Topology mode.
resource "capi_cluster" "classy" {
  name               = "classy"
  kubernetes_version = "v1.34.0"
  class              = capi_cluster_class.main.ref
  control_plane      = { replicas = 3 }
}

# Self-managed Tinkerbell cluster: bootstrap, providers, seed inventory and manifests are create-only.
resource "capi_cluster" "prod" {
  name               = "prod"
  namespace          = "tinkerbell"
  kubernetes_version = "v1.34.0"
  network            = { control_plane_endpoint = { host = "192.168.1.100" } }

  infrastructure = { provider = "tinkerbell", spec = { templateInline = file("workflow.yaml") } }   # TinkerbellCluster.spec
  control_plane = {
    provider     = "talos"
    replicas     = 3
    machine_spec = { hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "cp" } } }] } }  # own template: CRDs do not exist before the bootstrap
    spec         = { controlPlaneConfig = { controlplane = { talosVersion = "v1.13", strategicPatches = [...] } } } # TalosControlPlane.spec
  }

  management = {
    self_managed = true
    providers = {                    # the six clusterctl provider maps, keyed by provider name
      infrastructure = { tinkerbell = { version = "v0.7.9", fetch_config = { owner = "tinkerbell-community" } } }
      bootstrap      = { talos = { version = "v0.8.2", fetch_config = { owner = "sidero-community" }, resource_config = yamlencode({ talosVersion = "v1.13" }) } }
      control_plane  = { talos = { version = "v0.8.0", fetch_config = { owner = "sidero-community" } } }
      addon          = { helm = {} }
    }
    bootstrap = {
      type = "kairos", machine = "nuc", on_failure = "resume"
      boot   = { method = "manual" | "auto" | "virtual_media", device = "usb", timeout = "10m", attempts = 3 }
      kairos = { iso = "http://.../kairos.iso", address = "192.168.1.50", ssh = { user = "kairos", private_key = file("...") } }
      addons = { helm = [{ name = "tinkerbell", chart = "oci://...", values = yamlencode({ deployment = { hostNetwork = true } }) }] }
    }
  }
  manifests = { management = [local.cilium_proxy, local.tinkerbell_proxy] }   # HelmChartProxy/CRS YAML for the workload's addons
  inventory = { machine = [{ hostname = "nuc", network = {...}, bmc = {...}, labels = { type = "worker" } }, ...] }
  wait      = { conditions = ["Available"] }
  timeouts  = { create = "3h", delete = "2h" }
}

# Node groups, addons and machines are their own resources, applied against
# the cluster once it manages itself (provider alias on ephemeral.capi_kubeconfig).
```

### 3.2 Attributes

| Attribute | Type / mutability | Notes |
| --- | --- | --- |
| `name`, `namespace` | Required / Optional-Computed (`default`); RequiresReplace | the Cluster's identity |
| `kubernetes_version` | Optional, in place | CP `version`, or `spec.topology.version` (required with `class`) |
| `network` | Optional, in place | `control_plane_endpoint {host, port}`, `pods`/`services {cidr_blocks}`, `service_domain` |
| `infrastructure` | `{ provider (RequiresReplace), spec (Dynamic) }` | direct mode; exclusive with `class` |
| `control_plane` | `{ provider (RequiresReplace), replicas, infrastructure = ref, machine_spec (Dynamic), spec (Dynamic) }` | with `class` only `replicas`; otherwise `provider` and one of `infrastructure`/`machine_spec` required |
| `class` | `{ name, namespace }` | topology mode; class↔direct switch replaces (ModifyPlan) |
| `management` | `{ self_managed (RequiresReplace), providers { core, infrastructure, bootstrap, control_plane, ipam, addon }, bootstrap { type, machine, on_failure, boot, kairos, addons } }` | `providers` and `bootstrap` only with `self_managed` (ValidateConfig) |
| `manifests.management` | List(String), re-applied | applied before the cluster objects and again after a move |
| `inventory` | `{ manage, source, machine[] }` | seed inventory of a self-managed Tinkerbell cluster; later machines are `capi_hardware` |
| `wait` | `{ conditions (default ["Available"]) }` | `Available` is `Ready` on the legacy contract; empty list waits for nothing |
| `timeouts` | `{ create (2h), update (60m), delete (2h) }` | operation budgets; the waits get what is left (`remaining(ctx, …)`) |
| `status` | Computed | `phase`, `error`, `endpoint`, `ca_certificate`, `description`, `bootstrap_cluster`, `conditions[]` |
| `provider_secrets` | Computed, sensitive, `UseNonNullStateForUnknown` | root credential captured after the pivot (`<name>-talos` bundle or `<name>-ca`) |

Removed in v5: `cluster_class`, `topology` (control plane → `control_plane.replicas`; workers → `capi_machine_deployment`), `output.kubeconfig_path` and `status.kubeconfig` (→ `ephemeral.capi_kubeconfig`), the six top-level provider maps (→ `management.providers`), `management.{kubeconfig, skip_init, uninstall, namespace}` (the provider block is the management connection; providers on it are `capi_provider`).

### 3.3 Provider maps (`management.providers`)

`providerMapAttribute(desc, lifecycle)` with the shared `providerNestedObject()`
(`version`, `fetch_config {owner, repository, url, oci}`, `config_variables`,
`secret_config_variables`, `deployment`, `manager`, `additional_manifests`,
`manifest_patches`, `patches`, `resource_config` YAML/JSON string). Entries become
`capi.ProviderConfig` in a `capi.ProviderSet`; `fetch_config` resolves through
`capi.ResolveFetchURL` (`internal/capi/fetch.go`); customizations through the
customizing repository factory injected into both the init and the cluster
client (`newClusterctlClient`). Mutability: `infrastructure`/`bootstrap`/
`control_plane` key changes replace (`providerMapRequiresReplace` compares key
sets); other changes are `clusterctl upgrade apply` / init / delete in place
(`providerChanges` → `UpgradeProviders`/`RemovedProviders`, `SkipInit` when
nothing changed). `resource_config` of the infrastructure and control-plane
entries merges under the cluster's `infrastructure.spec` / `control_plane.spec`
(RFC 7396: `providerDefaults` ← `resource_config` ← cluster spec ← wiring).

### 3.4 Generated objects (`internal/capi/generate.go`)

Direct mode: `Cluster <name>` (network + refs), `<Infra>Cluster <name>`,
`<CP>ControlPlane <name>-control-plane` (`version`, `replicas`,
`machineTemplate` ref in the shape `ControlPlaneRefShape` decides), and, with
`machine_spec`, the hash-named `<Infra>MachineTemplate
<name>-control-plane-<hash>` (a spec change rotates it and rolls the control
plane). With a `control_plane.infrastructure` ref nothing else is generated.
Topology mode: one `Cluster` with `spec.topology {classRef|class, version,
controlPlane.replicas}`; the ClusterClass is `capi_cluster_class`. Golden
manifests: `internal/capi/testdata/golden/` (`UPDATE_GOLDEN=1 go test
./internal/capi -run TestGenerate` regenerates). CRD fixtures:
`internal/capi/testdata/crds/`.

### 3.5 Lifecycle (`cluster_resource.go`, `internal/capi/manager.go`)

- **Create/Update** share `run`: external management (`!self_managed`) →
  the provider block's kubeconfig, `SkipInit`; a self-managed cluster that has
  pivoted (`status.phase == ready`) → a kubeconfig derived from
  `provider_secrets` + `status.endpoint` (`derivedKubeconfig`), providers
  converged by `providerChanges`; otherwise the bootstrap flow
  (`ManagementKubeconfig = ""`, `SelfManaged`, `KeepBootstrapOnFailure` when
  `on_failure = resume`).
- **Phases** (`capi.Phase`): bootstrapping → initialized →
  control_plane_initialized → paused → handed_over → moved → pivoted → ready, reported
  through `CreateClusterOptions.OnPhase` and carried by `*capi.PhaseError`
  (with the bootstrap cluster left in place). With `on_failure = resume` a
  failure saves `status.phase`/`status.error` with a warning; `ModifyPlan`
  marks `status` unknown for any non-terminal phase so the next apply re-runs
  the workflow (every step is check-before-act: kind reuses an existing
  cluster, Kairos reuses a node answering over SSH, init converges, objects
  are server-side applied). `fail` returns the error (taint and replace),
  except after the move: from `moved` on a failure is always kept for the
  resume, since a replace would provision the machines a second time.
- **Private state** (`cluster_checkpoint.go`, `internal/capi/checkpoint.go`):
  `status.phase` stays the visible summary (only a visible change plans the
  resuming apply); key `create` holds a versioned `capi.Checkpoint`: steps
  that must not run twice (`bootstrap_addons_removed`, `moved`,
  `bootstrap_deleted`), the bootstrap record (type, name, and for kind the
  host it runs on), the workload kubeconfig from the move until the root
  credential is captured, attempts. The manager skips recorded steps; after
  `moved` it resumes on the cluster itself (`resumeAfterMove` →
  `finishPivot`), never on the bootstrap. `foreignKind` stops a resume,
  refresh connection or destroy (in `ModifyPlan`, at plan time) that needs a
  kind bootstrap living on another host. Key `delete` holds
  `DeleteCheckpoint.MovedTo` after a failed reverse pivot; a lost bootstrap is
  then an error, not a silent success. Private state is saved only when an
  RPC returns (written on every path), never diffed, never readable by
  other resources (so `provider_secrets` stays an attribute), and survives
  state upgrades untouched (hence the `v` field). Unit tests use
  `fakePrivate`; `TestClusterResource_ModifyPlan_DestroyNeedsTheBootstrapHost`
  feeds real private bytes through the protocol server.
- **Pivot** (`Manager.CreateCluster`, at most one DHCP responder at any step):
  bootstrap → providers + `bootstrap.addons` → inventory (bootstrap machine
  excluded) + `manifests.management` → cluster objects →
  `ControlPlaneInitialized` → `handOver` (pause the Cluster, Tinkerbell
  quiescence, uninstall bootstrap addons, `WaitForAddons` on the CAAPH
  proxies) → init workload → `clusterctl move` → re-apply inventory and
  manifests → delete bootstrap → `rootSecrets` into `provider_secrets` →
  readiness.
- **Read**: `GetClusterInfo` through `managementKubeconfig` (provider block,
  kind bootstrap path while unpivoted, derived otherwise). `ErrClusterNotFound`
  removes the resource; any other error keeps state. Status carries the
  Cluster's conditions and `RenderObjectTree` description.
- **Delete**: unpivoted self-managed → delete through the kind bootstrap or
  power the Kairos node off; otherwise `DeleteCluster` (wait for the Cluster
  to vanish; self-managed = reverse pivot through a fresh kind cluster,
  `deleteSelfManaged`, resumable). Failures are errors, never warnings.
- **Import**: `[namespace/]name` on the provider block's management cluster;
  status phase `ready`. A self-managed cluster is imported through its own
  kubeconfig in the provider block.

### 3.6 State upgrades (`cluster_resource_upgrade.go`)

Every upgrader is a raw-JSON rewrite to v5: `upgradeV0JSON` (flat → v1 shape),
`upgradeV1JSON` (→ v2 → v3 → v4 → v5), `upgradeV2JSON`, `upgradeV3JSON`
(`upgradeV3ToV4JSON` refuses Talos/in-place bootstraps), `upgradeV4JSON`:
namespace to the top level, provider maps under `management.providers`,
`cluster_class`/`topology` → `class` or `infrastructure` + `control_plane`
(the generated control-plane template becomes `machine_spec` as a typed
dynamic value, `dynamicJSON`, so the hash and the template name are unchanged
and nothing rolls), `output`/`status.kubeconfig` dropped, `wait.enabled` →
`wait.conditions` (`wait.timeout` dropped, `timeouts` null: a configured
duration moves to the configuration's `timeouts`), `status.phase = ready`,
`bootstrap.on_failure = resume`.

| Version | Change |
| --- | --- |
| 0 | flat attributes |
| 1 | nested objects + inventory |
| 2 | provider maps, `fetch_config`, `topology`, `ipam` |
| 3 | generated manifests: `resource_config`, `network`, `cluster_class` |
| 4 | Kairos pivot only; `cni`, `management.addons`, Talos bootstrapper removed |
| 5 | EKS shape: `namespace`, `infrastructure`/`control_plane`/`class`, `management.providers`, phases and `on_failure`, `wait.conditions`, derived connection |

---

## 4. Other resources: what to know

- **Templates** (`template_resource.go`): `ModifyPlan` computes `object_name`,
  `revision`, `ref` from the planned spec; `collect` keeps the current,
  referenced and `revision_history` unreferenced revisions
  (`capi.TemplateReferences` scans MDs, MachineSets, MachinePools,
  ClusterClasses, control planes). Delete refuses while the current revision
  is referenced. `api_version` renders at plan; otherwise resolved from CRDs.
- **Node groups** (`machine_deployment_*.go`): `nodeGroupInputs` → direct
  `MachineDeployment` (autoscaler annotations on the object, selector labels
  on the template) or a topology entry applied under
  `terraform-provider-capi/machine-deployment/<name>` (listType=map keyed by
  name, so capi_cluster never touches it); `readClusterFacts` supplies the
  core version, topology version, worker classes and failure domains; a wait
  timeout lists each machine's phase, failing conditions and the deletion
  timeout that applies (`readMachineFailures`). Deleting a topology entry
  waits for the controller to remove the realised MD.
- **Addons**: `helmChartProxyReady` requires the proxy's observed generation,
  the conditions, and a Ready `HelmReleaseProxy` (label
  `helmreleaseproxy.addons.cluster.x-k8s.io/helmchartproxy-name`) per matched
  cluster; vacuously ready with no matches. CRS: ConfigMaps `<name>-<i>`,
  Secrets `<name>-secret-<i>` (type `addons.cluster.x-k8s.io/resource-set`),
  `strategy` and the selector immutable (webhook), bindings read from
  `ClusterResourceSetBinding <cluster>`, `objectReady` per kind.
- **Hardware**: `capi.HardwareObjects`; CAPT ownership labels
  `v1alpha1.tinkerbell.org/ownerName|ownerNamespace`, provisioned annotation
  `v1alpha1.tinkerbell.org/provisioned`; `adopted` = provisioned + PXE off.
- **Provider**: `ClusterctlInstaller.Init` with `NoDefaults` (clusterctl's
  `-` provider for bootstrap/control-plane) so no kubeadm sneaks in; the core
  is still added on a first init. Declare `core` and depend on it. A provider
  already installed at create (or imported) is adopted: private key
  `adopted`, version still converged, destroy leaves it installed (warning in
  `ModifyPlan`).
- **Ephemeral `capi_kubeconfig`**: no `RenewAt` (the resource has no Renew,
  and a renewed certificate cannot reach configured provider blocks); a
  derived certificate defaults to 8h. `capi_cluster`'s own derived
  kubeconfig lives for what is left of the operation's timeout + 1h.
- **Cluster class**: v1beta2 `templateRef` vs legacy `ref`; `variables` and
  `patches` are dynamic lists passed through.

---

## 5. Conventions and tooling

Structure and style follow terraform-provider-unifi.

- **Layout**: the provider package is `capi/` (package `capi`, which imports
  the engine `internal/capi` under the same name, as unifi imports go-unifi):
  one resource per `<thing>_resource.go`, data sources `*_data_source.go`,
  actions `*_action.go` (shared bits in `action_helpers.go`), the ephemeral
  resource `kubeconfig_ephemeral.go`, tests named after the file they test.
  Implementation and model types are unexported (`clusterResource`,
  `clusterResourceModel`); constructors `New*` are exported. Interface
  assertions sit in one `var ( ... )` block per type under "Ensure provider
  defined types fully satisfy framework interfaces.".
- **Timeouts**: every resource has `timeouts` (create, update, delete) from
  terraform-plugin-framework-timeouts; each operation reads
  `data.Timeouts.<Op>(ctx, default)` and wraps ctx in `context.WithTimeout`
  (defaults in `timeouts_helper.go`). `wait` blocks only name conditions;
  the wait budget is `remaining(ctx, fallback)`. Models built outside
  configuration (import) set `Timeouts: timeoutsNullValue()`.
- **Identity**: every resource implements `ResourceWithIdentity`
  (`identity.go`: namespaced, node group, provider, object). Identity is set
  next to every `resp.State.Set` and at the top of every Read (the framework
  rejects a Read, even a removal, without one). `ImportState` starts with
  `importID(ctx, req, <format>)`, so an import by identity runs the ID path;
  templates resolve a logical name to the newest revision. Examples:
  `examples/resources/<type>/import-by-identity.tf`.
- **Validators**: `capi/validators` (CIDR, IP, MAC, Go duration) and
  terraform-plugin-framework-validators (`stringvalidator.OneOf`,
  `listvalidator.ValueStringsAre`) on the attribute, not in ValidateConfig,
  when a value is checkable on its own.
- **Formatting**: `make fmt` (`golangci-lint fmt`: gci, gofmt with the
  `interface{}`→`any` rewrite, gofumpt, goimports, golines at 100 columns).
  Signatures wrap one parameter per line.
- **Tests**: `make test` (gotestsum, `junit.xml`, `coverage.txt`) or `go test
  ./...`; render/readback tests next to each resource; fake dynamic client
  with an SSA reactor in `internal/capi/objects_test.go` (`newFakeObjectClient`,
  register kinds in `testKinds`); framework-level tests through
  `providerserver.NewProtocol6WithError(New("test")())` (identity, private
  state); `fakePrivate` stands in for the framework's private state.
- **Lint**: `if ! rtk proxy golangci-lint run ./... ; then` (RTK masks exit
  codes); three pre-existing findings are ignored (`customize_test.go`
  forcetypeassert, `customize.go` nilerr, `bootstrap.go` unparam). CI gates on
  new issues only.
- **Docs**: `make generate` (tfplugindocs) renders `templates/` (index,
  resources, data sources, `guides/`) with `examples/`; nothing in `docs/`
  is edited by hand except `docs/superpowers`.
- **CI**: `.github/workflows/ci.yaml` (lint, build, generate diff, unit
  tests), `acctest.yaml` (Terraform version matrix), `release.yaml`
  (goreleaser); `CHANGELOG.md` keeps an `[Unreleased]` section with
  ✨ Features, ⚠️ Breaking Changes and 🐛 Bug Fixes.
- `.test/`: gitignored harness on the Kairos pivot topology; `go install ./...
  && terraform validate` there is the only place schema-construction errors
  surface.
