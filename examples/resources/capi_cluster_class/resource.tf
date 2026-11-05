# A class assembled from template refs. A new revision of any template moves
# the ref, and the clusters using the class roll out.
resource "capi_cluster_class" "main" {
  name      = "prod"
  namespace = "capi"

  infrastructure = capi_machine_template.cluster.ref # role = "cluster"

  control_plane = {
    template       = capi_control_plane_template.main.ref
    infrastructure = capi_machine_template.cp.ref
  }

  workers = {
    worker-amd64 = {
      infrastructure = capi_machine_template.worker_amd64.ref
      bootstrap      = capi_bootstrap_template.worker.ref
      labels         = { tier = "worker" }
    }
  }

  variables = [
    {
      name     = "diskSize"
      required = false
      schema   = { openAPIV3Schema = { type = "string", default = "100Gi" } }
    },
  ]

  patches = [
    {
      name = "diskSize"
      definitions = [{
        selector    = { apiVersion = "infrastructure.cluster.x-k8s.io/v1beta1", kind = "TinkerbellMachineTemplate", matchResources = { machineDeploymentClass = { names = ["worker-amd64"] } } }
        jsonPatches = [{ op = "add", path = "/spec/template/spec/diskSize", valueFrom = { variable = "diskSize" } }]
      }]
    },
  ]
}
