// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func healthInputs(core string) healthCheckInputs {
	return healthCheckInputs{
		Cluster:            "prod",
		Namespace:          "capi",
		Name:               "workers",
		CoreVersion:        core,
		MatchLabels:        map[string]string{deploymentNameLabel: "workers"},
		NodeStartupTimeout: dur(15 * time.Minute),
		NodeConditions: []unhealthyCondition{
			{Type: "Ready", Status: "False", Timeout: 3 * time.Minute},
		},
		MachineConditions: []unhealthyCondition{
			{Type: "HealthCheckSucceeded", Status: "False", Timeout: time.Minute},
		},
		MaxUnhealthy: "40%",
		RemediationTemplate: &objectRef{
			APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
			Kind:       "Metal3RemediationTemplate",
			Name:       "reboot",
		},
	}
}

func TestRenderMachineHealthCheck_V1Beta2(t *testing.T) {
	obj := renderMachineHealthCheck(healthInputs("v1beta2"))
	if obj.GetAPIVersion() != "cluster.x-k8s.io/v1beta2" || obj.GetKind() != "MachineHealthCheck" ||
		obj.GetLabels()[clusterNameLabel] != "prod" {
		t.Errorf("identity = %s %s %v", obj.GetAPIVersion(), obj.GetKind(), obj.GetLabels())
	}
	if nested(t, obj.Object, "spec", "clusterName") != "prod" ||
		nestedMap(t, obj.Object, "spec", "selector", "matchLabels")[deploymentNameLabel] != "workers" {
		t.Errorf("spec = %v", obj.Object["spec"])
	}
	if nested(t, obj.Object, "spec", "checks", "nodeStartupTimeoutSeconds") != int64(900) {
		t.Error("nodeStartupTimeoutSeconds")
	}
	nodeConds := nestedSlice(t, obj.Object, "spec", "checks", "unhealthyNodeConditions")
	if len(nodeConds) != 1 || asMap(t, nodeConds[0])["timeoutSeconds"] != int64(180) ||
		asMap(t, nodeConds[0])["status"] != "False" {
		t.Errorf("node conditions = %v", nodeConds)
	}
	if got := nestedSlice(
		t,
		obj.Object,
		"spec",
		"checks",
		"unhealthyMachineConditions",
	); len(
		got,
	) != 1 {
		t.Errorf("machine conditions = %v", got)
	}
	if nested(
		t,
		obj.Object,
		"spec",
		"remediation",
		"triggerIf",
		"unhealthyLessThanOrEqualTo",
	) != "40%" {
		t.Error("triggerIf")
	}
	ref := nestedMap(t, obj.Object, "spec", "remediation", "templateRef")
	if ref["kind"] != "Metal3RemediationTemplate" || ref["name"] != "reboot" {
		t.Errorf("templateRef = %v", ref)
	}
	for _, legacy := range []string{"unhealthyConditions", "maxUnhealthy", "nodeStartupTimeout", "remediationTemplate"} {
		if _, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", legacy); ok {
			t.Errorf("v1beta2 shape has no %s", legacy)
		}
	}
	// Round trip.
	back := readLiveHealthCheck(obj)
	if back.Cluster != "prod" || back.MatchLabels[deploymentNameLabel] != "workers" ||
		back.NodeStartupTimeout == nil ||
		*back.NodeStartupTimeout != 15*time.Minute {
		t.Errorf("read back = %+v", back)
	}
	if len(back.NodeConditions) != 1 || back.NodeConditions[0].Timeout != 3*time.Minute ||
		len(back.MachineConditions) != 1 ||
		back.MaxUnhealthy != "40%" ||
		back.RemediationTemplate == nil ||
		back.RemediationTemplate.Name != "reboot" {
		t.Errorf("read back = %+v", back)
	}
}

