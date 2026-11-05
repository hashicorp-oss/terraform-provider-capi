# Have the MachineHealthCheck replace one machine now.
action "capi_remediate_machine" "worker" {
  config {
    machine   = "prod-workers-7c9f4-xk2pq"
    namespace = "capi"
  }
}
