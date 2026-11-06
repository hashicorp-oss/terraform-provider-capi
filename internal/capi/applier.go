// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
				Err: fmt.Errorf(
					"%w: resolving resource type for %s: %v",
					ErrManifestApply,
					gvk,
					err,
				),
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
				Err: fmt.Errorf(
					"%w: marshaling %s/%s: %v",
					ErrManifestApply,
					gvk.Kind,
					obj.GetName(),
					err,
				),
			}
		}

		force := true
		_, err = resourceClient.Patch(
			ctx,
			obj.GetName(),
			k8stypes.ApplyPatchType,
			data,
			meta_v1.PatchOptions{
				FieldManager: a.fieldManager,
				Force:        &force,
			},
		)
		if err != nil {
			return &CAPIError{
				Operation: "apply-manifest",
				Cluster:   cluster.Name,
				Err: fmt.Errorf(
					"%w: applying %s/%s: %v",
					ErrManifestApply,
					gvk.Kind,
					obj.GetName(),
					err,
				),
			}
		}
	}

	return nil
}

// Delete deletes a CAPI cluster by name and namespace from the management
// cluster, at the core API version the cluster stores. Uses foreground
// cascading delete to ensure all owned resources are cleaned up. A cluster
// that does not exist returns ErrClusterNotFound; callers wait for the
// object to disappear with Waiter.WaitForClusterDeleted.
func (a *DynamicApplier) Delete(
	ctx context.Context,
	cluster *Cluster,
	clusterName, namespace string,
) error {
	const op = "delete-cluster"
	clusters, err := a.clusters(ctx, cluster, namespace)
	if err != nil {
		return &CAPIError{
			Operation: op,
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %v", ErrManifestDelete, err),
		}
	}

	// Delete the CAPI Cluster resource - CAPI controllers will cascade delete all owned resources.
	propagationPolicy := meta_v1.DeletePropagationForeground
	err = clusters.Delete(
		ctx,
		clusterName,
		meta_v1.DeleteOptions{PropagationPolicy: &propagationPolicy},
	)
	if apierrors.IsNotFound(err) {
		return &CAPIError{
			Operation: op,
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %s/%s", ErrClusterNotFound, namespace, clusterName),
		}
	}
	if err != nil {
		return &CAPIError{
			Operation: op,
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %v", ErrManifestDelete, err),
		}
	}
	return nil
}

// Get reads the core Cluster object. A cluster that does not exist returns
// ErrClusterNotFound; any other failure (unreachable management cluster,
// CAPI not installed) is reported as is, so a caller never mistakes an
// outage for a deleted cluster.
func (a *DynamicApplier) Get(
	ctx context.Context,
	cluster *Cluster,
	clusterName, namespace string,
) (*unstructured.Unstructured, error) {
	const op = "get-cluster"
	clusters, err := a.clusters(ctx, cluster, namespace)
	if err != nil {
		return nil, &CAPIError{Operation: op, Cluster: cluster.Name, Err: err}
	}
	obj, err := clusters.Get(ctx, clusterName, meta_v1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, &CAPIError{
			Operation: op,
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %s/%s", ErrClusterNotFound, namespace, clusterName),
		}
	}
	if err != nil {
		return nil, &CAPIError{Operation: op, Cluster: cluster.Name, Err: err}
	}
	return obj, nil
}

// Pause sets spec.paused on the Cluster, so CAPI creates no further Machines
// or Workflows while management is handed over. A cluster that does not
// exist returns ErrClusterNotFound.
func (a *DynamicApplier) Pause(
	ctx context.Context,
	cluster *Cluster,
	clusterName, namespace string,
) error {
	const op = "pause-cluster"
	clusters, err := a.clusters(ctx, cluster, namespace)
	if err != nil {
		return &CAPIError{Operation: op, Cluster: cluster.Name, Err: err}
	}
	patch := []byte(`{"spec":{"paused":true}}`)
	_, err = clusters.Patch(
		ctx,
		clusterName,
		k8stypes.MergePatchType,
		patch,
		meta_v1.PatchOptions{},
	)
	if apierrors.IsNotFound(err) {
		return &CAPIError{
			Operation: op,
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %s/%s", ErrClusterNotFound, namespace, clusterName),
		}
	}
	if err != nil {
		return &CAPIError{Operation: op, Cluster: cluster.Name, Err: err}
	}
	return nil
}

// ReadSecret implements SecretReader: the Secret's data, base64-decoded. A
// Secret that does not exist returns ErrClusterNotFound, so a caller can try
// the next candidate name.
func (a *DynamicApplier) ReadSecret(
	ctx context.Context,
	cluster *Cluster,
	namespace, name string,
) (map[string]string, error) {
	const op = "read-secret"
	dynClient, err := buildDynamicClient(cluster.KubeconfigPath)
	if err != nil {
		return nil, &CAPIError{Operation: op, Cluster: cluster.Name, Err: err}
	}
	if namespace == "" {
		namespace = "default"
	}
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	obj, err := dynClient.Resource(gvr).Namespace(namespace).Get(ctx, name, meta_v1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, &CAPIError{
			Operation: op,
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: secret %s/%s", ErrClusterNotFound, namespace, name),
		}
	}
	if err != nil {
		return nil, &CAPIError{Operation: op, Cluster: cluster.Name, Err: err}
	}
	data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
	out := make(map[string]string, len(data))
	for k, v := range data {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, &CAPIError{
				Operation: op,
				Cluster:   cluster.Name,
				Err:       fmt.Errorf("decoding %s/%s key %s: %w", namespace, name, k, err),
			}
		}
		out[k] = string(decoded)
	}
	return out, nil
}

// clusters returns the namespaced client for core Cluster objects at the
// version the management cluster stores.
func (a *DynamicApplier) clusters(
	ctx context.Context,
	cluster *Cluster,
	namespace string,
) (dynamic.ResourceInterface, error) {
	dynClient, err := buildDynamicClient(cluster.KubeconfigPath)
	if err != nil {
		return nil, err
	}
	coreVersion, err := coreClusterVersion(ctx, dynClient)
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		namespace = "default"
	}
	return dynClient.Resource(clustersGVR(coreVersion)).Namespace(namespace), nil
}
