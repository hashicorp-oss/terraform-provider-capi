// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// SecretGVK is the core Secret kind.
var SecretGVK = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}

// ErrNoKubeconfig reports a workload cluster whose kubeconfig Secret is not
// on the management cluster: its control plane has not initialized, or the
// cluster is gone.
var ErrNoKubeconfig = errors.New("no kubeconfig for cluster")

// ClusterKubeconfig reads a workload cluster's kubeconfig from the
// `<cluster>-kubeconfig` Secret CAPI writes on the management cluster, the
// way `clusterctl get kubeconfig` does.
func (c *ObjectClient) ClusterKubeconfig(
	ctx context.Context,
	namespace, cluster string,
) ([]byte, error) {
	secret, err := c.Get(ctx, SecretGVK, namespace, cluster+"-kubeconfig")
	if errors.Is(err, ErrObjectNotFound) {
		return nil, fmt.Errorf(
			"%w: Secret %s/%s-kubeconfig is not on %s",
			ErrNoKubeconfig,
			namespace,
			cluster,
			c.cluster.Name,
		)
	}
	if err != nil {
		return nil, err
	}
	data, _, _ := unstructured.NestedStringMap(secret.Object, "data")
	encoded, ok := data["value"]
	if !ok || encoded == "" {
		return nil, fmt.Errorf(
			"%w: Secret %s/%s-kubeconfig has no value key",
			ErrNoKubeconfig,
			namespace,
			cluster,
		)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decoding Secret %s/%s-kubeconfig: %w", namespace, cluster, err)
	}
	return raw, nil
}

// WorkloadClient connects to a workload cluster through its kubeconfig
// Secret. The cleanup removes the kubeconfig file the connection needs.
func (c *ObjectClient) WorkloadClient(
	ctx context.Context,
	namespace, cluster, fieldManager string,
) (*ObjectClient, func(), error) {
	raw, err := c.ClusterKubeconfig(ctx, namespace, cluster)
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "capi-tf-workload-")
	if err != nil {
		return nil, nil, fmt.Errorf("creating the kubeconfig directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, cluster+".kubeconfig")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("writing the kubeconfig of %s: %w", cluster, err)
	}
	client, err := NewObjectClient(
		&Cluster{Name: cluster, KubeconfigPath: path, Namespace: namespace},
		fieldManager,
	)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("connecting to %s: %w", cluster, err)
	}
	return client, cleanup, nil
}

// DecodeDocuments parses a multi-document YAML (or JSON) manifest into
// objects, skipping empty documents. Every object must name a kind.
func DecodeDocuments(data []byte) ([]*unstructured.Unstructured, error) {
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for i := 0; ; i++ {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("document %d: %w", i+1, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetKind() == "" || obj.GetAPIVersion() == "" {
			return nil, fmt.Errorf("document %d: no apiVersion or kind", i+1)
		}
		if strings.TrimSpace(obj.GetName()) == "" && obj.GetGenerateName() == "" {
			return nil, fmt.Errorf("document %d (%s): no metadata.name", i+1, obj.GetKind())
		}
		out = append(out, &obj)
	}
}
