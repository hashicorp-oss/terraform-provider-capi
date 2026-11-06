# The Cluster, on the management cluster the capi provider block names (here
# the cluster itself: it was stood up outside CAPI and manages itself).
resource "capi_cluster" "this" {
  name               = var.cluster_name
  namespace          = var.namespace
  kubernetes_version = local.kubernetes_version
  class              = capi_cluster_class.this.ref

  network = {
    control_plane_endpoint = { host = var.virtual_ip, port = 6443 }
    pods                   = { cidr_blocks = [var.pod_cidr] }
    services               = { cidr_blocks = [var.service_cidr] }
  }

  control_plane = { replicas = length(var.control_plane_nodes) }

  # Bare-metal provisioning: imaging and the first boot take a while.
  wait = { conditions = ["Available"], timeout = "90m" }

  # Machines need Hardware to claim. On destroy the cluster goes first, so
  # CAPT releases every Hardware before capi_hardware deletes it.
  depends_on = [capi_hardware.node]
}

# One topology worker entry per group, each under its own field manager, so a
# group is added, changed or removed without touching capi_cluster.
resource "capi_machine_deployment" "worker" {
  for_each = local.worker_deployments

  cluster   = capi_cluster.this.name
  namespace = var.namespace
  name      = replace(each.key, "_", "-")
  class     = { name = each.key }

  # The autoscaler owns the count when enabled: capi_machine_deployment writes
  # the min/max annotations and leaves replicas out.
  replicas    = each.value.autoscaling == null ? each.value.replicas : null
  autoscaling = each.value.autoscaling

  # One machine at a time: no spare boards to surge onto.
  rollout = { strategy = "RollingUpdate", max_surge = 0, max_unavailable = 1 }

  wait = { timeout = "60m" }
}
