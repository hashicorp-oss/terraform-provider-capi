// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/client-go/tools/clientcmd"
)

func kubernetesBlock(t *testing.T, ctx context.Context, m kubernetesConfigModel) types.Object {
	t.Helper()
	obj, d := types.ObjectValueFrom(ctx, kubernetesConfigAttrTypes(), m)
	if d.HasError() {
		t.Fatal(d)
	}
	return obj
}

func nullKubernetesBlock() kubernetesConfigModel {
	return kubernetesConfigModel{
		Host:                  types.StringNull(),
		Username:              types.StringNull(),
		Password:              types.StringNull(),
		Insecure:              types.BoolNull(),
		TLSServerName:         types.StringNull(),
		ClientCertificate:     types.StringNull(),
		ClientKey:             types.StringNull(),
		ClusterCACertificate:  types.StringNull(),
		ConfigPaths:           types.ListNull(types.StringType),
		ConfigPath:            types.StringNull(),
		ConfigContext:         types.StringNull(),
		ConfigContextAuthInfo: types.StringNull(),
		ConfigContextCluster:  types.StringNull(),
		Token:                 types.StringNull(),
		ProxyURL:              types.StringNull(),
	}
}

const twoContextKubeconfig = `apiVersion: v1
kind: Config
current-context: a
clusters:
- name: a
  cluster: {server: https://a:6443}
- name: b
  cluster: {server: https://b:6443}
contexts:
- name: a
  context: {cluster: a, user: a}
- name: b
  context: {cluster: b, user: b}
users:
- name: a
  user: {token: ta}
- name: b
  user: {token: tb}
`

func TestProviderConnect(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KUBE_CONFIG_PATH", "")
	t.Setenv("KUBE_CONFIG_PATHS", "")

	// Nothing configured: no connection, and a clear error.
	m := capiProviderModel{
		Kubeconfig: types.StringNull(),
		Kubernetes: types.ObjectNull(kubernetesConfigAttrTypes()),
	}
	if _, err := m.connect(ctx); !errors.Is(err, errNoManagementConnection) {
		t.Fatalf("err = %v", err)
	}

	// Kubeconfig content is written to a private file.
	m.Kubeconfig = types.StringValue(twoContextKubeconfig)
	conn, err := m.connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(conn.KubeconfigPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig file: %v %v", st, err)
	}
	cfg, err := clientcmd.LoadFromFile(conn.KubeconfigPath)
	if err != nil || cfg.CurrentContext != "a" ||
		conn.Cluster.KubeconfigPath != conn.KubeconfigPath {
		t.Errorf("written kubeconfig: %+v %v", cfg, err)
	}

	// A config path with a context override is minified to that context.
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(twoContextKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	k := nullKubernetesBlock()
	k.ConfigPath = types.StringValue(path)
	k.ConfigContext = types.StringValue("b")
	m = capiProviderModel{Kubeconfig: types.StringNull(), Kubernetes: kubernetesBlock(t, ctx, k)}
	conn, err = m.connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ = clientcmd.LoadFromFile(conn.KubeconfigPath)
	if cfg.CurrentContext != "b" || len(cfg.Clusters) != 1 ||
		cfg.Clusters["b"].Server != "https://b:6443" ||
		cfg.AuthInfos["b"].Token != "tb" {
		t.Errorf("context override: %+v", cfg)
	}

	// A host with a token builds a kubeconfig from scratch.
	k = nullKubernetesBlock()
	k.Host = types.StringValue("https://10.0.0.160:6443")
	k.Token = types.StringValue("secret")
	k.ClusterCACertificate = types.StringValue(
		"-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n",
	)
	m = capiProviderModel{Kubeconfig: types.StringNull(), Kubernetes: kubernetesBlock(t, ctx, k)}
	conn, err = m.connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ = clientcmd.LoadFromFile(conn.KubeconfigPath)
	if c := cfg.Clusters["management"]; c == nil || c.Server != "https://10.0.0.160:6443" ||
		len(c.CertificateAuthorityData) == 0 ||
		cfg.AuthInfos["management"].Token != "secret" {
		t.Errorf("host connection: %+v", cfg)
	}

	// The environment is the last resort.
	t.Setenv("KUBE_CONFIG_PATH", path)
	m = capiProviderModel{
		Kubeconfig: types.StringNull(),
		Kubernetes: types.ObjectNull(kubernetesConfigAttrTypes()),
	}
	if conn, err = m.connect(ctx); err != nil || conn.KubeconfigPath == "" {
		t.Errorf("env: %v %v", conn, err)
	}
}

func TestProviderData_ManagementIsBuiltOnce(t *testing.T) {
	ctx := context.Background()
	pd := &providerData{
		model: capiProviderModel{
			Kubeconfig: types.StringValue(twoContextKubeconfig),
			Kubernetes: types.ObjectNull(kubernetesConfigAttrTypes()),
		},
	}
	a, err := pd.management(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pd.management(ctx)
	if a != b {
		t.Error("the connection is built once")
	}
	var nilPD *providerData
	if _, d := nilPD.managementOrDiag(ctx); !d.HasError() {
		t.Error("an unconfigured provider is an error")
	}
}
