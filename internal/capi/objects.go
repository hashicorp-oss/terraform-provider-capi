// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// ErrObjectNotFound is returned when an object, or the CRD that would serve
// it, does not exist.
var ErrObjectNotFound = errors.New("object not found")

// DefaultFieldManager is the server-side-apply field manager the provider's
// object resources use.
const DefaultFieldManager = "terraform-provider-capi"

// ObjectClient is the server-side-apply engine behind every per-object
// resource: apply under a field manager, get, list, delete, patch, wait for
// conditions or for deletion, and the kind catalog of the cluster.
type ObjectClient struct {
	cluster      *Cluster
	dyn          dynamic.Interface
	mapper       meta.RESTMapper
	resetMapper  func()
	fieldManager string
}

// NewObjectClient connects to a cluster's API with client-go.
func NewObjectClient(cluster *Cluster, fieldManager string) (*ObjectClient, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", cluster.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig of %s: %w", cluster.Name, err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client for %s: %w", cluster.Name, err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating discovery client for %s: %w", cluster.Name, err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))
	return NewObjectClientFor(cluster, dyn, mapper, mapper.Reset, fieldManager), nil
}

// NewObjectClientFor builds an ObjectClient on an existing dynamic client and
// REST mapper; reset, when set, invalidates the mapper's discovery cache so a
// CRD installed since the last call is found.
func NewObjectClientFor(
	cluster *Cluster,
	dyn dynamic.Interface,
	mapper meta.RESTMapper,
	reset func(),
	fieldManager string,
) *ObjectClient {
	if fieldManager == "" {
		fieldManager = DefaultFieldManager
	}
	return &ObjectClient{
		cluster:      cluster,
		dyn:          dyn,
		mapper:       mapper,
		resetMapper:  reset,
		fieldManager: fieldManager,
	}
}

// WithFieldManager returns a client sharing this one's connection that
// applies under another field manager, so a resource can own its part of an
// object (one topology entry of a Cluster) independently of the rest.
func (c *ObjectClient) WithFieldManager(fieldManager string) *ObjectClient {
	if fieldManager == "" {
		fieldManager = DefaultFieldManager
	}
	cp := *c
	cp.fieldManager = fieldManager
	return &cp
}

// Cluster is the cluster the client talks to.
func (c *ObjectClient) Cluster() *Cluster { return c.cluster }

// FieldManager is the server-side-apply manager name.
func (c *ObjectClient) FieldManager() string { return c.fieldManager }

// resource resolves a kind to its REST resource, namespaced when the kind
// is. A kind the mapper does not know is retried once after a discovery
// reset, then reported as ErrObjectNotFound: no CRD, no objects.
func (c *ObjectClient) resource(
	gvk schema.GroupVersionKind,
	namespace string,
) (dynamic.ResourceInterface, error) {
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil && c.resetMapper != nil {
		c.resetMapper()
		mapping, err = c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, fmt.Errorf(
				"%w: no resource serves %s on %s",
				ErrObjectNotFound,
				gvk,
				c.cluster.Name,
			)
		}
		return nil, fmt.Errorf("resolving %s on %s: %w", gvk, c.cluster.Name, err)
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if namespace == "" {
			namespace = "default"
		}
		return c.dyn.Resource(mapping.Resource).Namespace(namespace), nil
	}
	return c.dyn.Resource(mapping.Resource), nil
}

// Apply server-side-applies obj under the client's field manager and
// returns the live object. force takes ownership of fields another manager
// holds; without it a conflict is an error naming that manager.
func (c *ObjectClient) Apply(
	ctx context.Context,
	obj *unstructured.Unstructured,
	force bool,
) (*unstructured.Unstructured, error) {
	gvk := obj.GroupVersionKind()
	res, err := c.resource(gvk, obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	// status is a subresource and never part of an apply.
	toApply := obj.DeepCopy()
	delete(toApply.Object, "status")
	data, err := json.Marshal(toApply.Object)
	if err != nil {
		return nil, fmt.Errorf("encoding %s %s: %w", gvk.Kind, obj.GetName(), err)
	}
	live, err := res.Patch(
		ctx,
		obj.GetName(),
		k8stypes.ApplyPatchType,
		data,
		meta_v1.PatchOptions{FieldManager: c.fieldManager, Force: &force},
	)
	if err != nil {
		if apierrors.IsConflict(err) {
			return nil, fmt.Errorf(
				"applying %s %s/%s: another field manager owns fields this configuration sets; set force_conflicts to take them over: %w",
				gvk.Kind,
				obj.GetNamespace(),
				obj.GetName(),
				err,
			)
		}
		return nil, fmt.Errorf(
			"applying %s %s/%s: %w",
			gvk.Kind,
			obj.GetNamespace(),
			obj.GetName(),
			err,
		)
	}
	return live, nil
}

// Get reads one object. A missing object or kind returns ErrObjectNotFound.
func (c *ObjectClient) Get(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
) (*unstructured.Unstructured, error) {
	res, err := c.resource(gvk, namespace)
	if err != nil {
		return nil, err
	}
	obj, err := res.Get(ctx, name, meta_v1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s %s/%s", ErrObjectNotFound, gvk.Kind, namespace, name)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s %s/%s: %w", gvk.Kind, namespace, name, err)
	}
	return obj, nil
}

