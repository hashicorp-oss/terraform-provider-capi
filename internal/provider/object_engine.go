// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// Labels and annotations the provider stamps on objects it owns.
const (
	// templateLabel groups the revisions of one template resource.
	templateLabel = "capi.terraform.io/template"
	// revisionAnnotation carries a revision's content hash.
	revisionAnnotation = "capi.terraform.io/revision"
)

// objectClient connects to the management cluster the provider block names.
func (p *providerData) objectClient(ctx context.Context) (*capi.ObjectClient, diag.Diagnostics) {
	conn, diags := p.managementOrDiag(ctx)
	if diags.HasError() {
		return nil, diags
	}
	client, err := capi.NewObjectClient(conn.Cluster, capi.DefaultFieldManager)
	if err != nil {
		diags.AddError("Management cluster connection", err.Error())
		return nil, diags
	}
	return client, diags
}

// --- shared schema pieces ---

// ObjectWaitModel is the `wait` block of object resources.
type ObjectWaitModel struct {
	Conditions types.List   `tfsdk:"conditions"`
	Timeout    types.String `tfsdk:"timeout"`
}

func objectWaitAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"conditions": types.ListType{ElemType: types.StringType},
		"timeout":    types.StringType,
	}
}

// objectWaitAttribute is the `wait` block: conditions to wait for after an
// apply, and how long. defaults names the conditions used when the block
// is absent, for the description.
func objectWaitAttribute(defaults []string) schema.SingleNestedAttribute {
	desc := "What to wait for after the object is applied."
	if len(defaults) > 0 {
		desc += fmt.Sprintf(" Default conditions: %s.", strings.Join(defaults, ", "))
	} else {
		desc += " By default nothing is waited for."
	}
	return schema.SingleNestedAttribute{
		MarkdownDescription: desc,
		Optional:            true,
		Attributes: map[string]schema.Attribute{
			"conditions": schema.ListAttribute{
				MarkdownDescription: "Status conditions that must be `True`. An empty list waits for nothing.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"timeout": schema.StringAttribute{
				MarkdownDescription: "Go duration. Default `10m`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("10m"),
			},
		},
	}
}

