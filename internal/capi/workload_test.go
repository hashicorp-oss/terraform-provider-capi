// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestClusterKubeconfig(t *testing.T) {
	ctx := context.Background()
	secret := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "prod-kubeconfig", "namespace": "capi"},
		"data":     map[string]any{"value": base64.StdEncoding.EncodeToString([]byte("apiVersion: v1\nkind: Config\n"))},
	}}
	empty := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "bare-kubeconfig", "namespace": "capi"},
	}}
	client, _ := newFakeObjectClient(t, secret, empty)
	raw, err := client.ClusterKubeconfig(ctx, "capi", "prod")
	if err != nil || !strings.HasPrefix(string(raw), "apiVersion: v1") {
		t.Fatalf("kubeconfig = %q, %v", raw, err)
	}
	if _, err := client.ClusterKubeconfig(ctx, "capi", "missing"); !errors.Is(err, ErrNoKubeconfig) {
		t.Errorf("missing Secret: %v", err)
	}
	if _, err := client.ClusterKubeconfig(ctx, "capi", "bare"); !errors.Is(err, ErrNoKubeconfig) {
		t.Errorf("Secret without value: %v", err)
	}
}

func TestDecodeDocuments(t *testing.T) {
	docs, err := DecodeDocuments([]byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a\n---\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n  namespace: a\n"))
	if err != nil || len(docs) != 2 || docs[0].GetKind() != "Namespace" || docs[1].GetNamespace() != "a" {
		t.Fatalf("docs = %v, %v", docs, err)
	}
	docs, err = DecodeDocuments([]byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"}}`))
	if err != nil || len(docs) != 1 || docs[0].GetName() != "s" {
		t.Errorf("json = %v, %v", docs, err)
	}
	if _, err := DecodeDocuments([]byte("apiVersion: v1\nkind: ConfigMap\n")); err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("nameless: %v", err)
	}
	if _, err := DecodeDocuments([]byte("foo: [")); err == nil {
		t.Error("invalid YAML must fail")
	}
	if docs, err := DecodeDocuments(nil); err != nil || len(docs) != 0 {
		t.Errorf("empty = %v, %v", docs, err)
	}
}
