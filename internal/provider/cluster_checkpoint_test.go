// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// fakePrivate is a resource's private state for tests (the framework's own
// type cannot be built outside it).
type fakePrivate map[string][]byte

func (f fakePrivate) GetKey(_ context.Context, key string) ([]byte, diag.Diagnostics) {
	return f[key], nil
}

func (f fakePrivate) SetKey(_ context.Context, key string, value []byte) diag.Diagnostics {
	if len(value) == 0 {
		delete(f, key)
	} else {
		f[key] = value
	}
	return nil
}

// onHost makes the run look as if it happens on host.
func onHost(t *testing.T, host string) {
	t.Helper()
	prev := hostname
	hostname = func() (string, error) { return host, nil }
	t.Cleanup(func() { hostname = prev })
}

func TestCheckpoint_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store := fakePrivate{}
	cp, d := loadCheckpoint(ctx, store)
	if d.HasError() || cp.Attempts != 0 || len(cp.Steps) != 0 {
		t.Fatalf("an absent checkpoint is empty: %+v %v", cp, d)
	}
	if keptCheckpoint(cp) != nil {
		t.Error("an empty checkpoint is not kept")
	}

	onHost(t, "runner-a")
	startAttempt(cp, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	cp.Mark(capi.StepMoved)
	cp.Bootstrap = &capi.BootstrapRecord{Name: "prod-bootstrap"}
	stampBootstrap(cp, "kind")
	cp.WorkloadKubeconfig = "kubeconfig"
	if d := saveCheckpoint(ctx, store, keptCheckpoint(cp)); d.HasError() {
		t.Fatal(d)
	}
	if !json.Valid(store[privateKeyCreate]) {
		t.Fatalf("private state must be JSON: %s", store[privateKeyCreate])
	}
	got, d := loadCheckpoint(ctx, store)
	if d.HasError() || !got.Done(capi.StepMoved) || got.Bootstrap.Host != "runner-a" || got.Bootstrap.Type != "kind" || got.Attempts != 1 || got.Started != "2026-11-05T10:00:00Z" || got.WorkloadKubeconfig != "kubeconfig" {
		t.Errorf("round trip = %+v", got)
	}

	// A later run does not re-stamp the machine a bootstrap was made on.
	onHost(t, "runner-b")
	stampBootstrap(got, "kind")
	if got.Bootstrap.Host != "runner-a" {
		t.Error("the bootstrap's host is where it was created")
	}

	if d := saveCheckpoint(ctx, store, nil); d.HasError() || len(store) != 0 {
		t.Errorf("a nil checkpoint removes the key: %v", store)
	}

	store[privateKeyCreate] = []byte(`{"v":99}`)
	if _, d := loadCheckpoint(ctx, store); !d.HasError() {
		t.Error("a checkpoint from a newer provider is refused")
	}
}

func TestDeleteCheckpoint_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store := fakePrivate{}
	if d := saveDeleteCheckpoint(ctx, store, &capi.DeleteCheckpoint{}); d.HasError() || len(store) != 0 {
		t.Error("nothing moved, nothing recorded")
	}
	if d := saveDeleteCheckpoint(ctx, store, &capi.DeleteCheckpoint{MovedTo: &capi.BootstrapRecord{Type: "kind", Name: "prod-bootstrap", Host: "runner-a"}}); d.HasError() {
		t.Fatal(d)
	}
	got, d := loadDeleteCheckpoint(ctx, store)
	if d.HasError() || got.MovedTo == nil || got.MovedTo.Host != "runner-a" {
		t.Errorf("round trip = %+v", got)
	}
}

func TestForeignKind(t *testing.T) {
	onHost(t, "runner-b")
	if err := foreignKind(nil, "x"); err != nil {
		t.Error("no record, no constraint")
	}
	if err := foreignKind(&capi.BootstrapRecord{Type: "kairos", Name: "nuc", Host: "runner-a"}, "x"); err != nil {
		t.Error("a Kairos node is reachable from anywhere")
	}
	if err := foreignKind(&capi.BootstrapRecord{Type: "kind", Name: "prod-bootstrap", Host: "runner-b"}, "x"); err != nil {
		t.Error("same machine")
	}
	err := foreignKind(&capi.BootstrapRecord{Type: "kind", Name: "prod-bootstrap", Host: "runner-a"}, "The create of prod")
	if err == nil || !strings.Contains(err.Error(), "runner-a") || !strings.Contains(err.Error(), "runner-b") {
		t.Errorf("err = %v", err)
	}
}

