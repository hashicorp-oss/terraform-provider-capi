# Provider-Generated Cluster Manifests Design

Date: 2026-09-16
Status: approved for implementation

## 1. Goal

Replace the clusterctl cluster template (`flavor` / `template.source` / `template.variables`)
with manifests the provider generates itself from the configured `infrastructure`, `bootstrap`
and `control_plane` providers. Each provider map entry gains a `resource_config` attribute whose
value is deep-merged into the spec of that provider's cluster-scoped CAPI object
(`TinkerbellCluster`, `TalosControlPlane`, `TalosConfigTemplate`, ...). A new `cluster_class`
boolean selects between two generation modes: direct objects (the shape a flavor template
renders today) or a `ClusterClass` plus a `Cluster` with `spec.topology`.

Provider kinds and API versions are discovered from the CRDs installed on the management cluster,
not from a hardcoded table, so any provider that follows the CAPI contract works without changes
to this provider.

## 2. Decisions already made

| Decision | Choice |
| --- | --- |
| Template concept | Removed. `template` and `flavor` leave the schema (version 3). No escape hatch. |
| Generation mode | `cluster_class = false` (default) generates direct objects; `true` generates a `ClusterClass` and a topology `Cluster`. Both modes share discovery and the merge engine. |
| Kind / version resolution | Discovered from installed CRDs by clusterctl provider label and kind suffix; version from the CRD's contract label for the core contract in use. |
| Core `Cluster.spec` settings | New top-level `network` attribute (`control_plane_endpoint`, `pods`, `services`, `service_domain`). Nothing else on `Cluster.spec` is exposed. |
| Per-machine-group config | Under `topology`: `control_plane.infrastructure`, `machine_deployments[].infrastructure` and `.bootstrap` (dynamic). `bootstrap.<name>.resource_config` is the default worker config; `infrastructure.<name>.resource_config` targets only the `<Infra>Cluster`. |
| Merge precedence | Known-provider required-field defaults, then `resource_config` (RFC 7396 merge), then generator-owned wiring fields set last so they always win. |
| Mutability | `resource_config` and the topology dynamics update in place. Provider maps keep replacing on any other change. `cluster_class` replaces. |
| Immutable templates | Named `<base>-<hash>` where the hash covers the rendered spec; a change produces a new object and an updated ref. |

## 3. Terraform schema (version 3)

### 3.1 Removed

`template` (source, variables) and `flavor`. The v2→v3 state upgrader drops them; v0 and v1
upgraders emit v3 directly (Terraform does not chain upgraders).

### 3.2 Added top-level attributes

```hcl
cluster_class = false            # Bool, Optional+Computed, default false, RequiresReplace

network = {                      # SingleNested, Optional, mutable
  control_plane_endpoint = { host = "10.0.0.160", port = 6443 }   # Cluster.spec.controlPlaneEndpoint
  pods                   = { cidr_blocks = ["10.244.0.0/16"] }    # Cluster.spec.clusterNetwork.pods
  services               = { cidr_blocks = ["10.96.0.0/12"] }     # Cluster.spec.clusterNetwork.services
  service_domain         = "cluster.local"                        # Cluster.spec.clusterNetwork.serviceDomain
}
```

`port` defaults to 6443 when `host` is set. Unset `network` means the core `Cluster` carries no
`clusterNetwork` / `controlPlaneEndpoint` (providers such as Docker fill the endpoint themselves).

### 3.3 Provider object: `resource_config`

`schema.DynamicAttribute`, Optional, on the shared provider object. The value must be an object
or map; keys are the CRD's camelCase field names.

| Map | Direct mode target | ClusterClass mode target |
| --- | --- | --- |
| `infrastructure` | `<Infra>Cluster.spec` | `<Infra>ClusterTemplate.spec.template.spec` |
| `control_plane` | `<CP>ControlPlane.spec` | `<CP>ControlPlaneTemplate.spec.template.spec` |
| `bootstrap` | default `<Bootstrap>ConfigTemplate.spec.template.spec` for every machine deployment | same, per worker class |
| `core`, `ipam`, `addon` | validation error | validation error |

The provider maps move from `mapplanmodifier.RequiresReplace()` to `RequiresReplaceIf` that
compares state and plan with every entry's `resource_config` blanked: any other difference still
replaces the cluster; a `resource_config` change is an in-place update.

