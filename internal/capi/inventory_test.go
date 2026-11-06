// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"strings"
	"testing"
)

func testInventory() InventoryOptions {
	return InventoryOptions{
		Namespace:        "tinkerbell",
		ClusterName:      "bm-cluster",
		BootstrapMachine: "cp-1",
		Machines: []InventoryMachine{
			{
				Hostname: "nuc-15",
				MAC:      "88:ae:dd:75:3d:a0",
				VLANID:   "40",
				Disk:     "/dev/nvme0n1",
				BMC: &InventoryBMC{
					Address:  "https://10.1.40.38:16993",
					Username: "admin",
					Password: "amt-pass",
				},
				Labels: map[string]string{"type": "worker"},
			},
			{
				Hostname:    "cp-1",
				IP:          "10.1.40.35",
				Netmask:     "255.255.0.0",
				Gateway:     "10.0.0.1",
				MAC:         "aa:bb:cc:dd:ee:01",
				Nameservers: []string{"10.0.0.1", "1.1.1.1"},
				Disk:        "/dev/nvme0n1",
				BMC: &InventoryBMC{
					Address:  "https://10.0.197.82",
					Username: "admin",
					Password: "secret",
				},
				Labels: map[string]string{"type": "cp"},
			},
			{Hostname: "pi-3", IP: "10.1.40.33", MAC: "dc:a6:32:00:00:03"},
		},
	}
}

// Sorted by hostname; the bootstrap machine is provisioned with PXE off; AMT
// ports select the IntelAMT rufio provider; everything else is Redfish with
// basic auth; a machine without a BMC gets only Hardware.
func TestInventoryManifests_Golden(t *testing.T) {
	got, err := InventoryManifests(testInventory())
	if err != nil {
		t.Fatal(err)
	}
	assertManifestMatchesGolden(t, "inventory-tinkerbell", got)
}

func TestInventoryManifests_NoBootstrapMachineMeansAllNetboot(t *testing.T) {
	opts := testInventory()
	opts.BootstrapMachine = ""
	got, err := InventoryManifests(opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), hardwareProvisionedAnnotation) ||
		strings.Contains(string(got), "allowPXE: false") {
		t.Fatalf("no machine may be provisioned without a bootstrap machine:\n%s", got)
	}
}

func TestInventoryManifests_RejectsMissingHostnameAndBadBMC(t *testing.T) {
	if _, err := InventoryManifests(
		InventoryOptions{Machines: []InventoryMachine{{MAC: "aa"}}},
	); err == nil {
		t.Error("missing hostname must be rejected")
	}
	_, err := InventoryManifests(
		InventoryOptions{
			Machines: []InventoryMachine{
				{Hostname: "x", BMC: &InventoryBMC{Address: "http://:443"}},
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "x") {
		t.Errorf("bad bmc address must be rejected naming the machine, got %v", err)
	}
}

func TestInventoryManifests_DefaultsNamespace(t *testing.T) {
	got, err := InventoryManifests(
		InventoryOptions{Machines: []InventoryMachine{{Hostname: "a", MAC: "aa"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "namespace: default") {
		t.Fatalf("namespace must default:\n%s", got)
	}
}
