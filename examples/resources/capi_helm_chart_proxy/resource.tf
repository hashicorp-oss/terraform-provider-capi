# Cilium on one cluster. The wait watches the HelmReleaseProxy CAAPH creates
# for the cluster and quotes Helm's message when the release fails.
resource "capi_helm_chart_proxy" "cilium" {
  name      = "cilium"
  namespace = "capi"
  cluster   = capi_cluster.prod.name

  chart = {
    name       = "cilium"
    repository = "oci://quay.io/cilium/charts"
    version    = "1.18.2"
  }

  release = {
    name      = "cilium"
    namespace = "kube-system"
    values    = file("${path.module}/cilium-values.yaml")
  }

  options = {
    wait    = true
    timeout = "10m"
  }

  wait = { timeout = "15m" }
}

# cert-manager on every cluster labelled tier=edge, installed once and then
# left alone. Values may template over the matched Cluster.
resource "capi_helm_chart_proxy" "cert_manager" {
  name             = "cert-manager"
  namespace        = "capi"
  cluster_selector = { tier = "edge" }
  strategy         = "InstallOnce"

  chart = {
    name       = "cert-manager"
    repository = "https://charts.jetstack.io"
    version    = "v1.17.0"
  }

  release = {
    name      = "cert-manager"
    namespace = "cert-manager"
    values    = <<-EOT
      crds:
        enabled: true
      global:
        commonLabels:
          cluster: {{ .Cluster.metadata.name }}
    EOT
  }

  options = {
    install = { create_namespace = true }
  }

  force_delete = true
}
