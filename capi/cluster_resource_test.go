// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

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
					resource.TestCheckResourceAttr(
						"capi_cluster.test",
						"infrastructure.provider",
						"docker",
					),
					resource.TestCheckResourceAttr(
						"capi_cluster.test",
						"control_plane.replicas",
						"1",
					),
					resource.TestCheckResourceAttr(
						"capi_cluster.test",
						"management.self_managed",
						"false",
					),
					resource.TestCheckResourceAttr("capi_cluster.test", "status.phase", "ready"),
					resource.TestCheckResourceAttrSet("capi_cluster.test", "id"),
					resource.TestCheckResourceAttr("capi_cluster.test", "namespace", "default"),
				),
			},
			// ImportState testing
			{
				ResourceName:      "capi_cluster.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					// The cluster's inputs are not read back from the live
					// objects; only the status is.
					"management", "wait", "status", "kubernetes_version",
					"infrastructure", "control_plane",
				},
			},
		},
	})
}

// testAccClusterResourceConfig is a direct-mode docker cluster on the
// management cluster the provider block names, with its control-plane
// machine template as its own resource.
func testAccClusterResourceConfig(name string) string {
	return `
resource "capi_machine_template" "cp" {
  name           = "` + name + `-cp"
  infrastructure = "docker"
  spec           = {}
}

resource "capi_cluster" "test" {
  name               = "` + name + `"
  kubernetes_version = "v1.31.0"

  infrastructure = { provider = "docker" }
  control_plane = {
    provider       = "kubeadm"
    replicas       = 1
    infrastructure = capi_machine_template.cp.ref
  }

  wait = { conditions = [] }
}
`
}

// TestAccClusterResource_KairosBootstrap stands a self-managed Tinkerbell
// cluster up from a disposable Kairos live node. It needs real hardware and
// is skipped unless TF_ACC and the machine variables are set:
//
//	TF_ACC=1 TF_VAR_bootstrap_ip=10.0.0.50 TF_VAR_bootstrap_bmc=10.0.1.50 TF_VAR_bmc_password=... \
//	TF_VAR_cp_ip=10.0.0.5 TF_VAR_cp_mac=... TF_VAR_cp_bmc=10.0.1.5 TF_VAR_ssh_private_key="$(cat key)" \
//	go test ./internal/provider/ -run TestAccClusterResource_KairosBootstrap -v -timeout 120m
func TestAccClusterResource_KairosBootstrap(t *testing.T) {
	if os.Getenv("TF_ACC") == "" || os.Getenv("TF_VAR_bootstrap_bmc") == "" {
		t.Skip("set TF_ACC and the TF_VAR_* machine variables to run against real hardware")
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccClusterResourceKairosConfig(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("capi_cluster.kairos", "id"),
					resource.TestCheckResourceAttr(
						"capi_cluster.kairos",
						"management.bootstrap.type",
						"kairos",
					),
					// The pivot completed: the bootstrap cluster is gone and the
					// cluster manages itself.
					resource.TestCheckNoResourceAttr(
						"capi_cluster.kairos",
						"status.bootstrap_cluster",
					),
					resource.TestCheckResourceAttr("capi_cluster.kairos", "status.phase", "ready"),
					resource.TestCheckResourceAttrSet("capi_cluster.kairos", "provider_secrets"),
				),
			},
		},
	})
}

// testAccClusterResourceKairosConfig fixes everything except the
// machine-specific settings, which come from TF_VAR_* on the command line.
func testAccClusterResourceKairosConfig() string {
	return `
variable "bootstrap_ip" { type = string }
variable "bootstrap_bmc" { type = string }
variable "bootstrap_mac" {
  type    = string
  default = "aa:bb:cc:dd:ee:50"
}
variable "cp_ip" { type = string }
variable "cp_mac" { type = string }
variable "cp_bmc" { type = string }
variable "bmc_username" {
  type    = string
  default = "admin"
}
variable "bmc_password" {
  type      = string
  sensitive = true
}
variable "ssh_private_key" {
  type      = string
  sensitive = true
}
variable "kairos_iso" {
  type    = string
  default = "http://files.lab.internal/kairos-bootstrap.iso"
}

resource "capi_machine_template" "cp" {
  name           = "acc-kairos-cp"
  infrastructure = "tinkerbell"
  spec           = { hardwareAffinity = { required = [{ labelSelector = { matchLabels = { type = "cp" } } }] } }
}

resource "capi_cluster" "kairos" {
  name               = "acc-kairos"
  kubernetes_version = "v1.34.0"

  network = { control_plane_endpoint = { host = "10.0.0.160" } }

  infrastructure = { provider = "tinkerbell" }
  control_plane = {
    provider       = "talos"
    replicas       = 1
    infrastructure = capi_machine_template.cp.ref
  }

  management = {
    self_managed = true

    providers = {
      infrastructure = { tinkerbell = { version = "v0.7.9", fetch_config = { owner = "tinkerbell-community" } } }
      bootstrap      = { talos = { version = "v0.8.2", fetch_config = { owner = "sidero-community" } } }
      control_plane  = { talos = { version = "v0.7.1", fetch_config = { owner = "sidero-community" } } }
      addon          = { helm = {} }
    }

    bootstrap = {
      type    = "kairos"
      machine = "nuc"

      boot = {
        method   = "manual"
        timeout  = "20m"
        attempts = 1
      }

      kairos = {
        iso = var.kairos_iso
        ssh = { private_key = var.ssh_private_key }
      }

      addons = {
        helm = [{
          name      = "tinkerbell"
          namespace = "tinkerbell"
          chart     = "oci://ghcr.io/tinkerbell/charts/tinkerbell"
          values    = yamlencode({ deployment = { hostNetwork = true }, publicIP = var.bootstrap_ip })
        }]
      }
    }
  }

  inventory = {
    machine = [
      {
        hostname = "nuc"
        network = {
          ip_address  = var.bootstrap_ip
          netmask     = "255.255.255.0"
          gateway     = "10.0.0.1"
          mac_address = var.bootstrap_mac
        }
        bmc = {
          address  = var.bootstrap_bmc
          username = var.bmc_username
          password = var.bmc_password
        }
        labels = { type = "worker" }
      },
      {
        hostname = "cp-1"
        network = {
          ip_address  = var.cp_ip
          netmask     = "255.255.255.0"
          gateway     = "10.0.0.1"
          mac_address = var.cp_mac
        }
        disk = { device = "/dev/nvme0n1" }
        bmc = {
          address  = var.cp_bmc
          username = var.bmc_username
          password = var.bmc_password
        }
        labels = { type = "cp" }
      },
    ]
  }

  wait = { timeout = "90m" }
}
`
}
