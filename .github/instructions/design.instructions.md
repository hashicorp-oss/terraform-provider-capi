---
description: Design guide for implementing the CAPI cluster resource with lifecycle patterns derived from EKS Anywhere and capi-utils (Talos/Sidero). Load when modifying cluster_resource.go, manager.go, interfaces.go, types.go, or any file under internal/capi/ or internal/provider/.
applyTo: 'internal/capi/**,internal/provider/cluster_resource*,**'
---

# Terraform-Provider-CAPI: Design & Implementation Guide

This document is the authoritative design reference for the `capi_cluster` resource.
It synthesises three upstream pattern sources into a single implementation blueprint:

1. **EKS Anywhere** (`aws/eks-anywhere`) — cluster lifecycle management, Tinkerbell bare-metal provisioning
2. **capi-utils** (`siderolabs/capi-utils`) — Talos/Sidero CAPI orchestration, infrastructure provider interface, deploy/check/scale patterns
3. **This provider** (`terraform-provider-capi`) — Terraform Plugin Framework resource wrapping the Manager workflow

---

## 1. High-Level Goal

Provide a Terraform resource (`capi_cluster`) that:

1. Stands up an **ephemeral bootstrap management machine** (Redfish ISO or kind)
2. Installs **Tinkerbell stack + CAPI providers** on bootstrap
3. Generates and applies a **CAPI cluster template** to provision bare-metal workload nodes
4. **Waits** for control plane + workers to become ready
5. **Pivots** CAPI management from bootstrap → workload (clusterctl move)
6. **Tears down** the ephemeral bootstrap machine
7. Outputs a **kubeconfig** for the fully operational workload cluster

On destroy, the reverse happens: CAPI resources are deleted from the management context, then any retained bootstrap infrastructure is cleaned up.

---

## 2. Lifecycle Model (EKS Anywhere Alignment)

### 2.1 Creation Workflow

The create workflow maps directly to EKS Anywhere's `pkg/workflows/management/create.go`:

