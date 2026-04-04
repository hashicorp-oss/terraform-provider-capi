// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func makeDeployment(name string) unstructured.Unstructured {
	return unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "test-system",
			},
			"spec": map[string]interface{}{
				"replicas": int64(1),
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"serviceAccountName": "default",
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "manager",
								"image": "example.com/controller:v1",
								"args":  []interface{}{"--leader-elect=true", "--v=1"},
							},
						},
					},
				},
			},
		},
	}
}

func makeConfigMap(name string) unstructured.Unstructured {
	return unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "test-system",
			},
			"data": map[string]interface{}{
				"key": "value",
			},
		},
	}
}

func TestCustomizeDeployment_Replicas(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}
	replicas := int64(3)
	cfg := &DeploymentConfig{Replicas: &replicas}

	result, err := customizeDeployment(objs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _, _ := unstructured.NestedInt64(result[0].Object, "spec", "replicas")
	if got != 3 {
		t.Errorf("expected replicas 3, got %d", got)
	}
}

func TestCustomizeDeployment_NodeSelector(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}
	cfg := &DeploymentConfig{
		NodeSelector: map[string]string{"node-role": "infra"},
	}

	result, err := customizeDeployment(objs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ns, _, _ := unstructured.NestedStringMap(result[0].Object, "spec", "template", "spec", "nodeSelector")
	if ns["node-role"] != "infra" {
		t.Errorf("expected nodeSelector 'node-role: infra', got %v", ns)
	}
}

func TestCustomizeDeployment_ServiceAccountName(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}
	cfg := &DeploymentConfig{ServiceAccountName: "custom-sa"}

	result, err := customizeDeployment(objs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sa, _, _ := unstructured.NestedString(result[0].Object, "spec", "template", "spec", "serviceAccountName")
	if sa != "custom-sa" {
		t.Errorf("expected serviceAccountName 'custom-sa', got %q", sa)
	}
}

func TestCustomizeDeployment_ContainerImage(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}
	cfg := &DeploymentConfig{
		Containers: []ContainerConfig{
			{Name: "manager", ImageURL: "example.com/controller:v2"},
		},
	}

	result, err := customizeDeployment(objs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	containers, _, _ := unstructured.NestedSlice(result[0].Object, "spec", "template", "spec", "containers")
	cMap := containers[0].(map[string]interface{})
	if cMap["image"] != "example.com/controller:v2" {
		t.Errorf("expected image 'example.com/controller:v2', got %v", cMap["image"])
	}
}

func TestCustomizeDeployment_SkipsNonControllerManager(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("webhook-server")}
	replicas := int64(5)
	cfg := &DeploymentConfig{Replicas: &replicas}

	result, err := customizeDeployment(objs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _, _ := unstructured.NestedInt64(result[0].Object, "spec", "replicas")
	if got != 1 {
		t.Errorf("expected replicas unchanged at 1, got %d", got)
	}
}

func TestCustomizeManager_FeatureGates(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}
	cfg := &ManagerConfig{
		FeatureGates: map[string]bool{"MachinePool": true, "ClusterTopology": false},
	}

	result, err := customizeManager(objs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	containers, _, _ := unstructured.NestedSlice(result[0].Object, "spec", "template", "spec", "containers")
	cMap := containers[0].(map[string]interface{})
	args := cMap["args"].([]interface{})

	found := false
	for _, a := range args {
		s := a.(string)
		if len(s) > len("--feature-gates=") && s[:len("--feature-gates=")] == "--feature-gates=" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --feature-gates arg, got args: %v", args)
	}
}

func TestCustomizeManager_Verbosity(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}
	v := int64(5)
	cfg := &ManagerConfig{Verbosity: &v}

	result, err := customizeManager(objs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	containers, _, _ := unstructured.NestedSlice(result[0].Object, "spec", "template", "spec", "containers")
	cMap := containers[0].(map[string]interface{})
	args := cMap["args"].([]interface{})

	found := false
	for _, a := range args {
		if a.(string) == "--v=5" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --v=5, got args: %v", args)
	}
}

