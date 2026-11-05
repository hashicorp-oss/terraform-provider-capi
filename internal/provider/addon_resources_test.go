// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func boolp(b bool) *bool { return &b }

func TestAddonResources_Schema(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		res      resource.Resource
		typeName string
		attrs    []string
	}{
		{NewHelmChartProxyResource(), "capi_helm_chart_proxy", []string{"cluster", "cluster_selector", "chart", "release", "strategy", "options", "credentials", "tls", "wait", "force_delete", "status", "conditions"}},
		{NewClusterResourceSetResource(), "capi_cluster_resource_set", []string{"cluster", "cluster_selector", "strategy", "manifests", "secrets", "prune", "wait", "objects", "status", "conditions"}},
	} {
		resp := &resource.SchemaResponse{}
		tc.res.Schema(ctx, resource.SchemaRequest{}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", tc.typeName, resp.Diagnostics)
		}
		for _, a := range tc.attrs {
			if _, ok := resp.Schema.Attributes[a]; !ok {
				t.Errorf("%s: missing attribute %s", tc.typeName, a)
			}
		}
		meta := &resource.MetadataResponse{}
		tc.res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "capi"}, meta)
		if meta.TypeName != tc.typeName {
			t.Errorf("type name = %s", meta.TypeName)
		}
	}
}

func helmInputs() helmChartProxyInputs {
	return helmChartProxyInputs{
		Name: "cilium", Namespace: "capi", APIVersion: "addons.cluster.x-k8s.io/v1alpha1",
		ClusterSelector: map[string]string{clusterNameLabel: "prod"},
		ChartName:       "cilium", RepoURL: "oci://quay.io/cilium/charts", Version: "1.18.2",
		ReleaseName: "cilium", ReleaseNamespace: "kube-system", Values: "kubeProxyReplacement: true\n",
		Strategy: "Continuous",
		Options: helmOptions{
			Wait: boolp(true), Timeout: dur(10 * time.Minute), SkipCRDs: boolp(false),
			CreateNamespace: boolp(true), Force: boolp(true), MaxHistory: i64(3), KeepHistory: boolp(false), UninstallDescription: "bye",
		},
		CredentialsName: "quay-creds", CASecret: "repo-ca", InsecureSkipTLS: boolp(false),
	}
}

