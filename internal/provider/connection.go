// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

// errNoManagementConnection is returned by providerData.management when the
// provider block names no management cluster.
var errNoManagementConnection = errors.New("the provider block configures no management cluster: set `kubeconfig` or the `kubernetes` block")

// providerData is what every resource, data source and ephemeral resource
// receives from Configure: the provider configuration and, built once on
// first use, the connection to the management cluster it names. Building it
// lazily keeps a `capi_cluster` that bootstraps its own management cluster
// working with an empty provider block.
type providerData struct {
	model CapiProviderModel

	once sync.Once
	conn *managementConnection
	err  error
}

// managementConnection is the management cluster the provider block names,
// as a kubeconfig file: clusterctl and the rest of the capi package take a
// path, so content from the configuration (an ephemeral kubeconfig, say) is
// written to a private file for the life of the process.
type managementConnection struct {
	KubeconfigPath string
	Cluster        *capi.Cluster
}

// management returns the connection, building it on first use.
func (p *providerData) management(ctx context.Context) (*managementConnection, error) {
	p.once.Do(func() {
		p.conn, p.err = p.model.connect(ctx)
	})
	return p.conn, p.err
}

// managementOrDiag is management with the error as a diagnostic.
func (p *providerData) managementOrDiag(ctx context.Context) (*managementConnection, diag.Diagnostics) {
	var diags diag.Diagnostics
	if p == nil {
		diags.AddError("Provider not configured", errNoManagementConnection.Error())
		return nil, diags
	}
	conn, err := p.management(ctx)
	if err != nil {
		diags.AddError("Management cluster connection", err.Error())
		return nil, diags
	}
	return conn, diags
}

// connect materialises the provider block's management cluster as a
// kubeconfig file. Precedence: `kubeconfig` content, then the `kubernetes`
// block (a config path with optional context overrides, or a host with
// credentials), then the KUBE_CONFIG_PATH environment.
func (m CapiProviderModel) connect(ctx context.Context) (*managementConnection, error) {
	if !m.Kubeconfig.IsNull() && !m.Kubeconfig.IsUnknown() && strings.TrimSpace(m.Kubeconfig.ValueString()) != "" {
		cfg, err := clientcmd.Load([]byte(m.Kubeconfig.ValueString()))
		if err != nil {
			return nil, fmt.Errorf("provider kubeconfig: %w", err)
		}
		return writeConnection(cfg)
	}

	var k KubernetesConfigModel
	if !m.Kubernetes.IsNull() && !m.Kubernetes.IsUnknown() {
		if d := m.Kubernetes.As(ctx, &k, basetypes.ObjectAsOptions{UnhandledNullAsEmpty: true, UnhandledUnknownAsEmpty: true}); d.HasError() {
			return nil, fmt.Errorf("provider kubernetes block: %v", d)
		}
	}
	str := func(s basetypes.StringValue) string {
		if s.IsNull() || s.IsUnknown() {
			return ""
		}
		return s.ValueString()
	}

	if host := str(k.Host); host != "" {
		cfg := clientcmdapi.NewConfig()
		cluster := &clientcmdapi.Cluster{Server: host, InsecureSkipTLSVerify: !k.Insecure.IsNull() && k.Insecure.ValueBool(), TLSServerName: str(k.TLSServerName), ProxyURL: str(k.ProxyURL)}
		if ca := str(k.ClusterCACertificate); ca != "" {
			cluster.CertificateAuthorityData = []byte(ca)
		}
		user := &clientcmdapi.AuthInfo{Token: str(k.Token), Username: str(k.Username), Password: str(k.Password)}
		if crt := str(k.ClientCertificate); crt != "" {
			user.ClientCertificateData = []byte(crt)
		}
		if key := str(k.ClientKey); key != "" {
			user.ClientKeyData = []byte(key)
		}
		cfg.Clusters["management"] = cluster
		cfg.AuthInfos["management"] = user
		cfg.Contexts["management"] = &clientcmdapi.Context{Cluster: "management", AuthInfo: "management"}
		cfg.CurrentContext = "management"
		return writeConnection(cfg)
	}

	// Only what the configuration or the environment names, never the
	// current user's default kubeconfig: a management cluster is chosen, not
	// stumbled into.
	rules := &clientcmd.ClientConfigLoadingRules{}
	rules.ExplicitPath = str(k.ConfigPath)
	if rules.ExplicitPath == "" {
		rules.ExplicitPath = os.Getenv("KUBE_CONFIG_PATH")
	}
	if !k.ConfigPaths.IsNull() && !k.ConfigPaths.IsUnknown() && len(k.ConfigPaths.Elements()) > 0 {
		var paths []string
		if d := k.ConfigPaths.ElementsAs(ctx, &paths, false); d.HasError() {
			return nil, fmt.Errorf("provider kubernetes.config_paths: %v", d)
		}
		rules.Precedence = paths
	} else if env := os.Getenv("KUBE_CONFIG_PATHS"); env != "" && rules.ExplicitPath == "" {
		rules.Precedence = filepath.SplitList(env)
	}
	if rules.ExplicitPath == "" && len(rules.Precedence) == 0 {
		return nil, errNoManagementConnection
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: str(k.ConfigContext)}
	overrides.Context.AuthInfo = str(k.ConfigContextAuthInfo)
	overrides.Context.Cluster = str(k.ConfigContextCluster)
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).RawConfig()
	if err != nil {
		return nil, fmt.Errorf("provider kubernetes config: %w", err)
	}
	if overrides.CurrentContext != "" {
		raw.CurrentContext = overrides.CurrentContext
	}
	if c, ok := raw.Contexts[raw.CurrentContext]; ok {
		if overrides.Context.AuthInfo != "" {
			c.AuthInfo = overrides.Context.AuthInfo
		}
		if overrides.Context.Cluster != "" {
			c.Cluster = overrides.Context.Cluster
		}
	}
	if err := clientcmdapi.MinifyConfig(&raw); err != nil {
		return nil, fmt.Errorf("provider kubernetes config: %w", err)
	}
	if err := clientcmdapi.FlattenConfig(&raw); err != nil {
		return nil, fmt.Errorf("provider kubernetes config: %w", err)
	}
	return writeConnection(&raw)
}

// writeConnection writes a kubeconfig to a private file and returns the
// connection around it.
func writeConnection(cfg *clientcmdapi.Config) (*managementConnection, error) {
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("encoding the management kubeconfig: %w", err)
	}
	dir, err := os.MkdirTemp("", "capi-tf-")
	if err != nil {
		return nil, fmt.Errorf("creating the provider's kubeconfig directory: %w", err)
	}
	path := filepath.Join(dir, "management.kubeconfig")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("writing the management kubeconfig: %w", err)
	}
	return &managementConnection{KubeconfigPath: path, Cluster: &capi.Cluster{Name: "management", KubeconfigPath: path}}, nil
}

// kubernetesConfigAttrTypes is the attribute type map of the provider's
// kubernetes block.
func kubernetesConfigAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"host": types.StringType, "username": types.StringType, "password": types.StringType, "insecure": types.BoolType,
		"tls_server_name": types.StringType, "client_certificate": types.StringType, "client_key": types.StringType,
		"cluster_ca_certificate": types.StringType, "config_paths": types.ListType{ElemType: types.StringType},
		"config_path": types.StringType, "config_context": types.StringType, "config_context_auth_info": types.StringType,
		"config_context_cluster": types.StringType, "token": types.StringType, "proxy_url": types.StringType,
	}
}