```
┌─────────────────────────────────────────────────────────────────┐
│  Terraform Apply (capi_cluster Create)                          │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  0. validateLifecycleConfig()          ← preflight checks       │
│     • provider is supported                                     │
│     • tinkerbell requires self_managed = true                   │
│     • tinkerbell requires kubeadm bootstrap/control plane       │
│                                                                 │
│  1. Bootstrap management (Bootstrapper.Create)                  │
│     • kind cluster (Docker provider) OR                         │
│     • Redfish ISO ephemeral machine (Tinkerbell provider)       │
│                                                                 │
│  2. Install CAPI + Tinkerbell stack (Installer.Init)            │
│     • clusterctl init with core + infra + bootstrap + CP        │
│     • EKS-A equivalent: installCAPIComponentsTask               │
│     • capi-utils equivalent: Manager.Install() →                │
│       InstallCore() + InstallProvider()                         │
│                                                                 │
│  3. Generate cluster template (TemplateGenerator.Generate)      │
│     • clusterctl generate cluster                               │
│     • capi-utils equivalent: provider.GetClusterTemplate()      │
│       with ClusterVars() injected into config                   │
│                                                                 │
│  4. Apply template (Applier.Apply)                              │
│     • kubectl apply / runtimeClient.Create per object           │
│     • capi-utils: iterates template.Objs() creating each       │
│                                                                 │
│  5. Wait for readiness (Waiter.WaitForClusterReady)             │
│     • Poll Cluster Ready condition                              │
│     • Poll KubeadmControlPlane Available + replicas             │
│     • Poll MachineDeployment phase + replicas                   │
│     • capi-utils: CheckClusterReady() with retry loop           │
│     • EKS-A: waitForControlPlaneReplicasReady +                 │
│              waitForMachineDeploymentReplicasReady               │
│     • Tinkerbell-specific: 20min node startup timeout           │
│       (vs 10min default) due to bare-metal boot time            │
│                                                                 │
│  6. Retrieve kubeconfig (KubeconfigRetriever.GetKubeconfig)     │
│     • clusterctl get kubeconfig                                 │
│     • capi-utils: client.GetKubeconfig()                        │
│                                                                 │
│  7. Self-managed pivot (Mover.Move with retry)                  │
│     • Install CAPI on workload cluster                          │
│     • clusterctl move bootstrap → workload                      │
│     • EKS-A: moveClusterManagementTask with retry policy        │
│     • Retry: exponential backoff for transient network faults   │
│       (connection reset, timeout, EOF, TLS handshake timeout)   │
│                                                                 │
│  8. Tinkerbell PostWorkloadInit                                 │
│     • Install Tinkerbell stack on workload cluster              │
│     • Uninstall Tinkerbell from bootstrap (UninstallLocal)      │
│     • EKS-A: PostWorkloadInit() in tinkerbell provider          │
│                                                                 │
│  9. Tear down bootstrap (Bootstrapper.Delete)                   │
│     • kind delete / Redfish power-off ephemeral machine         │
│     • Non-fatal: workload cluster is already self-managing      │
│                                                                 │
│  10. Describe cluster + populate state                          │
│     • clusterctl describe cluster                               │
│     • Store kubeconfig, endpoint, CA cert, description          │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 Update Workflow (Reconcile)

Updates to mutable fields trigger **template regeneration + apply** against the existing management context. This mirrors EKS Anywhere's in-place reconciliation model:

- **Mutable** (update-in-place): `kubernetes_version`, `control_plane_machine_count`, `worker_machine_count`, `flavor`, `wait_for_ready`, `skip_init`, `kubeconfig_path`
- **Immutable** (RequiresReplace): `name`, `infrastructure_provider`, `management_kubeconfig`, `bootstrap_provider`, `control_plane_provider`, `core_provider`, `target_namespace`, `self_managed`

For Tinkerbell clusters, EKS Anywhere enforces additional constraints during upgrade:
- Cannot scale + change Kubernetes version simultaneously
- Control plane count cannot change during version upgrade
- Worker node groups cannot be added/removed during version upgrade

### 2.3 Deletion Workflow

Maps to EKS Anywhere's `cmd/eksctl-anywhere/cmd/deletecluster.go`:

1. Resolve management kubeconfig (explicit, or self-managed workload kubeconfig)
2. Delete CAPI Cluster resource via `Applier.Delete`
3. Wait for Cluster object to be gone (capi-utils: retry loop with IsNotFound check)
4. If bootstrap cluster exists, delete it
5. Clean up local files (kubeconfig on disk)

### 2.4 Machine Health Checks

EKS Anywhere installs MachineHealthCheck resources with provider-specific timeouts:

| Parameter | Default | Tinkerbell Override |
|-----------|---------|---------------------|
| NodeStartupTimeout | 10min | **20min** |
| UnhealthyMachineTimeout | 5min | 5min |
| MaxUnhealthy (CP) | 100% | 100% |
| MaxUnhealthy (Workers) | 40% | 40% |

The provider should install MHC resources after cluster creation when the infrastructure provider is `tinkerbell`, using the extended timeouts.

---

## 3. Infrastructure Provider Interface (capi-utils Pattern)

### 3.1 Interface Definition

Borrowed from `capi-utils/pkg/capi/infrastructure/infrastructure.go`, adapted for this provider:

```go
type InfraProvider interface {
    Name() string
    Namespace() string
    Version() string
    WatchingNamespace() string
    Configure(opts any) error
    ProviderVars() (Variables, error)        // env vars for clusterctl init
    ClusterVars(opts any) (Variables, error)  // env vars for template generation
    IsInstalled(ctx, clientset) (bool, error)
    GetClusterTemplate(client, opts) (Template, error)
    WaitReady(ctx, clientset) error
    BootstrapOptions() BootstrapOptions       // NEW: provider-specific bootstrap config
    PostWorkloadInit(ctx, mgmt, workload) error // NEW: post-pivot setup (Tinkerbell stack)
    ValidateCreate(opts) error                 // NEW: preflight validation
    ValidateUpgrade(current, desired) error    // NEW: upgrade constraint validation
}
```

### 3.2 Key Patterns from capi-utils

**Config injection** (`config.go`):
- Custom `config.Reader` implementation wrapping viper
- Supports file, URL, and env-var config sources for clusterctl
- `patchConfig()` injects provider-specific variables into the config before template generation

**Deploy workflow** (`deploy.go`):
- `DeployCluster()` uses functional options pattern (`DeployOption`)
- Resolves provider from installed list
- Injects common vars (`KUBERNETES_VERSION`, `CLUSTER_NAME`, machine counts)
- Calls `provider.ClusterVars()` for provider-specific template vars
- Calls `provider.GetClusterTemplate()` to render template
- Iterates `template.Objs()` creating each object via `runtimeClient.Create()`
- Waits via `retry.Constant(30min)` polling `CheckClusterReady()`

**Check workflow** (`check.go`):
- `CheckClusterReady()` is the canonical readiness check
- Verifies Cluster `Ready` condition
- Verifies control plane `ready` + `initialized` booleans
- Verifies control plane `readyReplicas == replicas`
- Iterates all MachineDeployments: checks phase == `Running` and `readyReplicas == replicas`
- All checks use `retry.ExpectedError()` to signal retryable conditions

**Scale workflow** (`scale.go`):
- `Cluster.Scale()` supports both ControlPlaneNodes and WorkerNodes
- Patches replica count via `runtimeClient.Update()`
- Applies 2-second delay before polling (avoids false-positive on unstarted scale)
- Polls for 30min with `CheckClusterReady()` + node count verification

**Cluster model** (`cluster.go`):
- `Cluster` struct wraps management client, cluster unstructured object, node lists
- `Sync()` re-fetches Machines, kubeconfig, node addresses
- `ControlPlanes()` follows `spec.controlPlaneRef` to get KubeadmControlPlane (or TalosControlPlane)
- `Workers()` lists MachineDeployments by cluster label
- `Health()` delegates to Talos cluster health check (provider-specific)

**Metal client** (`metalclient.go`):
- `GetMetalClient()` creates a `controller-runtime` client with CAPI CRD schemes
- Used for typed access to Cluster, Machine, MachineDeployment resources

---

## 4. Tinkerbell Bare-Metal Provisioning (EKS Anywhere)

### 4.1 Tinkerbell Stack Components

| Component | Role |
|-----------|------|
| Boots/Smee | DHCP + TFTP for network booting |
| Hegel | Metadata service for booted machines |
| Tink | Provisioning engine (workflows + templates) |
| Rufio | BMC management (Redfish, IPMI) for power control |
| kube-vip | HA control plane load balancer |

### 4.2 Two-Stage Stack Lifecycle

**Stage 1 — Bootstrap cluster:**
1. `PreCAPIInstallOnBootstrap()` — install Tinkerbell CRDs
2. `GenerateCAPISpecForCreate()` — render CAPI manifests with hardware selectors
3. Apply hardware resources to bootstrap
4. Wait for BMC connectivity (Rufio)
5. Provision machines via Tinkerbell workflows

**Stage 2 — After workload pivot:**
1. `PostWorkloadInit()` — install Tinkerbell stack on workload cluster
2. `UninstallLocal()` — remove Tinkerbell from bootstrap
3. `CleanupLocalBoots()` — stop bootstrap DHCP/TFTP

### 4.3 Hardware Management

From EKS Anywhere's hardware catalogue:
- Hardware inventory is provided via CSV or Kubernetes Hardware resources
- Indexed by: Hardware ID, BMC reference, secret name
- Validated for sufficient machines per role before creation/upgrade
- **For this provider**: hardware inventory should be a Terraform attribute (list of BMC endpoints + credentials, or a path to hardware CSV)

### 4.4 Redfish ISO Bootstrap Strategy

Our provider's bootstrap for Tinkerbell differs from EKS Anywhere's kind cluster:

1. **Boot ephemeral management machine** via Redfish `VirtualMedia.InsertMedia` (ISO URL)
2. Machine boots into a minimal OS with k8s + CAPI + Tinkerbell pre-installed
3. Provider connects to the machine's API and drives the CAPI workflow
4. After pivot, the bootstrap machine is powered off via Redfish `ComputerSystem.Reset`
5. The machine returns to idle until next cluster create/reconcile

This maps to a new `RedfishBootstrapper` implementation of the `Bootstrapper` interface:

```go
type RedfishBootstrapper struct {
    bmcEndpoint string
    bmcUser     string
    bmcPassword string
    isoURL      string
}

func (r *RedfishBootstrapper) Create(ctx, opts) (*Cluster, error) {
    // 1. Insert virtual media (ISO)
    // 2. Set boot to virtual media
    // 3. Power on
    // 4. Wait for API readiness
    // 5. Return cluster with kubeconfig
}

