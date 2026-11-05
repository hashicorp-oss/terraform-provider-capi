// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package kairos

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Kube probes the live node's Kubernetes API.
type Kube interface {
	// APIReachable reports whether the API server answers a version request.
	APIReachable(ctx context.Context) (bool, error)
	// NodeReady reports whether at least one node has Ready=True.
	NodeReady(ctx context.Context) (bool, error)
}

// KubeFactory builds a Kube from a kubeconfig path once one exists.
type KubeFactory func(kubeconfigPath string) (Kube, error)

// ClientGoKube implements Kube with client-go.
type ClientGoKube struct {
	cs kubernetes.Interface
}

// NewClientGoKube builds a Kube from a kubeconfig file.
func NewClientGoKube(kubeconfigPath string) (Kube, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 10 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &ClientGoKube{cs: cs}, nil
}

// APIReachable implements Kube. Transport errors are reported as "not
// reachable", never as errors, so callers can keep polling.
func (k *ClientGoKube) APIReachable(context.Context) (bool, error) {
	_, err := k.cs.Discovery().ServerVersion()
	return err == nil, nil
}

// NodeReady implements Kube. Like APIReachable, a transport error means
// "not ready yet" rather than a failure, so callers keep polling.
func (k *ClientGoKube) NodeReady(ctx context.Context) (bool, error) {
	nodes, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, nil //nolint:nilerr // not-ready, not an error; see doc comment
	}
	for _, n := range nodes.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
	}
	return false, nil
}
