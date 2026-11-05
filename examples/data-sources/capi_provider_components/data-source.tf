# The kinds and API versions a provider release ships, to name `api_version`
# on templates and objects so they render completely at plan.
data "capi_provider_components" "tinkerbell" {
  type         = "infrastructure"
  name         = "tinkerbell"
  version      = "v0.7.9"
  fetch_config = { owner = "tinkerbell-community" }
}

output "machine_template_api_version" {
  value = data.capi_provider_components.tinkerbell.kinds["TinkerbellMachineTemplate"].api_version
}
