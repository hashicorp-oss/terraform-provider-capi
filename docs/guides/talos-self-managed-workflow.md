---
page_title: "Talos Self-Managed Cluster Workflow"
subcategory: "Guides"
description: |-
  Provision a self-managed bare-metal or virtual cluster using a temporary Talos bootstrap node, CAPI components, optional Helm addon customization, and automatic pivot.
---

# Talos Self-Managed Cluster Workflow

This guide describes the recommended workflow for creating a fully self-managed
cluster using a temporary Talos-based bootstrap node. The bootstrap node exists
only long enough to provision the target cluster, move all CAPI management
resources into it, and then destroy itself — never to be spun up again unless
full re-provisioning is required.

## Overview

```
┌────────────────────────────────────────────────────────────────────┐
│  Step 1   Start temporary Talos cluster (Docker or QEMU)          │
│  Step 2   clusterctl init — install CAPI + providers + addons     │
│  Step 3   (Optional) Install Helm addon provider for day-2 setup  │
│  Step 4   Apply CAPI cluster manifests — target cluster goes live │
│  Step 5   clusterctl move — pivot resources → destroy bootstrap   │
└────────────────────────────────────────────────────────────────────┘
```

After step 5 completes, the target cluster is fully self-managing.
The bootstrap node is deleted and is not required for ongoing operation.

## Workflow Detail

### Step 1 — Start a Temporary Talos Bootstrap Cluster

Create a short-lived Talos cluster that serves as the initial CAPI management
plane. This cluster can run on Docker (fastest for CI/dev) or QEMU (closer to
bare-metal). It does not need to be long-lived or highly available — a single
control-plane node is sufficient.

**Docker (recommended for development):**

```bash
# Create an ephemeral single-node Talos cluster
talosctl cluster create \
  --name capi-bootstrap \
  --provisioner docker \
  --controlplanes 1 \
  --workers 0

# Export the kubeconfig
talosctl kubeconfig --nodes 10.5.0.2 --force /tmp/capi-bootstrap.kubeconfig
```

**QEMU (recommended for production/bare-metal parity):**

```bash
talosctl cluster create \
  --name capi-bootstrap \
  --provisioner qemu \
  --controlplanes 1 \
  --workers 0 \
  --cpus 2 \
  --memory 4096

talosctl kubeconfig --nodes 10.5.0.2 --force /tmp/capi-bootstrap.kubeconfig
```

At this point you have a running Kubernetes API on the bootstrap node with a
kubeconfig at `/tmp/capi-bootstrap.kubeconfig`.

### Step 2 — Install CAPI Components on the Bootstrap Cluster

Run `clusterctl init` against the bootstrap cluster to install the core CAPI
controller, the infrastructure provider, and the bootstrap/control-plane
providers. After this step, the bootstrap cluster is capable of provisioning
new workload clusters.

In Terraform, the `capi_cluster` resource handles this automatically unless
`management.skip_init = true`. If you are driving the workflow manually:

```bash
clusterctl init \
  --kubeconfig /tmp/capi-bootstrap.kubeconfig \
  --core cluster-api:v1.12.2 \
  --bootstrap talos:v0.6.7 \
  --control-plane talos:v0.6.7 \
  --infrastructure tinkerbell:v0.5.4
```

Wait for all provider pods to become `Ready`:

```bash
kubectl --kubeconfig /tmp/capi-bootstrap.kubeconfig \
  get pods -A -l clusterctl.cluster.x-k8s.io
```

### Step 3 — (Optional) Install the Helm Addon Provider

The [CAPI Helm addon provider](https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm)
lets you declaratively install Helm charts on the target cluster as part of the
CAPI reconciliation loop. It runs as a controller on the management cluster and
watches `HelmChartProxy` and `HelmReleaseProxy` resources.

**Why this matters for bootstrap workflows:**

When the Helm addon provider is installed on the bootstrap cluster, any
`HelmChartProxy` resources you create will be included in the `clusterctl move`
object graph — they carry the
`clusterctl.cluster.x-k8s.io/move` label and are automatically transferred to
the target cluster. This means:

1. You install the Helm addon on the bootstrap cluster **once**.
2. Create `HelmChartProxy` resources for everything the target cluster needs
   (CNI, CSI, ingress controllers, monitoring, GitOps agents, etc.).
3. When the pivot happens, those `HelmChartProxy` resources move with the
   cluster objects. The addon controller on the **target** cluster picks them up
   and continues reconciling — no second installation step required.

```bash
# Install the Helm addon provider alongside the other providers
clusterctl init \
  --kubeconfig /tmp/capi-bootstrap.kubeconfig \
  --addon helm:v0.2.6
```

Or in Terraform, add it to the infrastructure provider list — CAPI treats addon
providers as regular providers during init:

```hcl
resource "capi_cluster" "workload" {
  name               = "production"
  kubernetes_version = "v1.31.0"

  infrastructure {
    provider = "tinkerbell:v0.5.4"
  }

  bootstrap {
    provider = "talos:v0.6.7"
  }

  control_plane {
    provider      = "talos:v0.6.7"
    machine_count = 3
  }

  core {
    provider = "cluster-api:v1.12.2"
  }

  management {
    kubeconfig  = "/tmp/capi-bootstrap.kubeconfig"
    self_managed = true
  }

  workers {
    machine_count = 3
  }

  wait {
    enabled = true
    timeout = "45m"
  }
}
```

Then apply `HelmChartProxy` resources to the bootstrap cluster before the pivot
completes:

