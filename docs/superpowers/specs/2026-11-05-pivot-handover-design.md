# Pivot handover: engine contract, Metal3-ready

Status: design only (2026-11-05), nothing implemented. Builds on provider-design §4.3/§4.5
and bootstrap-topologies §6.3. Code references are to `db0f207`. The migration of
`internal/provider` to `capi/` is in flight, but the pivot itself lives in `internal/capi`
(`manager.go`, `quiescer.go`, `checkpoint.go`) and has not moved.

Upstream sources read for this (shallow clones, commit and date):

| Repo | Commit | Used for |
| --- | --- | --- |
| `kubernetes-sigs/cluster-api` | v1.14.0 (vendored) | `cmd/clusterctl/client/cluster/{objectgraph,mover}.go` |
| `metal3-io/cluster-api-provider-metal3` | `abcb79e` 2026-11-05 | `baremetal/metal3machine_manager.go`, `controllers/metal3machine_controller.go` |
| `metal3-io/baremetal-operator` | `9c811a3` 2026-11-05 | `internal/controller/metal3.io/{baremetalhost_controller,host_state_machine}.go` |
| `metal3-io/metal3-docs` | `4455730` 2026-11-05 | `docs/user-guide/src/capm3/pivoting.md` |
| `metal3-io/metal3-dev-env` | `7076f07` 2026-11-03 | `tests/roles/run_tests/tasks/move.yml` (Metal3's CI pivot) |
| `tinkerbell/tinkerbell` | `3f475e4` 2026-11-04 | `crd/bases/v1alpha1/*.yaml` |

---

## 1. Summary

The current handover (`Manager.handOver`, `manager.go:855`) is written for Tinkerbell:
`TinkerbellQuiescer`, a Tinkerbell-only inventory, "uninstall every bootstrap addon" as the
way to stop the engine, and an order (pause, then drain) that only works because tink-server
finishes Workflows without CAPT. Checking Metal3's pivot procedure against its source
confirms the one-responder rule the provider already follows, and shows five gaps that
matter for Tinkerbell too.

Changes:

1. **An `Engine` contract** (§4.1) replaces `Quiescer`: it covers move labels, drain, the
   pre-move snapshot, stopping the responder, target readiness, release and adoption. The
   implementations are `tinkerbell`, `metal3` and `none`, chosen by the infrastructure
   provider.
2. **Freeze → drain → pause → re-check**, replacing pause → drain (§4.2 H2–H5). Pausing a
   Metal3 cluster freezes each claimed host's status at that moment, so draining must
   finish first.
3. **Move readiness is ensured, not assumed.** The provider applies the CRD move labels on
   both clusters, labels the inventory BMC Secrets to move, waits on `block-move` itself
   within `wait.timeout` (errors name the object), and checks that no webhook is left
   without a backend.
4. **An adoption gate after the move** (H10): the target engine must hold every host in the
   state it had before the move, before anything is unfrozen and before the bootstrap is
   torn down.
5. **The reverse pivot uses the same contract** (§4.4). It now stops the workload's own
   responder before the move back, and Metal3 hosts are made deletable from a bootstrap that
   has no access to the provisioning network.
6. **A correction to provider-design §4.3 step 8:** Workflows and Templates do not move (§3,
   D2).

---

## 2. Gemini's Metal3 guidance, checked

Most of Gemini's text paraphrases `metal3-docs/.../capm3/pivoting.md`. The verdicts below
come from the code.

| # | Claim | Verdict | Evidence and what it changes |
| --- | --- | --- | --- |
| G1 | `clusterctl move` pauses, which propagates to CAPM3 and the BMHs | **Correct**, with a gap | clusterctl sets `spec.paused` on the Cluster (`mover.go:357`). The Metal3Machine controller then sets `baremetalhost.metal3.io/paused: metal3.io/capm3` on **its claimed BMH only** (`metal3machine_controller.go:254`, `SetPauseAnnotation`). **Nothing pauses unclaimed BMHs** (Available or Ready spares, a future bootstrap host). → §4.2 H5 |
| G2 | "BMO serializes inspection data and provisioning state into `baremetalhost.metal3.io/status`" | **Wrong actor, wrong content** | **CAPM3** writes it, in the same patch as the pause, and **deletes `hardware`** from the JSON (`metal3machine_manager.go:397–416`). Hardware details travel in `HardwareData`, which BMO prefers on restore. BMO's part is the restore on the target (`baremetalhost_controller.go:2776`). That restore runs **only once the paused annotation is gone** (the pause check at `:153` comes first), and the annotation is then deleted (`:2805`). |
| G3 | CAPM3 applies `clusterctl.cluster.x-k8s.io/block-move` until the pause and status are committed | **Correct, recent** | Added 2026-06-15 (`4d381c5`), first released in CAPM3 v1.14.0. clusterctl waits about 9.5 min per blocking object (5 s × 1.5ⁿ, 10 steps, `mover.go:369`). One stuck BMH stalls the **whole** move, and the annotation tracks CAPM3's own bookkeeping, not Ironic (the upstream doc says so). → H5 pre-wait |
| G4 | The target BMO rebuilds status without re-inspection or re-imaging and re-registers the hosts in a fresh Ironic | **Correct for claimed hosts** | BMO registers each host again and calls Ironic **adopt** for provisioned and externally provisioned hosts (`:1556`, `:2073`). Adoption failures show only on the BMH (`operationalStatus: error`), **not on Cluster conditions**. → H10. Unclaimed hosts have no annotation, so after registering they go to `inspecting` (`host_state_machine.go:417–421`): a PXE boot of IPA on the target. → H5 |
| G5 | Every BMH must be steady (`provisioned`, `externally provisioned`, `available`) before the move | **Correct, and stricter than stated** | The restored status is whatever CAPM3 captured **at pause time**. Pausing mid-provision captures `provisioning`, and the target then holds a host its Ironic never deployed. Draining after the pause, as we do for Tinkerbell, is therefore wrong for Metal3. → H2–H5 |
| G6 | Stop the bootstrap Ironic before starting the target Ironic or moving | **Correct** | Both the doc and CI agree. CI order (`move.yml`): remove the source Ironic → label CRDs → `clusterctl init` the target → deploy BMO + Ironic on the target and wait for every Deployment → move → **remove the source BMO only after the move**. Two Ironics mean two dnsmasqs and two power-sync loops on the same BMCs. Same principle as our one DHCP responder. → H6 |
| G7 | The target's provisioning bridge needs a static IP before `ironic-endpoint-keepalived` can hold the endpoint VIP | **Correct** (deployment detail) | Per the doc. The reason upstream wants the **same endpoint IP** on both sides is that BMO's `IRONIC_ENDPOINT`, IPA callbacks and image URLs served by Ironic's httpd stay valid. Generalised in §4.3 as endpoint continuity. |
| G8 | Initialise both clusters with `clusterctl init`, or put `clusterctl.cluster.x-k8s.io: ""` on BMO CRDs **and BMC Secrets** | **Partly wrong** | That label only makes a **kind** discoverable. clusterctl v1.14 moves an object only if it belongs to a tenant through its owner chain, or is force-moved (`objectgraph.go:679`). BMHs have no owner chain to the Cluster, so Metal3's CI labels the **BMH CRD** with `clusterctl.cluster.x-k8s.io`, `/move` and `/move-hierarchy`, and **HardwareData** with `clusterctl.cluster.x-k8s.io` and `/move`, on **both** clusters (for the pivot back). BMC Secrets move because BMO gives them an owner reference to the BMH (move-hierarchy). Putting the discovery label on a Secret does nothing. The object-level `clusterctl.cluster.x-k8s.io/move` label on a Secret does work (`objectgraph.go:319`). → H1, D4 |
| G9 | (missing) DHCP leases | **Gemini omits it** | The upstream doc warns that leases do not move: a provisioned host that relies on a dnsmasq lease can change address after the move. Metal3 needs static addressing (Metal3DataTemplate networkData, or IPAM). Tinkerbell is not affected: Hardware carries per-MAC addresses that smee on the target serves the same way. |
| G10 | (missing) deprovisioning on the reverse pivot | **Gemini omits it** | Deleting a Metal3 host with automated cleaning boots IPA, which needs a dnsmasq on the provisioning L2 network. A kind bootstrap on the runner has no such access. → §4.4 |

---

## 3. What this shows about the current design

| | Gap | Affects |
| --- | --- | --- |
| D1 | **The pause → drain order works only by luck.** Tinkerbell works because tink-server runs a Workflow to completion without CAPT. Metal3 freezes the BMH status at pause (G5). | metal3 |
| D2 | **Provider-design §4.3 step 8 is wrong**: "clusterctl move (which carries Hardware, rufio Machines, Workflows, Templates…)". In Tinkerbell `3f475e4` only the Hardware and `bmc.tinkerbell.org` Machine CRDs carry `clusterctl.cluster.x-k8s.io` + `/move`. The Workflow and Template CRDs are unlabelled, so clusterctl never discovers them: they stay on the bootstrap and die with it (bootstrap-topologies §6.3 had this right). On the target, CAPT must therefore never need a Workflow for an already-provisioned machine. That depends on the `v1alpha1.tinkerbell.org/provisioned` annotation and is now checked by H10. Whether the HelmChartProxies move is unverified. | tinkerbell, docs |
| D3 | **Move labels are assumed.** The provider labels nothing and relies on the Tinkerbell chart's CRDs. Metal3 needs labels applied, on both sides. | both |
| D4 | **BMC Secrets are re-applied after the move**, which leaves a window where the target's rufio has a Machine with no Secret. Inventory Secrets can simply carry the object-level `/move` label. | tinkerbell |
| D5 | **Nothing checks adoption.** `finishPivot` goes straight from the re-apply to tearing down the bootstrap. A failed Metal3 adoption, or CAPT re-imaging a provisioned machine, would only show after the bootstrap is gone. | both |
| D6 | **`block-move` is left to clusterctl's fixed backoff**, and its error does not say which object blocked or why. | metal3 |
| D7 | **"Uninstall every bootstrap addon" is too coarse a stop.** Metal3 needs Ironic (the responder) gone before the move and BMO (the controller, with its BMH webhook) kept until after it. A half-removed controller whose webhook stays registered fails clusterctl's finalizer patches mid-move, leaving objects on both sides and the target paused. | metal3 |
| D8 | **The reverse pivot never stops the workload's engine.** That is harmless for Tinkerbell in kind (host networking stays inside the kind node), but with Metal3 two Ironics poll the same BMCs, and with a Kairos bootstrap there are two Tinkerbell responders. | both |
| D9 | **Unclaimed hosts move unpaused and without a snapshot**, so on the target they are re-registered and re-inspected (G4). | metal3 |

---

## 4. Design

### 4.1 The engine contract

`Quiescer` becomes one method of a broader interface. An engine is whatever answers
DHCP/PXE for the cluster's machines, and polls their BMCs.

```go
// Engine is the bare-metal provisioning stack a pivot hands over between
// management clusters. Every method is idempotent; list-returning methods
// return what is still pending (empty = done) so the manager owns the wait.
type Engine interface {
    Name() string

    // MoveLabels are the labels each engine CRD needs, on both clusters, for
    // clusterctl move to carry its objects there and back.
    MoveLabels() map[string][]string // CRD name → label keys

    // Settled lists what is mid-provision in the namespace.
    Settled(ctx context.Context, c *Cluster, ns string) ([]string, error)

    // Snapshot runs with the Cluster paused. It makes every host movable,
    // pausing the ones no CAPI provider paused, and returns the record
    // Adopted checks against plus the hosts not yet movable.
    Snapshot(ctx context.Context, c *Cluster, ns string) ([]HostRecord, []string, error)

    // Responder names the bootstrap addon releases that answer DHCP/PXE or
    // poll BMCs; they are removed before the target's engine starts.
    // Every other bootstrap release lives until the bootstrap is torn down.
    Responder(set AddonSet) AddonSet

    // TargetReady lists what keeps the cluster's own engine from serving at
    // the expected endpoint (beyond the CAAPH releases WaitForAddons checks).
    TargetReady(ctx context.Context, c *Cluster, ns string) ([]string, error)

    // Release undoes Snapshot's pauses on the cluster that now holds the hosts.
    Release(ctx context.Context, c *Cluster, ns string) error

    // Adopted lists recorded hosts not yet back in their recorded state.
    Adopted(ctx context.Context, c *Cluster, ns string, hosts []HostRecord) ([]string, error)

    // PrepareRemoteTeardown readies hosts for deletion from a bootstrap with
    // no access to the provisioning network (Metal3: no automated cleaning).
    PrepareRemoteTeardown(ctx context.Context, c *Cluster, ns string) error
}

// HostRecord is one host as it stood before the move.
type HostRecord struct {
    Name  string `json:"name"`
    State string `json:"state"` // BMH provisioning.state; "provisioned" for Hardware with the annotation
}
```

The engine is chosen from `infrastructure.provider`, or from the single infrastructure entry
in `management.providers` (what `inventoryManifest` already does):

| | `tinkerbell` | `metal3` | `none` (docker, clouds) |
| --- | --- | --- | --- |
| MoveLabels | Hardware and `machines.bmc.tinkerbell.org`: `clusterctl.cluster.x-k8s.io`, `/move` (ensured even though the chart ships them) | `baremetalhosts.metal3.io`: `clusterctl.cluster.x-k8s.io`, `/move`, `/move-hierarchy`; `hardwaredata.metal3.io`: `clusterctl.cluster.x-k8s.io`, `/move` | none |
| Settled | no Workflow in `PREPARING`/`PENDING`/`RUNNING`/`POST` (today's quiescer); **no rufio Job without a `Completed` or `Failed` condition** | every BMH in the namespace is in `available`, `provisioned`, `externally provisioned`, `unmanaged` or `ready`; none `servicing`; none carrying `errorType` | always settled |
| Snapshot | records Hardware with the provisioned annotation; nothing to pause | for every BMH **without** `baremetalhost.metal3.io/paused`: one patch setting `paused: terraform-provider-capi` and `status: <status JSON minus hardware>`, the same shape CAPM3 writes and BMO's webhook validates. Not movable while any BMH lacks paused + status, or carries `block-move` | none |
| Responder | every bootstrap release (today's behaviour) | the releases marked `remove_at = "handover"` (§4.5): Ironic, dnsmasq, httpd, keepalived. BMO stays | every release |
| TargetReady | – (the CAAPH releases cover it) | Ironic answers at the endpoint (§4.3); the BMO Deployment is Available | – |
| Release | – | remove `paused: terraform-provider-capi` (CAPM3 removes its own once clusterctl unpauses the Cluster) | – |
| Adopted | every recorded Hardware still has the provisioned annotation and owner labels; **no Workflow exists for a recorded Hardware** (one would mean CAPT is about to re-image it) | every recorded BMH has `provisioning.state` equal to its record, `operationalStatus: OK`, no `errorType`, no leftover status annotation, and a HardwareData | – |
| PrepareRemoteTeardown | – (rufio power-off needs only the BMC) | `automatedCleaningMode: disabled` on BMHs and Metal3Machines, with a plan-time warning that disks are not wiped | – |

### 4.2 The sequence (self-managed create)

Phase one is unchanged: bootstrap node, providers, bootstrap addons, inventory, manifests,
cluster objects, `ControlPlaneInitialized`. The handover becomes:

| Step | Action | Phase / checkpoint |
| --- | --- | --- |
| H1 | **Labels.** Ensure `Engine.MoveLabels` on the bootstrap's CRDs. Stamp `clusterctl.cluster.x-k8s.io/move` on inventory BMC Secrets at render time (`inventoryObject`), so they move with their rufio Machines (D4). | – |
| H2 | **Freeze.** Annotate the control-plane object and every MachineDeployment, MachineSet, MachinePool and MachineHealthCheck of the cluster with `cluster.x-k8s.io/paused`, under field manager `terraform-provider-capi/pivot`. No new Machines are created and no remediation runs. Machines already in flight continue, because the Machine controller and CAPM3/CAPT are not paused. The topology controller leaves the annotation alone (SSA, another manager). | – |
| H3 | **Drain.** Wait for `Engine.Settled` to be empty, within `wait.timeout`. The set of Machines is fixed now, so this converges. A Machine still waiting for hardware (the bootstrap node's) counts as settled: nothing is in flight for it. | – |
| H4 | **Pause.** Set `spec.paused` on the Cluster (today's `Applier.Pause`). CAPM3 pauses its claimed BMHs and snapshots their status; CAPT stops. | – |
| H5 | **Re-check and snapshot.** If `Settled` is no longer empty (a spare was claimed between H3 and H4), unpause and go back to H3. Unpausing is safe on the source: BMO drops a stale status annotation once a host has status (`:2805`). Otherwise run `Engine.Snapshot` until nothing is pending. Wait until no object of a discovered kind in the namespace carries `block-move` (D6), and check that every webhook covering a moving kind has a Service with ready endpoints (D7). Store the HostRecords in the checkpoint. | `paused`; checkpoint `hosts` |
| H6 | **Stop the source responder.** Uninstall `Engine.Responder(bootstrap.addons)`. From here no responder exists until H7. | `bootstrap_addons_removed` (unchanged meaning, now limited to the responder) |
| H7 | **The cluster's own engine.** `WaitForAddons` (CAAPH), then `Engine.TargetReady`. Then `clusterctl init` on the workload, and `Engine.MoveLabels` on its CRDs (for the pivot back). | `handed_over` |
| H8 | **Move.** `clusterctl move`. `block-move` already cleared in H5. | `moved`; checkpoint `moved` + kubeconfig (unchanged) |
| H9 | **Release.** `Engine.Release` on the workload; re-apply inventory and `manifests.management` (today's 7f, still needed for the bootstrap machine and anything unlabelled). | `moved` |
| H10 | **Adoption gate.** Wait for `Engine.Adopted(hosts)` to be empty, within `wait.timeout`. On failure the error names each host and its state, the bootstrap is kept, and the phase stays `moved` (resumable: H9 and H10 are idempotent). | `moved` |
| H11 | **Unfreeze, then tear down.** Remove the H2 annotations on the workload; the objects carried them across. KCP and the MachineDeployments resume, provisioning the remaining replicas and the bootstrap node from the cluster's own engine. Then delete the bootstrap (its BMO goes with it), capture the root credential, and wait for `Available`, as today. | `pivoted` → `ready` |

The visible phases stay as they are; the new steps fall under `paused`, `handed_over` and
`moved`. The checkpoint gains one optional field, `hosts []HostRecord`, written at H5 and read
at H10. It is needed because BMO consumes the status annotation on restore, and adding it does
not bump `CheckpointVersion` (older readers ignore it). Freeze and unfreeze need no step
record: both are idempotent, and a resume after `moved` always unfreezes after the gate.

Recovery is unchanged: before H6 a failure resumes from the top; between H6 and H8 the
responder is not reinstalled (the cluster's own may already be answering). If the target never
comes up, the H7 error says why (no Ironic endpoint, a release failure). **Rejected:**
reinstalling the bootstrap responder automatically when the target "verifiably isn't serving".
Readiness cannot prove that dnsmasq or smee is silent.

### 4.3 Endpoint continuity and addressing (engine preconditions)

- **Tinkerbell:** unchanged from bootstrap-topologies §6.3. The address need not survive;
  only the metadata URL in the install template does (by DNS name, or a token the provider
  substitutes).
- **Metal3:** the Ironic endpoint must be the **same address** on both sides, as upstream
  intends, because BMO's `IRONIC_ENDPOINT`, image URLs on Ironic's httpd and IPA callbacks
  reference it. On a Kairos bootstrap, keepalived on the live node holds the VIP while the
  bootstrap Ironic runs and releases it at H6; the target's keepalived takes it at H7. This is
  the one-responder order again, with VRRP as a backstop. The target control-plane nodes need
  a static address on the provisioning interface before Ironic starts (G7). That lives in the
  user's machine configuration, which the provider cannot read reliably (dynamic spec), so
  `TargetReady` reports it: `Ironic not answering at <vip>: the provisioning interface on the
  control-plane nodes needs a static address for keepalived`. Before H6, `Snapshot` also
  compares each BMH's `spec.image.url` host with the endpoint and fails if one points at the
  bootstrap node's own address rather than the VIP or a name.
- **Metal3 addressing:** the node network must not depend on dnsmasq leases (G9). Document
  it as a precondition. The provider cannot detect it, and the failure (a lease renewal hours
  later) is beyond the gate.

### 4.4 The reverse pivot (delete)

`deleteSelfManaged` runs the same steps with the workload as the source and the fresh
bootstrap as the target: H1 on the bootstrap, then H2–H5 on the workload, then H6 stops the
**workload's** responder, through its CAAPH HelmChartProxies or by scaling the engine's
Deployments to zero, since the workload is being destroyed anyway (D8). H7 runs on the
bootstrap (`bootstrap.addons` installed in full), then H8 moves, H9 releases, and H10 waits
for adoption: deprovisioning needs BMO and Ironic, or rufio, to hold the hosts before the
Cluster is deleted. Then comes the existing delete-and-wait and teardown, resumable through
`DeleteCheckpoint.MovedTo` as today.

When the bootstrap has no access to the provisioning network (kind on the runner), the
manager calls `PrepareRemoteTeardown` on the workload before H4. For Metal3 that disables
automated cleaning, so deprovisioning is a BMC power-off with no IPA boot. `ModifyPlan`
warns about it at plan time. With a Kairos bootstrap on the provisioning segment, cleaning is
kept.

### 4.5 Schema impact (after the migration lands)

- `management.bootstrap.addons.helm[].remove_at`: `"handover"` (default, today's behaviour)
  or `"teardown"`. `Engine.Responder` keeps the `"teardown"` releases through the move. The
  Metal3 example marks the BMO release `"teardown"` and the Ironic release `"handover"`. With
  the default on every release, the H5 webhook check catches a BMO uninstalled together with
  Ironic: if Helm removed its webhook too, nothing is left dangling; if not, the check fails
  before anything moves.
- `ValidateConfig`: `metal3` with `self_managed` needs `bootstrap.type = "kairos"`, or `kind`
  with a warning that the runner must reach the provisioning network for the bootstrap
  Ironic. The inventory stays Tinkerbell-only. BMH inventory rendering is a follow-up that
  plugs into the same `inventoryManifest` switch: the bootstrap host is left out until H11,
  then registered as `available` once the target engine is adopted.
- No change to `status.phase` values.

---

## 5. Build order (each step with build, tests, lint and docs green)

1. `Engine` + `tinkerbellEngine` wrapping today's quiescer; `noneEngine`. No behaviour change.
   Manager tests assert call order through a recording fake.
2. H1 labels (both sides) and the `/move` label on inventory Secrets; golden update.
3. H2–H5: freeze, drain, pause, re-check loop, the `block-move` pre-wait, the webhook check,
   checkpoint `hosts`. Tests: a fake client in which a spare is claimed between drain and pause.
4. H9–H11: release, the adoption gate (including "no Workflow for a provisioned Hardware"),
   unfreeze before teardown. Resume tests from `moved` with and without `hosts`.
5. The reverse pivot through the contract (stopping the workload's responder,
   `PrepareRemoteTeardown`).
6. `metal3Engine`. Table tests over BMO states; the Snapshot patch is validated against the
   rules BMO's webhook applies to the status annotation (`validateStatusAnnotation`).
7. Schema: `remove_at` and the metal3 validation; the docs fix for D2 in provider-design §4.3
   step 8 and `.claude/CLAUDE.md` §3.5.

## 6. Open verification (hardware or a live cluster)

- CAPT on the target with no Workflow for an already-provisioned TinkerbellMachine
  (D2): it must not create one. If it does, the H10 check catches it before the bootstrap is
  gone, but the fix would be upstream or a provider-side Workflow copy.
- Whether CAAPH's HelmChartProxy CRD carries a move label (provider-design step 8 assumes so).
- KCP paused by annotation while a control-plane Machine it created is still joining:
  expected to finish (kubeadm or CABPT does the join), so status catches up on unfreeze.
- A Metal3 pivot end to end on the Kairos bootstrap: keepalived VIP hand-off at H6/H7, adoption
  of `externally provisioned` and `provisioned` hosts, spares restored without re-inspection.
