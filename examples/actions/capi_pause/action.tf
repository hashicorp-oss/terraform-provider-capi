# Pause a cluster's reconciliation before maintenance that must not be
# reconciled away, and resume it afterwards.
action "capi_pause" "prod" {
  config {
    cluster   = "prod"
    namespace = "capi"
  }
}

resource "terraform_data" "maintenance" {
  lifecycle {
    action_trigger {
      events  = [before_create, before_update]
      actions = [action.capi_pause.prod]
    }
  }
}