func (r *RedfishBootstrapper) Delete(ctx, cluster) error {
    // 1. Power off
    // 2. Eject virtual media
}
```

---

## 5. Component Architecture

### 5.1 Interface → Implementation Map

| Interface | Default Impl | Tinkerbell Impl | capi-utils Equivalent |
|-----------|-------------|-----------------|----------------------|
| `Bootstrapper` | `KindBootstrapper` | `RedfishBootstrapper` | N/A (assumes existing mgmt) |
| `Installer` | `ClusterctlInstaller` | `ClusterctlInstaller` + stack | `Manager.Install()` |
| `TemplateGenerator` | `ClusterctlTemplateGenerator` | + hardware vars injection | `provider.GetClusterTemplate()` |
| `Applier` | `DynamicApplier` | same | `runtimeClient.Create()` per obj |
| `Mover` | `ClusterctlMover` | + retry wrapper | N/A |
| `Waiter` | `DynamicWaiter` | + extended timeouts (20min) | `CheckClusterReady()` |
| `KubeconfigRetriever` | `ClusterctlInfoRetriever` | same | `client.GetKubeconfig()` |
| `ClusterDescriber` | `ClusterctlInfoRetriever` | same | N/A |

### 5.2 Factory Pattern

Borrow from EKS Anywhere's `pkg/dependencies/factory.go` and capi-utils' `infrastructure.NewProvider()`:

```go
func (m *Manager) configureForProvider(provider string) {
    switch provider {
    case "tinkerbell":
        m.bootstrapper = NewRedfishBootstrapper(...)
        m.waiter = NewDynamicWaiter(WithNodeStartupTimeout(20*time.Minute))
        // Wire Tinkerbell stack installer as post-init hook
    case "docker":
        m.bootstrapper = NewKindBootstrapper()
    default:
        m.bootstrapper = NewKindBootstrapper()
    }
}
```

### 5.3 Retry & Resilience Patterns

From EKS Anywhere `pkg/clustermanager/cluster_manager.go`:

| Operation | Retry Policy | Timeout |
|-----------|-------------|---------|
| CAPI move | Exponential backoff, network errors only | 15min |
| Control plane wait | Constant poll | 60min |
| Worker wait | Constant poll | 60min |
| Machine deployment wait | Per-machine timeout (10min default, 20min Tinkerbell) | Scaled by replica count |
| Cluster destroy wait | Constant poll, IsNotFound terminal | 30min |

From capi-utils:
- `retry.Constant(30*time.Minute, retry.WithUnits(10*time.Second))` for deploy and scale
- `retry.ExpectedError()` marks retryable conditions vs. terminal failures

---

## 6. Implementation Checklist

### Phase 1: Core Lifecycle (Complete)
- [x] Manager orchestrator with injectable components
- [x] KindBootstrapper for Docker provider
- [x] ClusterctlInstaller, TemplateGenerator, Mover, InfoRetriever
- [x] DynamicApplier, DynamicWaiter
- [x] Cluster resource with Create/Read/Update/Delete
- [x] Immutable field plan modifiers (RequiresReplace)
- [x] Preflight validation (validateLifecycleConfig)
- [x] Move retry with exponential backoff
- [x] Update as reconcile (template regeneration + apply)

### Phase 2: Tinkerbell Provider Support (Next)
- [ ] `RedfishBootstrapper` implementation (BMC virtual media boot)
- [ ] Tinkerbell stack installer (helm charts for boots, smee, hegel, tink, rufio)
- [ ] Hardware inventory schema attribute (`hardware_inventory` block)
- [ ] Extended wait timeouts for bare-metal (20min node startup)
- [ ] `PostWorkloadInit` hook for Tinkerbell stack migration
- [ ] MachineHealthCheck generation with Tinkerbell-specific timeouts

### Phase 3: Infrastructure Provider Interface
- [ ] Define `InfraProvider` interface modeled on capi-utils
- [ ] Port `ProviderVars()` / `ClusterVars()` pattern for template variable injection
- [ ] `IsInstalled()` check before init (skip if already present, like capi-utils)
- [ ] `WaitReady()` for provider-specific readiness
- [ ] Provider factory (`NewInfraProvider(name string)`)

### Phase 4: Advanced Operations
- [ ] Scale support (capi-utils `Cluster.Scale()` pattern)
- [ ] Upgrade constraint validation (Tinkerbell immutability rules)
- [ ] Cluster health checks (capi-utils `Cluster.Health()` pattern)
- [ ] In-cluster controller mode (EKS Anywhere ClusterController pattern)

### Phase 5: clusterctl Parity
- [ ] Add `WaitProviders: true` to Installer.Init with configurable timeout
- [ ] Adopt clusterctl backoff constants (write=40s, read=15s) for DynamicWaiter status checks
- [ ] Provider upgrade via `client.ApplyUpgrade()` on version attribute change
- [ ] Expose `DryRun` option for Move (useful for Terraform plan)
- [ ] Support `clusterctl.cluster.x-k8s.io/move` label on custom resources (Hardware, BMC)
- [ ] ClusterClass template support via `GetClusterTemplate` with topology

---

## 7. Coding Conventions

### 7.1 Component Implementation Pattern

Every CAPI component follows this structure (from `INSTRUCTIONS.md`):

1. **Interface** in `interfaces.go` with doc comment citing EKS Anywhere equivalent
2. **Options struct** in `types.go` with doc comments
3. **Implementation** in its own file (`bootstrap.go`, `installer.go`, etc.)
4. **Functional option** on Manager (`WithBootstrapper`, etc.)
5. **Test mock** in `mock_test.go` recording calls + supporting injectable behavior
6. **Manager integration** wired in correct workflow sequence position

### 7.2 Error Handling

- Wrap with typed errors: `CAPIError{Operation, Cluster, Err}`
- Use `fmt.Errorf("...: %w", sentinel, cause)` for wrappable errors
- Bootstrap cleanup on any failure during create (non-negotiable)
- Move errors: classify as retryable (network) vs. terminal

### 7.3 Terraform Resource Conventions

- **Required + ForceNew**: cluster identity fields (name, infra provider)
- **Optional + ForceNew**: lifecycle-anchor fields (management kubeconfig, self_managed)
- **Optional + Mutable**: spec fields (k8s version, machine counts, flavor)
- **Computed + Sensitive**: kubeconfig, CA certificate
- **Computed**: endpoint, cluster description, bootstrap cluster name, id
- Always populate computed fields from `ClusterResult`; preserve state values as fallback

### 7.4 Testing

- Unit tests: mock all components, verify call sequences and arguments
- Error recovery tests: verify bootstrap cleanup on every failure path
- Provider-specific tests: build-tagged integration tests (`//go:build integration`)
- Acceptance tests: `TF_ACC=1` with real infrastructure

---

## 8. Reference Architecture Mapping

### EKS Anywhere → This Provider

