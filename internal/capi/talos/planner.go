// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

// PlanInput is everything the planner may look at.
type PlanInput struct {
	Observation Observation
	History     History
	// MaxAttempts is boot.attempts from the configuration.
	MaxAttempts int
	// HasAddons is true when at least one Helm release or manifest was configured.
	HasAddons bool
	// PinNetwork is true when the node's static identity must be written to
	// META once it runs (static addressing was configured).
	PinNetwork bool
}

// Plan chooses exactly one action from the observation and history. It is a
// pure function: no I/O, no time, so every branch is table-testable.
func Plan(in PlanInput) Action {
	obs, h := in.Observation, in.History

	switch obs.Talos {
	case TalosUnreachable, TalosForeign:
		// A node we have already installed (seen as ours) that is now merely
		// unreachable is rebooting — most often the post-install kexec. Wait for
		// it to return instead of wiping a good install. Foreign is different: it
		// is some other cluster's node and must be re-imaged.
		if h.Installed && obs.Talos == TalosUnreachable {
			return Action{Kind: ActionAwaitNode}
		}
		if h.BootAttempts >= in.MaxAttempts {
			return Action{Kind: ActionFail, Err: ErrAttemptsExhausted}
		}
		// A normal-image boot got stuck on an already-installed node that we
		// cannot reach over the Talos API to reset (foreign/unreachable):
		// escalate to a one-shot reset (wipe) boot, exactly once. The wipe leaves
		// a blank disk, so the following normal boot reaches maintenance.
		if h.NormalBootStuck && !h.ResetBooted {
			return Action{Kind: ActionBootReset}
		}
		return Action{Kind: ActionBootInstaller}

	case TalosMaintenance:
		if !h.ConfigApplied {
			return Action{Kind: ActionApplyConfig}
		}
		if h.BootAttempts >= in.MaxAttempts {
			return Action{Kind: ActionFail, Err: ErrAttemptsExhausted}
		}
		return Action{Kind: ActionRebootToDisk}

	case TalosOurs:
		// A node we own that must be re-provisioned (recognized on create via
		// seeded secrets, before any config was applied this run) is reset to
		// maintenance over the Talos API — we have access because it is ours, so
		// no media and no wasted boot cycle.
		if h.NeedsReset && !h.ConfigApplied {
			return Action{Kind: ActionResetToMaintenance}
		}
		switch {
		case h.MediaAttached:
			return Action{Kind: ActionDetachMedia}
		case obs.Etcd != EtcdBootstrapped:
			return Action{Kind: ActionBootstrapEtcd}
		case in.PinNetwork && !h.NetworkPinned:
			return Action{Kind: ActionPinNetwork}
		case obs.Kubernetes == K8sUnreachable:
			return Action{Kind: ActionWaitKubernetes}
		case !h.AddonsInstalled:
			return Action{Kind: ActionInstallAddons}
		case in.HasAddons && obs.Kubernetes != K8sNodeReady:
			return Action{Kind: ActionWaitReady}
		default:
			return Action{Kind: ActionDone}
		}
	}

	return Action{Kind: ActionFail, Err: ErrUnknownState}
}