### 3.4 Topology additions

```hcl
topology = {
  control_plane = {
    replicas       = 3
    infrastructure = { hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "cp" } } }] } }
  }
  workers = {
    machine_deployments = [{
      name           = "md-0"
      class          = "worker"           # cluster_class mode only; defaults to name
      replicas       = 2
      infrastructure = { hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker" } } }] } }
      bootstrap      = { generateType = "worker", talosVersion = "v1.13" }   # overrides bootstrap.<name>.resource_config
    }]
  }
}
```

`control_plane.infrastructure` → the control plane's `<Infra>MachineTemplate.spec.template.spec`.
`machine_deployments[].infrastructure` → that deployment's `<Infra>MachineTemplate.spec.template.spec`.
`machine_deployments[].bootstrap` → that deployment's `<Bootstrap>ConfigTemplate.spec.template.spec`;
when unset, `bootstrap.<name>.resource_config` is used. All three are `schema.DynamicAttribute`.
`topology` has no plan modifier, so these update in place.

### 3.5 State upgrader v2→v3

Raw-JSON rewrite in the style of `upgradeV1JSON`: delete `template` and `flavor`; add
`cluster_class: false` and `network: null`; add `resource_config: null` to every entry of the six
provider maps; add `infrastructure: null` to `topology.control_plane` and `infrastructure: null`,
`bootstrap: null` to every machine deployment. `providerAttrNames` gains `resource_config`.

## 4. Backend types (`internal/capi`)

```go
type ProviderConfig struct {
    // ... existing fields ...
    // ResourceConfig is deep-merged into the spec of the provider's cluster-scoped
    // CAPI object. Nil means the object carries only defaults and wiring.
    ResourceConfig map[string]any
}
// HasCustomizations() is unchanged: ResourceConfig never alters component YAML.

type NetworkConfig struct {
    ControlPlaneEndpoint *APIEndpoint // Host, Port
    PodCIDRs, ServiceCIDRs []string
    ServiceDomain string
}

type ControlPlaneTopology struct {
    Replicas       *int64
    Infrastructure map[string]any // <Infra>MachineTemplate spec
}

type MachineDeploymentTopology struct {
    Name, Class, FailureDomain string
    Replicas                   *int64
    Labels, Annotations        map[string]string
    Infrastructure             map[string]any // <Infra>MachineTemplate spec
    Bootstrap                  map[string]any // <Bootstrap>ConfigTemplate spec; nil = provider default
}

type GenerateOptions struct { // replaces TemplateOptions
    ClusterName, Namespace, KubernetesVersion string
    Providers          ProviderSet
    Network            NetworkConfig
    ControlPlane       ControlPlaneTopology
    MachineDeployments []MachineDeploymentTopology
    ClusterClass       bool
}

type ManifestGenerator interface { // replaces TemplateGenerator
    Generate(ctx context.Context, mgmt *Cluster, opts GenerateOptions) ([]byte, error)
}
```

`CreateClusterOptions` loses `Flavor`, `TemplateSource` and `ControlPlaneMachineCount`; it gains
`Network`, `ControlPlane ControlPlaneTopology` and `ClusterClass`. `Variables` stays (clusterctl
component variables). `WorkerMachineCount()` is removed with the template.

## 5. CRD discovery (`internal/capi/kinds.go`)

```go
type KindResolver interface {
    // Resolve returns the GVK of the provider's CRD whose kind ends in suffix.
    Resolve(ctx context.Context, mgmt *Cluster, p ProviderKey, suffix string) (schema.GroupVersionKind, error)
    // CoreVersion returns the storage version of clusters.cluster.x-k8s.io.
    CoreVersion(ctx context.Context, mgmt *Cluster) (string, error)
    // ControlPlaneRefShape reports how the control-plane kind references its
    // machine template (see below).
    ControlPlaneRefShape(ctx context.Context, mgmt *Cluster, gvk schema.GroupVersionKind) (RefShape, error)
}
```

Algorithm for `Resolve`:

