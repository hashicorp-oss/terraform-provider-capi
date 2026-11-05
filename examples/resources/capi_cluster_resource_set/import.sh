#!/bin/bash
# namespace/name of the ClusterResourceSet; its ConfigMaps and Secrets are
# read back into manifests and secrets.
terraform import capi_cluster_resource_set.lb_pool capi/tinkerbell-lb-pool
