data "capi_cluster" "prod" {
  name      = "prod"
  namespace = "capi"
}

output "endpoint" {
  value = data.capi_cluster.prod.endpoint
}
