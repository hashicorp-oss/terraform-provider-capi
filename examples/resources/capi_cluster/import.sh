#!/bin/bash
# By name (the management kubeconfig and namespace come from the configuration
# on the next apply):
terraform import capi_cluster.example my-cluster
# With the management cluster's kubeconfig and the namespace, so the status is
# read right away. Both must match `management.kubeconfig` and
# `management.namespace` in the configuration; each requires replacement when
# changed.
terraform import capi_cluster.example ~/.kube/management.kubeconfig:capi-system/my-cluster
