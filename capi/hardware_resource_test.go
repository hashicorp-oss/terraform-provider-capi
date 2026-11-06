// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"strings"
	"testing"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestHardwareObjectsAndReadback(t *testing.T) {
	m := capi.InventoryMachine{
		Hostname:    "cp-1",
		IP:          "10.0.80.11",
		Netmask:     "255.255.255.0",
		Gateway:     "10.0.80.1",
		MAC:         "aa:bb:cc:dd:ee:01",
		Nameservers: []string{"10.0.80.1"},
		VLANID:      "80",
		Disk:        "/dev/nvme0n1",
		BMC: &capi.InventoryBMC{
			Address:  "10.0.80.101:16992",
			Username: "admin",
			Password: "secret",
		},
		Labels: map[string]string{"type": "cp"},
	}
	objs, err := capi.HardwareObjects("capi", "prod", m, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 || objs[0].GetKind() != "Secret" ||
		objs[1].GetKind() != capi.RufioMachineKind ||
		objs[2].GetKind() != capi.HardwareKind {
		t.Fatalf("objects = %d", len(objs))
	}
	if objs[0].GetName() != capi.BMCSecretName("cp-1") || objs[0].GetNamespace() != "capi" {
		t.Errorf("secret = %s/%s", objs[0].GetNamespace(), objs[0].GetName())
	}
	hw := objs[2]
	if hw.GetLabels()[clusterNameLabel] != "prod" || hw.GetLabels()["type"] != "cp" {
		t.Errorf("hardware labels = %v", hw.GetLabels())
	}
	f := readHardware(&hw)
	if f.Cluster != "prod" || f.MAC != m.MAC || f.IP != m.IP || f.Netmask != m.Netmask ||
		f.Gateway != m.Gateway ||
		f.VLANID != "80" ||
		f.Disk != m.Disk {
		t.Errorf("facts = %+v", f)
	}
	if strings.Join(f.Nameservers, ",") != "10.0.80.1" || f.Labels["type"] != "cp" ||
		f.Labels[clusterNameLabel] != "" {
		t.Errorf("facts = %+v", f)
	}
	if f.Owner != "" || f.Provisioned || !f.AllowPXE || f.Adopted {
		t.Errorf("fresh hardware = %+v", f)
	}

	// Adopted: provisioned and off PXE.
	objs, _ = capi.HardwareObjects("capi", "prod", m, true)
	f = readHardware(&objs[2])
	if !f.Provisioned || f.AllowPXE || !f.Adopted {
		t.Errorf("adopted = %+v", f)
	}

	// Claimed by CAPT: the owner labels are status, not user labels.
	claimed := objs[2].DeepCopy()
	labels := claimed.GetLabels()
	labels[capi.HardwareOwnerNameLabel] = "prod-workers-abc"
	labels[capi.HardwareOwnerNamespaceLabel] = "capi"
	claimed.SetLabels(labels)
	f = readHardware(claimed)
	if f.Owner != "prod-workers-abc" || f.OwnerNamespace != "capi" || len(f.Labels) != 1 {
		t.Errorf("claimed = %+v", f)
	}

	// No BMC: two objects fewer; a missing hostname is an error.
	m.BMC = nil
	objs, _ = capi.HardwareObjects("", "", m, false)
	if len(objs) != 1 || objs[0].GetNamespace() != "default" ||
		objs[0].GetLabels()[clusterNameLabel] != "" {
		t.Errorf("no bmc = %v", objs)
	}
	if _, err := capi.HardwareObjects("capi", "prod", capi.InventoryMachine{}, false); err == nil {
		t.Error("hostname is required")
	}
	if !addressesMatch("https://10.0.80.101:16992/", "10.0.80.101:16992") ||
		addressesMatch("10.0.80.101", "10.0.80.101:443") {
		t.Error("addressesMatch")
	}
}

func TestReadHardware_Legacy(t *testing.T) {
	hw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": capi.HardwareAPIVersion,
		"kind":       capi.HardwareKind,
		"metadata":   map[string]any{"name": "x", "namespace": "capi"},
		"spec": map[string]any{
			"interfaces": []any{map[string]any{"dhcp": map[string]any{"mac": "aa"}}},
		},
	}}
	f := readHardware(hw)
	if f.MAC != "aa" || !f.AllowPXE || f.Disk != "" || len(f.Labels) != 0 {
		t.Errorf("sparse hardware = %+v", f)
	}
}
