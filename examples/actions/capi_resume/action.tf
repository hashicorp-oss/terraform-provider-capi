action "capi_resume" "prod" {
  config {
    cluster   = "prod"
    namespace = "capi"
  }
}

resource "terraform_data" "maintenance" {
  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.capi_resume.prod]
    }
  }
}
