# A docker cluster on the management cluster the provider block names. Its
# control-plane machine template is a resource of its own; node groups are
# capi_machine_deployment resources.
resource "capi_machine_template" "cp" {
  name           = "my-cluster-cp"
  infrastructure = "docker"
  spec           = {}
}

resource "capi_cluster" "example" {
  name               = "my-cluster"
  kubernetes_version = "v1.31.0"

  infrastructure = { provider = "docker" }

  control_plane = {
    provider       = "kubeadm"
    replicas       = 1
    infrastructure = capi_machine_template.cp.ref
  }

  wait = { timeout = "30m" }
}

resource "capi_machine_template" "worker" {
  name           = "my-cluster-worker"
  infrastructure = "docker"
  spec           = {}
}

resource "capi_bootstrap_template" "worker" {
  name      = "my-cluster-worker"
  bootstrap = "kubeadm"
  spec      = {}
}

resource "capi_machine_deployment" "md_0" {
  cluster  = capi_cluster.example.name
  name     = "md-0"
  replicas = 2
  version  = "v1.31.0"
  template = {
    infrastructure = capi_machine_template.worker.ref
    bootstrap      = capi_bootstrap_template.worker.ref
  }
}

# A cluster instantiating a ClusterClass.
resource "capi_cluster" "classy" {
  name               = "classy"
  kubernetes_version = "v1.31.0"
  class              = capi_cluster_class.main.ref
  control_plane      = { replicas = 3 }
}
