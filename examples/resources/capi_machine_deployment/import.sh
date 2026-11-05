#!/bin/bash
# namespace/cluster/name, where name is the MachineDeployment's name (direct
# mode) or the topology entry's name (ClusterClass mode).
terraform import capi_machine_deployment.workers capi/prod/workers