```yaml
apiVersion: addons.cluster.x-k8s.io/v1alpha1
kind: HelmChartProxy
metadata:
  name: cilium
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      cni: cilium
  chartName: cilium
  repoURL: https://helm.cilium.io/
  namespace: kube-system
  valuesTemplate: |
    ipam:
      mode: kubernetes
```

Because `HelmChartProxy` is a CAPI-typed resource with the `move` label, it
transfers to the target cluster during step 5 automatically.

### Step 4 — Apply Cluster Manifests and Wait for Health

The `capi_cluster` resource generates a CAPI cluster template and applies it to
the bootstrap cluster. CAPI controllers then reconcile the desired state:
creating machines, running bootstrap workflows, and joining nodes to the
cluster.

The resource waits for:

- **Cluster** `Ready` condition
- **Control plane** `initialized` and `ready` booleans with correct replica count
- **MachineDeployments** in `Running` phase with correct `readyReplicas`

For Tinkerbell (bare-metal), the node startup timeout extends to 20 minutes
per node to account for PXE boot, OS install, and first-boot workflows.

You can monitor progress:

```bash
# Watch from the bootstrap cluster
clusterctl describe cluster production \
  --kubeconfig /tmp/capi-bootstrap.kubeconfig \
  --show-conditions all
```

Once all nodes are healthy and the control plane is initialized, the target
cluster is operational and serving the Kubernetes API.

### Step 5 — Pivot Resources and Destroy the Bootstrap Node

This is the critical step. `clusterctl move` transfers the entire CAPI object
graph — Cluster, Machines, MachineDeployments, Secrets, infrastructure
resources, and addon resources (like `HelmChartProxy`) — from the bootstrap
cluster to the target cluster.

**What happens during the pivot:**

1. CAPI providers are installed on the target cluster (`clusterctl init`)
2. All Clusters on the bootstrap are **paused** (reconciliation stops)
3. Resources are **created on the target** in topological order (owners before
   dependents)
4. Resources are **deleted from the bootstrap** with finalizers removed
5. Clusters are **resumed** on the target — controllers begin reconciling

The `capi_cluster` resource handles all of this when `management.self_managed = true`:

```hcl
management {
  kubeconfig   = "/tmp/capi-bootstrap.kubeconfig"
  self_managed = true   # ← triggers the full pivot workflow
}
```

After the move completes, the bootstrap cluster is deleted. The `capi_cluster`
resource handles this automatically — the bootstrap Talos node is powered down
and does not need to run again.

**The bootstrap node is ephemeral.** It exists solely for the initial
provisioning. Once all resources have been moved, the target cluster owns its
own CAPI management and the bootstrap is destroyed. It is only re-created if
the cluster needs to be fully re-provisioned from scratch.

If the pivot encounters a transient network error (connection reset, TLS
timeout, EOF), the resource automatically retries with exponential backoff
(up to 4 attempts over ~30 seconds) before failing.

### Post-Pivot State

After the workflow completes:

| Component | Location | Status |
|-----------|----------|--------|
| CAPI controllers | Target cluster | Running, reconciling |
| Cluster / Machine objects | Target cluster | Owned by target controllers |
| HelmChartProxy resources | Target cluster | Moved, addon controller reconciles |
| Helm releases (CNI, etc.) | Target cluster | Installed and managed |
| Bootstrap Talos node | **Destroyed** | Not running |
| Bootstrap kubeconfig | Stale | Can be deleted |

The target cluster is fully autonomous. Day-2 operations (scaling, upgrades,
addon changes) are performed directly against the target cluster's own API.

## Full Terraform Example

```hcl
# Provision via the pre-existing ephemeral Talos bootstrap cluster.
# The bootstrap node is automatically destroyed after pivot.

resource "capi_cluster" "production" {
  name               = "production"
  kubernetes_version = "v1.31.0"

  infrastructure {
    provider = "tinkerbell:v0.5.4"
  }

  bootstrap {
    provider = "talos:v0.6.7"
  }

  control_plane {
    provider      = "talos:v0.6.7"
    machine_count = 3
  }

  core {
    provider = "cluster-api:v1.12.2"
  }

  workers {
    machine_count = 5
  }

  management {
    kubeconfig   = "/tmp/capi-bootstrap.kubeconfig"
    self_managed = true
    namespace    = "default"
  }

  inventory {
    machine {
      hostname = "cp-1"
      network {
        ip_address  = "192.168.1.10"
        netmask     = "255.255.255.0"
        gateway     = "192.168.1.1"
        mac_address = "aa:bb:cc:dd:ee:01"
        nameservers = ["8.8.8.8"]
      }
      disk { device = "/dev/sda" }
      bmc {
        address  = "192.168.2.10"
        username = "admin"
        password = var.bmc_password
      }
      labels = { "type" = "cp" }
    }
    # ... additional machines ...
  }

  wait {
    enabled = true
    timeout = "45m"
  }

  output {
    kubeconfig_path = "${path.module}/production.kubeconfig"
  }
}

# After apply, the target cluster is self-managing.
# Use the kubeconfig to interact with it:
output "cluster_endpoint" {
  value = capi_cluster.production.status.endpoint
}

output "kubeconfig_path" {
  value = capi_cluster.production.output.kubeconfig_path
}
```

## When to Re-Provision

The bootstrap node is **destroyed after pivot** and never runs during normal
operation. You only need to recreate it for:

- **Full cluster replacement** — `terraform destroy` then `terraform apply`
- **Disaster recovery** — if the target cluster's CAPI controllers are
  unrecoverable
- **Major infrastructure migration** — moving to a different hardware set

For routine operations (scaling, Kubernetes version upgrades, addon
changes), the target cluster manages itself. No bootstrap node is involved.