func TestOnItself(t *testing.T) {
	status := func(phase string) *StatusModel {
		return &StatusModel{Phase: types.StringValue(phase)}
	}
	for phase, want := range map[string]bool{"ready": true, "pivoted": true, "": true, "handed_over": false, "moved": false, "bootstrapping": false} {
		if got := onItself(status(phase), &capi.Checkpoint{}); got != want {
			t.Errorf("phase %q: on itself = %v", phase, got)
		}
	}
	if !onItself(status("handed_over"), &capi.Checkpoint{Steps: []capi.Step{capi.StepMoved}}) {
		t.Error("a recorded move puts the objects on the cluster whatever the phase says")
	}
	if !onItself(&StatusModel{Phase: types.StringNull()}, nil) || !pivotComplete(nil) {
		t.Error("no recorded phase is a finished create (imports, older states)")
	}
	if pivotComplete(status("moved")) {
		t.Error("moved still has the pivot's tail to run")
	}
}

func TestDerivedCredentialTTL(t *testing.T) {
	ctx := context.Background()
	data := &ClusterResourceModel{Wait: types.ObjectNull(waitAttrTypesForTest())}
	if got := derivedCredentialTTL(ctx, data); got != 2*time.Hour {
		t.Errorf("default = %s", got)
	}
	w, _ := types.ObjectValue(waitAttrTypesForTest(), map[string]attr.Value{"conditions": types.ListNull(types.StringType), "timeout": types.StringValue("90m")})
	data.Wait = w
	if got := derivedCredentialTTL(ctx, data); got != 150*time.Minute {
		t.Errorf("90m wait = %s", got)
	}
}

func waitAttrTypesForTest() map[string]attr.Type {
	return map[string]attr.Type{"conditions": types.ListType{ElemType: types.StringType}, "timeout": types.StringType}
}

func TestResumeSummary(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if resumeSummary(&capi.Checkpoint{}, now) != "" {
		t.Error("no runs, no summary")
	}
	got := resumeSummary(&capi.Checkpoint{Attempts: 2, Started: "2026-11-05T10:30:00Z"}, now)
	if !strings.Contains(got, "2 earlier run(s)") || !strings.Contains(got, "1h30m0s ago") {
		t.Errorf("summary = %q", got)
	}
}

// The private state reaches ModifyPlan through the protocol: a destroy of an
// unfinished create whose kind bootstrap lives on another machine stops at
// plan time.
func TestClusterResource_ModifyPlan_DestroyNeedsTheBootstrapHost(t *testing.T) {
	ctx := context.Background()
	schemaResp := &resource.SchemaResponse{}
	NewClusterResource().Schema(ctx, resource.SchemaRequest{}, schemaResp)
	typ := schemaResp.Schema.Type().TerraformType(ctx)

	r, ok := NewClusterResource().(*ClusterResource)
	if !ok {
		t.Fatal("NewClusterResource must return a *ClusterResource")
	}
	imported := &resource.ImportStateResponse{State: tfsdk.State{Schema: schemaResp.Schema, Raw: tftypes.NewValue(typ, nil)}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "tinkerbell/prod"}, imported)
	st := imported.State
	mgmt, _ := types.ObjectValue(managementAttrTypes(), map[string]attr.Value{
		"self_managed": types.BoolValue(true),
		"providers":    types.ObjectNull(providersAttrTypes()),
		"bootstrap":    types.ObjectNull(managementBootstrapAttrTypes()),
	})
	if d := st.SetAttribute(ctx, path.Root("management"), mgmt); d.HasError() {
		t.Fatal(d)
	}
	if d := st.SetAttribute(ctx, path.Root("status").AtName("phase"), "control_plane_initialized"); d.HasError() {
		t.Fatal(d)
	}
	if d := st.SetAttribute(ctx, path.Root("status").AtName("bootstrap_cluster"), "prod-bootstrap"); d.HasError() {
		t.Fatal(d)
	}
	prior, err := tfprotov6.NewDynamicValue(typ, st.Raw)
	if err != nil {
		t.Fatal(err)
	}
	null, _ := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, nil))
	cp, _ := json.Marshal(capi.Checkpoint{Version: 1, Bootstrap: &capi.BootstrapRecord{Type: "kind", Name: "prod-bootstrap", Host: "runner-a"}, Attempts: 1})
	private, _ := json.Marshal(map[string][]byte{privateKeyCreate: cp})

	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	plan := func() []*tfprotov6.Diagnostic {
		resp, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
			TypeName: "capi_cluster", PriorState: &prior, ProposedNewState: &null, Config: &null, PriorPrivate: private,
		})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Diagnostics
	}

	onHost(t, "runner-b")
	diags := plan()
	if len(diags) != 1 || diags[0].Severity != tfprotov6.DiagnosticSeverityError || !strings.Contains(diags[0].Detail, "runner-a") {
		t.Fatalf("diagnostics = %+v", diags)
	}

	onHost(t, "runner-a")
	if diags := plan(); len(diags) != 0 {
		t.Errorf("on the bootstrap's own machine the destroy plans: %+v", diags)
	}
}