func TestCustomizeManager_ReplacesExistingArg(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}
	v := int64(3)
	cfg := &ManagerConfig{Verbosity: &v}

	result, err := customizeManager(objs, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	containers, _, _ := unstructured.NestedSlice(result[0].Object, "spec", "template", "spec", "containers")
	cMap := containers[0].(map[string]interface{})
	args := cMap["args"].([]interface{})

	vCount := 0
	for _, a := range args {
		s := a.(string)
		if len(s) >= 4 && s[:4] == "--v=" {
			vCount++
		}
	}
	if vCount != 1 {
		t.Errorf("expected exactly 1 --v arg, got %d in args: %v", vCount, args)
	}
}

func TestApplyManifestPatches(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}

	patches := []string{`{"spec": {"replicas": 5}}`}
	result, err := applyManifestPatches(objs, patches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _, _ := unstructured.NestedFloat64(result[0].Object, "spec", "replicas")
	if got != 5 {
		t.Errorf("expected replicas 5, got %v", got)
	}
}

func TestApplyManifestPatches_YAML(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}

	patches := []string{"spec:\n  replicas: 7"}
	result, err := applyManifestPatches(objs, patches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _, _ := unstructured.NestedFloat64(result[0].Object, "spec", "replicas")
	if got != 7 {
		t.Errorf("expected replicas 7, got %v", got)
	}
}

