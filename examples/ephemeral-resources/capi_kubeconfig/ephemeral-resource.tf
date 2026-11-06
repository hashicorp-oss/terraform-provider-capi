# A kubeconfig for a workload cluster, never written to state: read from its
# <cluster>-kubeconfig Secret on the management cluster.
ephemeral "capi_kubeconfig" "prod" {
  cluster   = capi_cluster.prod.name
  namespace = "capi"
}

provider "kubernetes" {
  alias                  = "prod"
  host                   = ephemeral.capi_kubeconfig.prod.host
  cluster_ca_certificate = ephemeral.capi_kubeconfig.prod.cluster_ca_certificate
  client_certificate     = ephemeral.capi_kubeconfig.prod.client_certificate
  client_key             = ephemeral.capi_kubeconfig.prod.client_key
}

# A self-managed cluster, reached through a certificate derived from its root
# credential. The certificate must outlast the run: a provider block is
# configured once and cannot be handed a renewed one.
ephemeral "capi_kubeconfig" "self" {
  cluster  = capi_cluster.mgmt.name
  secrets  = capi_cluster.mgmt.provider_secrets
  endpoint = capi_cluster.mgmt.status.endpoint
  ttl      = "8h"
}

provider "capi" {
  alias      = "self"
  kubeconfig = ephemeral.capi_kubeconfig.self.raw
}
