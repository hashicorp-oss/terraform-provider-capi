// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ObjectRef names an object by kind and name within one namespace.
type ObjectRef struct {
	Kind string
	Name string
}

// ReferenceIndex maps every referenced object to the objects referring to it
// ("Kind/name"), so a template can tell whether anything still uses it.
type ReferenceIndex map[ObjectRef][]string

// Referrers lists what references kind/name, sorted.
func (idx ReferenceIndex) Referrers(kind, name string) []string {
	out := append([]string(nil), idx[ObjectRef{Kind: kind, Name: name}]...)
	sort.Strings(out)
	return out
}

// TemplateReferences indexes every reference held by the objects that point
// at templates in a namespace: MachineDeployments and MachineSets (a
// MachineSet keeps the old template during a rollout), MachinePools,
// ClusterClasses, and the control planes of every control-plane provider
// the catalog knows. A core kind the cluster does not serve is skipped.
func TemplateReferences(
	ctx context.Context,
	c *ObjectClient,
	catalog *KindCatalog,
	namespace string,
) (ReferenceIndex, error) {
	idx := ReferenceIndex{}
	core := ProviderKey{Type: ProviderTypeCore, Name: "cluster-api"}
	var kinds []schema.GroupVersionKind
	for _, suffix := range []string{"MachineDeployment", "MachineSet", "MachinePool", "ClusterClass"} {
		gvk, err := catalog.Resolve(core, suffix)
		if errors.Is(err, ErrKindNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, gvk)
	}
	for _, p := range catalog.Providers(ProviderTypeControlPlane) {
		all, err := catalog.KindsOf(p)
		if err != nil {
			return nil, err
		}
		for _, gvk := range all {
			if strings.HasSuffix(gvk.Kind, "ControlPlane") {
				kinds = append(kinds, gvk)
			}
		}
	}
	for _, gvk := range kinds {
		objs, err := c.List(ctx, gvk, namespace, "")
		if err != nil {
			return nil, err
		}
		for i := range objs {
			from := gvk.Kind + "/" + objs[i].GetName()
			collectRefs(objs[i].Object["spec"], from, idx)
		}
	}
	return idx, nil
}

// collectRefs walks v and records every map that names a kind and a name:
// an infrastructureRef, a configRef, a templateRef, a ref.
func collectRefs(v any, from string, idx ReferenceIndex) {
	switch t := v.(type) {
	case map[string]any:
		kind, _ := t["kind"].(string)
		name, _ := t["name"].(string)
		if kind != "" && name != "" {
			ref := ObjectRef{Kind: kind, Name: name}
			idx[ref] = append(idx[ref], from)
		}
		for _, child := range t {
			collectRefs(child, from, idx)
		}
	case []any:
		for _, child := range t {
			collectRefs(child, from, idx)
		}
	}
}

// Providers lists the providers of a type the catalog's CRDs belong to,
// from clusterctl's provider label.
func (c *KindCatalog) Providers(typ ProviderType) []ProviderKey {
	seen := map[ProviderKey]bool{}
	var out []ProviderKey
	for _, crd := range c.crds {
		key, ok := providerKeyFromLabel(crd.Labels[providerLabel])
		if !ok || key.Type != typ || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// providerKeyFromLabel parses clusterctl's manifest label
// (<type>-<name>, "cluster-api" for core).
func providerKeyFromLabel(label string) (ProviderKey, bool) {
	if label == "cluster-api" {
		return ProviderKey{Type: ProviderTypeCore, Name: "cluster-api"}, true
	}
	for _, typ := range []ProviderType{ProviderTypeControlPlane, ProviderTypeInfrastructure, ProviderTypeBootstrap, ProviderTypeIPAM, ProviderTypeAddon} {
		prefix := string(typ) + "-"
		if strings.HasPrefix(label, prefix) && len(label) > len(prefix) {
			return ProviderKey{Type: typ, Name: strings.TrimPrefix(label, prefix)}, true
		}
	}
	return ProviderKey{}, false
}

// SpecHash is the short content hash objects are revisioned by.
func SpecHash(spec map[string]any) (string, error) {
	return specHash(spec)
}

// ByKind finds the GVK the catalog would emit for an exact kind name, across
// every provider; ambiguous kinds (two groups serve the same kind) are an
// error naming both.
func (c *KindCatalog) ByKind(kind string) (schema.GroupVersionKind, error) {
	var found []schema.GroupVersionKind
	for _, crd := range c.crds {
		if crd.Spec.Names.Kind != kind {
			continue
		}
		ver, err := c.versionFor(crd)
		if err != nil {
			return schema.GroupVersionKind{}, err
		}
		found = append(
			found,
			schema.GroupVersionKind{Group: crd.Spec.Group, Version: ver, Kind: kind},
		)
	}
	switch len(found) {
	case 0:
		return schema.GroupVersionKind{}, fmt.Errorf(
			"%w: no provider CRD serves kind %s",
			ErrKindNotFound,
			kind,
		)
	case 1:
		return found[0], nil
	}
	return schema.GroupVersionKind{}, fmt.Errorf(
		"kind %s is served by several groups %v: set api_version",
		kind,
		found,
	)
}

// ListTemplates returns the objects of a kind in a namespace carrying a
// label, by name.
func (c *ObjectClient) ListTemplates(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, labelSelector string,
) (map[string]unstructured.Unstructured, error) {
	objs, err := c.List(ctx, gvk, namespace, labelSelector)
	if err != nil {
		return nil, err
	}
	out := make(map[string]unstructured.Unstructured, len(objs))
	for i := range objs {
		out[objs[i].GetName()] = objs[i]
	}
	return out, nil
}
