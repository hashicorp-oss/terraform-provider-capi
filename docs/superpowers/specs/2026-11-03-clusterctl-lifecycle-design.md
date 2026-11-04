# clusterctl lifecycle support in `capi_cluster`

Date: 2026-11-03. Status: implemented (uncommitted on `talos-reconcile`).

`clusterctl` has a small, well-defined lifecycle: `init`, generate, `move`,
`upgrade apply`, `delete`, `get kubeconfig`, `describe`. Before this change the
resource covered init, generation, the create-time move and the two reads;
every provider change replaced the cluster, Delete issued a delete and walked
away, a self-managed cluster deleted itself from inside, and a vanished cluster
was never noticed. This document records how each operation now maps onto the
resource. The code lives in `internal/capi` (`installer.go`, `manager.go`,
`applier.go`, `waiter.go`, `describe.go`) and `internal/provider`
(`provider_lifecycle.go`, the CRUD methods in `cluster_resource.go`).

## 1. Providers: init, upgrade apply, delete

The six provider maps are owned by the resource whenever
`management.skip_init = false`. On Update, `providerChanges` diffs the prior
state and the plan per map:

| Change | Operation |
|---|---|
| Entry present in both, settings other than `resource_config` differ | `clusterctl upgrade apply` for that entry (`CreateClusterOptions.UpgradeProviders`) |
| Entry present in both, only `resource_config` differs | Cluster objects re-applied; providers untouched |
| Entry added | `clusterctl init` (clusterctl skips providers already at the requested version) |
| Entry removed from `ipam` or `addon` | `clusterctl delete <type> <name>`, CRDs and namespace kept |
| Entry removed from `core` | Nothing; the version is no longer pinned |
| Entry added to or removed from `infrastructure`, `bootstrap`, `control_plane` | Cluster replaced: the generated kinds change |

Update skips init entirely when the maps did not change, because
`clusterctl init` resolves every release again (GitHub API calls for
unpinned entries).

`Installer.Init` converges rather than installs: it lists the clusterctl
provider inventory, upgrades configured providers that are pinned to a version
other than the installed one (clusterctl refuses a second instance of a
provider), then runs `clusterctl init`, which installs what is missing and
skips the rest. An unpinned entry never triggers an upgrade.

`Installer.Upgrade` renders `namespace/name:version` items from the inventory.
A pinned target upgrades to its version; an unpinned target is reinstalled at
its installed version, which re-renders its components with the current
customizations (feature gates, patches, variables). clusterctl fetches upgrade
components through the *cluster* client, which does not inherit the injected
repository factory, so `newClusterctlClient` also injects a
`ClusterClientFactory` whose repository factory is the customizing one.

`management.uninstall = true` runs `clusterctl delete --all` (CRDs and
namespaces kept) on destroy, only for an external `management.kubeconfig`
with `skip_init = false`.

## 2. Delete

`Manager.DeleteCluster` deletes the Cluster object and waits for it to
disappear (`Waiter.WaitForClusterDeleted`, bounded by `wait.timeout`). CAPI
removes the object only once every owned object is gone, so machines and
their infrastructure are released before the management cluster (a kind
bootstrap, or the providers with `uninstall`) goes away. An already-gone
cluster is not an error. A failed or timed-out deletion is an error, not a
warning: Terraform must not forget provisioned machines.

A self-managed cluster is deleted the way the CAPI book describes, reversing
the create pivot (`deleteSelfManaged`):

1. read the Cluster on the workload cluster (gone → see resume below);
2. create a kind bootstrap cluster `<name>-bootstrap` (always the default
   manager; a Talos or Kairos bootstrapper would reboot a physical node);
3. `clusterctl init` with the configured providers, `management.addons`
   without prerequisites (rufio must be there to power machines off), the
   full inventory (BMC Secrets do not move with clusterctl) and
   `manifests.management`;
4. wait for Tinkerbell quiescence on the workload cluster;
5. `clusterctl move` workload → bootstrap;
6. delete the Cluster on the bootstrap cluster and wait;
7. delete the bootstrap cluster.

A failure before the move deletes the bootstrap cluster; a failure after the
move started leaves it in place, since it may hold the only copy of the
management objects. The next destroy finds the Cluster gone from the
workload cluster, asks `Bootstrapper.Exists` for `<name>-bootstrap` and, when
it exists, resumes at step 6. `DeleteClusterOptions.Spec` carries the create
options so the bootstrap cluster is initialised the same way.

In-place Talos clusters keep `TeardownInPlace`. A Kairos node that outlived
its pivot (status.bootstrap_cluster still set) is powered off through the
Kairos bootstrapper before the reverse pivot.

## 3. Read and import

`Manager.GetClusterInfo` reads the Cluster object first. `ErrClusterNotFound`
(a 404 on a reachable management cluster with the CRD present) makes Read
remove the resource from state; any other failure keeps the state and warns,
so an outage never looks like a deletion. `status.endpoint` and
`status.ca_certificate` come from the kubeconfig's current context, the
endpoint falling back to `Cluster.spec.controlPlaneEndpoint`;
`status.description` is the v1beta2 object tree (`RenderObjectTree`: NAME,
STATUS, REASON, SINCE, MESSAGE; Available where present, else Ready), since
cluster-api's own printer is in an internal package.

Import accepts `[kubeconfig:][namespace/]name`. The kubeconfig path and
namespace seed `management` so the first Read fetches the status. Both
require replacement when changed, so the configuration must repeat them.

## 4. Not done

- Removed MachineDeployments in direct mode are not pruned (server-side apply
  only adds); ClusterClass mode handles it through the topology.
- `clusterctl upgrade plan` (contract-wide upgrades) is not exposed; the
  resource upgrades exactly the entries it declares.
- Nothing here has run against hardware or a live management cluster; the
  manager sequences are covered by mocks (`internal/capi/lifecycle_test.go`).
