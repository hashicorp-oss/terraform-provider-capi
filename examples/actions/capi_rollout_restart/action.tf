# Replace every machine of a node group, for an image rebuild the template
# does not capture.
action "capi_rollout_restart" "workers" {
  config {
    kind      = "MachineDeployment"
    name      = capi_machine_deployment.workers.object_name
    namespace = "capi"
  }
}

resource "terraform_data" "image" {
  input = var.image_digest
  lifecycle {
    action_trigger {
      events  = [after_update]
      actions = [action.capi_rollout_restart.workers]
    }
  }
}
