terraform {
  required_version = ">= 1.11"

  required_providers {
    capi = { source = "hashicorp-oss/capi" }
    # One object only: the UniFi credentials Secret (ipam.tf). capi_object
    # writes an object's spec, and a Secret has none.
    kubernetes = { source = "hashicorp/kubernetes" }
  }
}