| EKS Anywhere | This Provider | File |
|---|---|---|
| `pkg/workflows/management/create.go` → `Run()` | `Manager.CreateCluster()` | `manager.go` |
| `createBootStrapClusterTask` | `Bootstrapper.Create()` | `bootstrap.go` |
| `installCAPIComponentsTask` | `Installer.Init()` | `installer.go` |
| `createWorkloadClusterTask` | `TemplateGenerator.Generate()` + `Applier.Apply()` | `template.go`, `applier.go` |
| `moveClusterManagementTask` | `Mover.Move()` with `moveWithRetry()` | `mover.go`, `manager.go` |
| `ClusterManager.waitForCAPI` | `Waiter.WaitForClusterReady()` | `waiter.go` |
| `ClusterManager.MoveCAPI` (retry policy) | `Manager.moveWithRetry()` | `manager.go` |
| `Provider` interface | `InfraProvider` interface (future) | `interfaces.go` |
| `pkg/providers/tinkerbell/create.go` | Tinkerbell-specific hooks (future) | TBD |

### capi-utils → This Provider

| capi-utils | This Provider | Adaptation |
|---|---|---|
| `Manager.Install()` | `Installer.Init()` | Split core vs. infra install |
| `Manager.DeployCluster()` | `Manager.CreateCluster()` steps 3-5 | Functional options → struct options |
| `Manager.DestroyCluster()` | `Manager.DeleteCluster()` | Add bootstrap cleanup |
| `Manager.CheckClusterReady()` | `DynamicWaiter.WaitForClusterReady()` | Poll loop with `hasCondition()` |
| `Cluster.Scale()` | Future: scale support in Update | Patch replica count + wait |
| `Cluster.Sync()` / `Cluster.Health()` | Future: Read enrichment | Refresh node state |
| `infrastructure.Provider` interface | Future: `InfraProvider` interface | Add Terraform-specific methods |
| `Config` (viper wrapper) | Config via clusterctl client library | Already using clusterctl.New() |
| `infrastructure.Variables` injection | Template generation env vars | Via `TemplateOptions` fields |

### clusterctl (`kubernetes-sigs/cluster-api`) → This Provider

| clusterctl Client Method | Our Interface | Our Method | Notes |
|---|---|---|---|
| `client.Init()` | `Installer` | `Init()` | Wraps CertManager + provider install |
| `client.GetClusterTemplate()` | `TemplateGenerator` | `Generate()` | Variable injection + repo template fetch |
| `client.Move()` → `objectMover.Move()` | `Mover` | `Move()` | Pause→graph→create→delete→resume all internal |
| `client.GetKubeconfig()` | `KubeconfigRetriever` | `GetKubeconfig()` | Via WorkloadCluster sub-client |
| `client.DescribeCluster()` | `ClusterDescriber` | `Describe()` | `tree.Discovery()` → ObjectTree |
| `client.Delete()` | *(not used directly)* | — | Deletes providers, not clusters |
| `client.ApplyUpgrade()` | *(future)* | — | Provider version upgrades |
| `client.PlanUpgrade()` | *(future)* | — | Upgrade plan discovery |

---

## 9. Key Decisions

1. **Redfish ISO for Tinkerbell bootstrap** instead of kind — the bootstrap machine IS a bare-metal server that exists only during provisioning; keeps the flow consistent with the target infrastructure.

2. **Self-managed mandatory for Tinkerbell** — bare-metal clusters must own their own CAPI management after provisioning (no persistent external management cluster).

3. **Retry on move, not on apply** — template apply is idempotent (CAPI reconciles), but move is a one-shot operation vulnerable to network faults.

4. **capi-utils CheckClusterReady pattern adopted** — checking Cluster condition + control plane ready/initialized + replica counts + MachineDeployment phase is more comprehensive than just the Cluster Ready condition alone.

5. **Infrastructure provider interface is deferred** — current implementation hardcodes provider names in validation; the full `InfraProvider` interface (with `ProviderVars`, `ClusterVars`, `IsInstalled`, `WaitReady`) will be implemented when the second infrastructure provider is added.

---

## 10. clusterctl Client Library Internals

This section documents the upstream `cmd/clusterctl/client` package from `kubernetes-sigs/cluster-api`.
The terraform provider wraps clusterctl-like functionality; understanding its architecture is essential for maintaining API parity and debugging interactions.

### 10.1 Architecture

```
┌──────────────────────────────────────────────────────────────┐
│  clusterctlClient (top-level)                                │
│                                                              │
│  configClient           config.Client                        │
│  repositoryClientFactory RepositoryClientFactory             │
│  clusterClientFactory    ClusterClientFactory                │
│  alphaClient             alpha.Client                        │
├──────────────────────────────────────────────────────────────┤
│  Methods: Init, Move, Delete, GetClusterTemplate,            │
│           GetKubeconfig, PlanUpgrade, ApplyUpgrade,           │
│           DescribeCluster, ProcessYAML                        │
└──────────────┬───────────────────────────────────────────────┘
               │ creates via clusterClientFactory
               ▼
┌──────────────────────────────────────────────────────────────┐
│  cluster.Client (per-cluster)                                │
│                                                              │
│  Proxy()              → Proxy (REST client)                  │
│  CertManager()        → CertManagerClient                    │
│  ProviderComponents() → ComponentsClient                     │
│  ProviderInventory()  → InventoryClient                      │
│  ProviderInstaller()  → ProviderInstaller                    │
│  ObjectMover()        → ObjectMover                          │
│  ProviderUpgrader()   → ProviderUpgrader                     │
│  Template()           → TemplateClient                       │
│  WorkloadCluster()    → WorkloadCluster                      │
└──────────────────────────────────────────────────────────────┘
```

**Key design patterns:**
- **Functional options** everywhere: `InjectConfig`, `InjectProxy`, `InjectRepositoryFactory`, `InjectClusterClientFactory`
- **Factory-based testability**: sub-clients are created via factories, allowing injection of mocks
- **Contract versioning**: every operation starts with `CheckCAPIContract()` to validate API compatibility
- **CRD bootstrapping**: `EnsureCustomResourceDefinitions()` called before provider operations

### 10.2 Init Workflow (clusterctl init)

Source: `cmd/clusterctl/client/init.go`

```
Init(ctx, InitOptions)
  │
  ├─ clusterClientFactory → cluster.Client
  ├─ ProviderInventory().EnsureCustomResourceDefinitions()
  ├─ ProviderInventory().CheckCAPIContract() [AllowCAPINotInstalled]
  │
  ├─ addDefaultProviders()                    ← if first run (no core provider):
  │    └─ auto-adds: cluster-api, kubeadm, kubeadm-control-plane
  │
  ├─ setupInstaller()
  │    ├─ ProviderInstaller()
  │    └─ for each provider type:
  │         getComponentsByName() → installer.Add(components)
  │
  ├─ installer.Validate()
  │    ├─ single instance per provider
  │    ├─ compatible contract versions
  │    └─ CRD naming scheme compliance
  │
  ├─ CertManager().EnsureInstalled()
  │
  └─ installer.Install(ctx, InstallOptions{WaitProviders, WaitProviderTimeout})
```

