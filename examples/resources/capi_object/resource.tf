# Any object of an installed provider; the API version is resolved from its
# CRDs. Only the fields the configuration sets are compared on read.
resource "capi_object" "pool" {
  provider_type = "ipam"
  provider_name = "unifi"
  kind          = "UnifiIPPool"
  name          = "workers"
  namespace     = "capi"
  spec = {
    network = "10.0.80.0/24"
    ranges  = [{ start = "10.0.80.100", end = "10.0.80.200" }]
  }
  wait = { conditions = ["Ready"], timeout = "5m" }
}

# With an explicit API version.
resource "capi_object" "hardware" {
  api_version = "tinkerbell.org/v1alpha1"
  kind        = "Hardware"
  name        = "worker-1"
  namespace   = "capi"
  spec = {
    metadata = { instance = { hostname = "worker-1" } }
  }
}