func TestApplyTargetedPatches_MergePatch(t *testing.T) {
	deploy := makeDeployment("test-controller-manager")
	cm := makeConfigMap("test-config")
	objs := []unstructured.Unstructured{deploy, cm}

	patches := []PatchConfig{
		{
			Patch:  `{"metadata": {"labels": {"custom": "true"}}}`,
			Target: &PatchSelector{Kind: "Deployment"},
		},
	}

	result, err := applyTargetedPatches(objs, patches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Deployment should have the label
	labels := result[0].GetLabels()
	if labels["custom"] != "true" {
		t.Errorf("expected custom=true label on Deployment, got %v", labels)
	}

	// ConfigMap should NOT have the label
	cmLabels := result[1].GetLabels()
	if _, ok := cmLabels["custom"]; ok {
		t.Errorf("ConfigMap should not have custom label, got %v", cmLabels)
	}
}

func TestApplyTargetedPatches_RFC6902(t *testing.T) {
	objs := []unstructured.Unstructured{makeDeployment("test-controller-manager")}

	patches := []PatchConfig{
		{
			Patch: `[{"op": "replace", "path": "/spec/replicas", "value": 10}]`,
		},
	}

	result, err := applyTargetedPatches(objs, patches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, _, _ := unstructured.NestedFloat64(result[0].Object, "spec", "replicas")
	if got != 10 {
		t.Errorf("expected replicas 10, got %v", got)
	}
}

func TestMatchesTarget(t *testing.T) {
	obj := makeDeployment("test-controller-manager")
	obj.SetLabels(map[string]string{"app": "controller"})

	tests := []struct {
		name   string
		target *PatchSelector
		want   bool
	}{
		{"nil target matches all", nil, true},
		{"kind match", &PatchSelector{Kind: "Deployment"}, true},
		{"kind mismatch", &PatchSelector{Kind: "Service"}, false},
		{"name match", &PatchSelector{Name: "test-controller-manager"}, true},
		{"name mismatch", &PatchSelector{Name: "other"}, false},
		{"label match", &PatchSelector{LabelSelector: "app=controller"}, true},
		{"label mismatch", &PatchSelector{LabelSelector: "app=other"}, false},
		{"group match", &PatchSelector{Group: "apps"}, true},
		{"group mismatch", &PatchSelector{Group: "core"}, false},
		{"combined match", &PatchSelector{Kind: "Deployment", Name: "test-controller-manager"}, true},
		{"combined partial mismatch", &PatchSelector{Kind: "Deployment", Name: "other"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesTarget(&obj, tt.target)
			if got != tt.want {
				t.Errorf("matchesTarget() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildComponentsAlterFn_FullPipeline(t *testing.T) {
	replicas := int64(3)
	verbosity := int64(5)
	addon := AddonConfig{
		Provider: "helm:v0.2.12",
		Deployment: &DeploymentConfig{
			Replicas: &replicas,
		},
		Manager: &ManagerConfig{
			Verbosity: &verbosity,
		},
		ManifestPatches: []string{
			`{"metadata": {"annotations": {"custom": "annotation"}}}`,
		},
		AdditionalManifests: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n  namespace: test\ndata:\n  key: val\n",
	}

	alterFn := BuildComponentsAlterFn(addon)
	objs := []unstructured.Unstructured{makeDeployment("helm-controller-manager")}

	result, err := alterFn(objs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check replicas were set (after JSON roundtrip, int64 becomes float64)
	gotReplicas, _, _ := unstructured.NestedFloat64(result[0].Object, "spec", "replicas")
	if gotReplicas != 3 {
		t.Errorf("expected replicas 3, got %v", gotReplicas)
	}

	// Check verbosity was set on manager container
	containers, _, _ := unstructured.NestedSlice(result[0].Object, "spec", "template", "spec", "containers")
	cMap := containers[0].(map[string]interface{})
	args := cMap["args"].([]interface{})
	foundV := false
	for _, a := range args {
		if a.(string) == "--v=5" {
			foundV = true
		}
	}
	if !foundV {
		t.Errorf("expected --v=5 in args, got %v", args)
	}

	// Check annotation from manifest patch
	annotations := result[0].GetAnnotations()
	if annotations["custom"] != "annotation" {
		t.Errorf("expected custom annotation, got %v", annotations)
	}

	// Check additional manifest was appended
	if len(result) != 2 {
		t.Fatalf("expected 2 objects (deployment + configmap), got %d", len(result))
	}
	if result[1].GetKind() != "ConfigMap" || result[1].GetName() != "extra" {
		t.Errorf("expected additional ConfigMap 'extra', got %s/%s", result[1].GetKind(), result[1].GetName())
	}
}

func TestParseMultiDocYAML(t *testing.T) {
	yaml := `apiVersion: v1
kind: ConfigMap
metadata:
  name: first
---
apiVersion: v1
kind: Secret
metadata:
  name: second
`
	objs, err := parseMultiDocYAML([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(objs))
	}
	if objs[0].GetKind() != "ConfigMap" {
		t.Errorf("expected first object kind ConfigMap, got %s", objs[0].GetKind())
	}
	if objs[1].GetKind() != "Secret" {
		t.Errorf("expected second object kind Secret, got %s", objs[1].GetKind())
	}
}

func TestSetArg_NewArg(t *testing.T) {
	args := []interface{}{"--leader-elect=true"}
	result := setArg(args, "--v", "3")
	if len(result) != 2 {
		t.Fatalf("expected 2 args, got %d", len(result))
	}
	if result[1].(string) != "--v=3" {
		t.Errorf("expected --v=3, got %s", result[1].(string))
	}
}

func TestSetArg_ReplaceExisting(t *testing.T) {
	args := []interface{}{"--leader-elect=true", "--v=1"}
	result := setArg(args, "--v", "5")
	if len(result) != 2 {
		t.Fatalf("expected 2 args, got %d", len(result))
	}
	if result[1].(string) != "--v=5" {
		t.Errorf("expected --v=5, got %s", result[1].(string))
	}
}

func TestAddonProviderStrings(t *testing.T) {
	addons := []AddonConfig{
		{Provider: "helm:v0.2.12"},
		{Provider: "flux:v0.1.0"},
	}
	result := AddonProviderStrings(addons)
	if len(result) != 2 || result[0] != "helm:v0.2.12" || result[1] != "flux:v0.1.0" {
		t.Errorf("unexpected result: %v", result)
	}
}

func TestCustomizedAddons(t *testing.T) {
	replicas := int64(3)
	addons := []AddonConfig{
		{Provider: "helm:v0.2.12"},
		{Provider: "flux:v0.1.0", Deployment: &DeploymentConfig{Replicas: &replicas}},
	}
	result := CustomizedAddons(addons)
	if len(result) != 1 {
		t.Fatalf("expected 1 customized addon, got %d", len(result))
	}
	if _, ok := result["flux"]; !ok {
		t.Error("expected 'flux' in customized addons")
	}
}

func TestCustomProcessor_Process(t *testing.T) {
	proc := newCustomProcessor(
		map[string]string{"MY_VAR": "custom-value"},
		map[string]string{"SECRET_VAR": "secret-value"},
	)

	input := []byte("name: ${MY_VAR}\nsecret: ${SECRET_VAR}")
	result, err := proc.Process(input, func(key string) (string, error) {
		return "default-" + key, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s := string(result)
	if s != "name: custom-value\nsecret: secret-value" {
		t.Errorf("unexpected result: %s", s)
	}
}

func TestCustomProcessor_FallsBackToResolver(t *testing.T) {
	proc := newCustomProcessor(
		map[string]string{"MY_VAR": "custom"},
		nil,
	)

	input := []byte("a: ${MY_VAR}\nb: ${OTHER_VAR}")
	result, err := proc.Process(input, func(key string) (string, error) {
		if key == "OTHER_VAR" {
			return "from-resolver", nil
		}
		return "", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s := string(result)
	if s != "a: custom\nb: from-resolver" {
		t.Errorf("unexpected result: %s", s)
	}
}

func TestParseProviderNameVersion(t *testing.T) {
	tests := []struct {
		input       string
		wantName    string
		wantVersion string
	}{
		{"helm:v0.2.12", "helm", "v0.2.12"},
		{"helm", "helm", ""},
		{"my-provider:v1.0.0", "my-provider", "v1.0.0"},
		{"  helm : v0.2.12 ", "helm", "v0.2.12"},
	}
	for _, tt := range tests {
		name, ver := parseProviderNameVersion(tt.input)
		if name != tt.wantName || ver != tt.wantVersion {
			t.Errorf("parseProviderNameVersion(%q) = (%q, %q), want (%q, %q)",
				tt.input, name, ver, tt.wantName, tt.wantVersion)
		}
	}
}

func TestHasCustomizations(t *testing.T) {
	tests := []struct {
		name  string
		addon AddonConfig
		want  bool
	}{
		{"empty", AddonConfig{Provider: "helm:v0.2.12"}, false},
		{"config vars", AddonConfig{Provider: "helm", ConfigVariables: map[string]string{"k": "v"}}, true},
		{"secret vars", AddonConfig{Provider: "helm", SecretConfigVariables: map[string]string{"k": "v"}}, true},
		{"deployment", AddonConfig{Provider: "helm", Deployment: &DeploymentConfig{}}, true},
		{"manager", AddonConfig{Provider: "helm", Manager: &ManagerConfig{}}, true},
		{"additional manifests", AddonConfig{Provider: "helm", AdditionalManifests: "yaml"}, true},
		{"manifest patches", AddonConfig{Provider: "helm", ManifestPatches: []string{"{}"}}, true},
		{"patches", AddonConfig{Provider: "helm", Patches: []PatchConfig{{Patch: "{}"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.addon.HasCustomizations(); got != tt.want {
				t.Errorf("HasCustomizations() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBuildComponentsAlterFn_NoCustomizations verifies that a no-op alter function
// doesn't modify the input objects.
func TestBuildComponentsAlterFn_NoCustomizations(t *testing.T) {
	addon := AddonConfig{Provider: "helm:v0.2.12"}
	alterFn := BuildComponentsAlterFn(addon)
	objs := []unstructured.Unstructured{makeDeployment("helm-controller-manager")}

	original, _ := json.Marshal(objs[0].Object)
	result, err := alterFn(objs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after, _ := json.Marshal(result[0].Object)

	if string(original) != string(after) {
		t.Error("expected no modifications when addon has no customizations")
	}
}
