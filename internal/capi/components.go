// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"sort"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
)

// ProviderComponents describes one provider release: where it comes from,
// the contract it implements and the kinds its CRDs serve, resolved against a
// core contract. It is what a configuration needs to render objects of that
// provider before any cluster exists.
type ProviderComponents struct {
	Name     string
	Type     ProviderType
	Version  string
	URL      string
	Contract string
	// Kinds is every CRD kind the release ships, keyed by kind name, at the
	// version the catalog would emit for it.
	Kinds map[string]schema.GroupVersionKind
	// Catalog resolves kinds by suffix the way the generator does.
	Catalog *KindCatalog
}

// FetchProviderComponents downloads the components of p (at its pinned
// version, else the repository's default) through clusterctl's repository
// client, so fetch_config and pinned versions resolve exactly as they do for
// init, and reads the CRDs out of them. coreContract is the core API version
// (v1beta2, say) the provider's kinds are resolved against; empty means the
// contract the release declares.
func FetchProviderComponents(
	ctx context.Context,
	configPath string,
	p ProviderConfig,
	coreContract string,
) (*ProviderComponents, error) {
	configClient, _, err := newProviderConfigClient(ctx, configPath, ProviderSet{p.Type: {p}})
	if err != nil {
		return nil, err
	}
	def, _ := lookupDefaultProvider(configClient, p)
	if def == nil {
		return nil, fmt.Errorf("provider %s/%s: %w", p.Type, p.Name, ErrUnknownProviderRepository)
	}
	repo, err := repository.New(ctx, def, configClient)
	if err != nil {
		return nil, fmt.Errorf("opening the repository of provider %s/%s: %w", p.Type, p.Name, err)
	}
	ver := p.Version
	if ver == "" {
		ver = repo.DefaultVersion()
	}
	comps, err := repo.Components().
		Get(ctx, repository.ComponentsOptions{Version: ver, SkipTemplateProcess: true})
	if err != nil {
		return nil, fmt.Errorf(
			"fetching components of provider %s/%s %s: %w",
			p.Type,
			p.Name,
			ver,
			err,
		)
	}
	out := &ProviderComponents{Name: p.Name, Type: p.Type, Version: comps.Version(), URL: def.URL()}

	if meta, err := repo.Metadata(comps.Version()).Get(ctx); err == nil {
		if parsed, err := version.ParseSemantic(comps.Version()); err == nil {
			if rs := meta.GetReleaseSeriesForVersion(parsed); rs != nil {
				out.Contract = rs.Contract
			}
		}
	}
	if coreContract == "" {
		coreContract = out.Contract
	}
	catalog, err := catalogFromObjects(comps.Objs(), coreContract)
	if err != nil {
		return nil, fmt.Errorf(
			"reading the CRDs of provider %s/%s %s: %w",
			p.Type,
			p.Name,
			ver,
			err,
		)
	}
	out.Catalog = catalog
	out.Kinds, err = catalog.KindsOf(ProviderKey{Type: p.Type, Name: p.Name})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// catalogFromObjects builds a catalog from the CRDs among rendered component
// objects. coreVersion stands in for the core Cluster CRD, which a non-core
// release does not ship.
func catalogFromObjects(
	objs []unstructured.Unstructured,
	coreVersion string,
) (*KindCatalog, error) {
	var crds []apiextv1.CustomResourceDefinition
	for i := range objs {
		if objs[i].GetKind() != "CustomResourceDefinition" {
			continue
		}
		var crd apiextv1.CustomResourceDefinition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(
			objs[i].Object,
			&crd,
		); err != nil {
			return nil, fmt.Errorf("decoding CRD %s: %w", objs[i].GetName(), err)
		}
		crds = append(crds, crd)
	}
	c := NewKindCatalog(crds)
	c.coreVersion = coreVersion
	return c, nil
}

// KindsOf returns every CRD kind of a provider at the version the catalog
// would emit for it, keyed by kind name.
func (c *KindCatalog) KindsOf(p ProviderKey) (map[string]schema.GroupVersionKind, error) {
	label := p.ManifestLabel()
	out := map[string]schema.GroupVersionKind{}
	for _, crd := range c.crds {
		if crd.Labels[providerLabel] != label {
			continue
		}
		ver, err := c.versionFor(crd)
		if err != nil {
			return nil, err
		}
		out[crd.Spec.Names.Kind] = schema.GroupVersionKind{
			Group:   crd.Spec.Group,
			Version: ver,
			Kind:    crd.Spec.Names.Kind,
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no CRDs labelled %s=%s", ErrKindNotFound, providerLabel, label)
	}
	return out, nil
}

// SortedKinds returns kind names in a stable order.
func SortedKinds(kinds map[string]schema.GroupVersionKind) []string {
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
