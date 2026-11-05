# A Tinkerbell machine template. The kind and API version come from the
# installed CRDs; a spec change creates a new revision and `ref` moves to it.
resource "capi_machine_template" "worker" {
  name           = "worker"
  namespace      = "capi"
  infrastructure = "tinkerbell"
  spec = {
    hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker" } } }] }
  }
  revision_history = 2
}

# The cluster template a ClusterClass refers to.
resource "capi_machine_template" "cluster" {
  name           = "prod"
  namespace      = "capi"
  infrastructure = "tinkerbell"
  role           = "cluster"
  spec           = {}
}
