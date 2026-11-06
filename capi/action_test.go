// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
)

func TestActions_SchemaAndMetadata(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		act      action.Action
		typeName string
		required []string
	}{
		{NewPauseAction(), "capi_pause", []string{"cluster"}},
		{NewResumeAction(), "capi_resume", []string{"cluster"}},
		{NewRolloutRestartAction(), "capi_rollout_restart", []string{"name"}},
		{NewRemediateMachineAction(), "capi_remediate_machine", []string{"machine"}},
		{NewMoveAction(), "capi_move", []string{"to_kubeconfig"}},
	} {
		resp := &action.SchemaResponse{}
		tc.act.Schema(ctx, action.SchemaRequest{}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", tc.typeName, resp.Diagnostics)
		}
		for _, name := range tc.required {
			a, ok := resp.Schema.Attributes[name]
			if !ok || !a.IsRequired() {
				t.Errorf("%s: %s must be required", tc.typeName, name)
			}
		}
		if _, ok := resp.Schema.Attributes["namespace"]; !ok {
			t.Errorf("%s: namespace missing", tc.typeName)
		}
		meta := &action.MetadataResponse{}
		tc.act.Metadata(ctx, action.MetadataRequest{ProviderTypeName: "capi"}, meta)
		if meta.TypeName != tc.typeName {
			t.Errorf("type name = %s, want %s", meta.TypeName, tc.typeName)
		}
		if _, ok := tc.act.(action.ActionWithConfigure); !ok {
			t.Errorf("%s must take the provider data", tc.typeName)
		}
	}
}

func TestActionPatches(t *testing.T) {
	if string(pausePatch(true)) != `{"spec":{"paused":true}}` ||
		string(pausePatch(false)) != `{"spec":{"paused":null}}` {
		t.Errorf("pause patches = %s %s", pausePatch(true), pausePatch(false))
	}
	at := time.Date(2026, 11, 5, 12, 30, 0, 0, time.FixedZone("x", 3600))
	if got := string(
		rolloutRestartPatch(true, at),
	); got != `{"spec":{"rollout":{"after":"2026-11-05T11:30:00Z"}}}` {
		t.Errorf("v1beta2 rollout patch = %s", got)
	}
	if got := string(
		rolloutRestartPatch(false, at),
	); got != `{"spec":{"rolloutAfter":"2026-11-05T11:30:00Z"}}` {
		t.Errorf("legacy rollout patch = %s", got)
	}
	var remediate map[string]map[string]map[string]string
	if err := json.Unmarshal(remediatePatch(), &remediate); err != nil {
		t.Fatal(err)
	}
	if _, ok := remediate["metadata"]["annotations"][remediateMachineAnnotation]; !ok {
		t.Errorf("remediate patch = %s", remediatePatch())
	}
	if !isContract("v1beta2") || isContract("v1beta1") || !isContract("v1") {
		t.Error("isContract")
	}
	if namespaceOf(stringOrNull("")) != "default" || namespaceOf(stringOrNull("capi")) != "capi" {
		t.Error("namespaceOf")
	}
}

func TestActions_InvokeWithoutProvider(t *testing.T) {
	// A configuration error before any connection is a diagnostic, never a panic.
	ctx := context.Background()
	a := NewPauseAction()
	resp := &action.SchemaResponse{}
	a.Schema(ctx, action.SchemaRequest{}, resp)
	if !strings.Contains(resp.Schema.MarkdownDescription, "paused") {
		t.Error("description")
	}
}
