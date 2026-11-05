# Remove the Talos bootstrapper and in-place mode; Kairos pivot only

Date: 2026-11-04. Status: approved, not started. Design:
`docs/superpowers/specs/2026-11-04-provider-design.md` §4.3, §4.5, §4.7.
Supersedes `docs/superpowers/plans/2026-11-03-in-place-bootstrapper.md` and
the in-place half of `docs/superpowers/specs/2026-11-03-bootstrap-topologies-design.md`.

## Why

In-place adoption has a structural hazard: CABPT's `UpdateMachine` applies
the full CAPI-rendered config on the first TalosControlPlane change, so the
adopted node drifts toward replacement, and every piece of the in-place
contract (PROVIDER_ID substitution, META pinning, endpoint convergence,
scheduling warnings, `TeardownInPlace`) exists to delay that. A disposable
Kairos node has no such contract: every cluster machine is CAPT-provisioned
the same way and the bootstrap node joins as an ordinary member after the
pivot. The Tinkerbell handover (bootstrap instance on host networking, stopped
before the move; the workload's own instance via CAAPH) removes the second
reason the Talos path existed, carrying a management stack across the pivot.

Sizes, from `wc -l` on 2026-11-04: `internal/capi/talos` is 6,601 lines plus
2,867 of tests. About 1,100 of those are BMC, media boot, addon and kube
helpers the Kairos bootstrapper imports and are kept. The provider's Talos,
in-place and `cni` files are 1,115 lines.

## What goes, what stays, what moves

| Goes | Stays (moves to) |
|---|---|
| `internal/capi/talos`: `reconciler.go`, `planner.go`, `state.go` (reconciler parts), `node.go`, `meta.go`, `image.go`, `config.go`, `capi_secrets.go`, `bootstrapper.go` (Talos parts), `errors.go` (Talos parts), all their tests, `sim_test.go`, `acceptance_test.go`, `amt_debug_test.go`, `probe_debug_test.go`, `manager_integration_test.go`, `teardown_test.go` | `bmc.go`, `mediaboot.go`, `backoff.go`, `state.go` enums (`BootDevice`, `PowerState`), `bootstrapper.go` types `BootConfig`, `BootMethod*`, `DefaultBoot*` → **`internal/capi/bmc`**; `addons.go`, `kube.go` → **`internal/capi/addons`** |
| `capi.PreTemplateManifester`, `capi.InPlaceTeardowner`, `CreateClusterOptions.InPlace` / `PreTemplateManifests` / `WorkloadAddons`, `DeleteClusterOptions.InPlace` / `Inventory` / `BootstrapMachine`, in-place branches in `Manager.CreateCluster`, `DeleteCluster`, `inventoryManifest` | `Quiescer`, `AddonInstaller`, `Bootstrapper`, the pivot, the reverse pivot, `DeleteBootstrapCluster`, `provider_secrets` (new producer, below) |
| `internal/provider`: `talos_bootstrap.go`, `talos_bootstrap_test.go`, `in_place_test.go`, `cni.go`, `cni_test.go`; models `TalosModel`, `TalosImageModel`, `CNIModel`, `GatewayAPIModel`, `ManagementModel.Addons`, `ManagementBootstrapModel.Mode` / `.Talos`; schema `management.bootstrap.mode`, `.talos`, `management.addons`, `cni`; `injectCNIPatch`, `applyInPlaceContract`, `talosClusterBootstrap`, `cniOptionsFor`, `cniRelease`, `workloadAddonsFor` | `kairos_bootstrap.go` (gains `managerFor`, `bootConfigFromModel`), `management_addons_test.go` (becomes `bootstrap_addons_test.go`), `bootstrap_validation_test.go` (Kairos cases only) |
| Examples `talos-bootstrap.tf`; `.test/main.tf` in-place config | `kairos-bootstrap.tf`, `kairos-cloud-config.yaml`, `cilium-values.yaml` (now the Cilium HelmChartProxy's values) |

## Tasks

Each task leaves `go build ./...`, `go test ./internal/...` and
`rtk proxy golangci-lint run ./...` (gated with `if !`) green.

### 1. Carve the shared packages out of `talos`

- Create `internal/capi/bmc`: move `bmc.go`, `bmc_test.go`, `mediaboot.go`,
  `reconciler_redirect_test.go` (the `attachISO` tests), `backoff.go`,
  `backoff_test.go`, the `BootDevice` and `PowerState` enums from `state.go`,
  and `BootConfig`, `BootMethod*`, `DefaultBootTimeout`, `DefaultBootAttempts`
  from `bootstrapper.go`. Add `BootMethodManual`.
- Create `internal/capi/addons`: move `addons.go`, `addons_test.go`,
  `kube.go`, `kube_test.go`. `NewManagementAddonInstaller` becomes
  `addons.NewInstaller`; `talos.Addons` becomes `addons.Set` (keep
  `PreHelmManifestURLs`, `Prerequisites`, `Helm`, `Manifests`). Add
  `Installer.Uninstall(ctx, cluster, set)`, which uninstalls the Helm
  releases in reverse order and ignores a release that is already gone.
- Rewire `internal/capi/kairos` and the provider's `Configure` to the new
  packages. `talos` still compiles against them at the end of this task.

Verify: tests green in `bmc`, `addons`, `kairos`, `talos`.

### 2. Delete the Talos bootstrapper

- Delete every remaining file in `internal/capi/talos` and the directory.
- `internal/capi/interfaces.go`: remove `PreTemplateManifester`,
  `InPlaceTeardowner`.
- `internal/capi/types.go`: remove `CreateClusterOptions.InPlace`,
  `PreTemplateManifests`, `WorkloadAddons`; rename `ManagementAddons` to
  `BootstrapAddons` with the new meaning (installed on the bootstrap cluster
  at create and on the kind cluster at delete, never on the workload); remove
  `DeleteClusterOptions.InPlace`, `Inventory`, `BootstrapMachine`.
- `internal/capi/manager.go`: remove the in-place branches and
  `PreTemplateManifests` handling; `inventoryManifest` keeps only the
  pivot exclusion. Delete the matching cases in `manager_test.go`,
  `lifecycle_test.go`, `mock_test.go` (`MockInPlaceBootstrapper`).
- Provider: delete the files listed above; `kairos_bootstrap.go` takes
  `managerFor` (Kairos or default) and `bootConfigFromModel`; remove the
  Talos branch of `Delete`, `validateManagementBootstrap`'s Talos and
  in-place rules, the `cni` validation, `injectCNIPatch` in
  `buildCreateOptions`. `bootstrapType` returns `kind` or `kairos`.

Verify: `grep -rn "talos\." internal --include='*.go'` finds only the CAPI
provider name strings (`control_plane["talos"]`, `ProviderKey{..., "talos"}`).

### 3. Schema version 4

- `management.bootstrap.type`: `kind` (default) or `kairos`, validated.
- Remove `management.bootstrap.mode`, `management.bootstrap.talos`,
  `management.addons`, `cni`. `boot.method` gains `manual`.
- Rewrite the descriptions of `management.bootstrap`, `bootstrap.addons`
  ("the bootstrap node's own stack; uninstalled before the move"),
  `manifests.management` (re-applied after the move; where HelmChartProxy
  declarations go until the addon resources exist) and `status.bootstrap_cluster`.
- `cluster_resource_upgrade.go`: `upgradeV3JSON` drops `cni`,
  `management.addons`, `management.bootstrap.mode`, `management.bootstrap.talos`;
  a state with `mode = "in_place"` or `type = "talos"` fails the upgrade with
  a message naming the cluster and pointing at import. The v0, v1, v2
  upgraders emit v4 directly. Tests for every upgrader path.

Verify: `go generate ./...`, `git checkout docs/guides`, the `.test` harness
validates (task 7 gives it a configuration).

### 4. The handover in the pivot

In `Manager.CreateCluster`, replace steps 7a–7e with §4.3 of the design:

1. After `ControlPlaneInitialized`: `Applier.Pause(cluster, name, ns)`
   (sets `spec.paused` on the Cluster; new `Applier` method, with `Get`
   already there).
2. `Quiescer.WaitForQuiescence` (unchanged).
3. `AddonInstaller.Uninstall(bootstrap, opts.BootstrapAddons)`.
4. New `Waiter.WaitForAddons(mgmt, clusterName, ns)`: every
   `HelmReleaseProxy` carrying the cluster-name label reports `Ready`. No-op
   when there are none (clusters without CAAPH).
5. `clusterctl init` on the workload, `moveWithRetry` (clusterctl resumes the
   Cluster on the target), re-apply inventory with the bootstrap machine and
   `manifests.management` on the workload (unchanged).
6. `Bootstrapper.Delete` (unchanged), then the phase-two Ready wait.
7. New `RootSecretReader` (default on `DynamicApplier`): read the
   `<name>-talos` Secret, else `<name>-ca`, from the workload cluster into
   `result.ProviderSecrets`. Kairos leaves `Cluster.ProviderSecrets` nil, so
   this is the only producer.

`DeleteCluster` for a self-managed cluster installs `BootstrapAddons` on the
kind cluster (rufio is what matters), pauses, quiesces, moves back, deletes.
A failed move leaves the Cluster paused on the workload; the resume path
(`Bootstrapper.Exists`) is unchanged and the error names the paused state.

Mocks and `lifecycle_test.go` cover the new order with a sequence assertion
like the existing reverse-pivot test.

### 5. `boot.method = manual`

In `internal/capi/kairos`: with `manual`, the BMC is not required on the
machine, Create skips `MediaBoot` and waits for SSH for `boot.timeout`,
Delete logs that the node must be powered off by hand instead of calling the
BMC. Validation: `kairos.address` is required with `manual` (no BMC to
discover anything from). Tests for both Create and Delete.

### 6. Examples and harness

- Delete `examples/resources/capi_cluster/talos-bootstrap.tf`.
- `kairos-bootstrap.tf`: drop `cni` and `management.addons`;
  `bootstrap.addons.helm` carries Tinkerbell with `hostNetwork: true` bound to
  `local.bootstrap_ip`; `manifests.management` carries the three workload
  declarations as YAML until the addon resources exist: a HelmChartProxy for
  Cilium (values from `cilium-values.yaml`, Talos defaults merged in by the
  user now that `cni` is gone, documented in the file), a ClusterResourceSet
  for the load-balancer pool and L2 policy, a HelmChartProxy for the
  customized Tinkerbell. `addon = { helm = {} }` installs CAAPH. The
  control-plane `resource_config` carries the Talos CNI patch
  (`cluster.network.cni.name: none`, `cluster.proxy.disabled: true`,
  KubePrism), moved from `cni.go` into the example as HCL.
- `kairos-cloud-config.yaml`: unchanged, plus a commented k3s auto-deploy
  alternative for Tinkerbell.
- `.test/main.tf`: the Kairos pivot configuration for the lab (NUC as
  bootstrap, Pis as control plane), `wait.enabled = false`, `boot.method =
  "manual"` until the AMT USB-R boot is fixed.
- `terraform fmt`, `go install ./...`, `terraform validate` and `plan` in `.test`.

### 7. Documentation and memory

- `.claude/CLAUDE.md`: §3.2 rewritten for `type = kind | kairos`, the
  handover sequence, `bootstrap.addons` semantics, `manual`; §3.6 (`cni`)
  replaced by a note that CNI is a workload addon; §8.2 adds v4; §10 rows for
  `mode`, `cni`, `management.addons` marked removed in v4.
- `docs/superpowers/plans/2026-11-03-in-place-bootstrapper.md` and the
  in-place sections of `2026-11-03-bootstrap-topologies-design.md`: a
  "superseded 2026-11-04" header pointing here; the files stay as history.
- `docs/resources/cluster.md` regenerated.
- Memory: `bootstrap-topology-decision.md` and
  `talos-bmc-bootstrapper-amt-ider.md` updated to record the removal and
  that the AMT media boot is now the Kairos path's only hardware dependency,
  with `manual` as the fallback.

### 8. Commit

Two commits: the carve-out (task 1) and the removal (tasks 2–7), so the
shared-package move is reviewable on its own.

## Verification on hardware (after the plan, not part of it)

1. Boot the Kairos ISO on the NUC: `virtual_media` over AMT USB-R (currently
   failing), else `manual`.
2. The bootstrap Tinkerbell on host networking netboots the first Pi.
3. Pause, quiesce, uninstall; confirm no DHCP offers from the NUC afterwards.
4. CAAPH brings up Cilium, the pool and Tinkerbell on the first Pi; confirm
   the Tinkerbell Service holds its pool address and answers DHCP.
5. Move; the remaining Pis and then the NUC netboot from the workload
   Tinkerbell; `Available`.
6. Destroy: reverse pivot through kind, machines powered off by rufio.
