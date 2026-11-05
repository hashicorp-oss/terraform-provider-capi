// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

var (
	_ ephemeral.EphemeralResource              = &KubeconfigEphemeralResource{}
	_ ephemeral.EphemeralResourceWithConfigure = &KubeconfigEphemeralResource{}
)

// NewKubeconfigEphemeralResource returns the capi_kubeconfig ephemeral resource.
func NewKubeconfigEphemeralResource() ephemeral.EphemeralResource {
	return &KubeconfigEphemeralResource{}
}

// KubeconfigEphemeralResource hands out a workload cluster's kubeconfig for
// the life of a run, without it entering state: read from the management
// cluster's `<name>-kubeconfig` Secret, or, for a self-managed cluster that
// nothing else holds a credential for, derived from its root secrets.
type KubeconfigEphemeralResource struct {
	pd *providerData
}

// KubeconfigEphemeralModel is the capi_kubeconfig model.
type KubeconfigEphemeralModel struct {
	Cluster              types.String  `tfsdk:"cluster"`
	Namespace            types.String  `tfsdk:"namespace"`
	Secrets              types.Dynamic `tfsdk:"secrets"`
	Endpoint             types.String  `tfsdk:"endpoint"`
	TTL                  types.String  `tfsdk:"ttl"`
	Raw                  types.String  `tfsdk:"raw"`
	Host                 types.String  `tfsdk:"host"`
	ClusterCACertificate types.String  `tfsdk:"cluster_ca_certificate"`
	ClientCertificate    types.String  `tfsdk:"client_certificate"`
	ClientKey            types.String  `tfsdk:"client_key"`
	Token                types.String  `tfsdk:"token"`
}

func (e *KubeconfigEphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kubeconfig"
}

func (e *KubeconfigEphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A workload cluster's kubeconfig for the life of a Terraform run, never written to state. With `secrets` unset it is read from the `<cluster>-kubeconfig` Secret on the management cluster the provider block names, the way `clusterctl get kubeconfig` reads it. With `secrets` set to a `capi_cluster`'s `provider_secrets` it is derived instead: a client certificate for `system:masters`, valid for `ttl`, signed by the cluster CA those secrets carry. That is how a self-managed cluster is reached, since no other authority holds a credential for it. Feed `raw` to the `capi`, `kubernetes` or `helm` provider blocks.",
		Attributes: map[string]schema.Attribute{
			"cluster": schema.StringAttribute{
				MarkdownDescription: "Cluster name.",
				Required:            true,
			},
			"namespace": schema.StringAttribute{
				MarkdownDescription: "Namespace of the Cluster object on the management cluster. Default `default`.",
				Optional:            true,
			},
			"secrets": schema.DynamicAttribute{
				MarkdownDescription: "A `capi_cluster`'s `provider_secrets`: the Talos machine-secrets bundle or the kubeadm CA of a self-managed cluster. When set, the kubeconfig is derived from it and nothing is read from a management cluster.",
				Optional:            true,
				Sensitive:           true,
			},
			"endpoint": schema.StringAttribute{
				MarkdownDescription: "API server URL the derived kubeconfig points at, e.g. `https://10.0.0.160:6443`. Required with `secrets`; a `capi_cluster`'s `status.endpoint`.",
				Optional:            true,
			},
			"ttl": schema.StringAttribute{
				MarkdownDescription: "Validity of a derived client certificate, as a Go duration. Default `1h`.",
				Optional:            true,
			},
			"raw": schema.StringAttribute{
				MarkdownDescription: "The kubeconfig.",
				Computed:            true,
				Sensitive:           true,
			},
			"host": schema.StringAttribute{
				MarkdownDescription: "API server URL of the kubeconfig's current context.",
				Computed:            true,
			},
			"cluster_ca_certificate": schema.StringAttribute{
				MarkdownDescription: "PEM cluster CA certificate.",
				Computed:            true,
			},
			"client_certificate": schema.StringAttribute{
				MarkdownDescription: "PEM client certificate, when the kubeconfig authenticates with one.",
				Computed:            true,
				Sensitive:           true,
			},
			"client_key": schema.StringAttribute{
				MarkdownDescription: "PEM client key, when the kubeconfig authenticates with a certificate.",
				Computed:            true,
				Sensitive:           true,
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "Bearer token, when the kubeconfig authenticates with one.",
				Computed:            true,
				Sensitive:           true,
			},
		},
	}
}

