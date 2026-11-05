# EKS-shaped resources: implementation plan

Date: 2026-11-05. Status: in progress. Design:
`docs/superpowers/specs/2026-11-04-provider-design.md` §12 build order.

The provider block becomes the management cluster (as the aws provider block
is the AWS API), every separable CAPI object becomes its own resource, and
`capi_cluster` keeps only what `aws_eks_cluster` would: identity, version,
network, the control plane, the create-only bootstrap, waits, status.

## Shared pieces

- `internal/provider/connection.go`: the provider block's management
  connection (`kubeconfig` content, or the `kubernetes` block with a path,
  context or host and credentials), materialised as a kubeconfig file for
  clusterctl and the existing path-based code. Built lazily, so `capi_cluster`
  with a bootstrap still works without one.
- `internal/capi/objects.go`: `ObjectClient`, the server-side-apply engine
  for arbitrary objects: apply with a field manager, get, list, delete, wait
  for conditions, wait for deletion, kind catalog of the cluster.
- `internal/provider/object_engine.go`: the resource-side half: render,
  apply-and-wait, read with subset drift detection (every field the
  configuration sets is compared against the live object; fields a controller
  owns are never compared), delete-and-wait, `namespace/name` import, the
  shared `wait` and metadata attributes, and `attr.Value` ↔ Go conversion
  for dynamic `spec` attributes.
- `internal/capi/kubeconfig.go`: derive a short-lived admin kubeconfig from a
  cluster's root secrets (the Talos bundle or the kubeadm CA).
- `internal/capi/components.go`: fetch a provider release's components and
  read the kinds it ships, for plan-time rendering.

## Steps and verification

Each step: `go build ./...`, `go test ./internal/...`, gated lint, docs
regenerated, `.test` plan, one commit.

Done: 1 (7514ba7), 2 (b0a3ea1), 3 (b8390b0: `capi_machine_deployment` with
per-Machine failure reporting on a stuck rollout, `capi_machine_health_check`
with the v1beta2 and legacy shapes, examples for every resource so far),
4 (`capi_helm_chart_proxy` waiting on the per-cluster HelmReleaseProxy with
Helm's message and revision in the diagnostic and `force_delete` for
releases whose cluster is gone; `capi_cluster_resource_set` owning its
ConfigMaps and Secrets, `prune` through the workload kubeconfig Secret,
`wait.applied` on the bindings and `wait.ready` on the applied objects;
`ObjectClient.WorkloadClient` and `capi.DecodeDocuments`), 5 (bd8eb26 →
`capi_hardware` with the owner-label refusal and `force` power-off,
`capi_provider` on the clusterctl lifecycle with `NoDefaults` init and the
inventory read, `capi_cluster_class` from template refs in both contract
shapes with `classes` and `ref`).

1. **Connection, credentials, reads.** Provider `kubeconfig` and
   `kubernetes` block; ephemeral `capi_kubeconfig` (from the management
   cluster's `<name>-kubeconfig` Secret, or derived from `provider_secrets`);
   data `capi_provider_components`; data `capi_cluster`.
2. **Templates.** `capi_machine_template` (roles `machine`, `cluster`,
   `control_plane`) and `capi_bootstrap_template` on the object engine with
   hash-named revisions, `ref`, rotation and garbage collection of
   unreferenced revisions beyond `revision_history`; `capi_object` as the
   generic form.
3. **Node groups.** `capi_machine_deployment` in direct mode (the object) and
   topology mode (one entry of the Cluster's machineDeployments list under its
   own field manager), with cluster-derived defaults, rollout waits and
   per-Machine failure reporting; `capi_machine_health_check`.
4. **Addons.** `capi_helm_chart_proxy` with per-cluster HelmReleaseProxy
   waits and the gone-cluster finalizer fix; `capi_cluster_resource_set`
   with the ConfigMap it references, pruning and applied waits.
5. **Inventory and providers.** `capi_hardware` (Hardware, rufio Machine,
   BMC Secret; `adopted`; refuses to delete claimed hardware without
   `force`); `capi_provider` (init, upgrade apply, delete; exports `ref`);
   `capi_cluster_class`.
6. **`capi_cluster` reshape, schema v5.** `management.providers`, `class`
   or `control_plane`, `status.phase` with `on_failure = resume | fail`,
   the derived connection for Read, `output.kubeconfig_path` removed,
   `topology.workers` removed in favour of `capi_machine_deployment`.
7. **Actions.** `capi_move`, `capi_rollout_restart`, `capi_pause`,
   `capi_resume`, `capi_remediate_machine`.
