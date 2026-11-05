# Move a namespace's CAPI objects to another management cluster whose
# providers are already installed (capi_provider resources against it).
action "capi_move" "to_new_management" {
  config {
    namespace     = "capi"
    to_kubeconfig = var.new_management_kubeconfig
  }
}
