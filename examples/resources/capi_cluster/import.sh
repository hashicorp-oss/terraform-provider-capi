#!/bin/bash
# [namespace/]name on the management cluster the provider block names. The
# configuration's inputs are not read back; the status is. A self-managed
# cluster is imported through its own kubeconfig in the provider block.
terraform import capi_cluster.example my-cluster
terraform import capi_cluster.example capi-system/my-cluster
