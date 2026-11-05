# Direct mode: the group owns a MachineDeployment built from the template
# resources' refs. Changing a ref rolls the group out.
resource "capi_machine_template" "worker" {
  name           = "worker"
  namespace      = "capi"
  infrastructure = "tinkerbell"
  spec = {
    hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker" } } }] }
  }
}

resource "capi_bootstrap_template" "worker" {
  name      = "worker"
  namespace = "capi"
  bootstrap = "talos"
  spec = {
    generateType = "worker"
    talosVersion = "v1.13"
  }
}

resource "capi_machine_deployment" "workers" {
  cluster   = capi_cluster.prod.name
  namespace = "capi"
  name      = "workers"
  replicas  = 3
  version   = "v1.34.0"

  template = {
    infrastructure = capi_machine_template.worker.ref
    bootstrap      = capi_bootstrap_template.worker.ref
  }

  labels  = { tier = "worker" }
  rollout = { strategy = "RollingUpdate", max_surge = 1, max_unavailable = 0 }
  timeouts = {
    node_drain    = "10m"
    node_deletion = "2m"
  }
}

# Topology mode: the group is one entry of the Cluster's topology workers,
# instantiating a worker class of the ClusterClass. The cluster autoscaler
# owns the count between min and max.
resource "capi_machine_deployment" "autoscaled" {
  cluster   = capi_cluster.prod.name
  namespace = "capi"
  name      = "workers-amd64"

  class = {
    name      = "worker-amd64"
    variables = { diskSize = "200Gi" }
  }

  autoscaling = { min = 2, max = 8 }

  scale_to_zero_on_delete = true
}
