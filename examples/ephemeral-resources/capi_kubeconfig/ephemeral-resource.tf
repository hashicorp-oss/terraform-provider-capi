# A short-lived kubeconfig for the workload cluster, never written to state.
ephemeral "capi_kubeconfig" "prod" {
  cluster   = capi_cluster.prod.name
  namespace = "capi"
  ttl       = "1h"
}

provider "kubernetes" {
  alias                  = "prod"
  host                   = ephemeral.capi_kubeconfig.prod.host
  cluster_ca_certificate = ephemeral.capi_kubeconfig.prod.cluster_ca_certificate
  client_certificate     = ephemeral.capi_kubeconfig.prod.client_certificate
  client_key             = ephemeral.capi_kubeconfig.prod.client_key
}
