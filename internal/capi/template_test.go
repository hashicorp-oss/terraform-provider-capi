// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"path/filepath"
	"testing"
)

func TestTemplateURL(t *testing.T) {
	abs, _ := filepath.Abs("templates/cluster-template-talos.yaml")
	cases := map[string]string{
		"https://github.com/o/r/blob/main/cluster-template.yaml": "https://github.com/o/r/blob/main/cluster-template.yaml",
		"file:///etc/capi/cluster-template.yaml":                 "file:///etc/capi/cluster-template.yaml",
		"/etc/capi/cluster-template.yaml":                        "/etc/capi/cluster-template.yaml",
		"templates/cluster-template-talos.yaml":                  abs,
	}
	for in, want := range cases {
		if got := templateURL(in); got != want {
			t.Errorf("templateURL(%q) = %q, want %q", in, got, want)
		}
	}
}
