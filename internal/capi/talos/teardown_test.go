// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

func TestBootstrapper_PinsNetworkIdentityWhenStaticallyAddressed(t *testing.T) {
	sim := newSim()
	b, _ := newTestBootstrapper(t, sim, func(c *Config) {
		c.Machine.Netmask, c.Machine.Gateway, c.Machine.MAC = "255.255.255.0", "10.0.0.1", "aa:bb:cc:dd:ee:01"
		c.Machine.Nameservers = []string{"10.0.0.1"}
	})
	create(t, b)
	if sim.pinned == nil {
		t.Fatal("expected the node identity to be written to META")
	}
	if sim.pinned.Address != "10.0.0.5/24" || sim.pinned.Hostname != "cp-1" || sim.pinned.Gateway != "10.0.0.1" || sim.pinned.MAC != "aa:bb:cc:dd:ee:01" {
		t.Errorf("pin = %+v", *sim.pinned)
	}
	if sim.count("pin-network") != 1 || !b.last.hist.NetworkPinned {
		t.Errorf("pin-network calls = %d, pinned = %v", sim.count("pin-network"), b.last.hist.NetworkPinned)
	}
}

func TestBootstrapper_DHCPNodeIsNotPinned(t *testing.T) {
	sim := newSim()
	b, _ := newTestBootstrapper(t, sim, nil)
	create(t, b)
	if sim.count("pin-network") != 0 || sim.pinned != nil {
		t.Errorf("a node without a netmask must not be pinned (calls=%d)", sim.count("pin-network"))
	}
}

func TestBootstrapper_PinFailureIsLoggedNotFatal(t *testing.T) {
	sim := newSim()
	sim.faults.pinErr = errors.New("meta write refused")
	b, _ := newTestBootstrapper(t, sim, func(c *Config) { c.Machine.Netmask = "255.255.255.0" })
	create(t, b)
	if sim.pinned != nil || sim.count("pin-network") != fastTimeouts().BMCRetries {
		t.Errorf("pin must be retried %d times then abandoned; calls=%d pinned=%v", fastTimeouts().BMCRetries, sim.count("pin-network"), sim.pinned)
	}
	if !b.last.hist.NetworkPinned {
		t.Error("an abandoned pin must not loop forever")
	}
}

// memberSim is a cluster member for teardown tests: installed by us and
// running, or foreign.
func memberSim(name string, ours bool, seq *[]string) *simMachine {
	s := newSim()
	s.name, s.seq = name, seq
	s.power, s.running, s.diskInstalled, s.installedByUs, s.etcd = PowerOn, "disk", true, ours, ours
	return s
}

func TestBootstrapper_TeardownInPlace_ResetsMembersThenTheBootstrapNode(t *testing.T) {
	seq := []string{}
	own := newSim()
	own.name, own.seq = "cp-1", &seq
	w1 := memberSim("w1", true, &seq)
	w2 := memberSim("w2", false, &seq)
	byIP := map[string]*simMachine{"10.0.0.6": w1, "10.0.0.7": w2}
	byBMC := map[string]*simMachine{"10.0.1.6": w1, "10.0.1.7": w2}

	b, _ := newTestBootstrapper(t, own, nil)
	b.nodeFactory = func(ip string) Node { return byIP[ip] }
	b.bmcFactory = func(c BMCCredentials) BMC { return byBMC[c.Address] }
	cluster := create(t, b)
	seq = seq[:0]

	machines := []capi.InventoryMachine{
		{Hostname: "w2", IP: "10.0.0.7", BMC: &capi.InventoryBMC{Address: "10.0.1.7"}},
		{Hostname: "w1", IP: "10.0.0.6", BMC: &capi.InventoryBMC{Address: "10.0.1.6"}},
	}
	if err := b.TeardownInPlace(context.Background(), cluster, machines); err != nil {
		t.Fatalf("TeardownInPlace() error = %v", err)
	}
	got := strings.Join(seq, ",")
	for _, want := range []string{"w1:reset", "w1:power-off", "w2:power-off", "cp-1:reset", "cp-1:power-off"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "w2:reset") {
		t.Errorf("a foreign member is powered off, never reset: %s", got)
	}
	if strings.LastIndex(got, "w1:") > strings.Index(got, "cp-1:reset") || strings.LastIndex(got, "w2:") > strings.Index(got, "cp-1:reset") {
		t.Errorf("members must go down before the bootstrap node: %s", got)
	}
	if w1.diskInstalled || own.diskInstalled {
		t.Error("reset nodes must be wiped")
	}
	if b.last != nil {
		t.Error("session must be dropped after teardown")
	}
}

func TestBootstrapper_TeardownInPlace_WithoutSecretsOnlyPowersOff(t *testing.T) {
	own := newSim()
	w1 := memberSim("w1", true, nil)
	b, _ := newTestBootstrapper(t, own, func(c *Config) { c.StateDir = t.TempDir() })
	b.nodeFactory = func(string) Node { return w1 }
	b.bmcFactory = func(BMCCredentials) BMC { return w1 }
	err := b.TeardownInPlace(context.Background(), &capi.Cluster{Name: "x"}, []capi.InventoryMachine{{Hostname: "w1", IP: "10.0.0.6", BMC: &capi.InventoryBMC{Address: "10.0.1.6"}}})
	if err != nil {
		t.Fatalf("TeardownInPlace() error = %v", err)
	}
	if w1.count("reset") != 0 || w1.count("power-off") != 1 {
		t.Errorf("without a talosconfig the member can only be powered off: reset=%d off=%d", w1.count("reset"), w1.count("power-off"))
	}
}

func TestBootstrapper_TeardownInPlace_CollectsMemberFailures(t *testing.T) {
	own := newSim()
	w1 := memberSim("w1", true, nil)
	w1.faults.virtualMediaUnsupported = true // irrelevant to power; keeps the sim honest
	b, _ := newTestBootstrapper(t, own, nil)
	b.nodeFactory = func(string) Node { return w1 }
	b.bmcFactory = func(BMCCredentials) BMC { return &failingBMC{simMachine: w1} }
	cluster := create(t, b)
	err := b.TeardownInPlace(context.Background(), cluster, []capi.InventoryMachine{{Hostname: "w1", IP: "10.0.0.6", BMC: &capi.InventoryBMC{Address: "10.0.1.6"}}})
	if err == nil || !strings.Contains(err.Error(), "w1") || !strings.Contains(err.Error(), "power off") {
		t.Fatalf("member power-off failure must be reported by hostname, got %v", err)
	}
	if own.count("power-off") != 1 || own.diskInstalled {
		t.Error("the bootstrap node must still be torn down after a member failure")
	}
}

// failingBMC is a simMachine whose power-off always fails.
type failingBMC struct{ *simMachine }

func (f *failingBMC) PowerOff(context.Context) error { return errors.New("bmc unreachable") }
