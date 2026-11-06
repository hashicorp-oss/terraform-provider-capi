# Cluster API Terraform Provider (terraform-provider-capi)

[![CI/CD](https://github.com/hashicorp-oss/terraform-provider-capi/actions/workflows/ci.yaml/badge.svg)](https://github.com/hashicorp-oss/terraform-provider-capi/actions/workflows/ci.yaml) [![Acceptance Tests](https://github.com/hashicorp-oss/terraform-provider-capi/actions/workflows/acctest.yaml/badge.svg)](https://github.com/hashicorp-oss/terraform-provider-capi/actions/workflows/acctest.yaml)

> **Note**: a self-managed cluster is stood up through a disposable bootstrap
> cluster and pivots into itself. While that runs, the bootstrap cluster (kind,
> or a Kairos live node on bare metal) is the only management plane: run the
> apply from a machine that can reach the cluster network, and see the
> `capi_cluster` documentation before interrupting one.

The provider manages [Cluster API](https://cluster-api.sigs.k8s.io) clusters
through clusterctl's own libraries, the way the `aws` provider manages EKS:
the provider block names the management cluster, `capi_cluster` is the
cluster (identity, version, network, control plane, waits), and every other
Cluster API object is its own resource: node groups, machine templates,
classes, health checks, addons, Tinkerbell hardware and clusterctl providers.

## Documentation

Browse the documentation on the [Terraform provider registry](https://registry.terraform.io/providers/hashicorp-oss/capi/latest/docs),
or in [`docs/`](./docs). The design is in
[`docs/superpowers/specs/2026-11-04-provider-design.md`](./docs/superpowers/specs/2026-11-04-provider-design.md).

## Supported Cluster API Versions

The provider follows the core contract installed on the management cluster:
v1beta2 (Cluster API 1.11 and later) and the legacy v1beta1 contract. Kinds and
API versions are discovered from the installed CRDs rather than hardcoded.

## Using the Provider

### Terraform 1.11 and above

```terraform
terraform {
  required_providers {
    capi = {
      source = "hashicorp-oss/capi"
    }
  }
}

provider "capi" {
  kubernetes = {
    config_path    = "~/.kube/config"
    config_context = "mgmt"
  }
}

resource "capi_machine_template" "cp" {
  name           = "dev-cp"
  infrastructure = "docker"
  spec           = {}
}

resource "capi_cluster" "dev" {
  name               = "dev"
  kubernetes_version = "v1.34.0"
  infrastructure     = { provider = "docker" }
  control_plane      = { provider = "kubeadm", replicas = 1, infrastructure = capi_machine_template.cp.ref }
  timeouts           = { create = "30m" }
}
```

[`examples/`](./examples) has an example for every resource, and
[`examples/talos-tinkerbell-cluster`](./examples/talos-tinkerbell-cluster) a
complete bare-metal Talos cluster on Tinkerbell.

## Development

You need [Go](https://go.dev) (the version in `go.mod`), Terraform and, for
the acceptance tests, Docker.

```shell
make build      # go install
make test       # unit tests through gotestsum (junit.xml, coverage.txt)
make testacc    # acceptance tests against a real management cluster
make lint       # golangci-lint
make fmt        # gci, gofumpt, goimports and golines through golangci-lint
make generate   # docs from templates/ and examples/ (tfplugindocs)
```

The provider package is [`capi/`](./capi) (resources `*_resource.go`, data
sources `*_data_source.go`, actions `*_action.go`, the ephemeral resource
`*_ephemeral.go`, validators in [`capi/validators`](./capi/validators)); the
Cluster API engine it drives (clusterctl, the bootstrap and pivot workflow,
manifest generation) is [`internal/capi`](./internal/capi).

To try a local build, point Terraform at it with a
[development override](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers)
for `registry.terraform.io/hashicorp-oss/capi`.
