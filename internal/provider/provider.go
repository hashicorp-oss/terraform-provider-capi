// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure CapiProvider satisfies various provider interfaces.
var _ provider.Provider = &CapiProvider{}
var _ provider.ProviderWithFunctions = &CapiProvider{}
var _ provider.ProviderWithEphemeralResources = &CapiProvider{}
var _ provider.ProviderWithActions = &CapiProvider{}

// CapiProvider defines the provider implementation.
type CapiProvider struct {
	// version is set to the provider version on release, "dev" when the
	// provider is built and ran locally, and "test" when running acceptance
	// testing.
	version string
}

// CapiProviderModel describes the provider data model: the management
// cluster every resource talks to, the way the aws provider block is the AWS
// API. Either a kubeconfig's content or the `kubernetes` block names it.
type CapiProviderModel struct {
	Kubeconfig types.String `tfsdk:"kubeconfig"`
	Kubernetes types.Object `tfsdk:"kubernetes"`
}

// KubernetesConfigModel configures the Kubernetes client for the management cluster.
type KubernetesConfigModel struct {
	Host                  types.String `tfsdk:"host"`
	Username              types.String `tfsdk:"username"`
	Password              types.String `tfsdk:"password"`
	Insecure              types.Bool   `tfsdk:"insecure"`
	TLSServerName         types.String `tfsdk:"tls_server_name"`
	ClientCertificate     types.String `tfsdk:"client_certificate"`
	ClientKey             types.String `tfsdk:"client_key"`
	ClusterCACertificate  types.String `tfsdk:"cluster_ca_certificate"`
	ConfigPaths           types.List   `tfsdk:"config_paths"`
	ConfigPath            types.String `tfsdk:"config_path"`
	ConfigContext         types.String `tfsdk:"config_context"`
	ConfigContextAuthInfo types.String `tfsdk:"config_context_auth_info"`
	ConfigContextCluster  types.String `tfsdk:"config_context_cluster"`
	Token                 types.String `tfsdk:"token"`
	ProxyURL              types.String `tfsdk:"proxy_url"`
}

func (p *CapiProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "capi"
	resp.Version = p.version
}

func (p *CapiProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages Cluster API clusters, their objects and their providers through clusterctl's own libraries. The provider block names the management cluster, the way the aws provider block names the AWS API: `kubeconfig` (content, typically an ephemeral `capi_kubeconfig`), or the `kubernetes` block with a config path and context, or a host and credentials. A `capi_cluster` that bootstraps its own management cluster needs neither.",
		Attributes: map[string]schema.Attribute{
			"kubeconfig": schema.StringAttribute{
				MarkdownDescription: "Kubeconfig content of the management cluster. Feed it from an ephemeral `capi_kubeconfig` so it never enters state. Takes precedence over `kubernetes`.",
				Optional:            true,
				Sensitive:           true,
			},
			"kubernetes": schema.SingleNestedAttribute{
				MarkdownDescription: "Kubernetes connection to the management cluster, with the attributes of the `kubernetes` provider: `config_path`/`config_paths` with `config_context*`, or `host` with a token, a client certificate or basic auth. `KUBE_CONFIG_PATH` and `KUBE_CONFIG_PATHS` are read when neither is set.",
				Optional:            true,
				Attributes:          kubernetesConfigSchema(),
			},
		},
	}
}

// kubernetesConfigSchema returns the schema for kubernetes configuration.
func kubernetesConfigSchema() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"host": schema.StringAttribute{
			MarkdownDescription: "The hostname (in form of URI) of the Kubernetes master",
			Optional:            true,
		},
		"username": schema.StringAttribute{
			MarkdownDescription: "The username to use for HTTP basic authentication when accessing the Kubernetes master endpoint",
			Optional:            true,
		},
		"password": schema.StringAttribute{
			MarkdownDescription: "The password to use for HTTP basic authentication when accessing the Kubernetes master endpoint",
			Optional:            true,
			Sensitive:           true,
		},
		"insecure": schema.BoolAttribute{
			MarkdownDescription: "Whether server should be accessed without verifying the TLS certificate",
			Optional:            true,
		},
		"tls_server_name": schema.StringAttribute{
			MarkdownDescription: "Server name passed to the server for SNI and is used in the client to check server certificates against",
			Optional:            true,
		},
		"client_certificate": schema.StringAttribute{
			MarkdownDescription: "PEM-encoded client certificate for TLS authentication",
			Optional:            true,
		},
		"client_key": schema.StringAttribute{
			MarkdownDescription: "PEM-encoded client certificate key for TLS authentication",
			Optional:            true,
			Sensitive:           true,
		},
		"cluster_ca_certificate": schema.StringAttribute{
			MarkdownDescription: "PEM-encoded root certificates bundle for TLS authentication",
			Optional:            true,
		},
		"config_paths": schema.ListAttribute{
			MarkdownDescription: "A list of paths to kube config files. Can be set with KUBE_CONFIG_PATHS environment variable",
			Optional:            true,
			ElementType:         types.StringType,
		},
		"config_path": schema.StringAttribute{
			MarkdownDescription: "Path to the kube config file. Can be set with KUBE_CONFIG_PATH environment variable",
			Optional:            true,
		},
		"config_context": schema.StringAttribute{
			MarkdownDescription: "Context to choose from the config file",
			Optional:            true,
		},
		"config_context_auth_info": schema.StringAttribute{
			MarkdownDescription: "Authentication info context of the kube config (name of the kubeconfig user, --user flag in kubectl)",
			Optional:            true,
		},
		"config_context_cluster": schema.StringAttribute{
			MarkdownDescription: "Cluster context of the kube config (name of the kubeconfig cluster, --cluster flag in kubectl)",
			Optional:            true,
		},
		"token": schema.StringAttribute{
			MarkdownDescription: "Token to authenticate a service account",
			Optional:            true,
			Sensitive:           true,
		},
		"proxy_url": schema.StringAttribute{
			MarkdownDescription: "URL to the proxy to be used for all API requests",
			Optional:            true,
		},
	}
}

func (p *CapiProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data CapiProviderModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)

	if resp.Diagnostics.HasError() {
		return
	}

	// The management connection is built on first use (see providerData), so
	// an empty provider block is valid for a cluster that bootstraps its own.
	pd := &providerData{model: data}
	resp.DataSourceData = pd
	resp.ResourceData = pd
	resp.EphemeralResourceData = pd
	resp.ActionData = pd
}

func (p *CapiProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewClusterResource,
		NewObjectResource,
		NewMachineTemplateResource,
		NewControlPlaneTemplateResource,
		NewBootstrapTemplateResource,
		NewMachineDeploymentResource,
		NewMachineHealthCheckResource,
		NewHelmChartProxyResource,
		NewClusterResourceSetResource,
	}
}

func (p *CapiProvider) EphemeralResources(ctx context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{
		NewKubeconfigEphemeralResource,
	}
}

func (p *CapiProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewClusterDataSource,
		NewProviderComponentsDataSource,
	}
}

func (p *CapiProvider) Functions(ctx context.Context) []func() function.Function {
	return []func() function.Function{}
}

func (p *CapiProvider) Actions(ctx context.Context) []func() action.Action {
	return []func() action.Action{}
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &CapiProvider{
			version: version,
		}
	}
}
