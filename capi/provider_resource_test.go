// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
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
		if _, ok := resp.Schema.Attributes["resource_config"]; ok &&
			tc.typeName == "capi_provider" {
			t.Error("capi_provider has no resource_config: it renders no cluster object")
		}
		meta := &resource.MetadataResponse{}
		tc.res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "capi"}, meta)
		if meta.TypeName != tc.typeName {
			t.Errorf("type name = %s", meta.TypeName)
		}
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
			f.installed = append(
				f.installed,
				capi.InstalledProvider{
					Name:      p.Name,
					Type:      p.Type,
					Version:   orDefault(p.Version, "v9.9.9"),
					Namespace: p.Name + "-system",
				},
			)
		}
	}
	return nil
}

func (f *fakeInstaller) Upgrade(
	_ context.Context,
	_ *capi.Cluster,
	opts capi.UpgradeOptions,
) error {
	f.upgrades = append(f.upgrades, opts.Providers)
	for i := range f.installed {
		for _, ps := range opts.Providers {
			for _, p := range ps {
				if f.installed[i].Name == p.Name && f.installed[i].Type == p.Type &&
					p.Version != "" {
					f.installed[i].Version = p.Version
				}
			}
		}
	}
	return nil
}

func (f *fakeInstaller) Uninstall(
	_ context.Context,
	_ *capi.Cluster,
	opts capi.UninstallOptions,
) error {
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

func (f *fakeInstaller) Installed(
	context.Context,
	*capi.Cluster,
) ([]capi.InstalledProvider, error) {
	return f.installed, nil
}

func TestProviderResource_ConfigAndInstalled(t *testing.T) {
	ctx := context.Background()
	fake := &fakeInstaller{}
	r := &providerResource{installer: fake}
	fc, _ := types.ObjectValueFrom(
		ctx,
		fetchConfigAttrTypes(),
		fetchConfigModel{
			Owner:      types.StringValue("tinkerbell-community"),
			Repository: types.StringNull(),
			URL:        types.StringNull(),
			OCI:        types.StringNull(),
		},
	)
	vars, _ := types.MapValueFrom(
		ctx,
		types.StringType,
		map[string]string{"CLUSTER_TOPOLOGY": "true"},
	)
	data := &providerResourceModel{
		Type: types.StringValue(
			"infrastructure",
		),
		Name:        types.StringValue("tinkerbell"),
		Version:     types.StringValue("v0.7.9"),
		FetchConfig: fc,
		ConfigVariables: types.MapNull(
			types.StringType,
		),
		SecretConfigVariables: types.MapNull(types.StringType),
		Deployment: types.ObjectNull(
			deploymentAttrTypes(),
		),
		Manager:             types.ObjectNull(managerAttrTypes()),
		AdditionalManifests: types.StringNull(),
		ManifestPatches: types.ListNull(
			types.StringType,
		),
		Patches:   types.ListNull(types.ObjectType{AttrTypes: patchAttrTypes()}),
		Variables: vars,
	}
	cfg, got, diags := r.config(ctx, data)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if cfg.Type != capi.ProviderTypeInfrastructure || cfg.Name != "tinkerbell" ||
		cfg.Version != "v0.7.9" ||
		cfg.FetchConfig == nil ||
		cfg.FetchConfig.Owner != "tinkerbell-community" ||
		cfg.ResourceConfig != nil {
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
	if err := fake.Init(
		ctx,
		cluster,
		capi.InitOptions{Providers: set, NoDefaults: true},
	); err != nil {
		t.Fatal(err)
	}
	inst, err := r.installed(ctx, cluster, cfg.Type, cfg.Name)
	if err != nil || inst == nil || inst.Version != "v0.7.9" ||
		inst.Namespace != "tinkerbell-system" {
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

// A provider installed before the resource existed is adopted: init
// converges it, the resource records the adoption and warns, and the
// deletion that follows leaves it installed. One the resource installed
// itself is deleted as before.

func TestProviderResource_Adoption(t *testing.T) {
	ctx := context.Background()
	cluster := &capi.Cluster{Name: "mgmt"}
	cfg := capi.ProviderConfig{
		Type:    capi.ProviderTypeInfrastructure,
		Name:    "tinkerbell",
		Version: "v0.7.9",
	}

	fake := &fakeInstaller{
		installed: []capi.InstalledProvider{
			{
				Name:      "tinkerbell",
				Type:      capi.ProviderTypeInfrastructure,
				Version:   "v0.7.8",
				Namespace: "capt-system",
			},
		},
	}
	r := &providerResource{installer: fake}
	store := fakePrivate{}
	inst, diags := r.install(ctx, cluster, cfg, nil, store)
	if diags.HasError() || inst == nil {
		t.Fatal(diags)
	}
	if adopted, _ := providerAdopted(ctx, store); !adopted {
		t.Error("a provider that was already installed is recorded as adopted")
	}
	if len(diags.Warnings()) != 1 ||
		!strings.Contains(diags.Warnings()[0].Detail(), "v0.7.8 in capt-system") {
		t.Errorf("the adoption is announced: %v", diags)
	}

	fresh := &fakeInstaller{}
	r = &providerResource{installer: fresh}
	store = fakePrivate{}
	if _, diags := r.install(
		ctx,
		cluster,
		cfg,
		nil,
		store,
	); diags.HasError() ||
		len(diags.Warnings()) != 0 {
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
