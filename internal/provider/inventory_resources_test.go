// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

func TestInventoryResources_Schema(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		res      resource.Resource
		typeName string
		attrs    []string
	}{
		{NewHardwareResource(), "capi_hardware", []string{"cluster", "network", "disk", "bmc", "labels", "adopted", "force", "manifest", "status"}},
		{NewProviderResource(), "capi_provider", []string{"type", "name", "version", "fetch_config", "manager", "patches", "variables", "installed_version", "namespace", "kinds", "ref"}},
		{NewClusterClassResource(), "capi_cluster_class", []string{"infrastructure", "control_plane", "workers", "variables", "patches", "wait", "classes", "ref", "conditions"}},
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
		if _, ok := resp.Schema.Attributes["resource_config"]; ok && tc.typeName == "capi_provider" {
			t.Error("capi_provider has no resource_config: it renders no cluster object")
		}
		meta := &resource.MetadataResponse{}
		tc.res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "capi"}, meta)
		if meta.TypeName != tc.typeName {
			t.Errorf("type name = %s", meta.TypeName)
		}
	}
}

func TestHardwareObjectsAndReadback(t *testing.T) {
	m := capi.InventoryMachine{
		Hostname: "cp-1", IP: "10.0.80.11", Netmask: "255.255.255.0", Gateway: "10.0.80.1", MAC: "aa:bb:cc:dd:ee:01",
		Nameservers: []string{"10.0.80.1"}, VLANID: "80", Disk: "/dev/nvme0n1",
		BMC:    &capi.InventoryBMC{Address: "10.0.80.101:16992", Username: "admin", Password: "secret"},
		Labels: map[string]string{"type": "cp"},
	}
	objs, err := capi.HardwareObjects("capi", "prod", m, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 || objs[0].GetKind() != "Secret" || objs[1].GetKind() != capi.RufioMachineKind || objs[2].GetKind() != capi.HardwareKind {
		t.Fatalf("objects = %d", len(objs))
	}
	if objs[0].GetName() != capi.BMCSecretName("cp-1") || objs[0].GetNamespace() != "capi" {
		t.Errorf("secret = %s/%s", objs[0].GetNamespace(), objs[0].GetName())
	}
	hw := objs[2]
	if hw.GetLabels()[clusterNameLabel] != "prod" || hw.GetLabels()["type"] != "cp" {
		t.Errorf("hardware labels = %v", hw.GetLabels())
	}
	f := readHardware(&hw)
	if f.Cluster != "prod" || f.MAC != m.MAC || f.IP != m.IP || f.Netmask != m.Netmask || f.Gateway != m.Gateway || f.VLANID != "80" || f.Disk != m.Disk {
		t.Errorf("facts = %+v", f)
	}
	if strings.Join(f.Nameservers, ",") != "10.0.80.1" || f.Labels["type"] != "cp" || f.Labels[clusterNameLabel] != "" {
		t.Errorf("facts = %+v", f)
	}
	if f.Owner != "" || f.Provisioned || !f.AllowPXE || f.Adopted {
		t.Errorf("fresh hardware = %+v", f)
	}

	// Adopted: provisioned and off PXE.
	objs, _ = capi.HardwareObjects("capi", "prod", m, true)
	f = readHardware(&objs[2])
	if !f.Provisioned || f.AllowPXE || !f.Adopted {
		t.Errorf("adopted = %+v", f)
	}

	// Claimed by CAPT: the owner labels are status, not user labels.
	claimed := objs[2].DeepCopy()
	labels := claimed.GetLabels()
	labels[capi.HardwareOwnerNameLabel] = "prod-workers-abc"
	labels[capi.HardwareOwnerNamespaceLabel] = "capi"
	claimed.SetLabels(labels)
	f = readHardware(claimed)
	if f.Owner != "prod-workers-abc" || f.OwnerNamespace != "capi" || len(f.Labels) != 1 {
		t.Errorf("claimed = %+v", f)
	}

	// No BMC: two objects fewer; a missing hostname is an error.
	m.BMC = nil
	objs, _ = capi.HardwareObjects("", "", m, false)
	if len(objs) != 1 || objs[0].GetNamespace() != "default" || objs[0].GetLabels()[clusterNameLabel] != "" {
		t.Errorf("no bmc = %v", objs)
	}
	if _, err := capi.HardwareObjects("capi", "prod", capi.InventoryMachine{}, false); err == nil {
		t.Error("hostname is required")
	}
	if !addressesMatch("https://10.0.80.101:16992/", "10.0.80.101:16992") || addressesMatch("10.0.80.101", "10.0.80.101:443") {
		t.Error("addressesMatch")
	}
}

func classInputs(core string) clusterClassInputs {
	return clusterClassInputs{
		Name: "prod", Namespace: "capi", CoreVersion: core,
		Infrastructure:     &objectRef{APIGroup: "infrastructure.cluster.x-k8s.io", APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1", Kind: "TinkerbellClusterTemplate", Name: "prod-1234abcd", Namespace: "capi"},
		ControlPlane:       &objectRef{APIGroup: "controlplane.cluster.x-k8s.io", APIVersion: "controlplane.cluster.x-k8s.io/v1alpha3", Kind: "TalosControlPlaneTemplate", Name: "prod-cp-aaaa1111"},
		ControlPlaneInfra:  &objectRef{APIGroup: "infrastructure.cluster.x-k8s.io", APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1", Kind: "TinkerbellMachineTemplate", Name: "cp-bbbb2222"},
		ControlPlaneLabels: map[string]string{"role": "cp"},
		Workers: []workerClassInputs{
			{Name: "worker-arm64", Infrastructure: &objectRef{APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1", Kind: "TinkerbellMachineTemplate", Name: "arm-1"}, Bootstrap: &objectRef{APIVersion: "bootstrap.cluster.x-k8s.io/v1alpha3", Kind: "TalosConfigTemplate", Name: "worker-1"}},
			{Name: "worker-amd64", Infrastructure: &objectRef{APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1", Kind: "TinkerbellMachineTemplate", Name: "amd-1"}, Bootstrap: &objectRef{APIVersion: "bootstrap.cluster.x-k8s.io/v1alpha3", Kind: "TalosConfigTemplate", Name: "worker-1"}, FailureDomain: "rack-1", Labels: map[string]string{"tier": "worker"}},
		},
		Variables: []any{map[string]any{"name": "diskSize", "required": false, "schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "string"}}}},
		Patches:   []any{map[string]any{"name": "diskSize", "definitions": []any{}}},
	}
}

func TestRenderClusterClass_V1Beta2(t *testing.T) {
	in := classInputs("v1beta2")
	obj := renderClusterClass(in)
	if obj.GetAPIVersion() != "cluster.x-k8s.io/v1beta2" || obj.GetKind() != "ClusterClass" || obj.GetName() != "prod" || obj.GetNamespace() != "capi" {
		t.Errorf("identity = %s %s %s/%s", obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName())
	}
	infra := nestedMap(t, obj.Object, "spec", "infrastructure", "templateRef")
	if infra["kind"] != "TinkerbellClusterTemplate" || infra["name"] != "prod-1234abcd" || infra["apiVersion"] != "infrastructure.cluster.x-k8s.io/v1beta1" {
		t.Errorf("infrastructure = %v", infra)
	}
	if _, ok := infra["namespace"]; ok {
		t.Error("v1beta2 template refs carry no namespace")
	}
	if nestedMap(t, obj.Object, "spec", "controlPlane", "templateRef")["name"] != "prod-cp-aaaa1111" || nestedMap(t, obj.Object, "spec", "controlPlane", "machineInfrastructure", "templateRef")["name"] != "cp-bbbb2222" {
		t.Errorf("controlPlane = %v", nestedMap(t, obj.Object, "spec", "controlPlane"))
	}
	if nestedMap(t, obj.Object, "spec", "controlPlane", "metadata", "labels")["role"] != "cp" {
		t.Error("control plane labels")
	}
	mds := nestedSlice(t, obj.Object, "spec", "workers", "machineDeployments")
	if len(mds) != 2 || asMap(t, mds[0])["class"] != "worker-amd64" || asMap(t, mds[1])["class"] != "worker-arm64" {
		t.Fatalf("classes are sorted: %v", mds)
	}
	amd := asMap(t, mds[0])
	if nestedMap(t, amd, "infrastructure", "templateRef")["name"] != "amd-1" || nestedMap(t, amd, "bootstrap", "templateRef")["name"] != "worker-1" || amd["failureDomain"] != "rack-1" || nestedMap(t, amd, "metadata", "labels")["tier"] != "worker" {
		t.Errorf("amd64 class = %v", amd)
	}
	if _, ok := amd["template"]; ok {
		t.Error("v1beta2 classes have no template holder")
	}
	if len(nestedSlice(t, obj.Object, "spec", "variables")) != 1 || len(nestedSlice(t, obj.Object, "spec", "patches")) != 1 {
		t.Error("variables and patches pass through")
	}
	live := readLiveClusterClass(obj)
	if live.Infrastructure != "prod-1234abcd" || live.ControlPlane != "prod-cp-aaaa1111" || live.ControlPlaneInfra != "cp-bbbb2222" {
		t.Errorf("live = %+v", live)
	}
	if w := live.Workers["worker-amd64"]; w.Infrastructure != "amd-1" || w.Bootstrap != "worker-1" || w.FailureDomain != "rack-1" {
		t.Errorf("live worker = %+v", w)
	}
	if len(live.Variables) != 1 || len(live.Patches) != 1 {
		t.Errorf("live variables/patches = %v %v", live.Variables, live.Patches)
	}
}

func TestRenderClusterClass_V1Beta1(t *testing.T) {
	in := classInputs("v1beta1")
	obj := renderClusterClass(in)
	ref := nestedMap(t, obj.Object, "spec", "infrastructure", "ref")
	if ref["name"] != "prod-1234abcd" || ref["namespace"] != "capi" {
		t.Errorf("legacy infrastructure ref = %v", ref)
	}
	if nestedMap(t, obj.Object, "spec", "controlPlane", "ref")["name"] != "prod-cp-aaaa1111" || nestedMap(t, obj.Object, "spec", "controlPlane", "machineInfrastructure", "ref")["namespace"] != "capi" {
		t.Errorf("legacy controlPlane = %v", nestedMap(t, obj.Object, "spec", "controlPlane"))
	}
	amd := asMap(t, nestedSlice(t, obj.Object, "spec", "workers", "machineDeployments")[0])
	if nestedMap(t, amd, "template", "infrastructure", "ref")["name"] != "amd-1" || nestedMap(t, amd, "template", "bootstrap", "ref")["name"] != "worker-1" || nestedMap(t, amd, "template", "metadata", "labels")["tier"] != "worker" {
		t.Errorf("legacy class = %v", amd)
	}
	live := readLiveClusterClass(obj)
	if live.Infrastructure != "prod-1234abcd" || live.Workers["worker-arm64"].Infrastructure != "arm-1" {
		t.Errorf("legacy live = %+v", live)
	}
	// Minimal: no workers, no variables.
	minimal := clusterClassInputs{Name: "x", Namespace: "capi", CoreVersion: "v1beta2", Infrastructure: in.Infrastructure, ControlPlane: in.ControlPlane}
	spec := nestedMap(t, renderClusterClass(minimal).Object, "spec")
	for _, absent := range []string{"workers", "variables", "patches"} {
		if _, ok := spec[absent]; ok {
			t.Errorf("%s rendered without a value", absent)
		}
	}
	if _, ok := nestedMap(t, spec, "controlPlane")["machineInfrastructure"]; ok {
		t.Error("no machineInfrastructure without a machine template")
	}
	if err := minimal.validate(); err != nil {
		t.Errorf("minimal is valid: %v", err)
	}
	minimal.ControlPlane = nil
	if err := minimal.validate(); err == nil || !strings.Contains(err.Error(), "control_plane.template") {
		t.Errorf("missing control plane: %v", err)
	}
	minimal.Workers = []workerClassInputs{{Name: "w"}}
	if err := minimal.validate(); err == nil || !strings.Contains(err.Error(), `worker class "w"`) {
		t.Errorf("incomplete worker: %v", err)
	}
}

func TestDynamicListRoundTrip(t *testing.T) {
	list := []any{map[string]any{"name": "diskSize", "required": true, "schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "string", "default": "100Gi"}}}}
	d, diags := dynamicFromList(list)
	if diags.HasError() {
		t.Fatal(diags)
	}
	back, diags := dynamicToList(d, "variables")
	if diags.HasError() || len(back) != 1 {
		t.Fatalf("back = %v %v", back, diags)
	}
	if m, ok := back[0].(map[string]any); !ok || m["name"] != "diskSize" || m["required"] != true {
		t.Errorf("back = %v", back)
	}
	if _, diags := dynamicToList(types.DynamicValue(types.StringValue("x")), "variables"); !diags.HasError() {
		t.Error("a string is not a list")
	}
	if l, diags := dynamicToList(types.DynamicNull(), "variables"); diags.HasError() || l != nil {
		t.Error("null is nil")
	}
	if d, _ := dynamicFromList(nil); !d.IsNull() {
		t.Error("nil is null")
	}
}

// fakeInstaller records the clusterctl calls the provider resource makes.
type fakeInstaller struct {
	inits, upgrades, uninstalls []capi.ProviderSet
	initOpts                    []capi.InitOptions
	installed                   []capi.InstalledProvider
}

func (f *fakeInstaller) Init(_ context.Context, _ *capi.Cluster, opts capi.InitOptions) error {
	f.inits = append(f.inits, opts.Providers)
	f.initOpts = append(f.initOpts, opts)
	for _, ps := range opts.Providers {
		for _, p := range ps {
			f.installed = append(f.installed, capi.InstalledProvider{Name: p.Name, Type: p.Type, Version: orDefault(p.Version, "v9.9.9"), Namespace: p.Name + "-system"})
		}
	}
	return nil
}

func (f *fakeInstaller) Upgrade(_ context.Context, _ *capi.Cluster, opts capi.UpgradeOptions) error {
	f.upgrades = append(f.upgrades, opts.Providers)
	for i := range f.installed {
		for _, ps := range opts.Providers {
			for _, p := range ps {
				if f.installed[i].Name == p.Name && f.installed[i].Type == p.Type && p.Version != "" {
					f.installed[i].Version = p.Version
				}
			}
		}
	}
	return nil
}

func (f *fakeInstaller) Uninstall(_ context.Context, _ *capi.Cluster, opts capi.UninstallOptions) error {
	f.uninstalls = append(f.uninstalls, opts.Providers)
	var keep []capi.InstalledProvider
	for _, inst := range f.installed {
		removed := false
		for _, p := range opts.Providers[inst.Type] {
			if p.Name == inst.Name {
				removed = true
			}
		}
		if !removed {
			keep = append(keep, inst)
		}
	}
	f.installed = keep
	return nil
}

func (f *fakeInstaller) Installed(context.Context, *capi.Cluster) ([]capi.InstalledProvider, error) {
	return f.installed, nil
}

func TestProviderResource_ConfigAndInstalled(t *testing.T) {
	ctx := context.Background()
	fake := &fakeInstaller{}
	r := &ProviderResource{installer: fake}
	fc, _ := types.ObjectValueFrom(ctx, fetchConfigAttrTypes(), FetchConfigModel{Owner: types.StringValue("tinkerbell-community"), Repository: types.StringNull(), URL: types.StringNull(), OCI: types.StringNull()})
	vars, _ := types.MapValueFrom(ctx, types.StringType, map[string]string{"CLUSTER_TOPOLOGY": "true"})
	data := &ProviderResourceModel{
		Type: types.StringValue("infrastructure"), Name: types.StringValue("tinkerbell"), Version: types.StringValue("v0.7.9"), FetchConfig: fc,
		ConfigVariables: types.MapNull(types.StringType), SecretConfigVariables: types.MapNull(types.StringType),
		Deployment: types.ObjectNull(deploymentAttrTypes()), Manager: types.ObjectNull(managerAttrTypes()), AdditionalManifests: types.StringNull(),
		ManifestPatches: types.ListNull(types.StringType), Patches: types.ListNull(types.ObjectType{AttrTypes: patchAttrTypes()}), Variables: vars,
	}
	cfg, got, diags := r.config(ctx, data)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if cfg.Type != capi.ProviderTypeInfrastructure || cfg.Name != "tinkerbell" || cfg.Version != "v0.7.9" || cfg.FetchConfig == nil || cfg.FetchConfig.Owner != "tinkerbell-community" || cfg.ResourceConfig != nil {
		t.Errorf("config = %+v", cfg)
	}
	if got["CLUSTER_TOPOLOGY"] != "true" {
		t.Errorf("variables = %v", got)
	}
	data.Type = types.StringValue("infra")
	if _, _, diags := r.config(ctx, data); !diags.HasError() {
		t.Error("an unknown type is an error")
	}

	cluster := &capi.Cluster{Name: "mgmt"}
	if inst, _ := r.installed(ctx, cluster, cfg.Type, cfg.Name); inst != nil {
		t.Error("nothing installed yet")
	}
	set := capi.ProviderSet{}
	set.Add(cfg)
	if err := fake.Init(ctx, cluster, capi.InitOptions{Providers: set, NoDefaults: true}); err != nil {
		t.Fatal(err)
	}
	inst, err := r.installed(ctx, cluster, cfg.Type, cfg.Name)
	if err != nil || inst == nil || inst.Version != "v0.7.9" || inst.Namespace != "tinkerbell-system" {
		t.Errorf("installed = %+v %v", inst, err)
	}
	if !fake.initOpts[0].NoDefaults {
		t.Error("init asks clusterctl for no default providers")
	}
	if !sameVersion("v0.7.9", "0.7.9") || sameVersion("v0.7.9", "v0.8.0") {
		t.Error("sameVersion")
	}
}

func TestProviderResource_ImportIDs(t *testing.T) {
	for _, id := range []string{"infrastructure/tinkerbell", "control-plane/talos", "core/cluster-api"} {
		parts := strings.SplitN(id, "/", 2)
		if !contains(providerTypeNames, parts[0]) {
			t.Errorf("%s: type not accepted", id)
		}
	}
	if contains(providerTypeNames, "controlplane") {
		t.Error("clusterctl spells it control-plane")
	}
}

func TestReadHardware_Legacy(t *testing.T) {
	hw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": capi.HardwareAPIVersion, "kind": capi.HardwareKind,
		"metadata": map[string]any{"name": "x", "namespace": "capi"},
		"spec":     map[string]any{"interfaces": []any{map[string]any{"dhcp": map[string]any{"mac": "aa"}}}},
	}}
	f := readHardware(hw)
	if f.MAC != "aa" || !f.AllowPXE || f.Disk != "" || len(f.Labels) != 0 {
		t.Errorf("sparse hardware = %+v", f)
	}
}

// A provider installed before the resource existed is adopted: init
// converges it, the resource records the adoption and warns, and the
// deletion that follows leaves it installed. One the resource installed
// itself is deleted as before.
func TestProviderResource_Adoption(t *testing.T) {
	ctx := context.Background()
	cluster := &capi.Cluster{Name: "mgmt"}
	cfg := capi.ProviderConfig{Type: capi.ProviderTypeInfrastructure, Name: "tinkerbell", Version: "v0.7.9"}

	fake := &fakeInstaller{installed: []capi.InstalledProvider{{Name: "tinkerbell", Type: capi.ProviderTypeInfrastructure, Version: "v0.7.8", Namespace: "capt-system"}}}
	r := &ProviderResource{installer: fake}
	store := fakePrivate{}
	inst, diags := r.install(ctx, cluster, cfg, nil, store)
	if diags.HasError() || inst == nil {
		t.Fatal(diags)
	}
	if adopted, _ := providerAdopted(ctx, store); !adopted {
		t.Error("a provider that was already installed is recorded as adopted")
	}
	if len(diags.Warnings()) != 1 || !strings.Contains(diags.Warnings()[0].Detail(), "v0.7.8 in capt-system") {
		t.Errorf("the adoption is announced: %v", diags)
	}

	fresh := &fakeInstaller{}
	r = &ProviderResource{installer: fresh}
	store = fakePrivate{}
	if _, diags := r.install(ctx, cluster, cfg, nil, store); diags.HasError() || len(diags.Warnings()) != 0 {
		t.Fatalf("diags = %v", diags)
	}
	if adopted, _ := providerAdopted(ctx, store); adopted {
		t.Error("a provider the resource installed is its own")
	}
	if diags := r.uninstall(ctx, cluster, cfg); diags.HasError() || len(fresh.uninstalls) != 1 {
		t.Errorf("the resource's own provider is deleted: %v %d", diags, len(fresh.uninstalls))
	}
	if diags := r.uninstall(ctx, cluster, cfg); diags.HasError() || len(fresh.uninstalls) != 1 {
		t.Error("a provider already gone is not an error and not deleted twice")
	}
}
