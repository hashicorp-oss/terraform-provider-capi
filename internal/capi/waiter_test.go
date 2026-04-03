// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestNewDynamicWaiter(t *testing.T) {
	w := NewDynamicWaiter()
	if w == nil {
		t.Fatal("expected non-nil waiter")
	}
}

func TestHasCondition_True(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   "Ready",
						"status": "True",
					},
					map[string]interface{}{
						"type":   "Available",
						"status": "False",
					},
				},
			},
		},
	}

	if !hasCondition(obj, "Ready", "True") {
		t.Error("expected Ready=True to match")
	}
	if hasCondition(obj, "Available", "True") {
		t.Error("expected Available=True to not match (status is False)")
	}
	if !hasCondition(obj, "Available", "False") {
		t.Error("expected Available=False to match")
	}
}

func TestHasCondition_NotFound(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   "Ready",
						"status": "True",
					},
				},
			},
		},
	}

	if hasCondition(obj, "NonExistent", "True") {
		t.Error("expected NonExistent condition to not match")
	}
}

func TestHasCondition_NoConditions(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{},
	}
	if hasCondition(obj, "Ready", "True") {
		t.Error("expected no conditions to match on empty object")
	}
}

func TestHasCondition_EmptyStatus(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{},
		},
	}
	if hasCondition(obj, "Ready", "True") {
		t.Error("expected no conditions to match on empty status")
	}
}

func TestHasCondition_EmptyConditionsList(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"status": map[string]interface{}{
				"conditions": []interface{}{},
			},
		},
	}
	if hasCondition(obj, "Ready", "True") {
		t.Error("expected no conditions to match on empty conditions list")
	}
}