**InitOptions** maps directly to our `InitOptions`:

| clusterctl InitOptions | Our InitOptions | Notes |
|---|---|---|
| `CoreProvider` | `CoreProvider` | e.g. `cluster-api:v1.12.2` |
| `BootstrapProviders` | `BootstrapProviders` | e.g. `["kubeadm:v1.12.2"]` |
| `ControlPlaneProviders` | `ControlPlaneProviders` | e.g. `["kubeadm:v1.12.2"]` |
| `InfrastructureProviders` | `InfrastructureProviders` | e.g. `["docker:v1.12.2"]` |
| `TargetNamespace` | *(not yet)* | Provider namespace |
| `WaitProviders` / `WaitProviderTimeout` | *(not yet)* | Default 5min |
| `IPAMProviders`, `RuntimeExtensionProviders`, `AddonProviders` | *(not yet)* | Extended provider types |

**Our provider's `Installer.Init` should:**
1. Create a `clusterctlClient` via `client.New()` with the management cluster kubeconfig
2. Call `client.Init()` with the appropriate `InitOptions`
3. CertManager installation is handled automatically by clusterctl
4. Add `WaitProviders: true` with a configurable timeout (default 5min)

### 10.3 Move Workflow (clusterctl move) — Deep Dive

Source: `cmd/clusterctl/client/cluster/mover.go`

This is the most complex operation and the one most critical to our self-managed cluster workflow.

#### 10.3.1 Object Graph

The move starts by building an **object graph** of all CAPI resources:

```go
type objectGraph struct {
    proxy             Proxy
    providerInventory InventoryClient
    uidToNode         map[types.UID]*node
    types             map[string]*discoveryTypeInfo
}

type node struct {
    identity           corev1.ObjectReference
    owners             map[*node]ownerReferenceAttributes  // explicit OwnerRef
    softOwners         map[*node]empty                     // naming-convention links
    forceMove          bool                                // CRD label: clusterctl.cluster.x-k8s.io/move
    forceMoveHierarchy bool                                // CRD label: clusterctl.cluster.x-k8s.io/move-hierarchy
    isGlobal           bool                                // cluster-scoped resource
    tenant             map[*node]empty                     // which Cluster/CRS owns this
    blockingMove       bool                                // has block-move annotation
    newUID             types.UID                           // UID after create on target
}
```

**Discovery:**
1. `getDiscoveryTypes()` — lists all CRDs labeled with `clusterctl.cluster.x-k8s.io`, plus Secret and ConfigMap
2. `Discovery()` — lists all objects of those types in the namespace, builds nodes + OwnerReference edges
3. `setSoftOwnership()` — links orphaned Secrets to Clusters by name (e.g. `<cluster>-kubeconfig`)
4. `setTenants()` — traces `forceMoveHierarchy` chains (Cluster, ClusterClass, ClusterResourceSet)
5. `getMoveNodes()` — returns nodes belonging to at least one tenant or labeled forceMove

#### 10.3.2 Pre-Move Validation

Before any mutation:
1. **`checkTargetProviders`** — all providers on source must exist on target with `≥` version
2. **`checkProvisioningCompleted`** — for every Cluster:
   - `Status.Initialization.InfrastructureProvisioned` must be true
   - `ClusterControlPlaneInitializedCondition` must be true
   - For every Machine: `Status.NodeRef` must be set (node is running)
3. **`checkClustersNotPaused`** — refuses if any Cluster is already paused
4. **`checkClusterClassesNotPaused`** — same for ClusterClasses

#### 10.3.3 Move Sequence

```
move(ctx, graph, toProxy, mutators)
  │
  ├─ 1. checkClustersNotPaused()
  ├─ 2. checkClusterClassesNotPaused()
  │
  ├─ 3. setClusterPause(source, clusters, true)     ← stop reconciliation
  ├─ 4. setClusterClassPause(source, classes, true)
  │
  ├─ 5. waitReadyForMove()
  │      ├─ For each node with blockingMove=true:
  │      │    poll until BlockMoveAnnotation is removed
  │      └─ Backoff: 5s base, 1.5x, 10 steps (~2min total)
  │
  ├─ 6. getMoveSequence(graph)                       ← topological sort
  │      └─ Groups nodes so owners precede dependents
  │         Group 1: Clusters, ClusterClasses
  │         Group 2: MachineDeployments, MachineSets
  │         Group 3: Machines
  │         Group 4: Secrets, ConfigMaps, infra objects
  │
  ├─ 7. createGroup() for each group (forward order)
  │      ├─ ensureNamespace() on target
  │      ├─ read source object
  │      ├─ clear ResourceVersion
  │      ├─ rebuild OwnerReferences with target UIDs
  │      ├─ apply mutators
  │      ├─ create on target (or update if AlreadyExists)
  │      ├─ patch managed fields to preserve SSA ownership
  │      └─ each create wrapped in retryWithExponentialBackoff(writeBackoff)
  │
  ├─ 8. deleteGroup() for each group (REVERSE order)
  │      ├─ add delete-for-move annotation
  │      ├─ delete object
  │      ├─ force-remove finalizers (immediate deletion)
  │      └─ each delete wrapped in retryWithExponentialBackoff(writeBackoff)
  │
  ├─ 9. setClusterClassPause(target, classes, false) ← resume reconciliation
  └─ 10. setClusterPause(target, clusters, false)
```

#### 10.3.4 What Our Mover Must Handle

Our `ClusterctlMover` wraps `clusterctlClient.Move()` which delegates to `objectMover.Move()`. Key implications:

1. **Pre-move provider installation on target** — before calling Move, we must have already run `clusterctl init` on the workload cluster (step 7 in our create workflow)
2. **Pause/resume is internal** — the mover handles pausing; we must NOT separately pause clusters
3. **Provisioning must be complete** — move will fail if infra is not provisioned or machines have no NodeRef. Our `WaitForClusterReady` must complete BEFORE attempting move
4. **BlockMoveAnnotation** — if any resource has this annotation, move waits up to ~2min. Providers can use this to block move during critical operations
5. **Force delete on source** — move removes finalizers from source objects. This is intentional: the objects now exist on the target
6. **AlreadyExists handling** — if interrupted mid-move, a retry will update existing objects on the target rather than failing

### 10.4 Template Generation

Source: `cmd/clusterctl/client/config.go`

