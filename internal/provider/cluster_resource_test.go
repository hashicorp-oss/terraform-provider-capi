// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccClusterResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create and Read testing
			{
				Config: testAccClusterResourceConfig("test-cluster"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("capi_cluster.test", "name", "test-cluster"),
					resource.TestCheckResourceAttr("capi_cluster.test", "infrastructure.provider", "docker"),
					resource.TestCheckResourceAttr("capi_cluster.test", "management.skip_init", "true"),
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
					"management", "wait", "output",
					"status",
				},
			},
		},
	})
}

func testAccClusterResourceConfig(name string) string {
	return `
resource "capi_cluster" "test" {
  name = "` + name + `"

  infrastructure {
    provider = "docker"
  }

  management {
    skip_init = true
  }

  wait {
    enabled = false
  }
}
`
}
