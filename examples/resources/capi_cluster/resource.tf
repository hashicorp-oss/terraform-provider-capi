resource "capi_cluster" "example" {
  name                        = "my-cluster"
  infrastructure_provider     = "docker"
  bootstrap_provider          = "kubeadm"
  control_plane_provider      = "kubeadm"
  kubernetes_version          = "v1.28.0"
  control_plane_machine_count = 1
  worker_machine_count        = 2
  skip_init                   = false
  wait_for_ready              = true
  target_namespace            = "default"
  kubeconfig_path             = "/tmp/my-cluster-kubeconfig"
}
