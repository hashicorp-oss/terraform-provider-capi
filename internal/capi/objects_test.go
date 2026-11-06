// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// testKinds are the kinds the fake cluster serves, with their resources.
var testKinds = map[schema.GroupVersionKind]string{
	{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta1", Kind: "TinkerbellMachineTemplate"}: "tinkerbellmachinetemplates",
	{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineDeployment"}:                        "machinedeployments",
	{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineSet"}:                               "machinesets",
	{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "ClusterClass"}:                             "clusterclasses",
	{Group: "controlplane.cluster.x-k8s.io", Version: "v1beta1", Kind: "TalosControlPlane"}:           "taloscontrolplanes",
	{Group: "", Version: "v1", Kind: "Secret"}:                                                        "secrets",
}

// newFakeObjectClient builds an ObjectClient on client-go's fake dynamic
// client, with server-side apply emulated by a reactor: create when absent,
// else replace spec and merge metadata.
func newFakeObjectClient(
	t *testing.T,
	objs ...runtime.Object,
) (*ObjectClient, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	mapper := meta.NewDefaultRESTMapper(nil)
	listKinds := map[schema.GroupVersionResource]string{}
	for gvk, resource := range testKinds {
		gvr := gvk.GroupVersion().WithResource(resource)
		mapper.AddSpecific(
			gvk,
			gvr,
			gvk.GroupVersion().WithResource(strings.TrimSuffix(resource, "s")),
			meta.RESTScopeNamespace,
		)
		listKinds[gvr] = gvk.Kind + "List"
	}
	fake := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objs...)
	fake.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(k8stesting.PatchAction)
		if !ok || pa.GetPatchType() != k8stypes.ApplyPatchType {
			return false, nil, nil
		}
		var applied unstructured.Unstructured
		if err := json.Unmarshal(pa.GetPatch(), &applied.Object); err != nil {
			return true, nil, err
		}
		existing, err := fake.Tracker().Get(pa.GetResource(), pa.GetNamespace(), pa.GetName())
		if apierrors.IsNotFound(err) {
			applied.SetUID(k8stypes.UID("uid-" + applied.GetName()))
			return true, &applied, fake.Tracker().
				Create(pa.GetResource(), &applied, pa.GetNamespace())
		}
		if err != nil {
			return true, nil, err
		}
		live, ok := existing.(*unstructured.Unstructured)
		if !ok {
			return true, nil, errors.New("unexpected tracker object")
		}
		merged := live.DeepCopy()
		merged.Object["spec"] = applied.Object["spec"]
		labels := merged.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for k, v := range applied.GetLabels() {
			labels[k] = v
		}
		merged.SetLabels(labels)
		return true, merged, fake.Tracker().Update(pa.GetResource(), merged, pa.GetNamespace())
	})
	cluster := &Cluster{Name: "mgmt", KubeconfigPath: "/nonexistent"}
	return NewObjectClientFor(cluster, fake, mapper, nil, "test-manager"), fake
}

func testObject(
	gvk schema.GroupVersionKind,
	name string,
	spec map[string]any,
) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetAPIVersion(gvk.GroupVersion().String())
	obj.SetKind(gvk.Kind)
	obj.SetNamespace("capi")
	obj.SetName(name)
	if spec != nil {
		obj.Object["spec"] = spec
	}
	return obj
}

var (
	gvkMachineTemplate = schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "TinkerbellMachineTemplate",
	}
	gvkMD = schema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: "v1beta2",
		Kind:    "MachineDeployment",
	}
	gvkTCP = schema.GroupVersionKind{
		Group:   "controlplane.cluster.x-k8s.io",
		Version: "v1beta1",
		Kind:    "TalosControlPlane",
	}
)

func TestObjectClient_ApplyGetListDelete(t *testing.T) {
	ctx := context.Background()
	client, _ := newFakeObjectClient(t)
	obj := testObject(
		gvkMachineTemplate,
		"worker-abc",
		map[string]any{"template": map[string]any{"spec": map[string]any{"a": int64(1)}}},
	)
	obj.SetLabels(map[string]string{"capi.terraform.io/template": "worker"})
	obj.Object["status"] = map[string]any{"ignored": true}

	live, err := client.Apply(ctx, obj, false)
	if err != nil {
		t.Fatal(err)
	}
	if live.GetUID() == "" || live.Object["status"] != nil {
		t.Errorf("apply must strip status and return the live object: %v", live.Object)
	}
	got, err := client.Get(ctx, gvkMachineTemplate, "capi", "worker-abc")
	if err != nil || got.GetName() != "worker-abc" {
		t.Fatalf("get: %v %v", got, err)
	}
	list, err := client.List(ctx, gvkMachineTemplate, "capi", "capi.terraform.io/template=worker")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if _, err := client.Get(
		ctx,
		gvkMachineTemplate,
		"capi",
		"nope",
	); !errors.Is(
		err,
		ErrObjectNotFound,
	) {
		t.Errorf("missing object: %v", err)
	}
	unknown := schema.GroupVersionKind{Group: "x.io", Version: "v1", Kind: "Nope"}
	if _, err := client.Get(ctx, unknown, "capi", "a"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("missing kind is not found: %v", err)
	}
	if list, err := client.List(ctx, unknown, "capi", ""); err != nil || len(list) != 0 {
		t.Errorf("missing kind lists nothing: %v %v", list, err)
	}
	if err := client.Delete(ctx, gvkMachineTemplate, "capi", "worker-abc"); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(ctx, gvkMachineTemplate, "capi", "worker-abc"); err != nil {
		t.Errorf("deleting twice is fine: %v", err)
	}
	if err := client.WaitForDeletion(
		ctx,
		gvkMachineTemplate,
		"capi",
		"worker-abc",
		WaitOptions{Timeout: time.Second, PollInterval: time.Millisecond},
	); err != nil {
		t.Errorf("gone: %v", err)
	}
}

