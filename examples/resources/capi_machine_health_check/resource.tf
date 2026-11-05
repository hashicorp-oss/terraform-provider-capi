# Watch a node group's machines; replace one whose node is not Ready for 5m,
# as long as no more than 40% of the group is unhealthy at once.
resource "capi_machine_health_check" "workers" {
  cluster            = capi_cluster.prod.name
  namespace          = "capi"
  name               = "workers"
  machine_deployment = capi_machine_deployment.workers.name

  node_startup_timeout = "15m"
  unhealthy_node_conditions = [
    { type = "Ready", status = "Unknown", timeout = "5m" },
    { type = "Ready", status = "False", timeout = "5m" },
  ]

  remediation = { max_unhealthy = "40%" }
}

# Watch the control plane with the default checks; never remediate more than
# one machine at a time.
resource "capi_machine_health_check" "control_plane" {
  cluster       = capi_cluster.prod.name
  namespace     = "capi"
  name          = "control-plane"
  control_plane = true

  remediation = { max_unhealthy = "1" }
}
