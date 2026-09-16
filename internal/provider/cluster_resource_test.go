// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccClusterResource(t *testing.T) {
	// The docker infrastructure provider's "development" flavor template is
	// ClusterClass-based, which requires the ClusterTopology feature gate to
	// be enabled on the core provider at install time.
	t.Setenv("CLUSTER_TOPOLOGY", "true")

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create and Read testing
			{
				Config: testAccClusterResourceConfig("test-cluster"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("capi_cluster.test", "name", "test-cluster"),
					resource.TestCheckResourceAttr("capi_cluster.test", "infrastructure.%", "1"),
					resource.TestCheckResourceAttr("capi_cluster.test", "topology.control_plane.replicas", "1"),
					resource.TestCheckResourceAttr("capi_cluster.test", "management.skip_init", "false"),
					resource.TestCheckResourceAttr("capi_cluster.test", "management.self_managed", "false"),
					resource.TestCheckResourceAttrSet("capi_cluster.test", "id"),
					resource.TestCheckResourceAttrSet("capi_cluster.test", "management.namespace"),
				),
			},
			// ImportState testing
			{
				ResourceName:      "capi_cluster.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					// kubernetes_version and infrastructure are
					// creation-time-only template parameters: the management
					// cluster used to create the workload cluster is an
					// ephemeral bootstrap cluster torn down after apply, so
					// there is no live source to recover them from on import.
					"management", "wait", "output",
					"status", "kubernetes_version",
					"infrastructure", "infrastructure.provider", "infrastructure.%",
				},
			},
		},
	})
}

func testAccClusterResourceConfig(name string) string {
	return `
resource "capi_cluster" "test" {
  name               = "` + name + `"
  kubernetes_version = "v1.31.0"

  infrastructure = { docker = {} }

  topology = {
    control_plane = { replicas = 1 }
    workers = {
      machine_deployments = [{ name = "md-0", replicas = 1 }]
    }
  }

  wait = {
    enabled = false
  }
}
`
}

// TestAccClusterResource_TalosBootstrap provisions a real bare-metal node as a
// Talos bootstrap cluster through the capi_cluster resource. Only the
// machine-specific settings are Terraform variables (passed via TF_VAR_* on the
// command line); the rest of the config is fixed. It runs only when TF_ACC and
// the machine variables are set, e.g.:
//
//	TF_ACC=1 \
//	TF_VAR_node_ip=10.0.0.160 \
//	TF_VAR_disk=/dev/nvme0n1 \
//	TF_VAR_bmc_address='https://10.0.0.160:16993' \
//	TF_VAR_bmc_username=admin \
//	TF_VAR_bmc_password=... \
//	go test ./internal/provider/ -run TestAccClusterResource_TalosBootstrap -v -timeout 90m
func TestAccClusterResource_TalosBootstrap(t *testing.T) {
	if os.Getenv("TF_ACC") == "" || os.Getenv("TF_VAR_bmc_address") == "" {
		t.Skip("set TF_ACC and TF_VAR_node_ip/disk/bmc_address/bmc_username/bmc_password to run against real hardware")
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccClusterResourceTalosConfig(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("capi_cluster.talos", "id"),
					resource.TestCheckResourceAttr("capi_cluster.talos", "management.bootstrap.type", "talos"),
					resource.TestCheckResourceAttrSet("capi_cluster.talos", "management.bootstrap.mode"),
					// The bootstrapper generated and persisted the machine-secrets
					// bundle, and the cluster came up enough to expose a kubeconfig.
					resource.TestCheckResourceAttrSet("capi_cluster.talos", "provider_secrets"),
					resource.TestCheckResourceAttrSet("capi_cluster.talos", "status.kubeconfig"),
				),
			},
		},
	})
}

// testAccClusterResourceTalosConfig fixes everything except the machine-specific
// settings, which come from TF_VAR_node_ip / disk / bmc_address / bmc_username /
// bmc_password on the command line.
func testAccClusterResourceTalosConfig() string {
	return `
variable "node_ip" { type = string }
variable "disk" {
  type    = string
  default = "/dev/nvme0n1"
}
variable "bmc_address" { type = string }
variable "bmc_username" {
  type    = string
  default = "admin"
}
variable "bmc_password" {
  type      = string
  sensitive = true
}

resource "capi_cluster" "talos" {
  name               = "acc-talos"
  kubernetes_version = "v1.34.0"

  infrastructure = { tinkerbell = { version = "v0.5.4" } }
  bootstrap      = { talos = { version = "v0.6.7" } }
  control_plane  = { talos = { version = "v0.6.7" } }

  topology = {
    control_plane = { replicas = 1 }
  }

  management = {
    self_managed = true

    bootstrap = {
      type    = "talos"
      mode    = "in_place"
      machine = "nuc"

      boot = {
        method   = "virtual_media"
        timeout  = "15m"
        attempts = 3
      }

      talos = {
        version      = "v1.13.10"
        architecture = "amd64"
        image        = { schematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba" }
      }
    }
  }

  inventory = {
    machine = [
      {
        hostname = "nuc"
        network = {
          ip_address  = var.node_ip
          netmask     = "255.255.0.0"
          gateway     = "10.0.0.1"
          mac_address = "88:ae:dd:75:3d:a0"
        }
        disk = { device = var.disk }
        bmc = {
          address  = var.bmc_address
          username = var.bmc_username
          password = var.bmc_password
        }
        labels = { type = "cp" }
      },
    ]
  }

  wait = { enabled = false }
}
`
}
