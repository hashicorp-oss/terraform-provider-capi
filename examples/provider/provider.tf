# The management cluster, through a kubeconfig file and context.
provider "capi" {
  kubernetes = {
    config_path    = "~/.kube/config"
    config_context = "mgmt"
  }
}

# Or kubeconfig content, typically an ephemeral capi_kubeconfig.
provider "capi" {
  alias      = "content"
  kubeconfig = file("~/.kube/mgmt.kubeconfig")
}