func TestObjectClient_WaitForConditions(t *testing.T) {
	ctx := context.Background()
	md := testObject(gvkMD, "md-0", map[string]any{"replicas": int64(1)})
	md.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{
				"type":    "Available",
				"status":  "False",
				"reason":  "ScalingUp",
				"message": "0 of 1 replicas",
			},
		},
	}
	client, fake := newFakeObjectClient(t, md)
	opts := WaitOptions{Timeout: 50 * time.Millisecond, PollInterval: 5 * time.Millisecond}
	err := client.WaitForConditions(ctx, gvkMD, "capi", "md-0", []string{"Available"}, opts)
	if err == nil || !strings.Contains(err.Error(), "ScalingUp") ||
		!strings.Contains(err.Error(), "0 of 1 replicas") {
		t.Fatalf("timeout must carry the condition's reason and message: %v", err)
	}
	ready := md.DeepCopy()
	ready.Object["status"] = map[string]any{
		"conditions": []any{map[string]any{"type": "Available", "status": "True"}},
	}
	if err := fake.Tracker().
		Update(gvkMD.GroupVersion().WithResource("machinedeployments"), ready, "capi"); err != nil {
		t.Fatal(err)
	}
	if err := client.WaitForConditions(
		ctx,
		gvkMD,
		"capi",
		"md-0",
		[]string{"Available"},
		opts,
	); err != nil {
		t.Errorf("ready: %v", err)
	}
	if err := client.WaitForConditions(ctx, gvkMD, "capi", "md-0", nil, opts); err != nil {
		t.Errorf("no conditions, no wait: %v", err)
	}
}

func TestTemplateReferences(t *testing.T) {
	ctx := context.Background()
	md := testObject(
		gvkMD,
		"md-0",
		map[string]any{"template": map[string]any{"spec": map[string]any{
			"infrastructureRef": map[string]any{
				"apiGroup": "infrastructure.cluster.x-k8s.io",
				"kind":     "TinkerbellMachineTemplate",
				"name":     "worker-old",
			},
			"bootstrap": map[string]any{
				"configRef": map[string]any{"kind": "TalosConfigTemplate", "name": "worker-cfg"},
			},
		}}},
	)
	tcp := testObject(
		gvkTCP,
		"cp",
		map[string]any{
			"machineTemplate": map[string]any{
				"infrastructureRef": map[string]any{
					"kind": "TinkerbellMachineTemplate",
					"name": "cp-abc",
				},
			},
		},
	)
	client, _ := newFakeObjectClient(t, md, tcp)
	catalog := NewKindCatalog(
		loadTestCRDs(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml"),
	)

	idx, err := TemplateReferences(ctx, client, catalog, "capi")
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.Referrers(
		"TinkerbellMachineTemplate",
		"worker-old",
	); len(got) != 1 ||
		got[0] != "MachineDeployment/md-0" {
		t.Errorf("worker-old referrers = %v", got)
	}
	if got := idx.Referrers("TalosConfigTemplate", "worker-cfg"); len(got) != 1 {
		t.Errorf("bootstrap config ref = %v", got)
	}
	if got := idx.Referrers(
		"TinkerbellMachineTemplate",
		"cp-abc",
	); len(got) != 1 ||
		got[0] != "TalosControlPlane/cp" {
		t.Errorf("control plane referrers = %v (control-plane kinds come from the catalog)", got)
	}
	if got := idx.Referrers("TinkerbellMachineTemplate", "worker-new"); len(got) != 0 {
		t.Errorf("unreferenced = %v", got)
	}
}

func TestKindCatalog_ProvidersAndByKind(t *testing.T) {
	catalog := NewKindCatalog(
		loadTestCRDs(t, "core.yaml", "capt.yaml", "cacppt.yaml", "cabpt.yaml"),
	)
	cps := catalog.Providers(ProviderTypeControlPlane)
	if len(cps) != 1 || cps[0].Name != "talos" {
		t.Errorf("control-plane providers = %v", cps)
	}
	gvk, err := catalog.ByKind("TinkerbellMachineTemplate")
	if err != nil || gvk.Group != "infrastructure.cluster.x-k8s.io" {
		t.Errorf("ByKind = %v %v", gvk, err)
	}
	if _, err := catalog.ByKind("Nope"); !errors.Is(err, ErrKindNotFound) {
		t.Errorf("unknown kind: %v", err)
	}
	for label, want := range map[string]ProviderKey{
		"cluster-api":               {Type: ProviderTypeCore, Name: "cluster-api"},
		"control-plane-talos":       {Type: ProviderTypeControlPlane, Name: "talos"},
		"infrastructure-tinkerbell": {Type: ProviderTypeInfrastructure, Name: "tinkerbell"},
		"ipam-in-cluster":           {Type: ProviderTypeIPAM, Name: "in-cluster"},
	} {
		if got, ok := providerKeyFromLabel(label); !ok || got != want {
			t.Errorf("%s = %v %v", label, got, ok)
		}
	}
	if _, ok := providerKeyFromLabel("something-else"); ok {
		t.Error("unknown label")
	}
}
