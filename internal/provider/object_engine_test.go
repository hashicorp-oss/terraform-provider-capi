// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"math/big"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGoFromAttrAndBack(t *testing.T) {
	obj, _ := types.ObjectValue(map[string]attr.Type{
		"replicas": types.NumberType, "name": types.StringType, "on": types.BoolType,
		"list":   types.TupleType{ElemTypes: []attr.Type{types.StringType, types.NumberType}},
		"nested": types.ObjectType{AttrTypes: map[string]attr.Type{"x": types.StringType, "none": types.StringType}},
	}, map[string]attr.Value{
		"replicas": types.NumberValue(big.NewFloat(3)), "name": types.StringValue("md-0"), "on": types.BoolValue(true),
		"list":   mustTuple(t, types.StringValue("a"), types.NumberValue(big.NewFloat(1.5))),
		"nested": mustObj(t, map[string]attr.Value{"x": types.StringValue("y"), "none": types.StringNull()}),
	})
	got, err := goFromAttr(types.DynamicValue(obj))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"replicas": int64(3), "name": "md-0", "on": true, "list": []any{"a", 1.5}, "nested": map[string]any{"x": "y"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("goFromAttr = %#v, want %#v", got, want)
	}

	back, err := attrFromGo(want)
	if err != nil {
		t.Fatal(err)
	}
	round, err := goFromAttr(back)
	if err != nil || !reflect.DeepEqual(round, want) {
		t.Errorf("round trip = %#v, %v", round, err)
	}

	if _, err := goFromAttr(types.StringUnknown()); err == nil {
		t.Error("unknown values cannot be rendered")
	}
	m, d := dynamicToMap(types.DynamicValue(types.StringValue("not an object")))
	if !d.HasError() || m != nil {
		t.Error("a scalar is not a spec")
	}
	if m, d := dynamicToMap(types.DynamicNull()); d.HasError() || m != nil {
		t.Error("null spec is nil")
	}
}

func mustTuple(t *testing.T, vals ...attr.Value) types.Tuple {
	t.Helper()
	elemTypes := make([]attr.Type, len(vals))
	for i, v := range vals {
		elemTypes[i] = v.Type(context.Background())
	}
	tv, d := types.TupleValue(elemTypes, vals)
	if d.HasError() {
		t.Fatal(d)
	}
	return tv
}

func mustObj(t *testing.T, attrs map[string]attr.Value) types.Object {
	t.Helper()
	attrTypes := map[string]attr.Type{}
	for k, v := range attrs {
		attrTypes[k] = v.Type(context.Background())
	}
	o, d := types.ObjectValue(attrTypes, attrs)
	if d.HasError() {
		t.Fatal(d)
	}
	return o
}

func TestSubsetDrift(t *testing.T) {
	desired := map[string]any{"replicas": int64(3), "template": map[string]any{"spec": map[string]any{"version": "v1.34.0"}}, "tags": []any{"a", "b"}}
	live := map[string]any{
		"replicas": float64(3), "clusterName": "prod", // a controller-added field is not drift
		"template": map[string]any{"spec": map[string]any{"version": "v1.34.0", "failureDomain": "x"}, "metadata": map[string]any{}},
		"tags":     []any{"a", "b"},
	}
	if !subsetEqual(desired, live) {
		t.Error("live carries every desired value")
	}
	live["replicas"] = int64(5)
	if subsetEqual(desired, live) {
		t.Error("a changed value is drift")
	}
	projected := projectSubset(desired, live)
	want := map[string]any{"replicas": int64(5), "template": map[string]any{"spec": map[string]any{"version": "v1.34.0"}}, "tags": []any{"a", "b"}}
	if !reflect.DeepEqual(projected, want) {
		t.Errorf("projected = %#v", projected)
	}
	delete(live, "tags")
	if subsetEqual(desired, live) {
		t.Error("a missing key is drift")
	}
	if _, ok := projectSubset(desired, live)["tags"]; ok {
		t.Error("a missing key is not projected")
	}
	if subsetEqual(map[string]any{"l": []any{map[string]any{"a": 1}}}, map[string]any{"l": []any{map[string]any{"a": 1, "b": 2}}}) {
		t.Error("list items are compared whole")
	}
}

func TestParseObjectImportID(t *testing.T) {
	for id, want := range map[string][3]string{
		"IPPool:capi/pool":  {"IPPool", "capi", "pool"},
		"ClusterClass:prod": {"ClusterClass", "", "prod"},
	} {
		kind, ns, name, ok := parseObjectImportID(id)
		if !ok || kind != want[0] || ns != want[1] || name != want[2] {
			t.Errorf("%q = %q %q %q %v", id, kind, ns, name, ok)
		}
	}
	for _, bad := range []string{"", "nokind", "Kind:", "Kind:ns/"} {
		if _, _, _, ok := parseObjectImportID(bad); ok {
			t.Errorf("%q must be invalid", bad)
		}
	}
}

func TestManifestYAMLAndNewObject(t *testing.T) {
	obj := newObject(gvkFor("cluster.x-k8s.io/v1beta2", "MachineDeployment"), "capi", "md-0", map[string]string{"a": "b"}, nil)
	if obj.GetAPIVersion() != "cluster.x-k8s.io/v1beta2" || obj.GetKind() != "MachineDeployment" || obj.GetLabels()["a"] != "b" || obj.GetAnnotations() != nil {
		t.Errorf("object = %v", obj.Object)
	}
	if y := manifestYAML(obj); y == "" || y[:11] != "apiVersion:" {
		t.Errorf("manifest = %q", y)
	}
	status := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"replicas": int64(2)}}}
	m, _, _ := unstructured.NestedMap(status.Object, "status")
	if d, diags := dynamicFromMap(m); diags.HasError() || d.IsNull() {
		t.Errorf("status dynamic = %v %v", d, diags)
	}
}