1. List `CustomResourceDefinition`s with label `cluster.x-k8s.io/provider=<label>` where `<label>`
   is clusterctl's manifest label: `cluster-api` for core, otherwise `<type>-<name>`
   (`infrastructure-tinkerbell`, `bootstrap-talos`, `control-plane-talos`).
2. Keep CRDs whose `spec.names.kind` ends in `suffix` and, for `Cluster`, `ControlPlane` and
   `MachineTemplate`, does not end in `<suffix>Template` when a plain suffix was asked for
   (`TinkerbellCluster` vs `TinkerbellClusterTemplate`).
3. One match → done. Several → the shortest kind wins if it is unique; otherwise an error listing
   the candidates.
4. Version: read the CRD label `cluster.x-k8s.io/<coreContract>` (e.g. `cluster.x-k8s.io/v1beta2: v1beta1`).
   The label value may list several versions separated by `_`; the first served one wins.
   Without the label fall back to the storage version.
5. Result is cached per (provider key, suffix) for the run.

`RefShape` is derived from the control-plane CRD's OpenAPI schema for the resolved version:
`spec.machineTemplate.spec.infrastructureRef` with an `apiGroup` property → `RefShapeContract`
(`{apiGroup, kind, name}` under `machineTemplate.spec`); `spec.machineTemplate.infrastructureRef`
→ `RefShapeLegacy` (`{apiVersion, kind, name, namespace}` under `machineTemplate`). Core
references (`Cluster.spec.infrastructureRef`, `MachineDeployment` refs) follow the core version:
`v1beta2` → contract shape, `v1beta1` → legacy shape.

Errors name the label and suffix that were searched and the CRDs that were found, so a missing
provider or an unsupported mode is diagnosable from the message.

The resolver is backed by an apiextensions clientset built from the management kubeconfig. Tests
use a fake resolver fed with the CRDs from the CAPT v0.7.9, CACPPT v0.7.1 and CABPT v0.8.2
component files (trimmed to `kind`, `names`, `versions[].name/served/storage`, labels and the
`machineTemplate` slice of the schema) plus a synthetic core CRD.

## 6. Manifest generation (`internal/capi/generate.go`, `generate_clusterclass.go`)

### 6.1 Merge engine

```go
func buildSpec(defaults, resourceConfig map[string]any) (map[string]any, error)
```

`resourceConfig` is applied to a copy of `defaults` with RFC 7396 semantics (`jsonpatch.MergePatch`
over the JSON encodings): nested maps merge, lists and scalars replace, `null` deletes. The
generator then sets its own fields with `unstructured.SetNestedField`, after the merge, so wiring
always wins. Numbers arriving from Terraform are integers when integral, else floats.

Known-provider defaults (`providerDefaults(key ProviderKey, role string)`):

| Provider | Object | Default |
| --- | --- | --- |
| control-plane `talos` | ControlPlane / ControlPlaneTemplate | `controlPlaneConfig.controlplane.generateType: controlplane` |
| bootstrap `talos` | ConfigTemplate | `generateType: worker` |
| control-plane `kubeadm` | ControlPlane / ControlPlaneTemplate | `kubeadmConfigSpec: {}` |
| everything else | — | `{}` |

### 6.2 Names and rotation

| Object | Name |
| --- | --- |
| `Cluster`, `<Infra>Cluster`, `ClusterClass` | `<cluster>` |
| `<CP>ControlPlane` | `<cluster>-control-plane` |
| `MachineDeployment` | `<cluster>-<md name>` |
| `<Infra>MachineTemplate` (control plane) | `<cluster>-control-plane-<hash>` |
| `<Infra>MachineTemplate`, `<Bootstrap>ConfigTemplate` (deployment / class) | `<cluster>-<md or class name>-<hash>` |
| `<Infra>ClusterTemplate`, `<CP>ControlPlaneTemplate` | `<cluster>-<hash>`, `<cluster>-control-plane-<hash>` |

`<hash>` is the first 8 hex characters of SHA-256 over the canonical JSON of the object's
`spec` before the name is set. Immutable template objects are never updated in place; a spec
change yields a new object and the referencing object's ref moves, which is how CAPI rolls out a
change. Stale templates are left for CAPI's owner references to collect at cluster deletion.

### 6.3 Direct mode

Objects, in apply order:

