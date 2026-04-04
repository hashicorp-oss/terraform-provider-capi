resource "capi_cluster" "example" {
  name               = "my-cluster"
  kubernetes_version = "v1.31.0"

  infrastructure {
    provider = "docker"
  }

  bootstrap {
    provider = "kubeadm"
  }

  control_plane {
    provider      = "kubeadm"
    machine_count = 1
  }

  workers {
    machine_count = 2
  }

  wait {
    enabled = true
    timeout = "30m"
  }

  output {
    kubeconfig_path = "/tmp/my-cluster-kubeconfig"
  }
}
