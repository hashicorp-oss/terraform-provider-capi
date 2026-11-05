// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package kairos

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestClientGoKube_NodeReady(t *testing.T) {
	notReady := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}}
	ready := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}

	k := &ClientGoKube{cs: fake.NewClientset(notReady)}
	got, err := k.NodeReady(context.Background())
	if err != nil || got {
		t.Fatalf("NodeReady() = %v, %v; want false, nil", got, err)
	}

	k = &ClientGoKube{cs: fake.NewClientset(notReady, ready)}
	got, err = k.NodeReady(context.Background())
	if err != nil || !got {
		t.Fatalf("NodeReady() = %v, %v; want true, nil", got, err)
	}
}

func TestClientGoKube_APIReachableWithFake(t *testing.T) {
	k := &ClientGoKube{cs: fake.NewClientset()}
	got, err := k.APIReachable(context.Background())
	if err != nil || !got {
		t.Fatalf("APIReachable() = %v, %v; want true, nil", got, err)
	}
}

func TestNewClientGoKube_BadPath(t *testing.T) {
	if _, err := NewClientGoKube("/definitely/missing/kubeconfig"); err == nil {
		t.Fatal("expected error for missing kubeconfig")
	}
}
