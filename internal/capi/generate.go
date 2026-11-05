// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"sigs.k8s.io/yaml"
)

const clusterNameLabel = "cluster.x-k8s.io/cluster-name"

// CatalogManifestGenerator generates the cluster's own objects from the
// installed provider CRDs, resolving every kind and API version through a
// KindCatalog so no provider table is hardcoded. Templates, node groups,
// classes and addons are separate resources and are only referenced here.
type CatalogManifestGenerator struct {
	resolver KindResolver
}

// NewManifestGenerator returns a generator backed by the given resolver.
func NewManifestGenerator(resolver KindResolver) *CatalogManifestGenerator {
	return &CatalogManifestGenerator{resolver: resolver}
}

// Generate implements ManifestGenerator.
func (g *CatalogManifestGenerator) Generate(ctx context.Context, mgmt *Cluster, opts GenerateOptions) ([]byte, error) {
	catalog, err := g.resolver.Kinds(ctx, mgmt)
	if err != nil {
		return nil, fmt.Errorf("discovering provider kinds: %w", err)
	}
	b, err := newManifestBuilder(catalog, opts)
	if err != nil {
		return nil, err
	}
	var objs []unstructured.Unstructured
	if opts.Class != nil {
		objs, err = b.topology()
	} else {
		objs, err = b.direct()
	}
	if err != nil {
		return nil, err
	}
	return renderManifest(objs)
}

// manifestBuilder holds the resolved providers shared by both generation modes.
type manifestBuilder struct {
	catalog     *KindCatalog
	opts        GenerateOptions
	coreVersion string
	infra       ProviderConfig
	cp          ProviderConfig
}

// defaultProviderName is what clusterctl init installs when a control-plane
// provider is not configured, so generation defaults the same way.
const defaultProviderName = "kubeadm"

func newManifestBuilder(catalog *KindCatalog, opts GenerateOptions) (*manifestBuilder, error) {
	core, err := catalog.CoreVersion()
	if err != nil {
		return nil, err
	}
	b := &manifestBuilder{catalog: catalog, opts: opts, coreVersion: core}
	if opts.Class != nil {
		return b, nil
	}
	b.infra = providerNamed(opts.Providers, ProviderTypeInfrastructure, opts.Infrastructure.Provider)
	if b.infra.Name == "" {
		return nil, fmt.Errorf("generating cluster %s: no infrastructure provider: set infrastructure.provider, or a class", opts.ClusterName)
	}
	b.cp = providerNamed(opts.Providers, ProviderTypeControlPlane, opts.ControlPlane.Provider)
	if b.cp.Name == "" {
		b.cp = ProviderConfig{Type: ProviderTypeControlPlane, Name: defaultProviderName}
	}
	return b, nil
}

// providerNamed returns the configured entry of a type by name (so its
// ResourceConfig applies), a bare entry of that name when it is not in the
// set, or the first entry of the type when no name is given.
func providerNamed(providers ProviderSet, typ ProviderType, name string) ProviderConfig {
	for _, p := range providers[typ] {
		if name == "" || strings.EqualFold(p.Name, name) {
			return p
		}
	}
	if name == "" {
		return ProviderConfig{}
	}
	return ProviderConfig{Type: typ, Name: name}
}

