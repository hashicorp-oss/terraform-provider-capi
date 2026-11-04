// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"errors"
	"testing"
)

func TestPlan(t *testing.T) {
	tests := []struct {
		name      string
		obs       Observation
		hist      History
		hasAddons bool
		want      ActionKind
		wantErr   error
	}{
		{name: "unreachable boots installer", obs: Observation{Talos: TalosUnreachable}, want: ActionBootInstaller},
		{name: "foreign boots installer", obs: Observation{Talos: TalosForeign}, want: ActionBootInstaller},
		{name: "unreachable after max attempts fails", obs: Observation{Talos: TalosUnreachable}, hist: History{BootAttempts: 3}, want: ActionFail, wantErr: ErrAttemptsExhausted},
		{name: "maintenance applies config", obs: Observation{Talos: TalosMaintenance}, want: ActionApplyConfig},
		{name: "maintenance after apply reboots to disk", obs: Observation{Talos: TalosMaintenance}, hist: History{ConfigApplied: true, BootAttempts: 1}, want: ActionRebootToDisk},
		{name: "maintenance after apply at max attempts fails", obs: Observation{Talos: TalosMaintenance}, hist: History{ConfigApplied: true, BootAttempts: 3}, want: ActionFail, wantErr: ErrAttemptsExhausted},
		{name: "ours with media attached detaches", obs: Observation{Talos: TalosOurs, Etcd: EtcdNotBootstrapped}, hist: History{MediaAttached: true}, want: ActionDetachMedia},
		{name: "ours not bootstrapped bootstraps", obs: Observation{Talos: TalosOurs, Etcd: EtcdNotBootstrapped}, want: ActionBootstrapEtcd},
		{name: "ours bootstrapped kube unreachable waits", obs: Observation{Talos: TalosOurs, Etcd: EtcdBootstrapped, Kubernetes: K8sUnreachable}, want: ActionWaitKubernetes},
		{name: "ours kube reachable installs addons", obs: Observation{Talos: TalosOurs, Etcd: EtcdBootstrapped, Kubernetes: K8sReachable}, want: ActionInstallAddons},
		{name: "addons installed with addons waits ready", obs: Observation{Talos: TalosOurs, Etcd: EtcdBootstrapped, Kubernetes: K8sReachable}, hist: History{AddonsInstalled: true}, hasAddons: true, want: ActionWaitReady},
		{name: "addons installed without addons is done", obs: Observation{Talos: TalosOurs, Etcd: EtcdBootstrapped, Kubernetes: K8sReachable}, hist: History{AddonsInstalled: true}, want: ActionDone},
		{name: "node ready is done", obs: Observation{Talos: TalosOurs, Etcd: EtcdBootstrapped, Kubernetes: K8sNodeReady}, hist: History{AddonsInstalled: true}, hasAddons: true, want: ActionDone},
		{name: "stuck installed foreign node boots reset image", obs: Observation{Talos: TalosForeign}, hist: History{NormalBootStuck: true, BootAttempts: 1}, want: ActionBootReset},
		{name: "stuck installed unreachable node boots reset image", obs: Observation{Talos: TalosUnreachable}, hist: History{NormalBootStuck: true, BootAttempts: 1}, want: ActionBootReset},
		{name: "reset image booted once then normal", obs: Observation{Talos: TalosForeign}, hist: History{NormalBootStuck: true, ResetBooted: true, BootAttempts: 2}, want: ActionBootInstaller},
		{name: "stuck at max attempts fails", obs: Observation{Talos: TalosForeign}, hist: History{NormalBootStuck: true, BootAttempts: 3}, want: ActionFail, wantErr: ErrAttemptsExhausted},
		{name: "ours needing reset resets to maintenance", obs: Observation{Talos: TalosOurs, Etcd: EtcdNotBootstrapped}, hist: History{NeedsReset: true}, want: ActionResetToMaintenance},
		{name: "ours needing reset after config proceeds", obs: Observation{Talos: TalosOurs, Etcd: EtcdNotBootstrapped}, hist: History{NeedsReset: true, ConfigApplied: true}, want: ActionBootstrapEtcd},
		{name: "unknown state fails", obs: Observation{Talos: TalosState("bogus")}, want: ActionFail, wantErr: ErrUnknownState},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Plan(PlanInput{Observation: tt.obs, History: tt.hist, MaxAttempts: 3, HasAddons: tt.hasAddons})
			if got.Kind != tt.want {
				t.Fatalf("Plan() kind = %q, want %q", got.Kind, tt.want)
			}
			if tt.wantErr != nil && !errors.Is(got.Err, tt.wantErr) {
				t.Fatalf("Plan() err = %v, want %v", got.Err, tt.wantErr)
			}
			if tt.wantErr == nil && got.Err != nil {
				t.Fatalf("Plan() unexpected err = %v", got.Err)
			}
		})
	}
}

func TestPlan_InstalledNodeUnreachableWaits(t *testing.T) {
	// A node we already installed that is briefly unreachable (post-install
	// reboot) must be waited for, not re-imaged — even with attempts remaining.
	got := Plan(PlanInput{
		Observation: Observation{Talos: TalosUnreachable},
		History:     History{Installed: true, ConfigApplied: true, BootAttempts: 1},
		MaxAttempts: 3,
	})
	if got.Kind != ActionAwaitNode {
		t.Fatalf("Plan() kind = %q, want %q", got.Kind, ActionAwaitNode)
	}
}

func TestPlan_ForeignNodeReimagesEvenIfInstalledFlagSet(t *testing.T) {
	// Foreign (another cluster's node) is always re-imaged; the Installed flag
	// only protects our own node while it is transiently unreachable.
	got := Plan(PlanInput{
		Observation: Observation{Talos: TalosForeign},
		History:     History{Installed: true, BootAttempts: 0},
		MaxAttempts: 3,
	})
	if got.Kind != ActionBootInstaller {
		t.Fatalf("Plan() kind = %q, want %q", got.Kind, ActionBootInstaller)
	}
}

func TestObservationString(t *testing.T) {
	obs := Observation{Power: PowerOn, Talos: TalosOurs, Etcd: EtcdBootstrapped, Kubernetes: K8sReachable}
	got := obs.String()
	want := "power=on talos=ours etcd=bootstrapped kubernetes=reachable"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestPlan_PinNetwork(t *testing.T) {
	ours := Observation{Talos: TalosOurs, Etcd: EtcdBootstrapped, Kubernetes: K8sUnreachable}
	if got := Plan(PlanInput{Observation: ours, PinNetwork: true, MaxAttempts: 3}); got.Kind != ActionPinNetwork {
		t.Errorf("static addressing must pin after etcd, got %s", got.Kind)
	}
	if got := Plan(PlanInput{Observation: ours, PinNetwork: true, History: History{NetworkPinned: true}, MaxAttempts: 3}); got.Kind != ActionWaitKubernetes {
		t.Errorf("pinned node proceeds, got %s", got.Kind)
	}
	if got := Plan(PlanInput{Observation: ours, MaxAttempts: 3}); got.Kind != ActionWaitKubernetes {
		t.Errorf("DHCP node never pins, got %s", got.Kind)
	}
	notBootstrapped := Observation{Talos: TalosOurs, Etcd: EtcdNotBootstrapped}
	if got := Plan(PlanInput{Observation: notBootstrapped, PinNetwork: true, MaxAttempts: 3}); got.Kind != ActionBootstrapEtcd {
		t.Errorf("etcd bootstrap comes before the pin, got %s", got.Kind)
	}
}
