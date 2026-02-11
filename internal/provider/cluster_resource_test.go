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
					resource.TestCheckResourceAttr("capi_cluster.test", "infrastructure_provider", "docker"),
					resource.TestCheckResourceAttr("capi_cluster.test", "skip_init", "true"),
					resource.TestCheckResourceAttrSet("capi_cluster.test", "id"),
					resource.TestCheckResourceAttrSet("capi_cluster.test", "target_namespace"),
					// Check that computed attributes exist (even if empty)
					resource.TestCheckResourceAttrSet("capi_cluster.test", "kubeconfig"),
					resource.TestCheckResourceAttrSet("capi_cluster.test", "cluster_description"),
				),
			},
			// ImportState testing
			{
				ResourceName:      "capi_cluster.test",
				ImportState:       true,
				ImportStateVerify: true,
				// These fields are not returned by import
				ImportStateVerifyIgnore: []string{"management_kubeconfig", "skip_init", "wait_for_ready", "kubeconfig", "cluster_description"},
			},
		},
	})
}

func testAccClusterResourceConfig(name string) string {
	return `
resource "capi_cluster" "test" {
  name                   = "` + name + `"
  infrastructure_provider = "docker"
  skip_init              = true
  wait_for_ready         = false
}
`
}
