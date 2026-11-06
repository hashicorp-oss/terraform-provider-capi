// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import "slices"

// CheckpointVersion is the version of the Checkpoint and DeleteCheckpoint
// JSON shapes. A reader refuses a newer version rather than misreading it.
const CheckpointVersion = 1

// Step is a sub-step of a self-managed create that must not run twice, or
// whose completion decides where the cluster's objects live. A resumed run
// skips the steps its checkpoint records.
type Step string

const (
	// StepBootstrapAddonsRemoved: the bootstrap cluster's own provisioning
	// stack (its Tinkerbell) was uninstalled in the handover. Installing it
	// again on a resume would put a second DHCP responder next to the
	// workload cluster's.
	StepBootstrapAddonsRemoved Step = "bootstrap_addons_removed"

	// StepMoved: clusterctl move finished, so the cluster's CAPI objects live
	// on the cluster itself and the bootstrap cluster holds none. A resume
	// continues on the cluster; applying the objects to the bootstrap again
	// would start a second management plane on the same machines.
	StepMoved Step = "moved"

	// StepBootstrapDeleted: the bootstrap cluster was torn down after the
	// move. A Kairos bootstrap node is a cluster member from then on, so it
	// must never be powered off again.
	StepBootstrapDeleted Step = "bootstrap_deleted"
)

// BootstrapRecord identifies a bootstrap cluster across runs.
type BootstrapRecord struct {
	// Type is the bootstrapper: kind or kairos.
	Type string `json:"type,omitempty"`
	// Name is the bootstrap cluster's name.
	Name string `json:"name,omitempty"`
	// Host is the machine a kind bootstrap cluster runs on. It exists only
	// there, so a run elsewhere cannot reach it.
	Host string `json:"host,omitempty"`
}

// Checkpoint is the progress of a self-managed create that a later run needs
// and a practitioner does not: the provider keeps it in the resource's
// private state. The coarse phase stays in status.phase, because only a
// change to a visible attribute makes Terraform plan the resuming apply.
type Checkpoint struct {
	Version int `json:"v"`

	// Steps are the completed sub-steps (see Step).
	Steps []Step `json:"steps,omitempty"`

	// Bootstrap is the bootstrap cluster the create runs through.
	Bootstrap *BootstrapRecord `json:"bootstrap,omitempty"`

	// WorkloadKubeconfig reaches the cluster once management has moved to
	// it, until the root credential is captured into provider_secrets (and
	// for a cluster whose providers keep none). Sensitive; private state is
	// never shown in plans or `terraform show`, but it is in the state file.
	WorkloadKubeconfig string `json:"workload_kubeconfig,omitempty"`

	// Attempts counts the runs of this create, Started is the first one
	// (RFC 3339) and LastError the error that stopped the latest.
	Attempts  int    `json:"attempts,omitempty"`
	Started   string `json:"started,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// Done reports whether the checkpoint records a step. A nil checkpoint
// records none.
func (c *Checkpoint) Done(s Step) bool {
	return c != nil && slices.Contains(c.Steps, s)
}

// Mark records a step once.
func (c *Checkpoint) Mark(s Step) {
	if !c.Done(s) {
		c.Steps = append(c.Steps, s)
	}
}

// recordBootstrap names the bootstrap cluster, keeping what the caller
// already recorded about it (its type, the host it runs on).
func (c *Checkpoint) recordBootstrap(name string) {
	if c.Bootstrap == nil {
		c.Bootstrap = &BootstrapRecord{}
	}
	c.Bootstrap.Name = name
}

// DeleteCheckpoint is the progress of a self-managed deletion, kept in
// private state when a destroy fails so the next one starts where it
// stopped.
type DeleteCheckpoint struct {
	Version int `json:"v"`

	// MovedTo is the bootstrap cluster management was moved to for the
	// deletion. From then on it holds the only copy of the cluster's
	// objects: if it is gone, the machines may still be provisioned.
	MovedTo *BootstrapRecord `json:"moved_to,omitempty"`
}
