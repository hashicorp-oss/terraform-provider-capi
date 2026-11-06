# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### ✨ Features

- **The provider block names the management cluster.** `kubeconfig` (content, typically from `ephemeral.capi_kubeconfig`) or a `kubernetes` block, the way the `aws` provider block names the AWS API.
- **Every separable Cluster API object is a resource.** `capi_machine_deployment` (direct or a topology entry under its own field manager, with autoscaler bounds), `capi_machine_health_check`, `capi_cluster_class`, `capi_machine_template` / `capi_control_plane_template` / `capi_bootstrap_template` (hash-named immutable revisions with a `ref`, like `aws_launch_template`), `capi_helm_chart_proxy` and `capi_cluster_resource_set` (addons with per-cluster readiness), `capi_hardware` (Tinkerbell Hardware, rufio Machine and BMC Secret), `capi_provider` (one clusterctl provider) and `capi_object` (any CR).
- **`ephemeral.capi_kubeconfig`**: a workload cluster's kubeconfig from its Secret, or derived from a self-managed cluster's root credential, never written to state.
- **Data sources** `capi_cluster` and `capi_provider_components`.
- **Actions** `capi_pause`, `capi_resume`, `capi_rollout_restart`, `capi_remediate_machine` and `capi_move`.
- **Resumable self-managed creates.** `capi_cluster` records its progress in `status.phase` (`bootstrapping` … `handed_over`, `moved`, `pivoted`, `ready`) and in private state (steps that must not run twice, the bootstrap cluster and the machine a kind bootstrap runs on, a kubeconfig until the root credential is captured). With `management.bootstrap.on_failure = "resume"` the next apply continues where the last stopped; after the move a failure is always kept for the resume. A resume or destroy that needs a kind bootstrap on another machine stops at plan time.
- **Disposable Kairos bootstrap node** for bare metal, with a one-DHCP-responder Tinkerbell handover before the pivot.
- **`timeouts`** (`create`, `update`, `delete`) on every resource, through terraform-plugin-framework-timeouts. Waits get what is left of the operation's timeout.
- **Resource identity** on every resource: import with an `identity` block (Terraform 1.12 and later) as well as by ID.
- **Validators** for CIDR blocks, IP and MAC addresses, Go durations and enumerations, so mistakes surface at plan.
- **`capi_provider` adopts** a provider that was already installed (or is imported): it manages the version, and destroy leaves it installed.

### ⚠️ Breaking Changes

- **`capi_cluster` schema v5.** The resource keeps what `aws_eks_cluster` would: `infrastructure { provider, spec }`, `control_plane { provider, replicas, infrastructure | machine_spec, spec }` or `class { name, namespace }`, `management { self_managed, providers, bootstrap }`. Node groups, templates and classes moved to their own resources. State upgrades from every earlier version run automatically; configuration needs to change. The mapping:
  - The six top-level provider maps → `management.providers.{core, infrastructure, bootstrap, control_plane, ipam, addon}` (self-managed clusters only; elsewhere use `capi_provider`).
  - `management.{kubeconfig, skip_init, uninstall, namespace}` → the provider block; `namespace` is a top-level attribute.
  - `topology` → `control_plane.replicas` and `capi_machine_deployment`; `cluster_class` → `capi_cluster_class` and `class`.
  - `output.kubeconfig_path` and `status.kubeconfig` → `ephemeral.capi_kubeconfig`.
  - `wait = { enabled, timeout }` → `wait = { conditions }` and `timeouts = { create, update, delete }` (`2h`, `60m`, `2h` by default). A configured `wait.timeout` moves to `timeouts`.
  - The Talos bootstrapper and the in-place bootstrap are removed; `management.bootstrap.type` is `kind` or `kairos`.
- **`wait.timeout` is gone from every resource**, replaced by `timeouts`.
- **`capi_machine_deployment`**: the node lifecycle durations moved from `timeouts = { node_drain, node_volume_detach, node_deletion }` to `deletion = { node_drain_timeout, node_volume_detach_timeout, node_deletion_timeout }`, freeing `timeouts` for the operation timeouts.
- **`ephemeral.capi_kubeconfig`**: a derived certificate is valid for `8h` by default (was `1h`).

### 🐛 Bug Fixes

- **`ephemeral.capi_kubeconfig` no longer fails runs longer than about an hour.** It asked Terraform to renew it without implementing renewal; it now asks for none, and the certificate outlasts the run.
- **A resumed self-managed create no longer reinstalls the bootstrap Tinkerbell** after the handover removed it, and **no longer re-applies the cluster to an emptied bootstrap** after `clusterctl move`.
- **A failure after the pivot keeps the captured root credential**, and phase `pivoted` resumes on the cluster itself rather than through a new bootstrap.
- **A refresh after the move reads the cluster itself** instead of dropping the resource because the bootstrap no longer holds it.
- **A destroy whose objects were moved to a lost bootstrap cluster fails** instead of reporting success while machines may still run.
- **The provider example** uses `kubernetes = { ... }` (a nested attribute), and the Kairos example names the template kinds next to their API versions.
