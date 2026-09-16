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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"sigs.k8s.io/yaml"
)

const clusterNameLabel = "cluster.x-k8s.io/cluster-name"

// CatalogManifestGenerator generates cluster manifests from the provider set,
// resolving every provider kind and API version through a KindCatalog so no
// provider table is hardcoded.
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
	objs, err := b.direct()
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
	cp          *ProviderConfig
	bootstrap   *ProviderConfig
}

func newManifestBuilder(catalog *KindCatalog, opts GenerateOptions) (*manifestBuilder, error) {
	infra, ok := opts.Providers.Infrastructure()
	if !ok {
		return nil, fmt.Errorf("generating cluster %s: no infrastructure provider configured", opts.ClusterName)
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		return nil, err
	}
	b := &manifestBuilder{catalog: catalog, opts: opts, coreVersion: core, infra: infra}
	if cps := opts.Providers[ProviderTypeControlPlane]; len(cps) > 0 {
		b.cp = &cps[0]
	}
	if bss := opts.Providers[ProviderTypeBootstrap]; len(bss) > 0 {
		b.bootstrap = &bss[0]
	}
	return b, nil
}

// direct generates the Cluster, provider cluster, control plane, and one
// MachineDeployment (with its templates) per topology entry.
func (b *manifestBuilder) direct() ([]unstructured.Unstructured, error) {
	if b.cp == nil {
		return nil, fmt.Errorf("generating cluster %s: no control_plane provider configured (direct generation needs one, e.g. control_plane = { kubeadm = {} })", b.opts.ClusterName)
	}
	if len(b.opts.MachineDeployments) > 0 && b.bootstrap == nil {
		return nil, fmt.Errorf("generating cluster %s: no bootstrap provider configured (machine deployments need one, e.g. bootstrap = { kubeadm = {} })", b.opts.ClusterName)
	}

	clusterGVK, err := b.catalog.Resolve(ProviderKey{ProviderTypeCore, "cluster-api"}, "Cluster")
	if err != nil {
		return nil, err
	}
	infraClusterGVK, err := b.catalog.Resolve(b.infra.key(), "Cluster")
	if err != nil {
		return nil, err
	}
	infraMTGVK, err := b.catalog.Resolve(b.infra.key(), "MachineTemplate")
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

	name := b.opts.ClusterName
	cpName := name + "-control-plane"

	// Cluster
	cluster := b.newObject(clusterGVK, name)
	clusterSpec := b.clusterNetworkSpec()
	clusterSpec["infrastructureRef"] = b.ref(b.coreRefShape(), infraClusterGVK, name)
	clusterSpec["controlPlaneRef"] = b.ref(b.coreRefShape(), cpGVK, cpName)
	cluster.Object["spec"] = clusterSpec

	// <Infra>Cluster
	infraCluster := b.newObject(infraClusterGVK, name)
	infraCluster.Object["spec"] = buildSpec(providerDefaults(b.infra.key()), b.infra.ResourceConfig)

	// Control plane machine template + control plane
	cpMT, err := b.templateObject(infraMTGVK, cpName, b.opts.ControlPlane.Infrastructure)
	if err != nil {
		return nil, err
	}
	controlPlane := b.newObject(cpGVK, cpName)
	cpSpec := buildSpec(providerDefaults(b.cp.key()), b.cp.ResourceConfig)
	if b.opts.KubernetesVersion != "" {
		cpSpec["version"] = b.opts.KubernetesVersion
	}
	if b.opts.ControlPlane.Replicas != nil {
		cpSpec["replicas"] = *b.opts.ControlPlane.Replicas
	}
	mtRef := b.ref(cpRefShape, infraMTGVK, cpMT.GetName())
	if cpRefShape == RefShapeLegacy {
		setPath(cpSpec, mtRef, "machineTemplate", "infrastructureRef")
	} else {
		setPath(cpSpec, mtRef, "machineTemplate", "spec", "infrastructureRef")
	}
	controlPlane.Object["spec"] = cpSpec

	objs := []unstructured.Unstructured{cluster, infraCluster, cpMT, controlPlane}

	// Machine deployments
	if len(b.opts.MachineDeployments) > 0 {
		mdGVK, err := b.catalog.Resolve(ProviderKey{ProviderTypeCore, "cluster-api"}, "MachineDeployment")
		if err != nil {
			return nil, err
		}
		bsGVK, err := b.catalog.Resolve(b.bootstrap.key(), "ConfigTemplate")
		if err != nil {
			return nil, err
		}
		for _, md := range b.opts.MachineDeployments {
			mdName := name + "-" + md.Name
			mt, err := b.templateObject(infraMTGVK, mdName, md.Infrastructure)
			if err != nil {
				return nil, err
			}
			bsConfig := md.Bootstrap
			if bsConfig == nil {
				bsConfig = b.bootstrap.ResourceConfig
			}
			ct, err := b.templateObject(bsGVK, mdName, buildSpec(providerDefaults(b.bootstrap.key()), bsConfig))
			if err != nil {
				return nil, err
			}
			deployment := b.machineDeployment(mdGVK, md, mdName, bsGVK, ct.GetName(), infraMTGVK, mt.GetName())
			objs = append(objs, mt, ct, deployment)
		}
	}
	return objs, nil
}

// machineDeployment builds one MachineDeployment wired to its templates.
func (b *manifestBuilder) machineDeployment(gvk schema.GroupVersionKind, md MachineDeploymentTopology, mdName string, bsGVK schema.GroupVersionKind, configName string, mtGVK schema.GroupVersionKind, mtName string) unstructured.Unstructured {
	selector := map[string]any{
		clusterNameLabel:                   b.opts.ClusterName,
		"cluster.x-k8s.io/deployment-name": mdName,
	}
	labels := map[string]any{}
	for k, v := range md.Labels {
		labels[k] = v
	}
	for k, v := range selector {
		labels[k] = v
	}
	meta := map[string]any{"labels": labels}
	if len(md.Annotations) > 0 {
		annotations := map[string]any{}
		for k, v := range md.Annotations {
			annotations[k] = v
		}
		meta["annotations"] = annotations
	}
	machineSpec := map[string]any{
		"clusterName":       b.opts.ClusterName,
		"bootstrap":         map[string]any{"configRef": b.ref(b.coreRefShape(), bsGVK, configName)},
		"infrastructureRef": b.ref(b.coreRefShape(), mtGVK, mtName),
	}
	if b.opts.KubernetesVersion != "" {
		machineSpec["version"] = b.opts.KubernetesVersion
	}
	if md.FailureDomain != "" {
		machineSpec["failureDomain"] = md.FailureDomain
	}
	spec := map[string]any{
		"clusterName": b.opts.ClusterName,
		"selector":    map[string]any{"matchLabels": selector},
		"template":    map[string]any{"metadata": meta, "spec": machineSpec},
	}
	if md.Replicas != nil {
		spec["replicas"] = *md.Replicas
	}
	obj := b.newObject(gvk, mdName)
	obj.Object["spec"] = spec
	return obj
}

// templateObject builds an immutable template object (spec.template.spec =
// spec) named <base>-<hash of its spec>, so any change becomes a new object.
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
