# A Cilium load-balancer pool for the Tinkerbell Service, applied to one
# cluster once its CNI is up. The manifest becomes a ConfigMap the set
# references; the wait returns when the cluster's binding reports it applied
# and the pool object is Ready.
resource "capi_cluster_resource_set" "lb_pool" {
  name      = "tinkerbell-lb-pool"
  namespace = "capi"
  cluster   = capi_cluster.prod.name
  strategy  = "Reconcile"

  manifests = [
    yamlencode({
      apiVersion = "cilium.io/v2alpha1"
      kind       = "CiliumLoadBalancerIPPool"
      metadata   = { name = "tinkerbell" }
      spec       = { blocks = [{ start = "10.0.80.230", stop = "10.0.80.239" }] }
    }),
  ]

  prune = true
  wait  = { ready = true }

  depends_on = [capi_helm_chart_proxy.cilium]
}

# Registry credentials for every edge cluster, from a Secret the set
# references; applied once per cluster.
resource "capi_cluster_resource_set" "registry" {
  name             = "registry-credentials"
  namespace        = "capi"
  cluster_selector = { tier = "edge" }

  secrets = [
    yamlencode({
      apiVersion = "v1"
      kind       = "Secret"
      metadata   = { name = "registry", namespace = "kube-system" }
      type       = "kubernetes.io/dockerconfigjson"
      stringData = { ".dockerconfigjson" = var.registry_auth }
    }),
  ]
}
