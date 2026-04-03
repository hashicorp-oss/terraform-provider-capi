// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/api/meta"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// DynamicApplier applies and deletes Kubernetes manifests using the client-go
// dynamic client with server-side apply. This mirrors the pattern used by the
// hashicorp/kubernetes and kubectl providers instead of shelling out to kubectl.
type DynamicApplier struct {
	fieldManager string
}

// NewDynamicApplier creates a new DynamicApplier.
func NewDynamicApplier() *DynamicApplier {
	return &DynamicApplier{fieldManager: "terraform-provider-capi"}
}

// NewDynamicApplierWithFieldManager creates a DynamicApplier with a custom field manager name.
func NewDynamicApplierWithFieldManager(fieldManager string) *DynamicApplier {
	return &DynamicApplier{fieldManager: fieldManager}
}

// Apply applies a YAML manifest to the cluster using server-side apply.
// Multi-document YAML manifests are supported; each document is applied
// individually via the Kubernetes dynamic client.
func (a *DynamicApplier) Apply(ctx context.Context, cluster *Cluster, manifest []byte) error {
	config, err := clientcmd.BuildConfigFromFlags("", cluster.KubeconfigPath)
	if err != nil {
		return &CAPIError{
			Operation: "apply-manifest",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: loading kubeconfig: %v", ErrManifestApply, err),
		}
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return &CAPIError{
			Operation: "apply-manifest",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: creating dynamic client: %v", ErrManifestApply, err),
		}
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return &CAPIError{
			Operation: "apply-manifest",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: creating discovery client: %v", ErrManifestApply, err),
		}
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))

	// Parse and apply each document in the multi-document YAML
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return &CAPIError{
				Operation: "apply-manifest",
				Cluster:   cluster.Name,
				Err:       fmt.Errorf("%w: parsing manifest: %v", ErrManifestApply, err),
			}
		}

		if obj.Object == nil {
			continue
		}

		// Resolve GVK to GVR via REST mapper
		gvk := obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return &CAPIError{
				Operation: "apply-manifest",
				Cluster:   cluster.Name,
				Err:       fmt.Errorf("%w: resolving resource type for %s: %v", ErrManifestApply, gvk, err),
			}
		}

		// Build the resource client (namespaced or cluster-scoped)
		var resourceClient dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ns := obj.GetNamespace()
			if ns == "" {
				ns = "default"
			}
			resourceClient = dynClient.Resource(mapping.Resource).Namespace(ns)
		} else {
			resourceClient = dynClient.Resource(mapping.Resource)
		}

		// Server-side apply via Patch with ApplyPatchType
		data, err := json.Marshal(obj.Object)
		if err != nil {
			return &CAPIError{
				Operation: "apply-manifest",
				Cluster:   cluster.Name,
				Err:       fmt.Errorf("%w: marshaling %s/%s: %v", ErrManifestApply, gvk.Kind, obj.GetName(), err),
			}
		}

		force := true
		_, err = resourceClient.Patch(ctx, obj.GetName(), k8stypes.ApplyPatchType, data, meta_v1.PatchOptions{
			FieldManager: a.fieldManager,
			Force:        &force,
		})
		if err != nil {
			return &CAPIError{
				Operation: "apply-manifest",
				Cluster:   cluster.Name,
				Err:       fmt.Errorf("%w: applying %s/%s: %v", ErrManifestApply, gvk.Kind, obj.GetName(), err),
			}
		}
	}

	return nil
}

// Delete deletes a CAPI cluster by name and namespace from the management cluster.
// Uses foreground cascading delete to ensure all owned resources are cleaned up.
func (a *DynamicApplier) Delete(ctx context.Context, cluster *Cluster, clusterName, namespace string) error {
	config, err := clientcmd.BuildConfigFromFlags("", cluster.KubeconfigPath)
	if err != nil {
		return &CAPIError{
			Operation: "delete-cluster",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: loading kubeconfig: %v", ErrManifestDelete, err),
		}
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return &CAPIError{
			Operation: "delete-cluster",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: creating dynamic client: %v", ErrManifestDelete, err),
		}
	}

	if namespace == "" {
		namespace = "default"
	}

	// Delete the CAPI Cluster resource - CAPI controllers will cascade delete all owned resources.
	gvr := schema.GroupVersionResource{
		Group:    "cluster.x-k8s.io",
		Version:  "v1beta1",
		Resource: "clusters",
	}

	propagationPolicy := meta_v1.DeletePropagationForeground
	err = dynClient.Resource(gvr).Namespace(namespace).Delete(ctx, clusterName, meta_v1.DeleteOptions{
		PropagationPolicy: &propagationPolicy,
	})
	if err != nil {
		return &CAPIError{
			Operation: "delete-cluster",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %v", ErrManifestDelete, err),
		}
	}

	return nil
}