// direct generates the Cluster, the <Infra>Cluster and the <CP>ControlPlane
// wired to the control-plane machine template the caller references.
func (b *manifestBuilder) direct() ([]unstructured.Unstructured, error) {
	name := b.opts.ClusterName
	hasRef := b.opts.ControlPlane.Template != nil && b.opts.ControlPlane.Template.Name != ""
	if !hasRef && b.opts.ControlPlane.TemplateSpec == nil {
		return nil, fmt.Errorf("generating cluster %s: control_plane.infrastructure (a capi_machine_template ref) or control_plane.machine_spec is required without a class", name)
	}
	clusterGVK, err := b.catalog.Resolve(ProviderKey{ProviderTypeCore, "cluster-api"}, "Cluster")
	if err != nil {
		return nil, err
	}
	infraClusterGVK, err := b.catalog.Resolve(b.infra.key(), "Cluster")
	if err != nil {
		return nil, err
	}
	cpGVK, err := b.catalog.Resolve(b.cp.key(), "ControlPlane")
	if err != nil {
		return nil, err
	}
	cpRefShape, err := b.catalog.ControlPlaneRefShape(cpGVK)
	if err != nil {
		return nil, err
	}

	cpName := name + "-control-plane"

	// Cluster
	cluster := b.newObject(clusterGVK, name)
	clusterSpec := b.clusterNetworkSpec()
	clusterSpec["infrastructureRef"] = b.ref(b.coreRefShape(), infraClusterGVK, name)
	clusterSpec["controlPlaneRef"] = b.ref(b.coreRefShape(), cpGVK, cpName)
	cluster.Object["spec"] = clusterSpec

	// <Infra>Cluster: provider defaults, the provider entry's settings, then
	// the cluster's own infrastructure spec.
	infraCluster := b.newObject(infraClusterGVK, name)
	infraCluster.Object["spec"] = buildSpec(buildSpec(providerDefaults(b.infra.key()), b.infra.ResourceConfig), b.opts.Infrastructure.Spec)

	// <CP>ControlPlane, on the referenced machine template.
	controlPlane := b.newObject(cpGVK, cpName)
	cpSpec := buildSpec(buildSpec(providerDefaults(b.cp.key()), b.cp.ResourceConfig), b.opts.ControlPlane.Spec)
	if b.opts.KubernetesVersion != "" {
		cpSpec["version"] = b.opts.KubernetesVersion
	}
	if b.opts.ControlPlane.Replicas != nil {
		cpSpec["replicas"] = *b.opts.ControlPlane.Replicas
	}
	objs := []unstructured.Unstructured{cluster, infraCluster}
	var mtGVK schema.GroupVersionKind
	var mtName string
	if hasRef {
		tmpl := b.opts.ControlPlane.Template
		mtGVK = schema.FromAPIVersionAndKind(tmpl.APIVersion, tmpl.Kind)
		mtName = tmpl.Name
	} else {
		// The template is the cluster's own: hash-named, so a spec change
		// is a new object and a control-plane rollout.
		infraMTGVK, err := b.catalog.Resolve(b.infra.key(), "MachineTemplate")
		if err != nil {
			return nil, err
		}
		cpMT, err := b.templateObject(infraMTGVK, cpName, b.opts.ControlPlane.TemplateSpec)
		if err != nil {
			return nil, err
		}
		objs = append(objs, cpMT)
		mtGVK, mtName = infraMTGVK, cpMT.GetName()
	}
	mtRef := b.ref(cpRefShape, mtGVK, mtName)
	if cpRefShape == RefShapeLegacy {
		setPath(cpSpec, mtRef, "machineTemplate", "infrastructureRef")
	} else {
		setPath(cpSpec, mtRef, "machineTemplate", "spec", "infrastructureRef")
	}
	controlPlane.Object["spec"] = cpSpec

	return append(objs, controlPlane), nil
}

// templateObject renders an immutable template named <base>-<hash of spec>.
func (b *manifestBuilder) templateObject(gvk schema.GroupVersionKind, base string, spec map[string]any) (unstructured.Unstructured, error) {
	if spec == nil {
		spec = map[string]any{}
	}
	full := map[string]any{"template": map[string]any{"spec": spec}}
	h, err := specHash(full)
	if err != nil {
		return unstructured.Unstructured{}, fmt.Errorf("hashing %s %s spec: %w", gvk.Kind, base, err)
	}
	obj := b.newObject(gvk, base+"-"+h)
	obj.Object["spec"] = full
	return obj, nil
}

// topology generates a Cluster whose spec.topology refers to an existing
// ClusterClass: v1beta2 classRef, or the legacy class and classNamespace.
func (b *manifestBuilder) topology() ([]unstructured.Unstructured, error) {
	name := b.opts.ClusterName
	if b.opts.KubernetesVersion == "" {
		return nil, fmt.Errorf("generating cluster %s: kubernetes_version is required with a class (Cluster.spec.topology.version)", name)
	}
	clusterGVK, err := b.catalog.Resolve(ProviderKey{ProviderTypeCore, "cluster-api"}, "Cluster")
	if err != nil {
		return nil, err
	}
	class := b.opts.Class
	topology := map[string]any{"version": b.opts.KubernetesVersion}
	if b.coreRefShape() == RefShapeContract {
		ref := map[string]any{"name": class.Name}
		if class.Namespace != "" && class.Namespace != b.opts.Namespace {
			ref["namespace"] = class.Namespace
		}
		topology["classRef"] = ref
	} else {
		topology["class"] = class.Name
		if class.Namespace != "" && class.Namespace != b.opts.Namespace {
			topology["classNamespace"] = class.Namespace
		}
	}
	if b.opts.ControlPlane.Replicas != nil {
		topology["controlPlane"] = map[string]any{"replicas": *b.opts.ControlPlane.Replicas}
	}
	cluster := b.newObject(clusterGVK, name)
	clusterSpec := b.clusterNetworkSpec()
	clusterSpec["topology"] = topology
	cluster.Object["spec"] = clusterSpec
	return []unstructured.Unstructured{cluster}, nil
}

