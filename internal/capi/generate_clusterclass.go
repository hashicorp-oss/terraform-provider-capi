// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// workerClass is one ClusterClass worker class and the deployments using it.
type workerClass struct {
	name           string
	infrastructure map[string]any
	bootstrap      map[string]any
	firstUser      string
}

// clusterClass generates the provider templates, a ClusterClass, and a
// Cluster whose spec.topology drives the CAPI topology controller.
func (b *manifestBuilder) clusterClass() ([]unstructured.Unstructured, error) {
	name := b.opts.ClusterName
	if b.coreRefShape() != RefShapeContract {
		return nil, fmt.Errorf("generating cluster %s: ClusterClass generation needs the v1beta2 core contract, but the management cluster stores %s", name, b.coreVersion)
	}
	if b.cp == nil {
		return nil, fmt.Errorf("generating cluster %s: no control_plane provider configured (a ClusterClass needs one)", name)
	}
	if len(b.opts.MachineDeployments) > 0 && b.bootstrap == nil {
		return nil, fmt.Errorf("generating cluster %s: no bootstrap provider configured (machine deployments need one)", name)
	}
	if b.opts.KubernetesVersion == "" {
		return nil, fmt.Errorf("generating cluster %s: kubernetes_version is required with cluster_class (Cluster.spec.topology.version)", name)
	}
	classes, err := b.workerClasses()
	if err != nil {
		return nil, err
	}

	clusterGVK, err := b.catalog.Resolve(ProviderKey{ProviderTypeCore, "cluster-api"}, "Cluster")
	if err != nil {
		return nil, err
	}
	ccGVK, err := b.catalog.Resolve(ProviderKey{ProviderTypeCore, "cluster-api"}, "ClusterClass")
	if err != nil {
		return nil, err
	}
	infraCTGVK, err := b.catalog.Resolve(b.infra.key(), "ClusterTemplate")
	if err != nil {
		return nil, err
	}
	infraMTGVK, err := b.catalog.Resolve(b.infra.key(), "MachineTemplate")
	if err != nil {
		return nil, err
	}
	cpTGVK, err := b.catalog.Resolve(b.cp.key(), "ControlPlaneTemplate")
	if err != nil {
		return nil, err
	}

	cpName := name + "-control-plane"
	infraCT, err := b.templateObject(infraCTGVK, name, buildSpec(providerDefaults(b.infra.key()), b.infra.ResourceConfig))
	if err != nil {
		return nil, err
	}
	cpT, err := b.templateObject(cpTGVK, cpName, buildSpec(providerDefaults(b.cp.key()), b.cp.ResourceConfig))
	if err != nil {
		return nil, err
	}
	cpMT, err := b.templateObject(infraMTGVK, cpName, b.opts.ControlPlane.Infrastructure)
	if err != nil {
		return nil, err
	}
	objs := []unstructured.Unstructured{infraCT, cpT, cpMT}

	var classEntries []any
	if len(classes) > 0 {
		bsGVK, err := b.catalog.Resolve(b.bootstrap.key(), "ConfigTemplate")
		if err != nil {
			return nil, err
		}
		for _, c := range classes {
			mt, err := b.templateObject(infraMTGVK, name+"-"+c.name, c.infrastructure)
			if err != nil {
				return nil, err
			}
			ct, err := b.templateObject(bsGVK, name+"-"+c.name, buildSpec(providerDefaults(b.bootstrap.key()), c.bootstrap))
			if err != nil {
				return nil, err
			}
			objs = append(objs, mt, ct)
			classEntries = append(classEntries, map[string]any{
				"class":          c.name,
				"bootstrap":      map[string]any{"templateRef": templateRef(bsGVK, ct.GetName())},
				"infrastructure": map[string]any{"templateRef": templateRef(infraMTGVK, mt.GetName())},
			})
		}
	}

	cc := b.newObject(ccGVK, name)
	ccSpec := map[string]any{
		"infrastructure": map[string]any{"templateRef": templateRef(infraCTGVK, infraCT.GetName())},
		"controlPlane": map[string]any{
			"templateRef":           templateRef(cpTGVK, cpT.GetName()),
			"machineInfrastructure": map[string]any{"templateRef": templateRef(infraMTGVK, cpMT.GetName())},
		},
	}
	if len(classEntries) > 0 {
		ccSpec["workers"] = map[string]any{"machineDeployments": classEntries}
	}
	cc.Object["spec"] = ccSpec

	cluster := b.newObject(clusterGVK, name)
	clusterSpec := b.clusterNetworkSpec()
	clusterSpec["topology"] = b.topology(name)
	cluster.Object["spec"] = clusterSpec

	return append(objs, cc, cluster), nil
}

// workerClasses groups machine deployments by class (default: the deployment
// name), in first-use order. Deployments sharing a class must agree on their
// infrastructure and bootstrap specs.
func (b *manifestBuilder) workerClasses() ([]workerClass, error) {
	var classes []workerClass
	index := map[string]int{}
	for _, md := range b.opts.MachineDeployments {
		className := md.Class
		if className == "" {
			className = md.Name
		}
		bootstrap := md.Bootstrap
		if bootstrap == nil && b.bootstrap != nil {
			bootstrap = b.bootstrap.ResourceConfig
		}
		if i, seen := index[className]; seen {
			if !reflect.DeepEqual(classes[i].infrastructure, md.Infrastructure) || !reflect.DeepEqual(classes[i].bootstrap, bootstrap) {
				return nil, fmt.Errorf("generating cluster %s: machine deployments %q and %q share class %q but differ in infrastructure or bootstrap",
					b.opts.ClusterName, classes[i].firstUser, md.Name, className)
			}
			continue
		}
		index[className] = len(classes)
		classes = append(classes, workerClass{name: className, infrastructure: md.Infrastructure, bootstrap: bootstrap, firstUser: md.Name})
	}
	return classes, nil
}

// topology renders Cluster.spec.topology from the control plane and machine
// deployment options.
func (b *manifestBuilder) topology(className string) map[string]any {
	topology := map[string]any{
		"classRef": map[string]any{"name": className},
		"version":  b.opts.KubernetesVersion,
	}
	if b.opts.ControlPlane.Replicas != nil {
		topology["controlPlane"] = map[string]any{"replicas": *b.opts.ControlPlane.Replicas}
	}
	var mds []any
	for _, md := range b.opts.MachineDeployments {
		class := md.Class
		if class == "" {
			class = md.Name
		}
		entry := map[string]any{"class": class, "name": md.Name}
		if md.Replicas != nil {
			entry["replicas"] = *md.Replicas
		}
		if md.FailureDomain != "" {
			entry["failureDomain"] = md.FailureDomain
		}
		meta := map[string]any{}
		if len(md.Labels) > 0 {
			meta["labels"] = toAnyMap(md.Labels)
		}
		if len(md.Annotations) > 0 {
			meta["annotations"] = toAnyMap(md.Annotations)
		}
		if len(meta) > 0 {
			entry["metadata"] = meta
		}
		mds = append(mds, entry)
	}
	if len(mds) > 0 {
		topology["workers"] = map[string]any{"machineDeployments": mds}
	}
	return topology
}

// templateRef is a ClusterClass template reference {apiVersion, kind, name}.
func templateRef(gvk schema.GroupVersionKind, name string) map[string]any {
	return map[string]any{"apiVersion": gvk.GroupVersion().String(), "kind": gvk.Kind, "name": name}
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
