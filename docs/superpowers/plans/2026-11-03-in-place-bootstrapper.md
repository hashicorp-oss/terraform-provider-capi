# In-Place Bootstrapper Completion Plan

**Goal:** Make `management.bootstrap.mode = "in_place"` adopt the Talos bootstrap node as the first
control-plane Machine of the cluster it hosts, per the contract in
`docs/superpowers/specs/2026-11-03-bootstrap-topologies-design.md` §4–§5.

**Architecture:** `internal/capi/tinkerbell` renders Hardware/rufio Machine/Secret objects from the
inventory; the manager applies them (and user `manifests.management`) before the generated cluster
objects. The Talos bootstrapper composes its machine config from the control-plane
`resource_config`, adds the provider ID, pins identity in META `0xa` after install, and tears the
whole cluster down in order on destroy. The addon installer gains a CNI-first order with
`prerequisites` between the CNI and the user's Helm releases.

**Constraints:** no schema version bump (new attributes are optional; upgraders emit null); lint
gate `rtk proxy golangci-lint run ./...`; docs regenerate with `go generate ./...` from the repo
root, then `git checkout docs/guides`; schema changes are confirmed with
`go install ./... && (cd .test && terraform validate && terraform plan)`.

## Tasks

- [ ] T1 `internal/capi/talos/addons.go`: `Addons.CNI []HelmRelease` installed first,
      `Addons.Prerequisites []string` applied after the CNI and before `Helm`; `applyCNI` fills
      `CNI`. Tests for order.
- [ ] T2 `internal/capi/tinkerbell`: `InventoryMachine` (in `capi/types.go`) and
      `GenerateInventoryManifests(namespace, clusterName, bootstrapHostname, machines)`; golden test.
- [ ] T3 Manager: `CreateClusterOptions.Inventory`, `BootstrapMachine`, `ManagementManifests`;
      apply inventory + management manifests at step 4a when the infrastructure provider is
      tinkerbell. `DeleteClusterOptions.InPlace` + `Inventory`; `InPlaceTeardowner` optional
      bootstrapper interface replaces `applier.Delete` for in-place. Mock tests.
- [ ] T4 Provider schema/models: `inventory.manage`, top-level `manifests.management`,
      `addons.prerequisites`; extractors; upgraders emit nulls; validation parses YAML.
- [ ] T5 Talos config convergence in `buildTalosBootstrapConfig`: compose control-plane
      strategic patches with `PROVIDER_ID` substitution, add kubelet provider-id when absent,
      endpoint convergence/validation, scheduling warning. Tests.
- [ ] T6 Talos META pin: `Node.PinNetwork`, `ActionPinNetwork` after etcd bootstrap when a
      netmask is set, META document in the `talosmeta` shape; sim + planner tests.
- [ ] T7 Talos in-place teardown: reset the other inventory machines over the Talos API with a
      talosconfig derived from the bundle, power them off through their BMCs, then the bootstrap
      node; provider `Delete` passes `InPlace` and the inventory. Sim test.
- [ ] T8 Docs: CLAUDE.md schema sections, example `talos-bootstrap.tf`, `.test/main.tf`,
      regenerated `docs/resources/cluster.md`; verification commands.