func TestRenderMachineHealthCheck_V1Beta1(t *testing.T) {
	in := healthInputs("v1beta1")
	in.MaxUnhealthy, in.UnhealthyRange = "", "[1-3]"
	obj := renderMachineHealthCheck(in)
	if obj.GetAPIVersion() != "cluster.x-k8s.io/v1beta1" {
		t.Errorf("apiVersion = %s", obj.GetAPIVersion())
	}
	conds := nestedSlice(t, obj.Object, "spec", "unhealthyConditions")
	if len(conds) != 1 || asMap(t, conds[0])["timeout"] != "3m0s" {
		t.Errorf("legacy conditions = %v", conds)
	}
	if nested(t, obj.Object, "spec", "nodeStartupTimeout") != "15m0s" ||
		nested(t, obj.Object, "spec", "unhealthyRange") != "[1-3]" {
		t.Errorf("legacy spec = %v", obj.Object["spec"])
	}
	ref := nestedMap(t, obj.Object, "spec", "remediationTemplate")
	if ref["namespace"] != "capi" ||
		ref["apiVersion"] != "infrastructure.cluster.x-k8s.io/v1beta1" {
		t.Errorf("legacy remediationTemplate = %v", ref)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "checks"); ok {
		t.Error("legacy shape has no checks")
	}
	back := readLiveHealthCheck(obj)
	if back.UnhealthyRange != "[1-3]" || back.MaxUnhealthy != "" || len(back.NodeConditions) != 1 ||
		back.NodeConditions[0].Timeout != 3*time.Minute ||
		back.NodeStartupTimeout == nil {
		t.Errorf("read back = %+v", back)
	}
}

func TestRenderMachineHealthCheck_DefaultsAndCount(t *testing.T) {
	in := healthCheckInputs{
		Cluster:      "prod",
		Namespace:    "capi",
		Name:         "cp",
		CoreVersion:  "v1beta2",
		MatchLabels:  map[string]string{controlPlaneLabel: ""},
		MaxUnhealthy: "2",
	}
	obj := renderMachineHealthCheck(in)
	conds := nestedSlice(t, obj.Object, "spec", "checks", "unhealthyNodeConditions")
	if len(conds) != 2 || asMap(t, conds[0])["status"] != "Unknown" ||
		asMap(t, conds[1])["timeoutSeconds"] != int64(300) {
		t.Errorf("defaults = %v", conds)
	}
	if nested(
		t,
		obj.Object,
		"spec",
		"remediation",
		"triggerIf",
		"unhealthyLessThanOrEqualTo",
	) != int64(
		2,
	) {
		t.Error("a count renders as an integer")
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(
		obj.Object,
		"spec",
		"checks",
		"unhealthyMachineConditions",
	); ok {
		t.Error("no machine conditions unless set")
	}
	if back := readLiveHealthCheck(obj); back.MaxUnhealthy != "2" {
		t.Errorf("count reads back as %q", back.MaxUnhealthy)
	}
}

func TestHealthCheckSelectorAndValidate(t *testing.T) {
	if got := healthCheckSelector(
		"workers",
		true,
		false,
		nil,
	); got[topologyDeploymentLabel] != "workers" {
		t.Errorf("topology selector = %v", got)
	}
	if got := healthCheckSelector(
		"workers",
		false,
		false,
		nil,
	); got[deploymentNameLabel] != "workers" {
		t.Errorf("direct selector = %v", got)
	}
	if got := healthCheckSelector("", false, true, nil); !hasKey(got, controlPlaneLabel) {
		t.Errorf("control plane selector = %v", got)
	}
	if got := healthCheckSelector(
		"",
		false,
		false,
		map[string]string{"tier": "w"},
	); got["tier"] != "w" {
		t.Errorf("label selector = %v", got)
	}
	for name, tc := range map[string]struct {
		mutate func(*healthCheckInputs)
		want   string
	}{
		"ok":          {func(*healthCheckInputs) {}, ""},
		"no selector": {func(in *healthCheckInputs) { in.MatchLabels = nil }, "one of machine_deployment"},
		"both triggers": {func(in *healthCheckInputs) {
			in.UnhealthyRange = "[1-2]"
		}, "exclusive"},
		"bad max":   {func(in *healthCheckInputs) { in.MaxUnhealthy = "many" }, "not a count or a percentage"},
		"bad range": {func(in *healthCheckInputs) { in.MaxUnhealthy = ""; in.UnhealthyRange = "1-2" }, "not of the form"},
		"bad condition": {func(in *healthCheckInputs) {
			in.NodeConditions = []unhealthyCondition{{Type: "Ready", Status: "False"}}
		}, "positive timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			in := healthInputs("v1beta2")
			tc.mutate(&in)
			err := in.validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