1. `<Infra>Cluster` `<cluster>` — spec = merge(defaults, `infrastructure.resource_config`).
2. `<Infra>MachineTemplate` for the control plane — `spec.template.spec = control_plane.infrastructure`.
3. `<CP>ControlPlane` `<cluster>-control-plane` — spec = merge(defaults, `control_plane.resource_config`),
   then `version`, `replicas` (when set) and the machine-template ref in the discovered `RefShape`.
4. Per deployment: `<Infra>MachineTemplate`, `<Bootstrap>ConfigTemplate`
   (spec = merge(defaults, `md.bootstrap ?? bootstrap.resource_config`)), then `MachineDeployment`
   `<cluster>-<md>` with `clusterName`, `replicas`, selector and template labels
   (`cluster.x-k8s.io/cluster-name`, `cluster.x-k8s.io/deployment-name`, plus `metadata.labels`),
   template annotations, `failureDomain`, `version`, `bootstrap.configRef` and `infrastructureRef`.
5. `Cluster` `<cluster>` — `clusterNetwork` and `controlPlaneEndpoint` from `network`,
   `infrastructureRef` and `controlPlaneRef` in the core version's ref shape.

Every object carries `metadata.namespace` and the label `cluster.x-k8s.io/cluster-name: <cluster>`.
The infrastructure provider is required; a missing `control_plane` entry is an error in this mode
(direct objects need a control-plane kind). A missing `bootstrap` entry is an error only when a
machine deployment exists.

### 6.4 ClusterClass mode

1. `<Infra>ClusterTemplate` `<cluster>-<hash>` — `spec.template.spec = merge(defaults, infrastructure.resource_config)`.
2. `<CP>ControlPlaneTemplate` `<cluster>-control-plane-<hash>` — `spec.template.spec = merge(defaults, control_plane.resource_config)`
   (no `version`, `replicas` or machine-template ref: the topology controller sets them).
3. `<Infra>MachineTemplate` for the control plane, as in direct mode.
4. One worker class per distinct `class` (default: the deployment name): `<Infra>MachineTemplate` and
   `<Bootstrap>ConfigTemplate` named after the class. Deployments sharing a class must have equal
   `infrastructure` and `bootstrap`; otherwise generation fails naming the class.
5. `ClusterClass` `<cluster>` — `infrastructure.templateRef`, `controlPlane.templateRef`,
   `controlPlane.machineInfrastructure.templateRef`, `workers.machineDeployments[]` with
   `class`, `bootstrap.templateRef`, `infrastructure.templateRef`. Refs are `{apiVersion, kind, name}`.
6. `Cluster` `<cluster>` — `clusterNetwork`, `controlPlaneEndpoint`, and `topology`:
   `classRef.name` (v1beta2) or `class` (v1beta1), `version`, `controlPlane.replicas`, and
   `workers.machineDeployments[]` with `class`, `name`, `replicas`, `failureDomain`, `metadata`.

`clusterctl init` receives `CLUSTER_TOPOLOGY=true` as a variable when `ClusterClass` is set (the
gate is Alpha and off by default in cluster-api v1.14). A provider without the needed `*Template`
kind fails `Resolve`, and the error names the kind suffix and provider label.

### 6.5 Rendering

Objects are marshalled as a multi-document YAML stream in the order above and handed to the
existing `Applier`, which server-side-applies each document. The Talos bootstrapper's
`PreTemplateManifests` (machine-secrets and kubeconfig Secrets keyed by cluster name) are applied
first exactly as today.

## 7. Manager changes

- `Manager` holds a `ManifestGenerator` (option `WithManifestGenerator`) and a `KindResolver`
  (option `WithKindResolver`); the default generator is `NewManifestGenerator(resolver)`.
- `CreateCluster` step 3 builds `GenerateOptions` from `CreateClusterOptions` and calls the
  generator; steps 1, 2, 4–7 are unchanged.
- `installer.Init` gets `CLUSTER_TOPOLOGY=true` added to its variables when `opts.ClusterClass`.
- `template.go`, `TemplateOptions`, `templateURL` and the clusterctl `GetClusterTemplate` path are
  deleted.

## 8. Waiter

