// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrUnknownProviderRepository is returned when a GitHub releases URL cannot
// be built because neither the user nor clusterctl's defaults name the repository.
var ErrUnknownProviderRepository = errors.New("provider repository unknown")

const githubLatest = "latest"

// ResolveFetchURL returns the clusterctl provider URL for p.
//
// URL and OCI in the fetch config are returned verbatim. Otherwise the URL is
// https://github.com/{owner}/{repository}/releases/{version|latest}/{type}-components.yaml
// with owner and repository falling back to those parsed from defaultURL,
// clusterctl's built-in entry for the provider ("" when clusterctl does not
// know it). Pinning the version in the URL keeps clusterctl from calling the
// GitHub API to resolve "latest".
func ResolveFetchURL(p ProviderConfig, defaultURL string) (string, error) {
	fc := p.FetchConfig
	if fc == nil {
		fc = &FetchConfig{}
	}
	if fc.URL != "" {
		return fc.URL, nil
	}
	if fc.OCI != "" {
		return fc.OCI, nil
	}

	owner, repo := fc.Owner, fc.Repository
	if owner == "" || repo == "" {
		defOwner, defRepo := githubOwnerRepo(defaultURL)
		if owner == "" {
			owner = defOwner
		}
		if repo == "" {
			repo = defRepo
		}
	}
	if owner == "" || repo == "" {
		return "", fmt.Errorf(
			"%w: %s provider %q is not built into clusterctl; set fetch_config.owner and fetch_config.repository, or fetch_config.url",
			ErrUnknownProviderRepository,
			p.Type,
			p.Name,
		)
	}

	version := p.Version
	if version == "" {
		version = githubLatest
	}
	return fmt.Sprintf(
		"https://github.com/%s/%s/releases/%s/%s",
		owner,
		repo,
		version,
		p.Type.ComponentsFile(),
	), nil
}

// githubOwnerRepo extracts owner and repository from a GitHub releases URL.
// Both are "" when raw is empty, not GitHub, or not in the releases form.
func githubOwnerRepo(raw string) (string, string) {
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != "github.com" {
		return "", ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[2] != "releases" {
		return "", ""
	}
	return parts[0], parts[1]
}
