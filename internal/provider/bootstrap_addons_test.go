// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func testAddonsModel(ctx context.Context, t *testing.T, prereqs []string, rels []HelmReleaseModel, manifests []string) BootstrapAddonsModel {
	t.Helper()
	list := func(v []string) types.List {
		if v == nil {
			return types.ListNull(types.StringType)
		}
		l, d := types.ListValueFrom(ctx, types.StringType, v)
		if d.HasError() {
			t.Fatal(d)
		}
		return l
	}
	helm := types.ListNull(types.ObjectType{AttrTypes: helmReleaseAttrTypes()})
	if rels != nil {
		helm = testHelmList(ctx, t, rels)
	}
	return BootstrapAddonsModel{Prerequisites: list(prereqs), Helm: helm, Manifests: list(manifests)}
}

func TestBuildCreateOptions_BootstrapAddons(t *testing.T) {
	ctx := context.Background()
	data := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) {
		bs.Addons = mustObject(t, ctx, bootstrapAddonsAttrTypes(), testAddonsModel(ctx, t,
			[]string{"kind: Namespace\n"},
			[]HelmReleaseModel{helmRel("tinkerbell", "tinkerbell", "oci://ghcr.io/tinkerbell/charts/tinkerbell", "15m", "hostNetwork: true\n")},
			[]string{"kind: ConfigMap\n"},
		))
	})

	opts, diags := buildCreateOptions(ctx, data)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	set := opts.BootstrapAddons
	if len(set.Prerequisites) != 1 || len(set.Helm) != 1 || len(set.Manifests) != 1 {
		t.Fatalf("bootstrap addons = %+v", set)
	}
	if set.Helm[0].Name != "tinkerbell" || set.Helm[0].Timeout.Minutes() != 15 || set.Helm[0].Values != "hostNetwork: true\n" {
		t.Errorf("helm release = %+v", set.Helm[0])
	}

	// A kind bootstrap without addons has an empty set.
	kind := tinkerbellTestData(ctx, t, buildTestBootstrap(ctx, t, "kind"), nil)
	opts, diags = buildCreateOptions(ctx, kind)
	if diags.HasError() || !opts.BootstrapAddons.Empty() {
		t.Errorf("no addons block, no set: %+v %v", opts.BootstrapAddons, diags)
	}
}

func TestValidateAddonsModel_RejectsBadEntries(t *testing.T) {
	ctx := context.Background()
	data := kairosData(ctx, t, true, func(bs *ManagementBootstrapModel, _ *KairosModel, _ *KairosSSHModel) {
		bs.Addons = mustObject(t, ctx, bootstrapAddonsAttrTypes(), testAddonsModel(ctx, t,
			[]string{"not: [valid"},
			[]HelmReleaseModel{helmRel("a", "ns", "c", "", ""), helmRel("a", "ns", "c", "", "")},
			nil,
		))
	})
	diags := runFullValidate(t, data)
	assertErrorContains(t, diags, "management.bootstrap.addons.helm[1] is a duplicate")
	assertErrorContains(t, diags, "management.bootstrap.addons.prerequisites[0] is not valid YAML")
}

// runFullValidate runs the resource's lifecycle validation.
func runFullValidate(t *testing.T, data *ClusterResourceModel) (diags diag.Diagnostics) {
	t.Helper()
	r := &ClusterResource{}
	r.validateLifecycleConfig(context.Background(), data, &diags)
	return diags
}