```
GetClusterTemplate(ctx, GetClusterTemplateOptions)
  │
  ├─ Resolve template source (ProviderRepository | URL | ConfigMap)
  ├─ Auto-detect namespace, provider, version from management cluster
  │
  ├─ templateOptionsToVariables():
  │    NAMESPACE              → options.TargetNamespace
  │    CLUSTER_NAME           → options.ClusterName (validated as DNS-1123)
  │    KUBERNETES_VERSION     → options.KubernetesVersion (semver validated)
  │    CONTROL_PLANE_MACHINE_COUNT → options.ControlPlaneMachineCount (default: 1)
  │    WORKER_MACHINE_COUNT   → options.WorkerMachineCount (default: 0)
  │
  ├─ repo.Templates(version).Get(flavor, namespace)
  └─ addClusterClassIfMissing()   ← resolves CC template if needed
```

**Our `TemplateGenerator.Generate` should:**
1. Use `clusterctlClient.GetClusterTemplate()` with `ProviderRepositorySource`
2. Set the infrastructure provider, flavor, and machine counts via options
3. Inject additional variables for provider-specific templates (e.g., Tinkerbell hardware selectors, BMC endpoints)
4. The template is returned as `[]byte` (YAML) ready for `Applier.Apply`

### 10.5 Describe Cluster

Source: `cmd/clusterctl/client/describe.go`

```go
DescribeCluster(ctx, DescribeClusterOptions) (*tree.ObjectTree, error)
```

Uses `tree.Discovery()` to build an **ObjectTree** — a hierarchical representation of all cluster resources (Cluster → ControlPlane → Machines → InfrastructureMachines → BootstrapConfigs).

Options control:
- `ShowOtherConditions` — additional condition types to display
- `ShowMachineSets` — include MachineSet layer
- `ShowClusterResourceSets` — include addon bindings
- `Grouping` — group machines with identical conditions
- `V1Beta1` — legacy condition format

**Our `ClusterDescriber.Describe` should:**
1. Call `clusterctlClient.DescribeCluster()` with the management kubeconfig
2. Format the `ObjectTree` into a human-readable string
3. Store in `ClusterResult.ClusterDescription` for Terraform state

### 10.6 clusterctl Delete

Source: `cmd/clusterctl/client/delete.go`

Deletes **providers** (not workload clusters) from a management cluster:
- `IncludeNamespace` — also delete the provider namespace
- `IncludeCRDs` — also delete provider CRDs (validates no objects exist first)
- `SkipInventory` — skip deleting clusterctl inventory tracking objects
- `DeleteAll` — delete all installed providers

