provider "capi" {
  # Optional: Configure kubernetes connection to the management cluster
  kubernetes {
    config_path = "~/.kube/config"
    # Or use explicit configuration:
    # host                   = "https://example.com:6443"
    # token                  = "token"
    # cluster_ca_certificate = file("~/.kube/ca.crt")
    # client_certificate     = file("~/.kube/client.crt")
    # client_key             = file("~/.kube/client.key")
  }
}