func TestRenderHelmChartProxy(t *testing.T) {
	in := helmInputs()
	obj := renderHelmChartProxy(in)
	if obj.GetAPIVersion() != "addons.cluster.x-k8s.io/v1alpha1" || obj.GetKind() != "HelmChartProxy" || obj.GetNamespace() != "capi" || obj.GetName() != "cilium" {
		t.Errorf("identity = %s %s %s/%s", obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName())
	}
	spec := nestedMap(t, obj.Object, "spec")
	if nestedMap(t, spec, "clusterSelector", "matchLabels")[clusterNameLabel] != "prod" || spec["chartName"] != "cilium" || spec["repoURL"] != "oci://quay.io/cilium/charts" || spec["version"] != "1.18.2" {
		t.Errorf("spec = %v", spec)
	}
	if spec["releaseName"] != "cilium" || spec["namespace"] != "kube-system" || spec["valuesTemplate"] != "kubeProxyReplacement: true\n" || spec["reconcileStrategy"] != "Continuous" {
		t.Errorf("release = %v", spec)
	}
	opts := nestedMap(t, spec, "options")
	if opts["wait"] != true || opts["timeout"] != "10m0s" || opts["skipCRDs"] != false {
		t.Errorf("options = %v", opts)
	}
	if _, ok := opts["atomic"]; ok {
		t.Error("unset options are not rendered")
	}
	if nestedMap(t, opts, "install")["createNamespace"] != true || nestedMap(t, opts, "upgrade")["force"] != true || nestedMap(t, opts, "upgrade")["maxHistory"] != int64(3) {
		t.Errorf("nested options = %v", opts)
	}
	if nestedMap(t, opts, "uninstall")["keepHistory"] != false || nestedMap(t, opts, "uninstall")["description"] != "bye" {
		t.Errorf("uninstall = %v", opts["uninstall"])
	}
	creds := nestedMap(t, spec, "credentials")
	if nestedMap(t, creds, "secret")["name"] != "quay-creds" || nestedMap(t, creds, "secret")["namespace"] != "capi" || creds["key"] != "config.json" {
		t.Errorf("credentials = %v", creds)
	}
	tls := nestedMap(t, spec, "tlsConfig")
	if nestedMap(t, tls, "caSecret")["name"] != "repo-ca" || tls["insecureSkipTLSVerify"] != false {
		t.Errorf("tls = %v", tls)
	}

	back := readLiveHelmChartProxy(obj)
	if back.ChartName != in.ChartName || back.RepoURL != in.RepoURL || back.Version != in.Version || back.ReleaseName != in.ReleaseName || back.ReleaseNamespace != in.ReleaseNamespace || back.Values != in.Values || back.Strategy != "Continuous" {
		t.Errorf("read back = %+v", back)
	}
	if back.ClusterSelector[clusterNameLabel] != "prod" || back.CredentialsName != "quay-creds" || back.CredentialsKey != "config.json" || back.CASecret != "repo-ca" || back.InsecureSkipTLS == nil || *back.InsecureSkipTLS {
		t.Errorf("read back = %+v", back)
	}
	o := back.Options
	if o.Wait == nil || !*o.Wait || o.Atomic != nil || o.Timeout == nil || *o.Timeout != 10*time.Minute || o.CreateNamespace == nil || o.Force == nil || o.MaxHistory == nil || *o.MaxHistory != 3 || o.KeepHistory == nil || o.UninstallDescription != "bye" {
		t.Errorf("read back options = %+v", o)
	}

	// Minimal: nothing optional rendered, the strategy defaulted.
	minimal := helmChartProxyInputs{Name: "x", Namespace: "capi", APIVersion: in.APIVersion, ClusterSelector: map[string]string{"tier": "edge"}, ChartName: "c", RepoURL: "https://r"}
	spec = nestedMap(t, renderHelmChartProxy(minimal).Object, "spec")
	for _, absent := range []string{"version", "releaseName", "namespace", "valuesTemplate", "options", "credentials", "tlsConfig"} {
		if _, ok := spec[absent]; ok {
			t.Errorf("%s rendered without a value", absent)
		}
	}
	if spec["reconcileStrategy"] != "Continuous" {
		t.Error("strategy defaults to Continuous")
	}
}

