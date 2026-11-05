// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/tools/clientcmd"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
)

// ErrKindNotFound reports that the management cluster holds no CRD for a
// provider kind the manifest generator needs.
var ErrKindNotFound = errors.New("no CRD found")

const (
	// providerLabel is the label clusterctl stamps on every provider object,
	// valued with clusterctl's manifest label (cluster-api, infrastructure-docker, ...).
	providerLabel = "cluster.x-k8s.io/provider"

	// contractLabelPrefix prefixes the CAPI contract labels a provider CRD
	// carries (cluster.x-k8s.io/v1beta2: v1beta1) naming the API version(s)
	// that implement a given core contract.
	contractLabelPrefix = "cluster.x-k8s.io/"

	coreClustersCRD = "clusters.cluster.x-k8s.io"
)

// RefShape describes how a control-plane kind references its machine
// template, which changed with the v1beta2 contract.
type RefShape string

const (
	// RefShapeContract is spec.machineTemplate.spec.infrastructureRef {apiGroup, kind, name}.
	RefShapeContract RefShape = "contract"
	// RefShapeLegacy is spec.machineTemplate.infrastructureRef {apiVersion, kind, name, namespace}.
	RefShapeLegacy RefShape = "legacy"
)

// ManifestLabel returns the clusterctl provider label value for this provider.
func (k ProviderKey) ManifestLabel() string {
	return clusterctlv1.ManifestLabel(k.Name, k.Type.Clusterctl())
}

// KindResolver produces the CRD catalog of a management cluster.
type KindResolver interface {
	Kinds(ctx context.Context, mgmt *Cluster) (*KindCatalog, error)
}

// CRDKindResolver lists the CAPI CRDs installed on a management cluster.
type CRDKindResolver struct{}

// NewCRDKindResolver returns a resolver backed by the apiextensions API.
func NewCRDKindResolver() *CRDKindResolver { return &CRDKindResolver{} }

// Kinds implements KindResolver.
func (r *CRDKindResolver) Kinds(ctx context.Context, mgmt *Cluster) (*KindCatalog, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", mgmt.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	cs, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating apiextensions client: %w", err)
	}
	list, err := cs.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{LabelSelector: providerLabel})
	if err != nil {
		return nil, fmt.Errorf("listing CRDs: %w", err)
	}
	return NewKindCatalog(list.Items), nil
}

// KindCatalog resolves provider kinds and API versions from the CRDs
// installed on a management cluster, so the generator never needs a
// hardcoded table of provider kinds.
type KindCatalog struct {
	crds []apiextv1.CustomResourceDefinition
	// coreVersion stands in for the core Cluster CRD's storage version when
	// the catalog was built from a single provider's release, which ships no
	// core CRD.
	coreVersion string
}

// NewKindCatalog builds a catalog from a CRD list.
func NewKindCatalog(crds []apiextv1.CustomResourceDefinition) *KindCatalog {
	return &KindCatalog{crds: crds}
}

