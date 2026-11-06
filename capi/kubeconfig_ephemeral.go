// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/tinkerbell-community/terraform-provider-capi/capi/validators"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
)

var (
	_ ephemeral.EphemeralResource              = &kubeconfigEphemeralResource{}
	_ ephemeral.EphemeralResourceWithConfigure = &kubeconfigEphemeralResource{}
)

// NewKubeconfigEphemeralResource returns the capi_kubeconfig ephemeral resource.
func NewKubeconfigEphemeralResource() ephemeral.EphemeralResource {
	return &kubeconfigEphemeralResource{}
}

// kubeconfigEphemeralResource hands out a workload cluster's kubeconfig for
// the life of a run, without it entering state: read from the management
// cluster's `<name>-kubeconfig` Secret, or, for a self-managed cluster that
// nothing else holds a credential for, derived from its root secrets.
type kubeconfigEphemeralResource struct {
	pd *providerData
}

// kubeconfigEphemeralModel is the capi_kubeconfig model.
type kubeconfigEphemeralModel struct {
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

func (e *kubeconfigEphemeralResource) Metadata(
	_ context.Context,
	req ephemeral.MetadataRequest,
	resp *ephemeral.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_kubeconfig"
}

func (e *kubeconfigEphemeralResource) Schema(
	_ context.Context,
	_ ephemeral.SchemaRequest,
	resp *ephemeral.SchemaResponse,
) {
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
				MarkdownDescription: "Validity of a derived client certificate, as a Go duration. Default `8h`. Make it outlast the longest run that uses it: Terraform configures a provider block once per run and cannot hand it a renewed certificate, so one that expires mid-apply fails every call after.",
				Optional:            true,
				Validators:          []validator.String{validators.DurationValidator()},
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

func (e *kubeconfigEphemeralResource) Configure(
	_ context.Context,
	req ephemeral.ConfigureRequest,
	resp *ephemeral.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Ephemeral Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData),
		)
		return
	}
	e.pd = pd
}

// defaultDerivedTTL is the default validity of a derived certificate: longer
// than any apply the provider's own waits allow.
const defaultDerivedTTL = 8 * time.Hour

func (e *kubeconfigEphemeralResource) Open(
	ctx context.Context,
	req ephemeral.OpenRequest,
	resp *ephemeral.OpenResponse,
) {
	var data kubeconfigEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	namespace := "default"
	if !data.Namespace.IsNull() && data.Namespace.ValueString() != "" {
		namespace = data.Namespace.ValueString()
	}
	ttl := defaultDerivedTTL
	if !data.TTL.IsNull() && data.TTL.ValueString() != "" {
		d, err := time.ParseDuration(data.TTL.ValueString())
		if err != nil {
			resp.Diagnostics.AddError(
				"Invalid ttl",
				fmt.Sprintf("ttl %q is not a Go duration: %s.", data.TTL.ValueString(), err),
			)
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
			resp.Diagnostics.AddError(
				"No root credential",
				fmt.Sprintf("secrets for cluster %q: %s.", data.Cluster.ValueString(), err),
			)
			return
		}
		if data.Endpoint.IsNull() || data.Endpoint.ValueString() == "" {
			resp.Diagnostics.AddError(
				"Missing endpoint",
				"endpoint is required to derive a kubeconfig from secrets: the cluster's API server URL (a capi_cluster's status.endpoint).",
			)
			return
		}
		// No RenewAt: Terraform would call Renew, but a renewed certificate
		// cannot reach provider blocks already configured with this one, so
		// the lifetime has to cover the run instead.
		raw, err = capi.DeriveKubeconfig(
			root,
			data.Endpoint.ValueString(),
			data.Cluster.ValueString(),
			ttl,
			time.Now(),
		)
		if err != nil {
			resp.Diagnostics.AddError("Deriving kubeconfig", err.Error())
			return
		}
	case !data.Secrets.IsNull() && !data.Secrets.IsUnknown():
		resp.Diagnostics.AddError(
			"No root credential",
			"secrets is set but empty; pass a capi_cluster's provider_secrets, or unset it to read the kubeconfig from the management cluster.",
		)
		return
	default:
		conn, d := e.pd.managementOrDiag(ctx)
		resp.Diagnostics.Append(d...)
		if d.HasError() {
			return
		}
		applier := capi.NewDynamicApplier()
		secret, err := applier.ReadSecret(
			ctx,
			conn.Cluster,
			namespace,
			data.Cluster.ValueString()+"-kubeconfig",
		)
		if errors.Is(err, capi.ErrClusterNotFound) {
			resp.Diagnostics.AddError(
				"Kubeconfig not found",
				fmt.Sprintf(
					"Secret %s/%s-kubeconfig does not exist on the management cluster yet: the cluster's control plane has not initialized, or it lives in another namespace.",
					namespace,
					data.Cluster.ValueString(),
				),
			)
			return
		}
		if err != nil {
			resp.Diagnostics.AddError("Reading kubeconfig", err.Error())
			return
		}
		value, ok := secret["value"]
		if !ok || strings.TrimSpace(value) == "" {
			resp.Diagnostics.AddError(
				"Kubeconfig not found",
				fmt.Sprintf(
					"Secret %s/%s-kubeconfig has no `value` key.",
					namespace,
					data.Cluster.ValueString(),
				),
			)
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
