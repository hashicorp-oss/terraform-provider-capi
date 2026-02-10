# Terraform Provider for Cluster API (CAPI)

This repository contains a [Terraform](https://www.terraform.io) provider for managing Cluster API resources using clusterctl.

## Features

- **Cluster Management**: Create and manage Cluster API clusters declaratively with Terraform
- **clusterctl Integration**: Uses the official clusterctl client library for cluster operations
- **Flexible Configuration**: Support for multiple infrastructure providers (Docker, AWS, Azure, etc.)
- **Management Cluster Init**: Optionally initialize management clusters or skip if already configured
- **Template Generation**: Automatic generation of cluster manifests using clusterctl

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
- [Go](https://golang.org/doc/install) >= 1.24
- A Kubernetes cluster for use as a management cluster (e.g., kind, minikube, or existing cluster)
- [clusterctl](https://cluster-api.sigs.k8s.io/user/quick-start.html) compatible environment

## Building The Provider

1. Clone the repository
1. Enter the repository directory
1. Build the provider using the Go `install` command:

```shell
go install
```

## Adding Dependencies

This provider uses [Go modules](https://github.com/golang/go/wiki/Modules).
Please see the Go documentation for the most up to date information about using Go modules.

To add a new dependency `github.com/author/dependency` to your Terraform provider:

```shell
go get github.com/author/dependency
go mod tidy
```

Then commit the changes to `go.mod` and `go.sum`.

## Using the provider

The provider exposes a `capi_cluster` resource for managing Cluster API clusters.

### Example Usage

```hcl
terraform {
  required_providers {
    capi = {
      source = "tinkerbell-community/capi"
    }
  }
}

provider "capi" {}

resource "capi_cluster" "my_cluster" {
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
}
```

### Available Resources

- **capi_cluster**: Manages a Cluster API cluster using clusterctl

For detailed documentation on all available attributes and their usage, see the [docs](./docs) directory.

## Developing the Provider

If you wish to work on the provider, you'll first need [Go](http://www.golang.org) installed on your machine (see [Requirements](#requirements) above).

To compile the provider, run `go install`. This will build the provider and put the provider binary in the `$GOPATH/bin` directory.

To generate or update documentation, run `make generate`.

In order to run the full suite of Acceptance tests, run `make testacc`.

*Note:* Acceptance tests create real resources, and often cost money to run.

```shell
make testacc
```