// CoreVersion returns the storage version of the core Cluster CRD, which is
// the CAPI contract every provider is resolved against.
func (c *KindCatalog) CoreVersion() (string, error) {
	if c.coreVersion != "" {
		return c.coreVersion, nil
	}
	for _, crd := range c.crds {
		if crd.Name != coreClustersCRD {
			continue
		}
		if v := storageVersion(crd); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("%w: %s (is the core provider installed?)", ErrKindNotFound, coreClustersCRD)
}

// Resolve returns the GVK of the provider's CRD whose kind ends in suffix
// (Cluster, ClusterTemplate, MachineTemplate, ControlPlane, ControlPlaneTemplate,
// ConfigTemplate). Several matches resolve to the shortest kind when it is
// unique; otherwise the call fails naming every candidate.
func (c *KindCatalog) Resolve(p ProviderKey, suffix string) (schema.GroupVersionKind, error) {
	label := p.ManifestLabel()
	var matches []apiextv1.CustomResourceDefinition
	var seen []string
	for _, crd := range c.crds {
		if crd.Labels[providerLabel] != label {
			continue
		}
		seen = append(seen, crd.Spec.Names.Kind)
		if strings.HasSuffix(crd.Spec.Names.Kind, suffix) {
			matches = append(matches, crd)
		}
	}
	if len(matches) == 0 {
		sort.Strings(seen)
		return schema.GroupVersionKind{}, fmt.Errorf("%w: kind *%s for provider %s (CRDs labelled %s=%s: %v)",
			ErrKindNotFound, suffix, label, providerLabel, label, seen)
	}
	sort.Slice(matches, func(i, j int) bool {
		ki, kj := matches[i].Spec.Names.Kind, matches[j].Spec.Names.Kind
		if len(ki) != len(kj) {
			return len(ki) < len(kj)
		}
		return ki < kj
	})
	if len(matches) > 1 && len(matches[0].Spec.Names.Kind) == len(matches[1].Spec.Names.Kind) {
		kinds := make([]string, 0, len(matches))
		for _, m := range matches {
			kinds = append(kinds, m.Spec.Names.Kind)
		}
		return schema.GroupVersionKind{}, fmt.Errorf("kind *%s for provider %s is ambiguous: %v", suffix, label, kinds)
	}
	crd := matches[0]
	ver, err := c.versionFor(crd)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	return schema.GroupVersionKind{Group: crd.Spec.Group, Version: ver, Kind: crd.Spec.Names.Kind}, nil
}

// versionFor picks the API version to emit for a CRD: the served version the
// CRD's contract label names for the core contract in use (the newest when
// the label lists several), else the storage version. Core CRDs are the
// contract, so they always use their storage version.
func (c *KindCatalog) versionFor(crd apiextv1.CustomResourceDefinition) (string, error) {
	core, err := c.CoreVersion()
	if err != nil {
		return "", err
	}
	if crd.Labels[providerLabel] == clusterctlv1.ManifestLabel("cluster-api", clusterctlv1.CoreProviderType) {
		return storageVersion(crd), nil
	}
	served := map[string]bool{}
	for _, v := range crd.Spec.Versions {
		served[v.Name] = v.Served
	}
	best := ""
	for v := range strings.SplitSeq(crd.Labels[contractLabelPrefix+core], "_") {
		if v == "" || !served[v] {
			continue
		}
		if best == "" || version.CompareKubeAwareVersionStrings(v, best) > 0 {
			best = v
		}
	}
	if best != "" {
		return best, nil
	}
	if v := storageVersion(crd); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("CRD %s has no storage version", crd.Name)
}

// ControlPlaneRefShape inspects the control-plane CRD's OpenAPI schema for
// gvk to learn where and how it references its machine template.
func (c *KindCatalog) ControlPlaneRefShape(gvk schema.GroupVersionKind) (RefShape, error) {
	for _, crd := range c.crds {
		if crd.Spec.Group != gvk.Group || crd.Spec.Names.Kind != gvk.Kind {
			continue
		}
		for _, v := range crd.Spec.Versions {
			if v.Name != gvk.Version {
				continue
			}
			if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				return "", fmt.Errorf("CRD %s version %s has no schema to inspect", crd.Name, v.Name)
			}
			mt := v.Schema.OpenAPIV3Schema.Properties["spec"].Properties["machineTemplate"]
			if _, ok := mt.Properties["spec"].Properties["infrastructureRef"]; ok {
				return RefShapeContract, nil
			}
			if _, ok := mt.Properties["infrastructureRef"]; ok {
				return RefShapeLegacy, nil
			}
			return "", fmt.Errorf("CRD %s version %s: no machineTemplate infrastructureRef in schema", crd.Name, v.Name)
		}
		return "", fmt.Errorf("CRD %s does not serve version %s", crd.Name, gvk.Version)
	}
	return "", fmt.Errorf("%w: %s", ErrKindNotFound, gvk)
}

func storageVersion(crd apiextv1.CustomResourceDefinition) string {
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			return v.Name
		}
	}
	return ""
}