// List lists objects of a kind in a namespace (every namespace when empty
// for namespaced kinds is not supported; pass the namespace). A missing
// kind lists nothing.
func (c *ObjectClient) List(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, labelSelector string,
) ([]unstructured.Unstructured, error) {
	res, err := c.resource(gvk, namespace)
	if errors.Is(err, ErrObjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	list, err := res.List(ctx, meta_v1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %s in %s: %w", gvk.Kind, namespace, err)
	}
	return list.Items, nil
}

// Delete deletes one object with foreground propagation. A missing object
// or kind is not an error.
func (c *ObjectClient) Delete(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
) error {
	res, err := c.resource(gvk, namespace)
	if errors.Is(err, ErrObjectNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	policy := meta_v1.DeletePropagationForeground
	err = res.Delete(ctx, name, meta_v1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting %s %s/%s: %w", gvk.Kind, namespace, name, err)
	}
	return nil
}

// Patch applies a patch of the given type.
func (c *ObjectClient) Patch(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	pt k8stypes.PatchType,
	data []byte,
) (*unstructured.Unstructured, error) {
	res, err := c.resource(gvk, namespace)
	if err != nil {
		return nil, err
	}
	obj, err := res.Patch(ctx, name, pt, data, meta_v1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s %s/%s", ErrObjectNotFound, gvk.Kind, namespace, name)
	}
	if err != nil {
		return nil, fmt.Errorf("patching %s %s/%s: %w", gvk.Kind, namespace, name, err)
	}
	return obj, nil
}

// WaitForConditions polls until every named condition is True. A timeout
// reports each condition's current state and message, which is where a
// controller explains itself.
func (c *ObjectClient) WaitForConditions(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	conditions []string,
	opts WaitOptions,
) error {
	if len(conditions) == 0 {
		return nil
	}
	_, timeout, poll := waitDefaults(namespace, opts)
	deadline := time.Now().Add(timeout)
	var last *unstructured.Unstructured
	for {
		obj, err := c.Get(ctx, gvk, namespace, name)
		if err != nil && !errors.Is(err, ErrObjectNotFound) {
			return err
		}
		if obj != nil {
			last = obj
			if allTrue(obj, conditions) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"%w: %s %s/%s did not reach %v within %v: %s",
				ErrClusterNotReady,
				gvk.Kind,
				namespace,
				name,
				conditions,
				timeout,
				ConditionSummary(last, conditions),
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// WaitForDeletion polls until the object is gone.
func (c *ObjectClient) WaitForDeletion(
	ctx context.Context,
	gvk schema.GroupVersionKind,
	namespace, name string,
	opts WaitOptions,
) error {
	_, timeout, poll := waitDefaults(namespace, opts)
	deadline := time.Now().Add(timeout)
	for {
		obj, err := c.Get(ctx, gvk, namespace, name)
		if errors.Is(err, ErrObjectNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"%w: %s %s/%s still exists after %v (finalizers: %v)",
				ErrClusterNotDeleted,
				gvk.Kind,
				namespace,
				name,
				timeout,
				obj.GetFinalizers(),
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Catalog lists the provider CRDs installed on the cluster.
func (c *ObjectClient) Catalog(ctx context.Context) (*KindCatalog, error) {
	return NewCRDKindResolver().Kinds(ctx, c.cluster)
}

func allTrue(obj *unstructured.Unstructured, conditions []string) bool {
	for _, cond := range conditions {
		if !hasCondition(obj, cond, "True") {
			return false
		}
	}
	return true
}

// ConditionSummary renders the state of the named conditions on obj, with
// reason and message, for diagnostics.
func ConditionSummary(obj *unstructured.Unstructured, conditions []string) string {
	if obj == nil {
		return "object not found"
	}
	raw, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	byType := map[string]map[string]any{}
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			typ, _, _ := unstructured.NestedString(m, "type")
			byType[typ] = m
		}
	}
	var parts []string
	for _, want := range conditions {
		m, ok := byType[want]
		if !ok {
			parts = append(parts, want+"=absent")
			continue
		}
		status, _, _ := unstructured.NestedString(m, "status")
		reason, _, _ := unstructured.NestedString(m, "reason")
		message, _, _ := unstructured.NestedString(m, "message")
		s := want + "=" + status
		if reason != "" {
			s += " (" + reason + ")"
		}
		if message != "" {
			s += ": " + message
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}
