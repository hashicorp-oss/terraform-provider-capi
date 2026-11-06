// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"errors"
	"testing"
)

const tinkerbellDefault = "https://github.com/tinkerbell/cluster-api-provider-tinkerbell/releases/latest/infrastructure-components.yaml"

func TestResolveFetchURL(t *testing.T) {
	tests := []struct {
		name       string
		p          ProviderConfig
		defaultURL string
		want       string
		wantErr    error
	}{
		{
			name: "owner override keeps default repo and pins version",
			p: ProviderConfig{
				Name:        "tinkerbell",
				Type:        ProviderTypeInfrastructure,
				Version:     "v0.7.9",
				FetchConfig: &FetchConfig{Owner: "tinkerbell-community"},
			},
			defaultURL: tinkerbellDefault,
			want:       "https://github.com/tinkerbell-community/cluster-api-provider-tinkerbell/releases/v0.7.9/infrastructure-components.yaml",
		},
		{
			name: "version only keeps default owner and repo",
			p: ProviderConfig{
				Name:    "tinkerbell",
				Type:    ProviderTypeInfrastructure,
				Version: "v0.7.9",
			},
			defaultURL: tinkerbellDefault,
			want:       "https://github.com/tinkerbell/cluster-api-provider-tinkerbell/releases/v0.7.9/infrastructure-components.yaml",
		},
		{
			name: "owner without version uses latest",
			p: ProviderConfig{
				Name:        "tinkerbell",
				Type:        ProviderTypeInfrastructure,
				FetchConfig: &FetchConfig{Owner: "me"},
			},
			defaultURL: tinkerbellDefault,
			want:       "https://github.com/me/cluster-api-provider-tinkerbell/releases/latest/infrastructure-components.yaml",
		},
		{
			name: "unknown provider with owner and repository",
			p: ProviderConfig{
				Name:    "unifi",
				Type:    ProviderTypeIPAM,
				Version: "v0.4.1",
				FetchConfig: &FetchConfig{
					Owner:      "ubiquiti-community",
					Repository: "cluster-api-ipam-provider-unifi",
				},
			},
			want: "https://github.com/ubiquiti-community/cluster-api-ipam-provider-unifi/releases/v0.4.1/ipam-components.yaml",
		},
		{
			name: "unknown provider without repository errors",
			p: ProviderConfig{
				Name:        "unifi",
				Type:        ProviderTypeIPAM,
				Version:     "v0.4.1",
				FetchConfig: &FetchConfig{Owner: "ubiquiti-community"},
			},
			wantErr: ErrUnknownProviderRepository,
		},
		{
			name:    "unknown provider with only a version errors",
			p:       ProviderConfig{Name: "unifi", Type: ProviderTypeIPAM, Version: "v0.4.1"},
			wantErr: ErrUnknownProviderRepository,
		},
		{
			name: "url is verbatim",
			p: ProviderConfig{
				Name:    "tinkerbell",
				Type:    ProviderTypeInfrastructure,
				Version: "v9",
				FetchConfig: &FetchConfig{
					URL: "https://example.com/x/infrastructure-components.yaml",
				},
			},
			defaultURL: tinkerbellDefault,
			want:       "https://example.com/x/infrastructure-components.yaml",
		},
		{
			name: "oci is verbatim",
			p: ProviderConfig{
				Name:        "unifi",
				Type:        ProviderTypeIPAM,
				FetchConfig: &FetchConfig{OCI: "oci://ghcr.io/org/unifi"},
			},
			want: "oci://ghcr.io/org/unifi",
		},
		{
			name: "non-github default with owner override errors",
			p: ProviderConfig{
				Name:        "x",
				Type:        ProviderTypeAddon,
				FetchConfig: &FetchConfig{Owner: "me"},
			},
			defaultURL: "https://example.com/releases/latest/addon-components.yaml",
			wantErr:    ErrUnknownProviderRepository,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveFetchURL(tt.p, tt.defaultURL)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}
