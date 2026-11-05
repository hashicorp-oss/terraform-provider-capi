# The core provider first; the others depend on it so clusterctl never adds
# defaults of its own.
resource "capi_provider" "core" {
  type    = "core"
  name    = "cluster-api"
  version = "v1.14.0"
  manager = {
    feature_gates = { ClusterTopology = true, MachinePool = true }
  }
}

resource "capi_provider" "tinkerbell" {
  type         = "infrastructure"
  name         = "tinkerbell"
  version      = "v0.7.9"
  fetch_config = { owner = "tinkerbell-community" }
  manager      = { feature_gates = { ClusterTopology = true } }

  depends_on = [capi_provider.core]
}

resource "capi_provider" "talos_bootstrap" {
  type         = "bootstrap"
  name         = "talos"
  version      = "v0.8.2"
  fetch_config = { owner = "sidero-community" }

  depends_on = [capi_provider.core]
}

resource "capi_provider" "talos_control_plane" {
  type         = "control-plane"
  name         = "talos"
  version      = "v0.7.1"
  fetch_config = { owner = "sidero-community" }

  depends_on = [capi_provider.core]
}

resource "capi_provider" "helm" {
  type = "addon"
  name = "helm"

  depends_on = [capi_provider.core]
}

# Objects of a provider take its kinds, so they render at plan.
resource "capi_machine_template" "worker" {
  name           = "worker"
  namespace      = "capi"
  infrastructure = capi_provider.tinkerbell.name
  api_version    = capi_provider.tinkerbell.kinds["TinkerbellMachineTemplate"].api_version
  spec           = { hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "worker" } } }] } }
}