func TestHelmChartProxyInputs_Validate(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*helmChartProxyInputs)
		want   string
	}{
		"ok":          {func(*helmChartProxyInputs) {}, ""},
		"no selector": {func(in *helmChartProxyInputs) { in.ClusterSelector = nil }, "one of cluster or cluster_selector"},
		"no chart":    {func(in *helmChartProxyInputs) { in.ChartName = "" }, "chart.name and chart.repository"},
		"bad repo":    {func(in *helmChartProxyInputs) { in.RepoURL = "quay.io/x" }, "must start with"},
		"bad strategy": {func(in *helmChartProxyInputs) {
			in.Strategy = "Sometimes"
		}, "not Continuous or InstallOnce"},
	} {
		t.Run(name, func(t *testing.T) {
			in := helmInputs()
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

func helmProxyObject(generation, observed int64, matching []string, conds []map[string]any) *unstructured.Unstructured {
	obj := renderHelmChartProxy(helmInputs())
	obj.SetGeneration(generation)
	clusters := make([]any, 0, len(matching))
	for _, c := range matching {
		clusters = append(clusters, map[string]any{"kind": "Cluster", "name": c})
	}
	raw := make([]any, 0, len(conds))
	for _, c := range conds {
		raw = append(raw, c)
	}
	obj.Object["status"] = map[string]any{"observedGeneration": observed, "matchingClusters": clusters, "conditions": raw}
	return obj
}

func releaseObject(cluster, status string, revision, generation, observed int64, ready bool, message string) *unstructured.Unstructured {
	obj := newObject(gvkFor("addons.cluster.x-k8s.io/v1alpha1", "HelmReleaseProxy"), "capi", "cilium-"+cluster, map[string]string{helmChartProxyLabel: "cilium"}, nil)
	obj.SetGeneration(generation)
	obj.Object["spec"] = map[string]any{"clusterRef": map[string]any{"name": cluster}}
	readyStatus := "False"
	if ready {
		readyStatus = "True"
	}
	obj.Object["status"] = map[string]any{
		"status": status, "revision": revision, "observedGeneration": observed,
		"conditions": []any{map[string]any{"type": helmReleaseReady, "status": readyStatus, "reason": "HelmInstallOrUpgradeFailed", "message": message}},
	}
	return obj
}

func TestHelmChartProxyReady(t *testing.T) {
	trueConds := []map[string]any{{"type": helmReleaseProxySpecsUpToDate, "status": "True"}, {"type": helmReleaseProxiesReady, "status": "True"}}
	// Stale generation: not ready whatever the conditions say.
	if ready, why := helmChartProxyReady(helmProxyObject(2, 1, nil, trueConds), nil, helmChartProxyWaitDefaults); ready || !strings.Contains(why, "current generation") {
		t.Errorf("stale = %v %q", ready, why)
	}
	// Vacuously ready with no matching clusters.
	if ready, _ := helmChartProxyReady(helmProxyObject(2, 2, nil, trueConds), nil, helmChartProxyWaitDefaults); !ready {
		t.Error("no clusters, conditions True: ready")
	}
	// A matched cluster without a release yet.
	if ready, why := helmChartProxyReady(helmProxyObject(2, 2, []string{"prod"}, trueConds), nil, helmChartProxyWaitDefaults); ready || !strings.Contains(why, "no HelmReleaseProxy for prod") {
		t.Errorf("missing release = %v %q", ready, why)
	}
	failed := readHelmRelease(releaseObject("prod", "failed", 2, 1, 1, false, "Error: timed out waiting for the condition"))
	if ready, why := helmChartProxyReady(helmProxyObject(2, 2, []string{"prod"}, trueConds), []helmRelease{failed}, helmChartProxyWaitDefaults); ready || !strings.Contains(why, "prod: failed (revision 2): Error: timed out") {
		t.Errorf("failed release = %v %q", ready, why)
	}
	// A release the release controller has not caught up on.
	stale := readHelmRelease(releaseObject("prod", "deployed", 2, 3, 2, true, ""))
	if ready, _ := helmChartProxyReady(helmProxyObject(2, 2, []string{"prod"}, trueConds), []helmRelease{stale}, helmChartProxyWaitDefaults); ready {
		t.Error("a release at a stale generation is not ready")
	}
	good := readHelmRelease(releaseObject("prod", "deployed", 2, 3, 3, true, ""))
	if ready, why := helmChartProxyReady(helmProxyObject(2, 2, []string{"prod"}, trueConds), []helmRelease{good}, helmChartProxyWaitDefaults); !ready {
		t.Errorf("ready release = %v %q", ready, why)
	}
	if good.String() != "prod: deployed (revision 2)" {
		t.Errorf("String = %q", good.String())
	}
	// A False condition names itself.
	falseConds := []map[string]any{{"type": helmReleaseProxySpecsUpToDate, "status": "True"}, {"type": helmReleaseProxiesReady, "status": "False", "reason": "HelmInstallOrUpgradeFailed", "message": "1 of 1 completed"}}
	if ready, why := helmChartProxyReady(helmProxyObject(2, 2, []string{"prod"}, falseConds), []helmRelease{good}, helmChartProxyWaitDefaults); ready || !strings.Contains(why, "HelmReleaseProxiesReady=False") {
		t.Errorf("false condition = %v %q", ready, why)
	}
	// No conditions requested: only the releases matter.
	if ready, _ := helmChartProxyReady(helmProxyObject(2, 2, []string{"prod"}, nil), []helmRelease{good}, nil); !ready {
		t.Error("no conditions, good release: ready")
	}
	if names := matchingClusterNames(helmProxyObject(1, 1, []string{"b", "a"}, nil)); strings.Join(names, ",") != "a,b" {
		t.Errorf("matching = %v", names)
	}
}

const crsManifests = `apiVersion: v1
kind: Namespace
metadata:
  name: gateway-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gateway
  namespace: gateway-system
spec:
  replicas: 2
---
`

func TestResourceSetRender(t *testing.T) {
	in := resourceSetInputs{
		Name: "gateway", Namespace: "capi", APIVersion: "addons.cluster.x-k8s.io/v1beta2",
		ClusterSelector: map[string]string{clusterNameLabel: "prod"}, Strategy: "Reconcile",
		Manifests: []string{crsManifests}, Secrets: []string{"apiVersion: v1\nkind: Secret\nmetadata:\n  name: token\n  namespace: gateway-system\nstringData:\n  t: x\n"},
	}
	objects, err := in.objects()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objects {
		got = append(got, o.String())
	}
	if strings.Join(got, ";") != "Namespace gateway-system;Deployment gateway-system/gateway;Secret gateway-system/token" {
		t.Errorf("objects = %v", got)
	}
	crs, holders := in.render()
	if crs.GetAPIVersion() != in.APIVersion || crs.GetKind() != "ClusterResourceSet" || crs.GetName() != "gateway" {
		t.Errorf("identity = %s %s %s", crs.GetAPIVersion(), crs.GetKind(), crs.GetName())
	}
	spec := nestedMap(t, crs.Object, "spec")
	if spec["strategy"] != "Reconcile" || nestedMap(t, spec, "clusterSelector", "matchLabels")[clusterNameLabel] != "prod" {
		t.Errorf("spec = %v", spec)
	}
	refs := resourceRefs(crs)
	if len(refs) != 2 || refs[0].Kind != "ConfigMap" || refs[0].Name != "gateway-0" || refs[1].Kind != "Secret" || refs[1].Name != "gateway-secret-0" {
		t.Errorf("refs = %v", refs)
	}
	if len(holders) != 2 {
		t.Fatalf("holders = %d", len(holders))
	}
	cm, secret := holders[0], holders[1]
	if cm.GetKind() != "ConfigMap" || cm.GetName() != "gateway-0" || cm.GetLabels()[resourceSetLabel] != "gateway" || nestedMap(t, cm.Object, "data")[resourceSetManifestKey] != crsManifests {
		t.Errorf("configmap = %v", cm.Object)
	}
	if secret.GetKind() != "Secret" || secret.GetName() != "gateway-secret-0" || secret.Object["type"] != resourceSetSecretType || nestedMap(t, secret.Object, "stringData")[resourceSetManifestKey] == "" {
		t.Errorf("secret = %v", secret.Object)
	}
	// Default strategy.
	in.Strategy = ""
	crs, _ = in.render()
	if nestedMap(t, crs.Object, "spec")["strategy"] != "ApplyOnce" {
		t.Error("strategy defaults to ApplyOnce")
	}
}

func TestValidateManifest(t *testing.T) {
	if err := validateManifest(crsManifests); err != nil {
		t.Errorf("valid: %v", err)
	}
	if err := validateManifest("apiVersion: v1\nkind: ConfigMap\n"); err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("nameless: %v", err)
	}
	if err := validateManifest("metadata:\n  name: x\n"); err == nil {
		t.Error("a document without a kind must fail")
	}
	if err := validateManifest(strings.Repeat("x", configMapLimit+1)); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("too large: %v", err)
	}
}

func TestBindingSatisfied(t *testing.T) {
	expected := []appliedObject{{Kind: "ConfigMap", Name: "gateway-0"}, {Kind: "Secret", Name: "gateway-secret-0"}}
	binding := newObject(gvkFor("addons.cluster.x-k8s.io/v1beta2", "ClusterResourceSetBinding"), "capi", "prod", nil, nil)
	binding.Object["spec"] = map[string]any{"bindings": []any{
		map[string]any{"clusterResourceSetName": "other", "resources": []any{map[string]any{"kind": "ConfigMap", "name": "x", "applied": true}}},
		map[string]any{"clusterResourceSetName": "gateway", "resources": []any{
			map[string]any{"kind": "ConfigMap", "name": "gateway-0", "applied": true, "hash": "sha256:abc"},
			map[string]any{"kind": "Secret", "name": "gateway-secret-0", "applied": false},
		}},
	}}
	bound, ok := readBinding(binding, "gateway")
	if !ok || len(bound) != 2 || bound[0].Hash != "sha256:abc" || !bound[0].Applied || bound[1].Applied {
		t.Errorf("binding = %v %v", bound, ok)
	}
	if _, ok := readBinding(binding, "missing"); ok {
		t.Error("a set the binding does not name is not bound")
	}
	if ok, why := bindingSatisfied(bound, expected); ok || !strings.Contains(why, "Secret gateway-secret-0 not applied") {
		t.Errorf("unapplied = %v %q", ok, why)
	}
	bound[1].Applied = true
	if ok, _ := bindingSatisfied(bound, expected); !ok {
		t.Error("all applied")
	}
	if ok, why := bindingSatisfied(bound[:1], expected); ok || !strings.Contains(why, "not bound yet") {
		t.Errorf("missing = %v %q", ok, why)
	}
}

func TestObjectReady(t *testing.T) {
	mk := func(apiVersion, kind string, generation int64, spec, status map[string]any) *unstructured.Unstructured {
		obj := newObject(gvkFor(apiVersion, kind), "ns", "x", nil, nil)
		obj.SetGeneration(generation)
		if spec != nil {
			obj.Object["spec"] = spec
		}
		if status != nil {
			obj.Object["status"] = status
		}
		return obj
	}
	for name, tc := range map[string]struct {
		obj   *unstructured.Unstructured
		ready bool
		why   string
	}{
		"deployment unobserved": {mk("apps/v1", "Deployment", 2, map[string]any{"replicas": int64(2)}, map[string]any{"observedGeneration": int64(1), "availableReplicas": int64(2)}), false, "not observed"},
		"deployment partial":    {mk("apps/v1", "Deployment", 2, map[string]any{"replicas": int64(2)}, map[string]any{"observedGeneration": int64(2), "availableReplicas": int64(1)}), false, "1 of 2 replicas"},
		"deployment ready":      {mk("apps/v1", "Deployment", 2, map[string]any{"replicas": int64(2)}, map[string]any{"observedGeneration": int64(2), "availableReplicas": int64(2)}), true, ""},
		"deployment default":    {mk("apps/v1", "StatefulSet", 1, map[string]any{}, map[string]any{"observedGeneration": int64(1), "availableReplicas": int64(1)}), true, ""},
		"daemonset":             {mk("apps/v1", "DaemonSet", 1, nil, map[string]any{"desiredNumberScheduled": int64(3), "numberReady": int64(2)}), false, "2 of 3 pods"},
		"job complete":          {mk("batch/v1", "Job", 1, nil, map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}), true, ""},
		"job failed":            {mk("batch/v1", "Job", 1, nil, map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True", "message": "backoff"}}}), false, "failed: backoff"},
		"crd":                   {mk("apiextensions.k8s.io/v1", "CustomResourceDefinition", 1, nil, map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "False", "reason": "Installing"}}}), false, "not established"},
		"ready condition":       {mk("cilium.io/v2alpha1", "CiliumLoadBalancerIPPool", 1, nil, map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "message": "conflict"}}}), false, "Ready=False: conflict"},
		"plain object":          {mk("v1", "Namespace", 1, nil, nil), true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ready, why := objectReady(tc.obj)
			if ready != tc.ready || !strings.Contains(why, tc.why) {
				t.Errorf("ready = %v why = %q", ready, why)
			}
		})
	}
}

func TestSelectorHelpers(t *testing.T) {
	if got := addonClusterSelector("prod", map[string]string{"tier": "x"}); got[clusterNameLabel] != "prod" || len(got) != 1 {
		t.Errorf("cluster wins: %v", got)
	}
	if got := addonClusterSelector("", map[string]string{"tier": "x"}); got["tier"] != "x" {
		t.Errorf("selector = %v", got)
	}
}