// clusterNetworkSpec renders Cluster.spec.clusterNetwork and
// controlPlaneEndpoint from the network options (port defaults to 6443).
func (b *manifestBuilder) clusterNetworkSpec() map[string]any {
	spec := map[string]any{}
	net := b.opts.Network
	network := map[string]any{}
	if len(net.PodCIDRs) > 0 {
		network["pods"] = map[string]any{"cidrBlocks": toAnySlice(net.PodCIDRs)}
	}
	if len(net.ServiceCIDRs) > 0 {
		network["services"] = map[string]any{"cidrBlocks": toAnySlice(net.ServiceCIDRs)}
	}
	if net.ServiceDomain != "" {
		network["serviceDomain"] = net.ServiceDomain
	}
	if len(network) > 0 {
		spec["clusterNetwork"] = network
	}
	if ep := net.ControlPlaneEndpoint; ep != nil && ep.Host != "" {
		port := int64(ep.Port)
		if port == 0 {
			port = 6443
		}
		spec["controlPlaneEndpoint"] = map[string]any{"host": ep.Host, "port": port}
	}
	return spec
}

// coreRefShape is how core objects (Cluster, MachineDeployment) reference
// provider objects: contract refs from the v1beta2 contract on.
func (b *manifestBuilder) coreRefShape() RefShape {
	if version.CompareKubeAwareVersionStrings(b.coreVersion, "v1beta2") >= 0 {
		return RefShapeContract
	}
	return RefShapeLegacy
}

// ref renders an object reference in the requested shape.
func (b *manifestBuilder) ref(shape RefShape, gvk schema.GroupVersionKind, name string) map[string]any {
	if shape == RefShapeLegacy {
		return map[string]any{
			"apiVersion": gvk.GroupVersion().String(),
			"kind":       gvk.Kind,
			"name":       name,
			"namespace":  b.opts.Namespace,
		}
	}
	return map[string]any{"apiGroup": gvk.Group, "kind": gvk.Kind, "name": name}
}

// newObject returns an object of the given kind with the cluster-name label.
func (b *manifestBuilder) newObject(gvk schema.GroupVersionKind, name string) unstructured.Unstructured {
	obj := unstructured.Unstructured{Object: map[string]any{}}
	obj.SetAPIVersion(gvk.GroupVersion().String())
	obj.SetKind(gvk.Kind)
	obj.SetName(name)
	obj.SetNamespace(b.opts.Namespace)
	obj.SetLabels(map[string]string{clusterNameLabel: b.opts.ClusterName})
	return obj
}

func (p ProviderConfig) key() ProviderKey { return ProviderKey{Type: p.Type, Name: p.Name} }

// specHash is the first 8 hex characters of SHA-256 over the canonical
// (sorted-key, compact) JSON of spec.
func specHash(spec map[string]any) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:8], nil
}

// setPath sets value at the nested key path, creating intermediate maps.
func setPath(m map[string]any, value any, path ...string) {
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[path[len(path)-1]] = value
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// renderManifest serializes objects as a multi-document YAML stream.
func renderManifest(objs []unstructured.Unstructured) ([]byte, error) {
	var buf bytes.Buffer
	for _, obj := range objs {
		data, err := yaml.Marshal(obj.Object)
		if err != nil {
			return nil, fmt.Errorf("rendering %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
		buf.WriteString("---\n")
		buf.Write(data)
	}
	return buf.Bytes(), nil
}

// buildSpec layers resourceConfig over defaults with RFC 7396 merge-patch
// semantics: nested maps merge, lists and scalars replace, a nil value
// deletes the key. Neither argument is modified; the result is always a
// non-nil map so callers can set wiring fields on it.
func buildSpec(defaults, resourceConfig map[string]any) map[string]any {
	out := mergeMaps(map[string]any{}, defaults)
	return mergeMaps(out, resourceConfig)
}

// mergeMaps applies patch to dst in place (RFC 7396) and returns dst.
func mergeMaps(dst, patch map[string]any) map[string]any {
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			base, _ := dst[k].(map[string]any)
			dst[k] = mergeMaps(mergeMaps(map[string]any{}, base), pv)
		case []any:
			dst[k] = copyList(pv)
		default:
			dst[k] = v
		}
	}
	return dst
}

func copyList(in []any) []any {
	out := make([]any, len(in))
	for i, v := range in {
		switch tv := v.(type) {
		case map[string]any:
			out[i] = mergeMaps(map[string]any{}, tv)
		case []any:
			out[i] = copyList(tv)
		default:
			out[i] = v
		}
	}
	return out
}

// providerDefaults returns the required-field minimums for providers this
// provider knows, so a bare entry still renders a valid object. resource_config
// merges over them. Unknown providers get nothing.
func providerDefaults(key ProviderKey) map[string]any {
	switch key {
	case ProviderKey{ProviderTypeControlPlane, "talos"}:
		return map[string]any{
			"controlPlaneConfig": map[string]any{
				"controlplane": map[string]any{"generateType": "controlplane"},
			},
		}
	case ProviderKey{ProviderTypeBootstrap, "talos"}:
		return map[string]any{"generateType": "worker"}
	case ProviderKey{ProviderTypeControlPlane, "kubeadm"}:
		return map[string]any{"kubeadmConfigSpec": map[string]any{}}
	}
	return nil
}