// objectWaitFromModel reads the wait block; defaults apply when it is absent
// or names no conditions.
func objectWaitFromModel(ctx context.Context, obj types.Object, defaults []string) (conditions []string, timeout time.Duration, diags diag.Diagnostics) {
	conditions, timeout = defaults, 10*time.Minute
	if obj.IsNull() || obj.IsUnknown() {
		return conditions, timeout, diags
	}
	var w ObjectWaitModel
	diags.Append(obj.As(ctx, &w, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nil, 0, diags
	}
	if !w.Conditions.IsNull() && !w.Conditions.IsUnknown() {
		var list []string
		diags.Append(w.Conditions.ElementsAs(ctx, &list, false)...)
		conditions = list
	}
	if !w.Timeout.IsNull() && !w.Timeout.IsUnknown() && w.Timeout.ValueString() != "" {
		d, err := time.ParseDuration(w.Timeout.ValueString())
		if err != nil {
			diags.AddError("Invalid wait.timeout", fmt.Sprintf("%q is not a Go duration: %s.", w.Timeout.ValueString(), err))
			return nil, 0, diags
		}
		timeout = d
	}
	return conditions, timeout, diags
}

// metadataMapAttribute is a labels or annotations map.
func metadataMapAttribute(desc string) schema.MapAttribute {
	return schema.MapAttribute{MarkdownDescription: desc, ElementType: types.StringType, Optional: true}
}

// stringMap reads a map(string) attribute, nil when unset.
func stringMap(ctx context.Context, m types.Map) (map[string]string, diag.Diagnostics) {
	if m.IsNull() || m.IsUnknown() {
		return nil, nil
	}
	out := map[string]string{}
	diags := m.ElementsAs(ctx, &out, false)
	return out, diags
}

// --- dynamic values ---

// goFromAttr converts a framework value (what a dynamic `spec` holds) to the
// plain Go shape Kubernetes objects use. Nulls become nil; an unknown is an
// error, since nothing can be rendered from it.
func goFromAttr(v attr.Value) (any, error) {
	if v == nil || v.IsNull() {
		return nil, nil
	}
	if v.IsUnknown() {
		return nil, errors.New("value is not known yet")
	}
	switch t := v.(type) {
	case types.Dynamic:
		return goFromAttr(t.UnderlyingValue())
	case types.String:
		return t.ValueString(), nil
	case types.Bool:
		return t.ValueBool(), nil
	case types.Int64:
		return t.ValueInt64(), nil
	case types.Float64:
		return t.ValueFloat64(), nil
	case types.Number:
		f := t.ValueBigFloat()
		if f.IsInt() {
			i, _ := f.Int64()
			return i, nil
		}
		fl, _ := f.Float64()
		return fl, nil
	case types.Object:
		return goFromAttrMap(t.Attributes())
	case types.Map:
		return goFromAttrMap(t.Elements())
	case types.Tuple:
		return goFromAttrList(t.Elements())
	case types.List:
		return goFromAttrList(t.Elements())
	case types.Set:
		return goFromAttrList(t.Elements())
	}
	return nil, fmt.Errorf("unsupported value type %T", v)
}

func goFromAttrMap(in map[string]attr.Value) (map[string]any, error) {
	out := make(map[string]any, len(in))
	for k, v := range in {
		g, err := goFromAttr(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		if g == nil {
			continue
		}
		out[k] = g
	}
	return out, nil
}

func goFromAttrList(in []attr.Value) ([]any, error) {
	out := make([]any, 0, len(in))
	for i, v := range in {
		g, err := goFromAttr(v)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out = append(out, g)
	}
	return out, nil
}

// attrFromGo converts a plain Go value to a framework value: maps become
// objects, lists tuples, numbers numbers.
func attrFromGo(v any) (attr.Value, error) {
	switch t := v.(type) {
	case nil:
		return types.DynamicNull(), nil
	case string:
		return types.StringValue(t), nil
	case bool:
		return types.BoolValue(t), nil
	case int:
		return types.NumberValue(big.NewFloat(float64(t))), nil
	case int32:
		return types.NumberValue(big.NewFloat(float64(t))), nil
	case int64:
		return types.NumberValue(new(big.Float).SetInt64(t)), nil
	case float32:
		return types.NumberValue(big.NewFloat(float64(t))), nil
	case float64:
		return types.NumberValue(big.NewFloat(t)), nil
	case map[string]any:
		attrs := make(map[string]attr.Value, len(t))
		attrTypes := make(map[string]attr.Type, len(t))
		for k, child := range t {
			cv, err := attrFromGo(child)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			attrs[k] = cv
			attrTypes[k] = cv.Type(context.Background())
		}
		obj, d := types.ObjectValue(attrTypes, attrs)
		if d.HasError() {
			return nil, fmt.Errorf("building object: %v", d)
		}
		return obj, nil
	case []any:
		elems := make([]attr.Value, 0, len(t))
		elemTypes := make([]attr.Type, 0, len(t))
		for i, child := range t {
			cv, err := attrFromGo(child)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			elems = append(elems, cv)
			elemTypes = append(elemTypes, cv.Type(context.Background()))
		}
		tuple, d := types.TupleValue(elemTypes, elems)
		if d.HasError() {
			return nil, fmt.Errorf("building tuple: %v", d)
		}
		return tuple, nil
	}
	return nil, fmt.Errorf("unsupported value type %T", v)
}

// dynamicToMap reads a dynamic attribute as an object; nil when unset.
func dynamicToMap(d types.Dynamic) (map[string]any, diag.Diagnostics) {
	const where = "spec"
	var diags diag.Diagnostics
	if d.IsNull() || d.IsUnknown() || d.IsUnderlyingValueNull() || d.IsUnderlyingValueUnknown() {
		return nil, diags
	}
	v, err := goFromAttr(d)
	if err != nil {
		diags.AddError("Invalid "+where, fmt.Sprintf("%s: %s.", where, err))
		return nil, diags
	}
	m, ok := v.(map[string]any)
	if !ok {
		diags.AddError("Invalid "+where, fmt.Sprintf("%s must be an object with the CRD's camelCase keys.", where))
		return nil, diags
	}
	return m, diags
}

// dynamicFromMap builds a dynamic attribute value from an object.
func dynamicFromMap(m map[string]any) (types.Dynamic, diag.Diagnostics) {
	var diags diag.Diagnostics
	if m == nil {
		return types.DynamicNull(), diags
	}
	v, err := attrFromGo(m)
	if err != nil {
		diags.AddError("Reading object", err.Error())
		return types.DynamicNull(), diags
	}
	return types.DynamicValue(v), diags
}

// --- subset drift ---

// projectSubset returns live restricted to the keys desired sets, so a
// comparison covers only what the configuration owns: a field a controller
// adds is never drift, a field the configuration sets and the cluster
// changed is. Lists are compared whole: a controller rewriting a list the
// configuration set is drift.
func projectSubset(desired, live map[string]any) map[string]any {
	out := map[string]any{}
	for k, want := range desired {
		got, ok := live[k]
		if !ok {
			continue
		}
		wm, wantMap := want.(map[string]any)
		gm, gotMap := got.(map[string]any)
		if wantMap && gotMap {
			out[k] = projectSubset(wm, gm)
			continue
		}
		out[k] = got
	}
	return out
}

// subsetEqual reports whether live carries every value desired sets.
func subsetEqual(desired, live map[string]any) bool {
	for k, want := range desired {
		got, ok := live[k]
		if !ok {
			return false
		}
		wm, wantMap := want.(map[string]any)
		gm, gotMap := got.(map[string]any)
		if wantMap && gotMap {
			if !subsetEqual(wm, gm) {
				return false
			}
			continue
		}
		if !valuesEqual(want, got) {
			return false
		}
	}
	return true
}

// valuesEqual compares scalars and lists loosely across the numeric types
// JSON decoding produces.
func valuesEqual(a, b any) bool {
	if fa, ok := toFloat(a); ok {
		if fb, ok := toFloat(b); ok {
			return fa == fb
		}
		return false
	}
	la, aList := a.([]any)
	lb, bList := b.([]any)
	if aList && bList {
		if len(la) != len(lb) {
			return false
		}
		for i := range la {
			ma, aMap := la[i].(map[string]any)
			mb, bMap := lb[i].(map[string]any)
			if aMap && bMap {
				if !subsetEqual(ma, mb) || !subsetEqual(mb, ma) {
					return false
				}
				continue
			}
			if !valuesEqual(la[i], lb[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case float32:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}

// --- CRUD helpers ---

// applyAndWait server-side-applies obj and waits for conditions.
func applyAndWait(ctx context.Context, client *capi.ObjectClient, obj *unstructured.Unstructured, force bool, conditions []string, timeout time.Duration) (*unstructured.Unstructured, diag.Diagnostics) {
	var diags diag.Diagnostics
	live, err := client.Apply(ctx, obj, force)
	if err != nil {
		diags.AddError("Applying "+obj.GetKind(), err.Error())
		return nil, diags
	}
	if len(conditions) > 0 {
		if err := client.WaitForConditions(ctx, obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName(), conditions, capi.WaitOptions{Timeout: timeout}); err != nil {
			diags.AddError("Waiting for "+obj.GetKind(), err.Error())
			return live, diags
		}
		live, err = client.Get(ctx, obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())
		if err != nil {
			diags.AddError("Reading "+obj.GetKind(), err.Error())
			return nil, diags
		}
	}
	return live, diags
}

// readLive reads an object; found is false when it (or its kind) is gone.
func readLive(ctx context.Context, client *capi.ObjectClient, gvk k8sschema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	live, err := client.Get(ctx, gvk, namespace, name)
	if errors.Is(err, capi.ErrObjectNotFound) {
		return nil, false, diags
	}
	if err != nil {
		diags.AddError("Reading "+gvk.Kind, err.Error())
		return nil, false, diags
	}
	return live, true, diags
}

// deleteAndWait deletes an object and waits for it to disappear.
func deleteAndWait(ctx context.Context, client *capi.ObjectClient, gvk k8sschema.GroupVersionKind, namespace, name string, timeout time.Duration) diag.Diagnostics {
	var diags diag.Diagnostics
	if err := client.Delete(ctx, gvk, namespace, name); err != nil {
		diags.AddError("Deleting "+gvk.Kind, err.Error())
		return diags
	}
	if err := client.WaitForDeletion(ctx, gvk, namespace, name, capi.WaitOptions{Timeout: timeout}); err != nil {
		diags.AddError("Deleting "+gvk.Kind, err.Error())
	}
	return diags
}

// manifestYAML renders an object as YAML for the `manifest` attribute.
func manifestYAML(obj *unstructured.Unstructured) string {
	out, err := yaml.Marshal(obj.Object)
	if err != nil {
		return ""
	}
	return string(out)
}

// newObject builds an object of a kind with metadata.
func newObject(gvk k8sschema.GroupVersionKind, namespace, name string, labels, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetAPIVersion(gvk.GroupVersion().String())
	obj.SetKind(gvk.Kind)
	obj.SetName(name)
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	if len(labels) > 0 {
		obj.SetLabels(labels)
	}
	if len(annotations) > 0 {
		obj.SetAnnotations(annotations)
	}
	return obj
}

// parseObjectImportID splits "kind:namespace/name" or "kind:name".
func parseObjectImportID(id string) (kind, namespace, name string, ok bool) {
	kind, rest, found := strings.Cut(strings.TrimSpace(id), ":")
	if !found || kind == "" || rest == "" {
		return "", "", "", false
	}
	if ns, n, found := strings.Cut(rest, "/"); found {
		return kind, ns, n, n != ""
	}
	return kind, "", rest, true
}

// gvkFor builds a GVK from an apiVersion string and a kind.
func gvkFor(apiVersion, kind string) k8sschema.GroupVersionKind {
	gv, _ := k8sschema.ParseGroupVersion(apiVersion)
	return gv.WithKind(kind)
}