`WaitForControlPlane` polls the `Cluster` object (`clusters.cluster.x-k8s.io`, version from
`KindResolver.CoreVersion`) until `status.conditions` has `ControlPlaneInitialized=True`
(v1beta2) or `ControlPlaneReady=True` (v1beta1). `WaitForWorkers` keeps polling
MachineDeployments but uses the discovered core version. `WaitForClusterReady` composes both.
The `KubeadmControlPlane` v1beta1 lookup by name is removed, which makes `wait.enabled = true`
valid for Talos control planes.

## 9. Provider changes (`internal/provider`)

- Schema: §3. `providerNestedObject` gains `resource_config`; `providerMapAttribute` uses the
  `RequiresReplaceIf` modifier; `topology` gains the dynamic attributes; `network` and
  `cluster_class` are added; `template` and `flavor` are removed; `Version: 3`.
- Models: `ProviderModel.ResourceConfig types.Dynamic`; `providerAttrTypes()["resource_config"] = types.DynamicType`;
  `ControlPlaneTopologyModel.Infrastructure`, `MachineDeploymentModel.Infrastructure/Bootstrap`;
  `NetworkModel` and children; `ClusterResourceModel.ClusterClass`, `.Network`.
- `dynamicToNative(attr.Value) (any, error)` converts a known, non-null dynamic value (object,
  map, tuple, list, set, string, number, bool) into plain Go values for the backend; unknown or
  non-object top-level values are validation errors.
- `buildCreateOptions` fills `Network`, `ControlPlane`, `MachineDeployments`, `ClusterClass` and
  each `ProviderConfig.ResourceConfig`.
- `validateLifecycleConfig`: `resource_config` only on infrastructure / control_plane / bootstrap;
  `resource_config`, `infrastructure` and `bootstrap` dynamics must be objects; direct mode needs a
  `control_plane` entry; deployments sharing a `class` must agree on their dynamics.
- Upgraders: §3.5.

## 10. Examples, docs, `.test`

- `examples/resources/capi_cluster/cluster-template-talos.yaml` is deleted and replaced by
  `examples/resources/capi_cluster/talos-tinkerbell.tf`, which expresses the same content:
  `network.control_plane_endpoint.host`, `infrastructure.tinkerbell.resource_config.templateInline`,
  `control_plane.talos.resource_config.controlPlaneConfig` (talosVersion, strategicPatches: install
  disk, `provider-id: PROVIDER_ID`, VIP), `bootstrap.talos.resource_config` (worker patches) and
  `topology.control_plane.infrastructure.hardwareAffinity`.
- `.test/main.tf` migrates the same way.
- `.claude/CLAUDE.md` §2, §3.3–3.7, §4, §8.2 and §10 are updated; `docs/` is regenerated.

## 11. Testing

- `kinds_test.go`: label filtering, suffix matching (`Cluster` vs `ClusterTemplate`), shortest-kind
  tie-break and ambiguity error, contract-label version selection with `_`-separated values,
  storage-version fallback, `RefShape` detection for the CACPPT v1beta1 schema and a synthetic
  legacy schema.
- `generate_test.go`: golden multi-document YAML for direct mode (docker+kubeadm minimal,
  tinkerbell+talos full) and ClusterClass mode (same two), merge precedence (defaults <
  resource_config < wiring), list replacement and `null` deletion, hash stability and rotation,
  missing-provider errors, class-conflict error.
- `manager_test.go`: generator receives the options built from `CreateClusterOptions`;
  `CLUSTER_TOPOLOGY` reaches init in ClusterClass mode; existing cleanup-on-failure tests keep
  passing with the mock generator.
- `waiter_test.go`: fake dynamic client with `Cluster` conditions.
- Provider: schema shape, `dynamicToNative`, validation rules, `RequiresReplaceIf` (version change
  → replace; `resource_config` change → no replace), v0/v1/v2 upgraders emit v3.
- `.test`: `terraform validate` and `plan`, then apply on the hardware node.

## 12. Delivery phases

1. Backend: discovery, direct-mode generator, merge engine, manager swap, template removal.
2. Provider: schema v3, models, dynamic conversion, validation, upgraders, options plumbing.
3. ClusterClass mode (generator + `CLUSTER_TOPOLOGY` at init + validation).
4. Waiter.
5. Examples, docs, `.test` migration, hardware verification.