**Relevance to our provider:**
- Used during cleanup when tearing down a bootstrap cluster that had providers installed
- NOT used for deleting workload clusters (that's done via `Applier.Delete` which removes CAPI Cluster objects)

### 10.7 clusterctl Upgrade

Source: `cmd/clusterctl/client/upgrade.go`

Provider upgrade workflows:
- **Plan upgrade**: auto-discover what can be upgraded
- **Apply upgrade**: upgrade by contract (auto) or by custom provider list
- CertManager is always upgraded to latest before providers
- `WaitProviderTimeout` defaults to 5min

**Relevance to our provider:**
- Future: if `core_provider` or `infrastructure_provider` version attributes change, trigger provider upgrade rather than cluster recreation
- Currently: version changes in provider specifications require ForceNew (cluster recreation)

---

## 11. Retry & Backoff Strategy (clusterctl Reference)

Source: `cmd/clusterctl/client/cluster/client.go`

clusterctl defines precise backoff configurations for different operation classes. Our provider should align with these for consistency:

### 11.1 Backoff Configurations

| Name | Duration | Factor | Steps | Jitter | Total | Use Case |
|------|----------|--------|-------|--------|-------|----------|
| `writeBackoff` | 500ms | 1.5 | 10 | 0.4 | ~40s | Create/delete objects, pause/resume, namespace creation |
| `readBackoff` | 250ms | 1.5 | 9 | 0.1 | ~15s | Read cluster/machine objects, CRD discovery |
| `connectBackoff` | 250ms | 1.5 | 9 | 0.1 | ~15s | Initial cluster connection |
| `shortConnectBackoff` | 250ms | 1.5 | 7 | 0.1 | ~5s | Quick reachability checks |
| `moveWaitBackoff` | 5s | 1.5 | 10 | 0.1 | ~2min | Wait for BlockMoveAnnotation removal |

### 11.2 `retryWithExponentialBackoff` Pattern

```go
func retryWithExponentialBackoff(ctx context.Context, opts wait.Backoff,
    operation func(ctx context.Context) error) error {
    i := 0
    err := wait.ExponentialBackoffWithContext(ctx, opts,
        func(ctx context.Context) (bool, error) {
            i++
            if err := operation(ctx); err != nil {
                if i < opts.Steps {
                    log.V(5).Info("Retrying with backoff", "cause", err.Error())
                    return false, nil  // retry
                }
                return false, err  // give up
            }
            return true, nil  // success
        })
    return errors.Wrapf(err, "action failed after %d attempts", i)
}
```

**Every individual object operation in move is wrapped** — each `createTargetObject`, `deleteSourceObject`, `ensureNamespace`, `setClusterPause`, and `setClusterClassPause` call retries independently with the appropriate backoff.

### 11.3 Current Provider Alignment

| Our Implementation | clusterctl Equivalent | Status |
|---|---|---|
| `moveWithRetry` (manager.go) | Move-level retry (exponential, 4 attempts, 5s base) | ✅ Higher-level retry complements clusterctl's per-object retry |
| `DynamicWaiter` poll loop | `readBackoff` + `PollImmediateWaiter` | ⚠️ Should adopt `readBackoff` for status checks |
| `Applier.Apply` (no retry) | N/A (template apply is not retried by clusterctl) | ✅ Correct — CAPI reconciles |
| `Installer.Init` (no retry) | Init itself doesn't retry, but `WaitProviders` polls | ⚠️ Should add `WaitProviders: true` |

---

## 12. Provider ↔ clusterctl Interface Mapping

This section maps our provider's interfaces to the clusterctl operations they wrap.

### 12.1 Direct Wrapping

```
┌─────────────────────────────┐     ┌─────────────────────────────────────┐
│  Our Interface              │     │  clusterctl Operation               │
├─────────────────────────────┤     ├─────────────────────────────────────┤
│                             │     │                                     │
│  Installer.Init()           │ ──► │  client.Init(InitOptions)           │
│                             │     │    + CertManager.EnsureInstalled    │
│                             │     │    + Installer.Validate/Install     │
│                             │     │                                     │
│  TemplateGenerator          │     │                                     │
│   .Generate()               │ ──► │  client.GetClusterTemplate(opts)    │
│                             │     │    + templateOptionsToVariables     │
│                             │     │    + repo.Templates().Get()         │
│                             │     │                                     │
│  Applier.Apply()            │ ──► │  kubectl apply (server-side)        │
│                             │     │  NOT clusterctl (it's manual)       │
│                             │     │                                     │
│  Applier.Delete()           │ ──► │  kubectl delete Cluster object      │
│                             │     │  CAPI controllers handle cascade    │
│                             │     │                                     │
│  Mover.Move()               │ ──► │  client.Move(MoveOptions)           │
│                             │     │    → objectMover.Move()             │
│                             │     │    → graph, pause, create, delete,  │
│                             │     │      resume (all internally)        │
│                             │     │                                     │
│  KubeconfigRetriever        │     │                                     │
│   .GetKubeconfig()          │ ──► │  client.GetKubeconfig(opts)         │
│                             │     │    → WorkloadCluster.GetKubeconfig  │
│                             │     │                                     │
│  ClusterDescriber           │     │                                     │
│   .Describe()               │ ──► │  client.DescribeCluster(opts)       │
│                             │     │    → tree.Discovery()               │
└─────────────────────────────┘     └─────────────────────────────────────┘
```

### 12.2 Divergences from clusterctl

| Aspect | clusterctl | Our Provider | Reason |
|--------|-----------|-------------|--------|
| **Lifecycle scope** | CLI commands (one-shot) | Terraform resource (managed lifecycle) | Terraform reconciles on every apply |
| **State tracking** | No state between commands | Terraform state file tracks all fields | Enables drift detection, update-in-place |
| **Bootstrap** | Assumes management cluster exists | Creates ephemeral bootstrap (kind/Redfish) | Self-contained cluster provisioning |
| **Move trigger** | Explicit `clusterctl move` command | Automatic on `self_managed=true` during Create | Terraform users shouldn't run clusterctl manually |
| **Move retry** | Per-object retry (~40s per operation) | Additional outer retry (4 attempts, exponential) | Network faults between clusters may persist longer |
| **Update model** | No update concept (template+apply is new) | Reconcile: regenerate template + re-apply | Terraform in-place updates for mutable fields |
| **Provider install** | `clusterctl init` (first run detection) | `Installer.Init` with `SkipInit` option | Skip on Update when providers already installed |
| **Delete scope** | `clusterctl delete` removes providers | `Applier.Delete` removes Cluster object | CAPI controllers cascade-delete infrastructure |
| **Upgrade** | `clusterctl upgrade apply` | *(not yet implemented)* | Future: trigger on provider version attribute change |

### 12.3 MoveOptions Mapping

| clusterctl MoveOptions | Our MoveOptions | Notes |
|---|---|---|
| `FromKubeconfig` | `FromKubeconfig` (path) | Source management cluster |
| `ToKubeconfig` | `ToKubeconfig` (path) | Target workload cluster |
| `Namespace` | `Namespace` | CAPI resource namespace |
| `DryRun` | *(not exposed)* | Could be useful for plan |
| `FromDirectory` / `ToDirectory` | *(not used)* | Backup/restore to filesystem |
| `ExperimentalResourceMutators` | *(not used)* | Resource mutation during move |

### 12.4 What clusterctl Handles Internally During Move

Our `Mover.Move()` calls `clusterctlClient.Move()` which internally handles ALL of these — we must NOT duplicate them:

1. ~~Build object graph~~ (done by mover)
2. ~~Check provisioning complete~~ (done by mover)
3. ~~Check not already paused~~ (done by mover)
4. ~~Pause source clusters~~ (done by mover)
5. ~~Wait for BlockMoveAnnotation removal~~ (done by mover)
6. ~~Topological sort~~ (done by mover)
7. ~~Create on target with OwnerRef rebuild~~ (done by mover)
8. ~~Delete from source with finalizer removal~~ (done by mover)
9. ~~Resume target clusters~~ (done by mover)

**Our responsibility is limited to:**
- Ensuring CAPI providers are installed on the target BEFORE calling Move
- Providing correct kubeconfig paths in MoveOptions
- Retrying the entire Move operation on transient network failures
- Handling the case where Move partially succeeded (idempotent retry)

---

## 13. Upstream clusterctl Labels & Annotations

Labels and annotations used by clusterctl that affect our provider's behavior:

| Label/Annotation | Applied To | Effect |
|---|---|---|
| `clusterctl.cluster.x-k8s.io/` | CRDs | Marks CRD as clusterctl-managed; included in object graph discovery |
| `clusterctl.cluster.x-k8s.io/move` | CRDs, Objects | Force-move: object moves regardless of owner refs |
| `clusterctl.cluster.x-k8s.io/move-hierarchy` | CRDs | Force-move with all dependents via owner chain |
| `clusterctl.cluster.x-k8s.io/block-move` | Objects | Blocks move until annotation is removed (provider signals "not ready") |
| `clusterctl.cluster.x-k8s.io/delete-for-move` | Objects | Added during delete phase; signals object deleted as part of move |
| `cluster.x-k8s.io/paused` | Cluster `spec.paused`, ClusterClass annotation | Stops reconciliation during move |

**Impact on our provider:**
- If Tinkerbell adds `block-move` to resources during provisioning, our Move will wait up to ~2min
- Custom resources we create (e.g., Hardware, BMC) must have the `clusterctl.cluster.x-k8s.io/move` label to be included in move
- The `paused` flag means no reconciliation during move — our waiter must avoid checking status during an active move

---

## 14. Node Inventory Support

Node inventory provides the hardware catalogue for bare-metal cluster provisioning. This design mirrors EKS Anywhere's hardware management and maps to Tinkerbell Hardware/BMC CRDs for the `tinkerbell` infrastructure provider.

### 14.1 Upstream Models

**EKS Anywhere Machine (CSV):**

| Field | Type | Description |
|-------|------|-------------|
| `hostname` | string | Machine hostname |
| `ip_address` | string | Primary IP address |
| `netmask` | string | Network mask |
| `gateway` | string | Default gateway |
| `nameservers` | `\|`-separated | DNS servers |
| `mac` | string | Primary NIC MAC address |
| `disk` | string | Boot disk device (e.g., `/dev/sda`) |
| `labels` | `\|`-separated key=value | Role labels (e.g., `type=cp`) |
| `bmc_ip` | string | BMC management IP |
| `bmc_username` | string | BMC credentials |
| `bmc_password` | string | BMC credentials (sensitive) |
| `vlan_id` | string | VLAN ID (optional) |

**EKS Anywhere Catalogue** (`pkg/providers/tinkerbell/hardware/catalogue.go`):
- `Catalogue` struct containing indexed collections of `tinkv1alpha1.Hardware`, `rufiov1alpha1.Machine` (BMC), and `corev1.Secret`
- Indexer-based lookup by Hardware ID, BMC reference, and secret name
- Parsed from YAML manifests (Hardware, Machine, Secret kinds) or CSV
- `MarshalCatalogue()` produces YAML for applying to the management cluster

**Tinkerbell Hardware CRD** (`tink/api/v1alpha1/hardware_types.go`):
- `HardwareSpec.BMCRef` — typed reference to a Rufio `Machine` resource for BMC management
- `HardwareSpec.Interfaces[]` — network interfaces with `DHCP` (mac, hostname, IP, nameservers) and `Netboot` (allowPXE, allowWorkflow, IPXE, OSIE)
- `HardwareSpec.Disks[]` — disk devices
- `HardwareSpec.Resources` — schedulable resources (CPU, memory)
- `HardwareSpec.UserData` / `VendorData` — cloud-init compatible data
- Labels: `clusterctl.cluster.x-k8s.io/move` ensures hardware moves with the cluster during pivot

### 14.2 Inventory Design for This Provider

The inventory schema supports two input modes:

1. **File-based** — path to an EKS-A compatible hardware CSV or YAML manifest file
2. **Inline** — structured machine definitions in HCL

Both produce the same internal representation that feeds into template generation and the applier workflow.

**Target HCL:**

```hcl
resource "capi_cluster" "workload" {
  name               = "bare-metal-cluster"
  kubernetes_version = "v1.31.0"

  infrastructure {
    provider = "tinkerbell:v0.5.4"
  }

  control_plane {
    provider      = "kubeadm:v1.12.2"
    machine_count = 3
  }

  workers {
    machine_count = 3
  }

  # File-based inventory (EKS-A CSV or YAML manifests)
  inventory {
    source = "/path/to/hardware.csv"
  }

  # OR inline inventory
  inventory {
    machine {
      hostname = "cp-1"

      network {
        ip_address  = "192.168.1.10"
        netmask     = "255.255.255.0"
        gateway     = "192.168.1.1"
        mac_address = "aa:bb:cc:dd:ee:01"
        nameservers = ["8.8.8.8", "8.8.4.4"]
        vlan_id     = "100"
      }

      disk {
        device = "/dev/sda"
      }

      bmc {
        address  = "192.168.2.10"
        username = "admin"
        password = var.bmc_password
      }

      labels = {
        "type" = "cp"
      }
    }
  }
}
```

### 14.3 Provider-Specific Inventory Mapping

| Terraform Schema | Tinkerbell Hardware CRD | Tinkerbell BMC (Rufio Machine) | EKS-A CSV |
|---|---|---|---|
| `machine.hostname` | `Interface.DHCP.Hostname` | — | `hostname` |
| `machine.network.ip_address` | `Interface.DHCP.IP.Address` | — | `ip_address` |
| `machine.network.netmask` | `Interface.DHCP.IP.Netmask` | — | `netmask` |
| `machine.network.gateway` | `Interface.DHCP.IP.Gateway` | — | `gateway` |
| `machine.network.mac_address` | `Interface.DHCP.MAC` | — | `mac` |
| `machine.network.nameservers` | `Interface.DHCP.NameServers` | — | `nameservers` |
| `machine.network.vlan_id` | `Interface.DHCP.VLANID` | — | `vlan_id` |
| `machine.disk.device` | `Disk.Device` | — | `disk` |
| `machine.bmc.address` | `Spec.BMCRef` → Machine | `Spec.Connection.Host` | `bmc_ip` |
| `machine.bmc.username` | `Spec.BMCRef` → Secret | `Spec.Connection.AuthSecretRef` | `bmc_username` |
| `machine.bmc.password` | `Spec.BMCRef` → Secret | `Spec.Connection.AuthSecretRef` | `bmc_password` |
| `machine.labels` | `ObjectMeta.Labels` | — | `labels` |

### 14.4 Inventory Lifecycle Integration

**Create workflow additions (between steps 2 and 3):**

1. Parse inventory (from `source` file or inline `machine` blocks)
2. Build `Catalogue` (Hardware + BMC + Secret resources)
3. Validate sufficient machines for requested roles:
   - At least `control_plane.machine_count` machines with `type=cp` label
   - At least `workers.machine_count` machines with `type=worker` label (or unlabeled)
   - EKS-A validation pattern: `pkg/providers/tinkerbell/hardware/validator.go`
4. Apply Hardware/BMC/Secret manifests to management cluster
5. Wait for BMC connectivity (Rufio power status)
6. Pass hardware selectors to template generation

**Move workflow additions:**
- Hardware, BMC Machine, and Secret resources must have `clusterctl.cluster.x-k8s.io/move` label
- These are automatically included in the object graph during `clusterctl move`
- No additional provider logic needed if labels are applied correctly

**Delete workflow additions:**
- Hardware resources are cascade-deleted when the Cluster object is removed (via CAPI owner references)
- If hardware was imported from an external source, consider preserving it (configurable)

### 14.5 Validation Rules

From EKS Anywhere's hardware validation:

| Rule | Source | Error |
|------|--------|-------|
| Minimum machines per role | `validator.go` | "not enough hardware for control plane: need N, have M" |
| Unique hostnames | `validator.go` | "duplicate hostname: X" |
| Unique IP addresses | `validator.go` | "duplicate IP address: X" |
| Unique MAC addresses | `validator.go` | "duplicate MAC address: X" |
| Valid BMC credentials when BMC present | `validator.go` | "BMC IP set but username/password missing" |
| Disk specified for all machines | `validator.go` | "disk device required for hardware provisioning" |
| `source` and `machine` mutually exclusive | Provider validation | "specify either source or inline machine blocks, not both" |

### 14.6 Future Provider Extensions

The inventory model is designed to be provider-agnostic. For providers other than Tinkerbell:

| Provider | Inventory Mapping |
|----------|-------------------|
| **Tinkerbell** | Hardware + Rufio Machine + Secret CRDs |
| **vSphere** | VSphereMachine templates with resource pools and datastores |
| **AWS** | AMI IDs, instance types, subnet mappings |
| **Docker** | Not applicable (machines are containers) |
| **Metal³** | BareMetalHost CRDs with similar BMC patterns |

The `InfraProvider` interface (Section 3) should include a method to convert inventory machines to provider-specific resources:

```go
type InfraProvider interface {
    // ... existing methods ...
    ConvertInventory(machines []Machine) ([]unstructured.Unstructured, error)
    ValidateInventory(machines []Machine, cpCount, workerCount int) error
}
```