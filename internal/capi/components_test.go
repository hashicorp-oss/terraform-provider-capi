// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestCatalogFromObjects_KindsOf(t *testing.T) {
	crds := loadTestCRDs(t, "capt.yaml", "cacppt.yaml")
	var objs []unstructured.Unstructured
	for i := range crds {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&crds[i])
		if err != nil {
			t.Fatal(err)
		}
		u := unstructured.Unstructured{Object: m}
		u.SetAPIVersion("apiextensions.k8s.io/v1")
		u.SetKind("CustomResourceDefinition")
		objs = append(objs, u)
	}
	// A ConfigMap among the objects is ignored.
	cm := unstructured.Unstructured{Object: map[string]any{}}
	cm.SetAPIVersion("v1")
	cm.SetKind("ConfigMap")
	objs = append(objs, cm)

	catalog, err := catalogFromObjects(objs, "v1beta2")
	if err != nil {
		t.Fatal(err)
	}
	// The release ships no core CRD; the supplied contract stands in.
	if core, err := catalog.CoreVersion(); err != nil || core != "v1beta2" {
		t.Fatalf("core = %q, %v", core, err)
	}
	kinds, err := catalog.KindsOf(ProviderKey{Type: ProviderTypeInfrastructure, Name: "tinkerbell"})
	if err != nil {
		t.Fatal(err)
	}
	mt, ok := kinds["TinkerbellMachineTemplate"]
	if !ok || mt.Group != "infrastructure.cluster.x-k8s.io" || mt.Version == "" {
		t.Errorf("TinkerbellMachineTemplate = %+v (%v)", mt, SortedKinds(kinds))
	}
	// The same version the suffix resolver would emit.
	gvk, err := catalog.Resolve(
		ProviderKey{Type: ProviderTypeInfrastructure, Name: "tinkerbell"},
		"MachineTemplate",
	)
	if err != nil || gvk != mt {
		t.Errorf("Resolve = %v, %v; KindsOf = %v", gvk, err, mt)
	}
	if _, err := catalog.KindsOf(
		ProviderKey{Type: ProviderTypeIPAM, Name: "unifi"},
	); !errors.Is(
		err,
		ErrKindNotFound,
	) {
		t.Errorf("unknown provider: %v", err)
	}
}