func (e *KubeconfigEphemeralResource) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Ephemeral Resource Configure Type", fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData))
		return
	}
	e.pd = pd
}

func (e *KubeconfigEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var data KubeconfigEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	namespace := "default"
	if !data.Namespace.IsNull() && data.Namespace.ValueString() != "" {
		namespace = data.Namespace.ValueString()
	}
	ttl := time.Hour
	if !data.TTL.IsNull() && data.TTL.ValueString() != "" {
		d, err := time.ParseDuration(data.TTL.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Invalid ttl", fmt.Sprintf("ttl %q is not a Go duration: %s.", data.TTL.ValueString(), err))
			return
		}
		ttl = d
	}

	var raw []byte
	secrets, d := providerSecretsFromDynamic(ctx, data.Secrets)
	resp.Diagnostics.Append(d...)
	switch {
	case len(secrets) > 0:
		root, err := capi.ParseRootSecrets(data.Cluster.ValueString(), secrets)
		if err != nil {
			resp.Diagnostics.AddError("No root credential", fmt.Sprintf("secrets for cluster %q: %s.", data.Cluster.ValueString(), err))
			return
		}
		if data.Endpoint.IsNull() || data.Endpoint.ValueString() == "" {
			resp.Diagnostics.AddError("Missing endpoint", "endpoint is required to derive a kubeconfig from secrets: the cluster's API server URL (a capi_cluster's status.endpoint).")
			return
		}
		raw, err = capi.DeriveKubeconfig(root, data.Endpoint.ValueString(), data.Cluster.ValueString(), ttl, time.Now())
		if err != nil {
			resp.Diagnostics.AddError("Deriving kubeconfig", err.Error())
			return
		}
		// Leave a margin so a run that started with a fresh certificate does
		// not outlive it by a few seconds.
		resp.RenewAt = time.Now().Add(ttl - 2*time.Minute)
	case !data.Secrets.IsNull() && !data.Secrets.IsUnknown():
		resp.Diagnostics.AddError("No root credential", "secrets is set but empty; pass a capi_cluster's provider_secrets, or unset it to read the kubeconfig from the management cluster.")
		return
	default:
		conn, d := e.pd.managementOrDiag(ctx)
		resp.Diagnostics.Append(d...)
		if d.HasError() {
			return
		}
		applier := capi.NewDynamicApplier()
		secret, err := applier.ReadSecret(ctx, conn.Cluster, namespace, data.Cluster.ValueString()+"-kubeconfig")
		if errors.Is(err, capi.ErrClusterNotFound) {
			resp.Diagnostics.AddError("Kubeconfig not found", fmt.Sprintf("Secret %s/%s-kubeconfig does not exist on the management cluster yet: the cluster's control plane has not initialized, or it lives in another namespace.", namespace, data.Cluster.ValueString()))
			return
		}
		if err != nil {
			resp.Diagnostics.AddError("Reading kubeconfig", err.Error())
			return
		}
		value, ok := secret["value"]
		if !ok || strings.TrimSpace(value) == "" {
			resp.Diagnostics.AddError("Kubeconfig not found", fmt.Sprintf("Secret %s/%s-kubeconfig has no `value` key.", namespace, data.Cluster.ValueString()))
			return
		}
		raw = []byte(value)
	}

	info, err := capi.ParseKubeconfig(raw)
	if err != nil {
		resp.Diagnostics.AddError("Invalid kubeconfig", err.Error())
		return
	}
	data.Raw = types.StringValue(string(raw))
	data.Host = types.StringValue(info.Server)
	data.ClusterCACertificate = stringOrNull(string(info.CertificateAuthorityData))
	data.ClientCertificate = stringOrNull(string(info.ClientCertificateData))
	data.ClientKey = stringOrNull(string(info.ClientKeyData))
	data.Token = stringOrNull(info.Token)
	resp.Diagnostics.Append(resp.Result.Set(ctx, &data)...)
}
