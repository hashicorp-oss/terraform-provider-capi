// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// Private state keys of capi_cluster. The create checkpoint lives from the
// first run of a self-managed create until it reaches ready; the delete
// checkpoint only while a destroy has failed after moving the objects.
const (
	privateKeyCreate = "create"
	privateKeyDelete = "delete"
)

// privateReader and privateWriter are the private state methods the
// framework's request and response types share (their type is internal to
// the framework).
type privateReader interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

type privateWriter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// hostname names the machine Terraform runs on; a variable so tests can run
// "elsewhere".
var hostname = os.Hostname

func runnerHost() string {
	h, err := hostname()
	if err != nil {
		return ""
	}
	return h
}

// loadCheckpoint reads the create checkpoint, an empty one when there is
// none (a fresh create, or a resource created before checkpoints existed).
func loadCheckpoint(ctx context.Context, r privateReader) (*capi.Checkpoint, diag.Diagnostics) {
	cp := &capi.Checkpoint{Version: capi.CheckpointVersion}
	diags := loadPrivateJSON(ctx, r, privateKeyCreate, cp)
	if cp.Version > capi.CheckpointVersion {
		diags.AddError(
			"Unknown checkpoint",
			fmt.Sprintf(
				"The resource's create checkpoint is version %d, newer than this provider understands (%d). Use the provider version that wrote it.",
				cp.Version,
				capi.CheckpointVersion,
			),
		)
	}
	cp.Version = capi.CheckpointVersion
	return cp, diags
}

// saveCheckpoint writes the create checkpoint, or removes it when nil.
func saveCheckpoint(ctx context.Context, w privateWriter, cp *capi.Checkpoint) diag.Diagnostics {
	if cp == nil {
		return w.SetKey(ctx, privateKeyCreate, nil)
	}
	cp.Version = capi.CheckpointVersion
	return savePrivateJSON(ctx, w, privateKeyCreate, cp)
}

func loadDeleteCheckpoint(
	ctx context.Context,
	r privateReader,
) (*capi.DeleteCheckpoint, diag.Diagnostics) {
	cp := &capi.DeleteCheckpoint{Version: capi.CheckpointVersion}
	diags := loadPrivateJSON(ctx, r, privateKeyDelete, cp)
	if cp.Version > capi.CheckpointVersion {
		diags.AddError(
			"Unknown checkpoint",
			fmt.Sprintf(
				"The resource's delete checkpoint is version %d, newer than this provider understands (%d). Use the provider version that wrote it.",
				cp.Version,
				capi.CheckpointVersion,
			),
		)
	}
	cp.Version = capi.CheckpointVersion
	return cp, diags
}

func saveDeleteCheckpoint(
	ctx context.Context,
	w privateWriter,
	cp *capi.DeleteCheckpoint,
) diag.Diagnostics {
	if cp == nil || cp.MovedTo == nil {
		return w.SetKey(ctx, privateKeyDelete, nil)
	}
	cp.Version = capi.CheckpointVersion
	return savePrivateJSON(ctx, w, privateKeyDelete, cp)
}

func loadPrivateJSON(ctx context.Context, r privateReader, key string, into any) diag.Diagnostics {
	if r == nil {
		return nil
	}
	raw, diags := r.GetKey(ctx, key)
	if diags.HasError() || len(raw) == 0 {
		return diags
	}
	if err := json.Unmarshal(raw, into); err != nil {
		diags.AddError(
			"Unreadable checkpoint",
			fmt.Sprintf("The resource's private %q state is not valid: %s.", key, err),
		)
	}
	return diags
}

func savePrivateJSON(ctx context.Context, w privateWriter, key string, v any) diag.Diagnostics {
	var diags diag.Diagnostics
	raw, err := json.Marshal(v)
	if err != nil {
		diags.AddError(
			"Recording progress",
			fmt.Sprintf("Encoding the %s checkpoint: %s.", key, err),
		)
		return diags
	}
	return w.SetKey(ctx, key, raw)
}

// startAttempt counts a run of a create workflow.
func startAttempt(cp *capi.Checkpoint, now time.Time) {
	cp.Attempts++
	if cp.Started == "" {
		cp.Started = now.UTC().Format(time.RFC3339)
	}
	cp.LastError = ""
}

// stampBootstrap records how the bootstrap cluster the manager named was
// made: its type, and for kind the machine it runs on, the only one that can
// reach it.
func stampBootstrap(cp *capi.Checkpoint, bootstrapType string) {
	if cp.Bootstrap == nil || cp.Bootstrap.Type != "" {
		return
	}
	cp.Bootstrap.Type = bootstrapType
	if bootstrapType == "kind" {
		cp.Bootstrap.Host = runnerHost()
	}
}

// foreignKind reports a kind bootstrap cluster that lives on another machine
// than this run's, and the error to give: a kind cluster exists only on the
// machine that created it, and a run elsewhere would start a second
// management plane for the same machines.
func foreignKind(rec *capi.BootstrapRecord, what string) error {
	if rec == nil || rec.Type != "kind" || rec.Host == "" {
		return nil
	}
	here := runnerHost()
	if here == "" || here == rec.Host {
		return nil
	}
	return fmt.Errorf(
		"%s runs in kind cluster %s on host %s, and this run is on %s: a kind cluster only exists on the machine that created it, and starting over here would put a second management plane on the same machines. Run Terraform on %s; if that machine and its kind cluster are gone, release the machines by hand and remove the resource from state",
		what,
		rec.Name,
		rec.Host,
		here,
		rec.Host,
	)
}

// pivotComplete reports a self-managed cluster whose create got past the
// pivot's tail (root credential captured, bootstrap gone) or finished. A
// missing phase is a finished one, as Read and Delete have always read it:
// imported clusters, and states from before phases existed.
func pivotComplete(status *statusModel) bool {
	if status == nil || status.Phase.IsNull() || status.Phase.IsUnknown() {
		return true
	}
	switch capi.Phase(status.Phase.ValueString()) {
	case capi.PhaseReady, capi.PhasePivoted, "":
		return true
	}
	return false
}

// onItself reports whether a self-managed cluster's objects live on the
// cluster itself: the pivot completed, or the checkpoint records the move.
func onItself(status *statusModel, cp *capi.Checkpoint) bool {
	return cp.Done(capi.StepMoved) || pivotComplete(status)
}

// resumeSummary describes the earlier runs for the resume warning.
func resumeSummary(cp *capi.Checkpoint, now time.Time) string {
	if cp == nil || cp.Attempts == 0 {
		return ""
	}
	s := fmt.Sprintf(" %d earlier run(s)", cp.Attempts)
	if started, err := time.Parse(time.RFC3339, cp.Started); err == nil {
		s += fmt.Sprintf(", first started %s ago", now.Sub(started).Round(time.Minute))
	}
	return s + "."
}

// keptCheckpoint is the checkpoint to persist, nil (the key removed) when
// it records nothing.
func keptCheckpoint(cp *capi.Checkpoint) *capi.Checkpoint {
	if cp == nil ||
		(len(cp.Steps) == 0 && cp.Bootstrap == nil && cp.WorkloadKubeconfig == "" && cp.Attempts == 0 && cp.LastError == "") {
		return nil
	}
	return cp
}

// derivedCredentialTTL is how long a kubeconfig derived for one operation
// stays valid: what is left of the operation's timeout, plus an hour of
// margin. A client certificate cannot be renewed mid-operation.
func derivedCredentialTTL(ctx context.Context) time.Duration {
	return remaining(ctx, defaultClusterUpdateTimeout) + time.Hour
}
