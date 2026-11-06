// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/adrg/xdg"
	"github.com/spf13/viper"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
)

// providerOverride is one entry of the clusterctl "providers" config list.
type providerOverride struct {
	Name string                    `json:"name" mapstructure:"name"`
	Type clusterctlv1.ProviderType `json:"type" mapstructure:"type"`
	URL  string                    `json:"url"  mapstructure:"url"`
}

// overlayReader is a clusterctl config.Reader backed by its own viper
// instance (clusterctl's default reader uses the global one, which is unsafe
// with concurrent resources). It reads the user's clusterctl.yaml and
// environment like clusterctl does, and layers provider URL overrides on top
// of the file's "providers" list. Overrides may be added after Init; clusterctl
// re-reads the key on every lookup.
type overlayReader struct {
	v *viper.Viper

	mu        sync.Mutex
	overrides []providerOverride
}

var _ config.Reader = &overlayReader{}

func newOverlayReader() *overlayReader {
	return &overlayReader{v: viper.New()}
}

// newOverlayConfigClient initializes an overlayReader from configPath (or
// clusterctl's default locations when empty) and wraps it in a clusterctl
// config client. config.New does not Init injected readers, so this helper
// does it explicitly.
func newOverlayConfigClient(
	ctx context.Context,
	configPath string,
) (config.Client, *overlayReader, error) {
	reader := newOverlayReader()
	if err := reader.Init(ctx, configPath); err != nil {
		return nil, nil, err
	}
	client, err := config.New(ctx, configPath, config.InjectReader(reader))
	if err != nil {
		return nil, nil, err
	}
	return client, reader, nil
}

// lookupDefaultProvider returns clusterctl's built-in entry for p, if any.
// clusterctl prefixes some names with their organization ("tinkerbell-tinkerbell",
// "harvester-harvester"); a plain name is also matched against that
// "<name>-<name>" form. exact reports whether p.Name itself is known.
func lookupDefaultProvider(
	client config.Client,
	p ProviderConfig,
) (def config.Provider, exact bool) {
	typ := p.Type.Clusterctl()
	if d, err := client.Providers().Get(p.Name, typ); err == nil {
		return d, true
	}
	if d, err := client.Providers().Get(p.Name+"-"+p.Name, typ); err == nil {
		return d, false
	}
	return nil, false
}

// AddOverride registers (or replaces) the URL for a provider.
func (r *overlayReader) AddOverride(name string, t clusterctlv1.ProviderType, url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.overrides {
		if r.overrides[i].Name == name && r.overrides[i].Type == t {
			r.overrides[i].URL = url
			return
		}
	}
	r.overrides = append(r.overrides, providerOverride{Name: name, Type: t, URL: url})
}

// Init mirrors clusterctl's viper reader: env vars with "-" mapped to "_",
// an explicit file path, or the first default config file that exists.
func (r *overlayReader) Init(_ context.Context, path string) error {
	r.v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	r.v.AllowEmptyEnv(true)
	r.v.AutomaticEnv()

	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("clusterctl config file: %w", err)
		}
		r.v.SetConfigFile(path)
		return r.v.ReadInConfig()
	}

	r.v.SetConfigName(config.ConfigName)
	if dir, err := xdg.ConfigFile(config.ConfigFolderXDG); err == nil {
		r.v.AddConfigPath(dir)
	}
	r.v.AddConfigPath(filepath.Join(xdg.Home, config.ConfigFolder))

	err := r.v.ReadInConfig()
	var notFound viper.ConfigFileNotFoundError
	if errors.As(err, &notFound) {
		return nil
	}
	return err
}

func (r *overlayReader) Get(key string) (string, error) {
	if r.v.Get(key) == nil {
		return "", fmt.Errorf(
			"failed to get value for variable %q. Please set the variable value using os env variables or using the .clusterctl config file",
			key,
		)
	}
	return r.v.GetString(key), nil
}

func (r *overlayReader) Set(key, value string) {
	r.v.Set(key, value)
}

// UnmarshalKey delegates to viper except for the providers list, where the
// overrides are merged over the file entries (same name+type wins).
func (r *overlayReader) UnmarshalKey(key string, rawval any) error {
	if key != config.ProvidersConfigKey {
		return r.v.UnmarshalKey(key, rawval)
	}

	var fromFile []providerOverride
	if err := r.v.UnmarshalKey(key, &fromFile); err != nil {
		return err
	}

	r.mu.Lock()
	merged := make([]providerOverride, 0, len(fromFile)+len(r.overrides))
	for _, f := range fromFile {
		overridden := false
		for _, o := range r.overrides {
			if o.Name == f.Name && o.Type == f.Type {
				overridden = true
				break
			}
		}
		if !overridden {
			merged = append(merged, f)
		}
	}
	merged = append(merged, r.overrides...)
	r.mu.Unlock()

	// Round-trip through viper's decoder so rawval can be any slice type
	// clusterctl uses (its configProvider is unexported).
	tmp := viper.New()
	tmp.Set(key, toMaps(merged))
	return tmp.UnmarshalKey(key, rawval)
}

func toMaps(in []providerOverride) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, p := range in {
		out = append(out, map[string]any{"name": p.Name, "type": string(p.Type), "url": p.URL})
	}
	return out
}
