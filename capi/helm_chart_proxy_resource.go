// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/tinkerbell-community/terraform-provider-capi/capi/validators"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

var (
	_ resource.Resource                = &helmChartProxyResource{}
	_ resource.ResourceWithIdentity    = &helmChartProxyResource{}
	_ resource.ResourceWithConfigure   = &helmChartProxyResource{}
	_ resource.ResourceWithImportState = &helmChartProxyResource{}
)

// CAAPH labels and conditions.
const (
	helmChartProxyLabel            = "helmreleaseproxy.addons.cluster.x-k8s.io/helmchartproxy-name"
	helmReleaseProxiesReady        = "HelmReleaseProxiesReady"
	helmReleaseProxySpecsUpToDate  = "HelmReleaseProxySpecsUpToDate"
	helmReleaseReady               = "HelmReleaseReady"
	helmReconcileStrategyDefault   = "Continuous"
	helmReconcileStrategyInstallOn = "InstallOnce"
)

// NewHelmChartProxyResource returns the capi_helm_chart_proxy resource.
func NewHelmChartProxyResource() resource.Resource {
	return &helmChartProxyResource{}
}

// helmChartProxyResource is a CAAPH HelmChartProxy: a Helm chart the addon
// provider installs on every cluster the selector matches, the way
// aws_eks_addon installs an addon on an EKS cluster. The wait watches the
// per-cluster HelmReleaseProxy and quotes Helm's failure.
type helmChartProxyResource struct {
	pd *providerData
}

// helmChartProxyResourceModel is the capi_helm_chart_proxy model.
type helmChartProxyResourceModel struct {
	ID              types.String   `tfsdk:"id"`
	Name            types.String   `tfsdk:"name"`
	Namespace       types.String   `tfsdk:"namespace"`
	Cluster         types.String   `tfsdk:"cluster"`
	ClusterSelector types.Map      `tfsdk:"cluster_selector"`
	Chart           types.Object   `tfsdk:"chart"`
	Release         types.Object   `tfsdk:"release"`
	Strategy        types.String   `tfsdk:"strategy"`
	Options         types.Object   `tfsdk:"options"`
	Credentials     types.Object   `tfsdk:"credentials"`
	TLS             types.Object   `tfsdk:"tls"`
	Wait            types.Object   `tfsdk:"wait"`
	ForceDelete     types.Bool     `tfsdk:"force_delete"`
	Manifest        types.String   `tfsdk:"manifest"`
	Status          types.Object   `tfsdk:"status"`
	Conditions      types.List     `tfsdk:"conditions"`
	Timeouts        timeouts.Value `tfsdk:"timeouts"`
}

// helmChartModel is the chart block.
type helmChartModel struct {
	Name       types.String `tfsdk:"name"`
	Repository types.String `tfsdk:"repository"`
	Version    types.String `tfsdk:"version"`
}

// helmChartReleaseModel is the release block.
type helmChartReleaseModel struct {
	Name      types.String `tfsdk:"name"`
	Namespace types.String `tfsdk:"namespace"`
	Values    types.String `tfsdk:"values"`
}

// helmOptionsModel is the options block.
type helmOptionsModel struct {
	Wait                     types.Bool   `tfsdk:"wait"`
	WaitForJobs              types.Bool   `tfsdk:"wait_for_jobs"`
	Timeout                  types.String `tfsdk:"timeout"`
	SkipCRDs                 types.Bool   `tfsdk:"skip_crds"`
	Atomic                   types.Bool   `tfsdk:"atomic"`
	DisableHooks             types.Bool   `tfsdk:"disable_hooks"`
	DependencyUpdate         types.Bool   `tfsdk:"dependency_update"`
	DisableOpenAPIValidation types.Bool   `tfsdk:"disable_openapi_validation"`
	TakeOwnership            types.Bool   `tfsdk:"take_ownership"`
	Install                  types.Object `tfsdk:"install"`
	Upgrade                  types.Object `tfsdk:"upgrade"`
	Uninstall                types.Object `tfsdk:"uninstall"`
}

// helmInstallOptionsModel is options.install.
type helmInstallOptionsModel struct {
	CreateNamespace types.Bool `tfsdk:"create_namespace"`
	IncludeCRDs     types.Bool `tfsdk:"include_crds"`
}

// helmUpgradeOptionsModel is options.upgrade.
type helmUpgradeOptionsModel struct {
	Force                types.Bool  `tfsdk:"force"`
	ResetValues          types.Bool  `tfsdk:"reset_values"`
	ReuseValues          types.Bool  `tfsdk:"reuse_values"`
	ResetThenReuseValues types.Bool  `tfsdk:"reset_then_reuse_values"`
	Recreate             types.Bool  `tfsdk:"recreate"`
	MaxHistory           types.Int64 `tfsdk:"max_history"`
	CleanupOnFail        types.Bool  `tfsdk:"cleanup_on_fail"`
}

// helmUninstallOptionsModel is options.uninstall.
type helmUninstallOptionsModel struct {
	KeepHistory types.Bool   `tfsdk:"keep_history"`
	Description types.String `tfsdk:"description"`
}

// helmCredentialsModel is the credentials block.
type helmCredentialsModel struct {
	Secret types.String `tfsdk:"secret"`
	Key    types.String `tfsdk:"key"`
}

// helmTLSModel is the tls block.
type helmTLSModel struct {
	CASecret           types.String `tfsdk:"ca_secret"`
	InsecureSkipVerify types.Bool   `tfsdk:"insecure_skip_verify"`
}

// helmChartProxyStatusModel is the status block.
type helmChartProxyStatusModel struct {
	MatchingClusters types.List `tfsdk:"matching_clusters"`
	Releases         types.List `tfsdk:"releases"`
}

// helmReleaseStatusModel is one entry of status.releases.
type helmReleaseStatusModel struct {
	Cluster  types.String `tfsdk:"cluster"`
	Status   types.String `tfsdk:"status"`
	Revision types.Int64  `tfsdk:"revision"`
	Ready    types.Bool   `tfsdk:"ready"`
	Message  types.String `tfsdk:"message"`
}

func helmChartAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":       types.StringType,
		"repository": types.StringType,
		"version":    types.StringType,
	}
}

func helmChartReleaseAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":      types.StringType,
		"namespace": types.StringType,
		"values":    types.StringType,
	}
}

func helmInstallOptionsAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"create_namespace": types.BoolType, "include_crds": types.BoolType}
}

func helmUpgradeOptionsAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"force":                   types.BoolType,
		"reset_values":            types.BoolType,
		"reuse_values":            types.BoolType,
		"reset_then_reuse_values": types.BoolType,
		"recreate":                types.BoolType,
		"max_history":             types.Int64Type,
		"cleanup_on_fail":         types.BoolType,
	}
}

func helmUninstallOptionsAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"keep_history": types.BoolType, "description": types.StringType}
}

func helmOptionsAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"wait":                       types.BoolType,
		"wait_for_jobs":              types.BoolType,
		"timeout":                    types.StringType,
		"skip_crds":                  types.BoolType,
		"atomic":                     types.BoolType,
		"disable_hooks":              types.BoolType,
		"dependency_update":          types.BoolType,
		"disable_openapi_validation": types.BoolType,
		"take_ownership":             types.BoolType,
		"install":                    types.ObjectType{AttrTypes: helmInstallOptionsAttrTypes()},
		"upgrade":                    types.ObjectType{AttrTypes: helmUpgradeOptionsAttrTypes()},
		"uninstall":                  types.ObjectType{AttrTypes: helmUninstallOptionsAttrTypes()},
	}
}

func helmCredentialsAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"secret": types.StringType, "key": types.StringType}
}

func helmTLSAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"ca_secret":            types.StringType,
		"insecure_skip_verify": types.BoolType,
	}
}

func helmReleaseStatusAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"cluster":  types.StringType,
		"status":   types.StringType,
		"revision": types.Int64Type,
		"ready":    types.BoolType,
		"message":  types.StringType,
	}
}

func helmChartProxyStatusAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"matching_clusters": types.ListType{ElemType: types.StringType},
		"releases": types.ListType{
			ElemType: types.ObjectType{AttrTypes: helmReleaseStatusAttrTypes()},
		},
	}
}

// helmChartProxyWaitDefaults are the conditions waited for by default.
var helmChartProxyWaitDefaults = []string{helmReleaseProxySpecsUpToDate, helmReleaseProxiesReady}

func (r *helmChartProxyResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_helm_chart_proxy"
}

// IdentitySchema implements [resource.ResourceWithIdentity].
func (r *helmChartProxyResource) IdentitySchema(
	_ context.Context,
	_ resource.IdentitySchemaRequest,
	resp *resource.IdentitySchemaResponse,
) {
	resp.IdentitySchema = namespacedIdentitySchema()
}

func boolAttr(desc string) schema.BoolAttribute {
	return schema.BoolAttribute{MarkdownDescription: desc, Optional: true}
}

func (r *helmChartProxyResource) Schema(
	ctx context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Helm chart the addon provider for Helm (CAAPH) installs on every cluster `cluster_selector` matches, the way `aws_eks_addon` installs an addon on an EKS cluster: a `HelmChartProxy` on the management cluster the provider block names. CAAPH creates a `HelmReleaseProxy` per matched cluster and runs Helm through that cluster's kubeconfig Secret; `wait` watches those and, when one fails, quotes Helm's message and revision. Requires the `helm` addon provider on the management cluster. Import with `namespace/name`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Object name.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"namespace": schema.StringAttribute{
				MarkdownDescription: "Namespace, which the matched Clusters share. Default `default`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("default"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"cluster": schema.StringAttribute{
				MarkdownDescription: "Install on this one cluster (selects by its `cluster.x-k8s.io/cluster-name` label). Exclusive with `cluster_selector`.",
				Optional:            true,
			},
			"cluster_selector": schema.MapAttribute{
				MarkdownDescription: "Install on every Cluster in the namespace carrying these labels. Exclusive with `cluster`.",
				ElementType:         types.StringType,
				Optional:            true,
			},
			"chart": schema.SingleNestedAttribute{
				MarkdownDescription: "The chart.",
				Required:            true,
				Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{
						MarkdownDescription: "Chart name in the repository.",
						Required:            true,
					},
					"repository": schema.StringAttribute{
						MarkdownDescription: "Repository URL (`https://...` or `oci://...`).",
						Required:            true,
					},
					"version": schema.StringAttribute{
						MarkdownDescription: "Chart version. Unset follows the latest.",
						Optional:            true,
					},
				},
			},
			"release": schema.SingleNestedAttribute{
				MarkdownDescription: "The Helm release on each cluster.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{
						MarkdownDescription: "Release name. CAAPH generates one when unset.",
						Optional:            true,
					},
					"namespace": schema.StringAttribute{
						MarkdownDescription: "Release namespace on the workload cluster. Default `default`.",
						Optional:            true,
					},
					"values": schema.StringAttribute{
						MarkdownDescription: "Values as YAML. Go templating over the matched Cluster is available (`{{ .Cluster.metadata.name }}`).",
						Optional:            true,
					},
				},
			},
			"strategy": schema.StringAttribute{
				MarkdownDescription: "`Continuous` (default: install, upgrade and uninstall as the configuration and selection change) or `InstallOnce` (never touched again once installed). Immutable.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(helmReconcileStrategyDefault),
				Validators: []validator.String{
					stringvalidator.OneOf("Continuous", "InstallOnce"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"options": schema.SingleNestedAttribute{
				MarkdownDescription: "Helm options (the CLI flags of install, upgrade and uninstall).",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"wait": boolAttr(
						"Wait for resources to be ready after install and upgrade.",
					),
					"wait_for_jobs": boolAttr("Wait for jobs to complete too."),
					"timeout": schema.StringAttribute{
						MarkdownDescription: "Time to wait for any individual Kubernetes operation, as a Go duration. Helm's default is `5m`.",
						Optional:            true,
						Validators:          []validator.String{validators.DurationValidator()},
					},
					"skip_crds": boolAttr("Install no CRDs."),
					"atomic": boolAttr(
						"Roll back or delete on failure (implies `wait`).",
					),
					"disable_hooks":     boolAttr("Run no hooks."),
					"dependency_update": boolAttr("Fetch missing chart dependencies."),
					"disable_openapi_validation": boolAttr(
						"Skip OpenAPI validation of the rendered manifests.",
					),
					"take_ownership": boolAttr(
						"Adopt existing resources that lack Helm's annotations.",
					),
					"install": schema.SingleNestedAttribute{
						Optional: true,
						Attributes: map[string]schema.Attribute{
							"create_namespace": boolAttr(
								"Create the release namespace. CAAPH's default is true.",
							),
							"include_crds": boolAttr(
								"Install the CRDs in the chart's templates directory.",
							),
						},
					},
					"upgrade": schema.SingleNestedAttribute{
						Optional: true,
						Attributes: map[string]schema.Attribute{
							"force": boolAttr(
								"Ignore warnings and upgrade anyway.",
							),
							"reset_values": boolAttr(
								"Reset to the chart's built-in values rather than merging.",
							),
							"reuse_values": boolAttr("Reuse the last supplied values."),
							"reset_then_reuse_values": boolAttr(
								"Reset to the built-ins, then merge the last supplied values.",
							),
							"recreate": boolAttr("Recreate pods after a rollback."),
							"max_history": schema.Int64Attribute{
								MarkdownDescription: "Revisions kept per release. Helm's default is 10.",
								Optional:            true,
							},
							"cleanup_on_fail": boolAttr(
								"Delete resources created by a failed upgrade.",
							),
						},
					},
					"uninstall": schema.SingleNestedAttribute{
						Optional: true,
						Attributes: map[string]schema.Attribute{
							"keep_history": boolAttr("Keep the release history on uninstall."),
							"description": schema.StringAttribute{
								MarkdownDescription: "Description recorded on uninstall.",
								Optional:            true,
							},
						},
					},
				},
			},
			"credentials": schema.SingleNestedAttribute{
				MarkdownDescription: "OCI registry credentials from a Secret in the same namespace.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"secret": schema.StringAttribute{
						MarkdownDescription: "Secret name.",
						Required:            true,
					},
					"key": schema.StringAttribute{
						MarkdownDescription: "Key holding the Docker config JSON. CAAPH's default is `config.json`.",
						Optional:            true,
					},
				},
			},
			"tls": schema.SingleNestedAttribute{
				MarkdownDescription: "TLS settings for the repository.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"ca_secret": schema.StringAttribute{
						MarkdownDescription: "Secret in the same namespace holding the CA at `ca.crt`.",
						Optional:            true,
					},
					"insecure_skip_verify": boolAttr(
						"Skip verification of the repository's certificate.",
					),
				},
			},
			"timeouts": timeouts.Attributes(
				ctx,
				timeouts.Opts{Create: true, Update: true, Delete: true},
			),
			"wait": objectWaitAttribute(helmChartProxyWaitDefaults),
			"force_delete": schema.BoolAttribute{
				MarkdownDescription: "On destroy, when CAAPH cannot uninstall a release because its cluster no longer exists, remove the finalizers it leaves behind instead of failing.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"manifest": schema.StringAttribute{
				MarkdownDescription: "The applied object as YAML.",
				Computed:            true,
			},
			"status": schema.SingleNestedAttribute{
				MarkdownDescription: "The clusters matched and the release on each.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"matching_clusters": schema.ListAttribute{
						ElementType: types.StringType,
						Computed:    true,
					},
					"releases": schema.ListNestedAttribute{
						Computed: true,
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"cluster": schema.StringAttribute{Computed: true},
								"status": schema.StringAttribute{
									MarkdownDescription: "Helm release status (`deployed`, `failed`, `pending-install`, ...).",
									Computed:            true,
								},
								"revision": schema.Int64Attribute{Computed: true},
								"ready":    schema.BoolAttribute{Computed: true},
								"message": schema.StringAttribute{
									MarkdownDescription: "The HelmReleaseReady condition's message: Helm's error when it failed.",
									Computed:            true,
								},
							},
						},
					},
				},
			},
			"conditions": schema.ListNestedAttribute{
				MarkdownDescription: "The object's conditions.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"type":    schema.StringAttribute{Computed: true},
						"status":  schema.StringAttribute{Computed: true},
						"reason":  schema.StringAttribute{Computed: true},
						"message": schema.StringAttribute{Computed: true},
					},
				},
			},
		},
	}
}

func (r *helmChartProxyResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData),
		)
		return
	}
	r.pd = pd
}

// --- rendering ---

// helmChartProxyInputs is everything a HelmChartProxy renders from. Pointer
// fields are rendered, and compared on read, only when set.
type helmChartProxyInputs struct {
	Name, Namespace  string
	APIVersion       string
	ClusterSelector  map[string]string
	ChartName        string
	RepoURL          string
	Version          string
	ReleaseName      string
	ReleaseNamespace string
	Values           string
	Strategy         string
	Options          helmOptions
	CredentialsName  string
	CredentialsKey   string
	CASecret         string
	InsecureSkipTLS  *bool
}

// helmOptions mirrors CAAPH's HelmOptions with every field optional.
type helmOptions struct {
	Wait, WaitForJobs, SkipCRDs, Atomic, DisableHooks, DependencyUpdate, DisableOpenAPIValidation, TakeOwnership *bool
	Timeout                                                                                                      *time.Duration
	CreateNamespace, IncludeCRDs                                                                                 *bool
	Force, ResetValues, ReuseValues, ResetThenReuseValues, Recreate, CleanupOnFail                               *bool
	MaxHistory                                                                                                   *int64
	KeepHistory                                                                                                  *bool
	UninstallDescription                                                                                         string
}

// validate checks the inputs.
func (in helmChartProxyInputs) validate() error {
	var errs []error
	if len(in.ClusterSelector) == 0 {
		errs = append(errs, errors.New("one of cluster or cluster_selector is required"))
	}
	if in.ChartName == "" || in.RepoURL == "" {
		errs = append(errs, errors.New("chart.name and chart.repository are required"))
	}
	if !strings.HasPrefix(in.RepoURL, "https://") && !strings.HasPrefix(in.RepoURL, "http://") &&
		!strings.HasPrefix(in.RepoURL, "oci://") {
		errs = append(
			errs,
			fmt.Errorf(
				"chart.repository %q must start with https://, http:// or oci://",
				in.RepoURL,
			),
		)
	}
	switch in.Strategy {
	case "", helmReconcileStrategyDefault, helmReconcileStrategyInstallOn:
	default:
		errs = append(errs, fmt.Errorf("strategy %q is not Continuous or InstallOnce", in.Strategy))
	}
	return errors.Join(errs...)
}

func setBool(m map[string]any, key string, v *bool) {
	if v != nil {
		m[key] = *v
	}
}

// renderOptions renders the options block; nil when nothing is set.
func (o helmOptions) render() map[string]any {
	out := map[string]any{}
	setBool(out, "wait", o.Wait)
	setBool(out, "waitForJobs", o.WaitForJobs)
	setBool(out, "skipCRDs", o.SkipCRDs)
	setBool(out, "atomic", o.Atomic)
	setBool(out, "disableHooks", o.DisableHooks)
	setBool(out, "dependencyUpdate", o.DependencyUpdate)
	setBool(out, "disableOpenAPIValidation", o.DisableOpenAPIValidation)
	setBool(out, "takeOwnership", o.TakeOwnership)
	if o.Timeout != nil {
		out["timeout"] = o.Timeout.String()
	}
	install := map[string]any{}
	setBool(install, "createNamespace", o.CreateNamespace)
	setBool(install, "includeCRDs", o.IncludeCRDs)
	if len(install) > 0 {
		out["install"] = install
	}
	upgrade := map[string]any{}
	setBool(upgrade, "force", o.Force)
	setBool(upgrade, "resetValues", o.ResetValues)
	setBool(upgrade, "reuseValues", o.ReuseValues)
	setBool(upgrade, "resetThenReuseValues", o.ResetThenReuseValues)
	setBool(upgrade, "recreate", o.Recreate)
	setBool(upgrade, "cleanupOnFail", o.CleanupOnFail)
	if o.MaxHistory != nil {
		upgrade["maxHistory"] = *o.MaxHistory
	}
	if len(upgrade) > 0 {
		out["upgrade"] = upgrade
	}
	uninstall := map[string]any{}
	setBool(uninstall, "keepHistory", o.KeepHistory)
	if o.UninstallDescription != "" {
		uninstall["description"] = o.UninstallDescription
	}
	if len(uninstall) > 0 {
		out["uninstall"] = uninstall
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// renderHelmChartProxy builds the object.
func renderHelmChartProxy(in helmChartProxyInputs) *unstructured.Unstructured {
	matchLabels := map[string]any{}
	for k, v := range in.ClusterSelector {
		matchLabels[k] = v
	}
	strategy := in.Strategy
	if strategy == "" {
		strategy = helmReconcileStrategyDefault
	}
	spec := map[string]any{
		"clusterSelector":   map[string]any{"matchLabels": matchLabels},
		"chartName":         in.ChartName,
		"repoURL":           in.RepoURL,
		"reconcileStrategy": strategy,
	}
	if in.Version != "" {
		spec["version"] = in.Version
	}
	if in.ReleaseName != "" {
		spec["releaseName"] = in.ReleaseName
	}
	if in.ReleaseNamespace != "" {
		spec["namespace"] = in.ReleaseNamespace
	}
	if in.Values != "" {
		spec["valuesTemplate"] = in.Values
	}
	if opts := in.Options.render(); opts != nil {
		spec["options"] = opts
	}
	if in.CredentialsName != "" {
		creds := map[string]any{
			"secret": map[string]any{"name": in.CredentialsName, "namespace": in.Namespace},
		}
		key := in.CredentialsKey
		if key == "" {
			key = "config.json"
		}
		creds["key"] = key
		spec["credentials"] = creds
	}
	if in.CASecret != "" || in.InsecureSkipTLS != nil {
		tls := map[string]any{}
		if in.CASecret != "" {
			tls["caSecret"] = map[string]any{"name": in.CASecret, "namespace": in.Namespace}
		}
		setBool(tls, "insecureSkipTLSVerify", in.InsecureSkipTLS)
		spec["tlsConfig"] = tls
	}
	obj := newObject(gvkFor(in.APIVersion, "HelmChartProxy"), in.Namespace, in.Name, nil, nil)
	obj.Object["spec"] = spec
	return obj
}

func liveBool(m map[string]any, path ...string) *bool {
	v, ok, _ := unstructured.NestedBool(m, path...)
	if !ok {
		return nil
	}
	return &v
}

// readLiveHelmChartProxy reads the configurable values back.
func readLiveHelmChartProxy(obj *unstructured.Unstructured) helmChartProxyInputs {
	in := helmChartProxyInputs{
		Name:       obj.GetName(),
		Namespace:  obj.GetNamespace(),
		APIVersion: obj.GetAPIVersion(),
	}
	in.ClusterSelector, _, _ = unstructured.NestedStringMap(
		obj.Object,
		"spec",
		"clusterSelector",
		"matchLabels",
	)
	in.ChartName, _, _ = unstructured.NestedString(obj.Object, "spec", "chartName")
	in.RepoURL, _, _ = unstructured.NestedString(obj.Object, "spec", "repoURL")
	in.Version, _, _ = unstructured.NestedString(obj.Object, "spec", "version")
	in.ReleaseName, _, _ = unstructured.NestedString(obj.Object, "spec", "releaseName")
	in.ReleaseNamespace, _, _ = unstructured.NestedString(obj.Object, "spec", "namespace")
	in.Values, _, _ = unstructured.NestedString(obj.Object, "spec", "valuesTemplate")
	in.Strategy, _, _ = unstructured.NestedString(obj.Object, "spec", "reconcileStrategy")
	if in.Strategy == "" {
		in.Strategy = helmReconcileStrategyDefault
	}
	o := &in.Options
	o.Wait = liveBool(obj.Object, "spec", "options", "wait")
	o.WaitForJobs = liveBool(obj.Object, "spec", "options", "waitForJobs")
	o.SkipCRDs = liveBool(obj.Object, "spec", "options", "skipCRDs")
	o.Atomic = liveBool(obj.Object, "spec", "options", "atomic")
	o.DisableHooks = liveBool(obj.Object, "spec", "options", "disableHooks")
	o.DependencyUpdate = liveBool(obj.Object, "spec", "options", "dependencyUpdate")
	o.DisableOpenAPIValidation = liveBool(obj.Object, "spec", "options", "disableOpenAPIValidation")
	o.TakeOwnership = liveBool(obj.Object, "spec", "options", "takeOwnership")
	if s, _, _ := unstructured.NestedString(obj.Object, "spec", "options", "timeout"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			o.Timeout = &d
		}
	}
	o.CreateNamespace = liveBool(obj.Object, "spec", "options", "install", "createNamespace")
	o.IncludeCRDs = liveBool(obj.Object, "spec", "options", "install", "includeCRDs")
	o.Force = liveBool(obj.Object, "spec", "options", "upgrade", "force")
	o.ResetValues = liveBool(obj.Object, "spec", "options", "upgrade", "resetValues")
	o.ReuseValues = liveBool(obj.Object, "spec", "options", "upgrade", "reuseValues")
	o.ResetThenReuseValues = liveBool(
		obj.Object,
		"spec",
		"options",
		"upgrade",
		"resetThenReuseValues",
	)
	o.Recreate = liveBool(obj.Object, "spec", "options", "upgrade", "recreate")
	o.CleanupOnFail = liveBool(obj.Object, "spec", "options", "upgrade", "cleanupOnFail")
	if n, ok, _ := unstructured.NestedInt64(
		obj.Object,
		"spec",
		"options",
		"upgrade",
		"maxHistory",
	); ok {
		o.MaxHistory = &n
	}
	o.KeepHistory = liveBool(obj.Object, "spec", "options", "uninstall", "keepHistory")
	o.UninstallDescription, _, _ = unstructured.NestedString(
		obj.Object,
		"spec",
		"options",
		"uninstall",
		"description",
	)
	in.CredentialsName, _, _ = unstructured.NestedString(
		obj.Object,
		"spec",
		"credentials",
		"secret",
		"name",
	)
	in.CredentialsKey, _, _ = unstructured.NestedString(obj.Object, "spec", "credentials", "key")
	in.CASecret, _, _ = unstructured.NestedString(
		obj.Object,
		"spec",
		"tlsConfig",
		"caSecret",
		"name",
	)
	in.InsecureSkipTLS = liveBool(obj.Object, "spec", "tlsConfig", "insecureSkipTLSVerify")
	return in
}

// helmRelease is what a HelmReleaseProxy reports.
type helmRelease struct {
	Cluster  string
	Status   string
	Revision int64
	Ready    bool
	Message  string
	// Observed is false while the release controller has not seen the
	// release's current generation.
	Observed bool
}

// readHelmRelease flattens a HelmReleaseProxy.
func readHelmRelease(rel *unstructured.Unstructured) helmRelease {
	out := helmRelease{}
	out.Cluster, _, _ = unstructured.NestedString(rel.Object, "spec", "clusterRef", "name")
	out.Status, _, _ = unstructured.NestedString(rel.Object, "status", "status")
	out.Revision, _, _ = unstructured.NestedInt64(rel.Object, "status", "revision")
	observed, _, _ := unstructured.NestedInt64(rel.Object, "status", "observedGeneration")
	out.Observed = observed >= rel.GetGeneration()
	for _, c := range readNodeGroupStatus(rel).Conditions {
		if c.Type == helmReleaseReady {
			out.Ready = c.Status == "True"
			out.Message = c.Message
		}
	}
	return out
}

// String renders the release the way the wait error reports it.
func (h helmRelease) String() string {
	s := fmt.Sprintf("%s: %s", h.Cluster, orDefault(h.Status, "pending"))
	if h.Revision > 0 {
		s += fmt.Sprintf(" (revision %d)", h.Revision)
	}
	if !h.Ready && h.Message != "" {
		s += ": " + h.Message
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// matchingClusterNames reads status.matchingClusters.
func matchingClusterNames(obj *unstructured.Unstructured) []string {
	raw, _, _ := unstructured.NestedSlice(obj.Object, "status", "matchingClusters")
	var out []string
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			if name, _ := m["name"].(string); name != "" {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// helmChartProxyReady decides whether a proxy's wait is over: the
// controller has seen the current generation, the conditions are True, and
// every matched cluster has a release that is Ready at its current
// generation. Releases are reported either way.
func helmChartProxyReady(
	proxy *unstructured.Unstructured,
	releases []helmRelease,
	conditions []string,
) (ready bool, why string) {
	observed, _, _ := unstructured.NestedInt64(proxy.Object, "status", "observedGeneration")
	if observed < proxy.GetGeneration() {
		return false, "the addon provider has not reconciled the current generation yet"
	}
	for _, want := range conditions {
		var found bool
		for _, c := range readNodeGroupStatus(proxy).Conditions {
			if c.Type == want {
				found = true
				if c.Status != "True" {
					return false, capi.ConditionSummary(proxy, conditions)
				}
			}
		}
		if !found {
			return false, capi.ConditionSummary(proxy, conditions)
		}
	}
	matching := matchingClusterNames(proxy)
	byCluster := map[string]helmRelease{}
	for _, r := range releases {
		byCluster[r.Cluster] = r
	}
	for _, cluster := range matching {
		rel, ok := byCluster[cluster]
		if !ok {
			return false, fmt.Sprintf("no HelmReleaseProxy for %s yet", cluster)
		}
		if !rel.Observed || !rel.Ready {
			return false, rel.String()
		}
	}
	return true, ""
}

// --- model conversion ---

func optBool(b types.Bool) *bool {
	if b.IsNull() || b.IsUnknown() {
		return nil
	}
	v := b.ValueBool()
	return &v
}

func (r *helmChartProxyResource) inputs(
	ctx context.Context,
	data *helmChartProxyResourceModel,
	apiVersion string,
) (helmChartProxyInputs, diag.Diagnostics) {
	var diags diag.Diagnostics
	in := helmChartProxyInputs{
		Name:       data.Name.ValueString(),
		Namespace:  data.Namespace.ValueString(),
		APIVersion: apiVersion,
		Strategy:   data.Strategy.ValueString(),
	}
	selector, d := stringMap(ctx, data.ClusterSelector)
	diags.Append(d...)
	if data.Cluster.ValueString() != "" && len(selector) > 0 {
		diags.AddError("Invalid helm chart proxy", "cluster and cluster_selector are exclusive.")
	}
	in.ClusterSelector = addonClusterSelector(data.Cluster.ValueString(), selector)
	var chart helmChartModel
	diags.Append(data.Chart.As(ctx, &chart, basetypes.ObjectAsOptions{})...)
	in.ChartName, in.RepoURL, in.Version = chart.Name.ValueString(), chart.Repository.ValueString(), chart.Version.ValueString()
	if !data.Release.IsNull() && !data.Release.IsUnknown() {
		var rel helmChartReleaseModel
		diags.Append(data.Release.As(ctx, &rel, basetypes.ObjectAsOptions{})...)
		in.ReleaseName, in.ReleaseNamespace, in.Values = rel.Name.ValueString(), rel.Namespace.ValueString(), rel.Values.ValueString()
	}
	if !data.Options.IsNull() && !data.Options.IsUnknown() {
		var o helmOptionsModel
		diags.Append(data.Options.As(ctx, &o, basetypes.ObjectAsOptions{})...)
		opts := &in.Options
		opts.Wait, opts.WaitForJobs, opts.SkipCRDs, opts.Atomic = optBool(
			o.Wait,
		), optBool(
			o.WaitForJobs,
		), optBool(
			o.SkipCRDs,
		), optBool(
			o.Atomic,
		)
		opts.DisableHooks, opts.DependencyUpdate, opts.DisableOpenAPIValidation, opts.TakeOwnership = optBool(
			o.DisableHooks,
		), optBool(
			o.DependencyUpdate,
		), optBool(
			o.DisableOpenAPIValidation,
		), optBool(
			o.TakeOwnership,
		)
		opts.Timeout = durationOrNil(o.Timeout, "options.timeout", &diags)
		if !o.Install.IsNull() && !o.Install.IsUnknown() {
			var i helmInstallOptionsModel
			diags.Append(o.Install.As(ctx, &i, basetypes.ObjectAsOptions{})...)
			opts.CreateNamespace, opts.IncludeCRDs = optBool(
				i.CreateNamespace,
			), optBool(
				i.IncludeCRDs,
			)
		}
		if !o.Upgrade.IsNull() && !o.Upgrade.IsUnknown() {
			var u helmUpgradeOptionsModel
			diags.Append(o.Upgrade.As(ctx, &u, basetypes.ObjectAsOptions{})...)
			opts.Force, opts.ResetValues, opts.ReuseValues, opts.ResetThenReuseValues = optBool(
				u.Force,
			), optBool(
				u.ResetValues,
			), optBool(
				u.ReuseValues,
			), optBool(
				u.ResetThenReuseValues,
			)
			opts.Recreate, opts.CleanupOnFail = optBool(u.Recreate), optBool(u.CleanupOnFail)
			if !u.MaxHistory.IsNull() && !u.MaxHistory.IsUnknown() {
				v := u.MaxHistory.ValueInt64()
				opts.MaxHistory = &v
			}
		}
		if !o.Uninstall.IsNull() && !o.Uninstall.IsUnknown() {
			var u helmUninstallOptionsModel
			diags.Append(o.Uninstall.As(ctx, &u, basetypes.ObjectAsOptions{})...)
			opts.KeepHistory, opts.UninstallDescription = optBool(
				u.KeepHistory,
			), u.Description.ValueString()
		}
	}
	if !data.Credentials.IsNull() && !data.Credentials.IsUnknown() {
		var c helmCredentialsModel
		diags.Append(data.Credentials.As(ctx, &c, basetypes.ObjectAsOptions{})...)
		in.CredentialsName, in.CredentialsKey = c.Secret.ValueString(), c.Key.ValueString()
	}
	if !data.TLS.IsNull() && !data.TLS.IsUnknown() {
		var t helmTLSModel
		diags.Append(data.TLS.As(ctx, &t, basetypes.ObjectAsOptions{})...)
		in.CASecret, in.InsecureSkipTLS = t.CASecret.ValueString(), optBool(t.InsecureSkipVerify)
	}
	return in, diags
}

// addonClusterSelector is the selector of an addon resource: the one
// cluster's name label, or the labels given.
func addonClusterSelector(cluster string, selector map[string]string) map[string]string {
	if cluster != "" {
		return map[string]string{clusterNameLabel: cluster}
	}
	return selector
}

// setSelectorFromLive folds a live selector back into cluster or
// cluster_selector, keeping the form the configuration used.
func setSelectorFromLive(
	ctx context.Context,
	cluster *types.String,
	selector *types.Map,
	live map[string]string,
	diags *diag.Diagnostics,
) {
	if !cluster.IsNull() && len(live) == 1 && live[clusterNameLabel] != "" {
		*cluster = types.StringValue(live[clusterNameLabel])
		*selector = types.MapNull(types.StringType)
		return
	}
	m, d := types.MapValueFrom(ctx, types.StringType, live)
	diags.Append(d...)
	*cluster = types.StringNull()
	*selector = m
}

func keepBool(configured types.Bool, live *bool) types.Bool {
	if configured.IsNull() {
		return configured
	}
	if live == nil {
		return types.BoolNull()
	}
	return types.BoolValue(*live)
}

func keepString(configured types.String, live string) types.String {
	if configured.IsNull() && live == "" {
		return configured
	}
	return stringOrNull(live)
}

// setFromLive writes the configured attributes from the live object. Fields
// the configuration leaves unset stay unset, so a default the controller
// fills in is never drift.
func (r *helmChartProxyResource) setFromLive(
	ctx context.Context,
	data *helmChartProxyResourceModel,
	live helmChartProxyInputs,
) diag.Diagnostics {
	var diags diag.Diagnostics
	setSelectorFromLive(ctx, &data.Cluster, &data.ClusterSelector, live.ClusterSelector, &diags)
	var chart helmChartModel
	if !data.Chart.IsNull() {
		diags.Append(data.Chart.As(ctx, &chart, basetypes.ObjectAsOptions{})...)
	}
	chart.Name, chart.Repository, chart.Version = types.StringValue(
		live.ChartName,
	), types.StringValue(
		live.RepoURL,
	), keepString(
		chart.Version,
		live.Version,
	)
	cv, d := types.ObjectValueFrom(ctx, helmChartAttrTypes(), chart)
	diags.Append(d...)
	data.Chart = cv
	if !data.Release.IsNull() || live.ReleaseName != "" || live.ReleaseNamespace != "" ||
		live.Values != "" {
		var rel helmChartReleaseModel
		if !data.Release.IsNull() {
			diags.Append(data.Release.As(ctx, &rel, basetypes.ObjectAsOptions{})...)
		}
		rel.Name, rel.Namespace, rel.Values = keepString(
			rel.Name,
			live.ReleaseName,
		), keepString(
			rel.Namespace,
			live.ReleaseNamespace,
		), keepString(
			rel.Values,
			live.Values,
		)
		rv, d := types.ObjectValueFrom(ctx, helmChartReleaseAttrTypes(), rel)
		diags.Append(d...)
		data.Release = rv
	}
	data.Strategy = types.StringValue(live.Strategy)
	if !data.Options.IsNull() {
		var o helmOptionsModel
		diags.Append(data.Options.As(ctx, &o, basetypes.ObjectAsOptions{})...)
		l := live.Options
		o.Wait, o.WaitForJobs, o.SkipCRDs, o.Atomic = keepBool(
			o.Wait,
			l.Wait,
		), keepBool(
			o.WaitForJobs,
			l.WaitForJobs,
		), keepBool(
			o.SkipCRDs,
			l.SkipCRDs,
		), keepBool(
			o.Atomic,
			l.Atomic,
		)
		o.DisableHooks, o.DependencyUpdate = keepBool(
			o.DisableHooks,
			l.DisableHooks,
		), keepBool(
			o.DependencyUpdate,
			l.DependencyUpdate,
		)
		o.DisableOpenAPIValidation, o.TakeOwnership = keepBool(
			o.DisableOpenAPIValidation,
			l.DisableOpenAPIValidation,
		), keepBool(
			o.TakeOwnership,
			l.TakeOwnership,
		)
		if !o.Timeout.IsNull() {
			o.Timeout = types.StringNull()
			if l.Timeout != nil {
				o.Timeout = types.StringValue(l.Timeout.String())
			}
		}
		if !o.Install.IsNull() {
			var i helmInstallOptionsModel
			diags.Append(o.Install.As(ctx, &i, basetypes.ObjectAsOptions{})...)
			i.CreateNamespace, i.IncludeCRDs = keepBool(
				i.CreateNamespace,
				l.CreateNamespace,
			), keepBool(
				i.IncludeCRDs,
				l.IncludeCRDs,
			)
			iv, d := types.ObjectValueFrom(ctx, helmInstallOptionsAttrTypes(), i)
			diags.Append(d...)
			o.Install = iv
		}
		if !o.Upgrade.IsNull() {
			var u helmUpgradeOptionsModel
			diags.Append(o.Upgrade.As(ctx, &u, basetypes.ObjectAsOptions{})...)
			u.Force, u.ResetValues, u.ReuseValues = keepBool(
				u.Force,
				l.Force,
			), keepBool(
				u.ResetValues,
				l.ResetValues,
			), keepBool(
				u.ReuseValues,
				l.ReuseValues,
			)
			u.ResetThenReuseValues, u.Recreate, u.CleanupOnFail = keepBool(
				u.ResetThenReuseValues,
				l.ResetThenReuseValues,
			), keepBool(
				u.Recreate,
				l.Recreate,
			), keepBool(
				u.CleanupOnFail,
				l.CleanupOnFail,
			)
			if !u.MaxHistory.IsNull() {
				u.MaxHistory = types.Int64Null()
				if l.MaxHistory != nil {
					u.MaxHistory = types.Int64Value(*l.MaxHistory)
				}
			}
			uv, d := types.ObjectValueFrom(ctx, helmUpgradeOptionsAttrTypes(), u)
			diags.Append(d...)
			o.Upgrade = uv
		}
		if !o.Uninstall.IsNull() {
			var u helmUninstallOptionsModel
			diags.Append(o.Uninstall.As(ctx, &u, basetypes.ObjectAsOptions{})...)
			u.KeepHistory, u.Description = keepBool(
				u.KeepHistory,
				l.KeepHistory,
			), keepString(
				u.Description,
				l.UninstallDescription,
			)
			uv, d := types.ObjectValueFrom(ctx, helmUninstallOptionsAttrTypes(), u)
			diags.Append(d...)
			o.Uninstall = uv
		}
		ov, d := types.ObjectValueFrom(ctx, helmOptionsAttrTypes(), o)
		diags.Append(d...)
		data.Options = ov
	}
	if !data.Credentials.IsNull() || live.CredentialsName != "" {
		var c helmCredentialsModel
		if !data.Credentials.IsNull() {
			diags.Append(data.Credentials.As(ctx, &c, basetypes.ObjectAsOptions{})...)
		}
		c.Secret, c.Key = types.StringValue(
			live.CredentialsName,
		), keepString(
			c.Key,
			live.CredentialsKey,
		)
		if live.CredentialsName == "" {
			data.Credentials = types.ObjectNull(helmCredentialsAttrTypes())
		} else {
			cv, d := types.ObjectValueFrom(ctx, helmCredentialsAttrTypes(), c)
			diags.Append(d...)
			data.Credentials = cv
		}
	}
	if !data.TLS.IsNull() || live.CASecret != "" || live.InsecureSkipTLS != nil {
		var t helmTLSModel
		if !data.TLS.IsNull() {
			diags.Append(data.TLS.As(ctx, &t, basetypes.ObjectAsOptions{})...)
		}
		t.CASecret, t.InsecureSkipVerify = keepString(
			t.CASecret,
			live.CASecret,
		), keepBool(
			t.InsecureSkipVerify,
			live.InsecureSkipTLS,
		)
		if data.TLS.IsNull() && live.InsecureSkipTLS != nil {
			t.InsecureSkipVerify = types.BoolValue(*live.InsecureSkipTLS)
		}
		tv, d := types.ObjectValueFrom(ctx, helmTLSAttrTypes(), t)
		diags.Append(d...)
		data.TLS = tv
	}
	return diags
}

// fill writes the computed attributes.
func (r *helmChartProxyResource) fill(
	ctx context.Context,
	data *helmChartProxyResourceModel,
	obj *unstructured.Unstructured,
	releases []helmRelease,
) diag.Diagnostics {
	var diags diag.Diagnostics
	matching, d := types.ListValueFrom(ctx, types.StringType, matchingClusterNames(obj))
	diags.Append(d...)
	sort.Slice(releases, func(i, j int) bool { return releases[i].Cluster < releases[j].Cluster })
	rels := make([]helmReleaseStatusModel, 0, len(releases))
	for _, rel := range releases {
		rels = append(rels, helmReleaseStatusModel{
			Cluster: types.StringValue(
				rel.Cluster,
			),
			Status:   stringOrNull(rel.Status),
			Revision: types.Int64Value(rel.Revision),
			Ready:    types.BoolValue(rel.Ready),
			Message:  stringOrNull(rel.Message),
		})
	}
	relList, d := types.ListValueFrom(
		ctx,
		types.ObjectType{AttrTypes: helmReleaseStatusAttrTypes()},
		rels,
	)
	diags.Append(d...)
	st, d := types.ObjectValueFrom(
		ctx,
		helmChartProxyStatusAttrTypes(),
		helmChartProxyStatusModel{MatchingClusters: matching, Releases: relList},
	)
	diags.Append(d...)
	data.Status = st
	conds := make([]conditionModel, 0)
	for _, c := range readNodeGroupStatus(obj).Conditions {
		conds = append(
			conds,
			conditionModel{
				Type:    types.StringValue(c.Type),
				Status:  types.StringValue(c.Status),
				Reason:  stringOrNull(c.Reason),
				Message: stringOrNull(c.Message),
			},
		)
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: conditionAttrTypes()}, conds)
	diags.Append(d...)
	data.Conditions = list
	return diags
}

// --- cluster access ---

// kinds resolves the CAAPH kinds from the installed CRDs.
func (r *helmChartProxyResource) kinds(
	ctx context.Context,
	client *capi.ObjectClient,
) (proxy, release k8sschema.GroupVersionKind, err error) {
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return proxy, release, err
	}
	proxy, err = catalog.ByKind("HelmChartProxy")
	if err != nil {
		return proxy, release, fmt.Errorf(
			"the helm addon provider is not installed on the management cluster: %w",
			err,
		)
	}
	release, err = catalog.ByKind("HelmReleaseProxy")
	return proxy, release, err
}

// releases lists the proxy's HelmReleaseProxies.
func (r *helmChartProxyResource) releases(
	ctx context.Context,
	client *capi.ObjectClient,
	gvk k8sschema.GroupVersionKind,
	namespace, name string,
) ([]helmRelease, []unstructured.Unstructured, error) {
	list, err := client.List(ctx, gvk, namespace, helmChartProxyLabel+"="+name)
	if err != nil {
		return nil, nil, err
	}
	out := make([]helmRelease, 0, len(list))
	for i := range list {
		out = append(out, readHelmRelease(&list[i]))
	}
	return out, list, nil
}

// waitReleases polls until the proxy and its releases are ready.
func (r *helmChartProxyResource) waitReleases(
	ctx context.Context,
	client *capi.ObjectClient,
	proxyGVK, releaseGVK k8sschema.GroupVersionKind,
	namespace, name string,
	conditions []string,
	timeout time.Duration,
) (*unstructured.Unstructured, []helmRelease, error) {
	deadline := time.Now().Add(timeout)
	for {
		obj, err := client.Get(ctx, proxyGVK, namespace, name)
		if err != nil {
			return nil, nil, err
		}
		releases, _, err := r.releases(ctx, client, releaseGVK, namespace, name)
		if err != nil {
			return nil, nil, err
		}
		if len(conditions) == 0 {
			return obj, releases, nil
		}
		ready, why := helmChartProxyReady(obj, releases, conditions)
		if ready {
			return obj, releases, nil
		}
		if time.Now().After(deadline) {
			var lines []string
			for _, rel := range releases {
				lines = append(lines, "  "+rel.String())
			}
			msg := fmt.Sprintf(
				"HelmChartProxy %s/%s not ready within %v: %s",
				namespace,
				name,
				timeout,
				why,
			)
			if len(lines) > 0 {
				msg += "\nreleases:\n" + strings.Join(lines, "\n")
			}
			return obj, releases, errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// --- CRUD ---

func (r *helmChartProxyResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var data helmChartProxyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	createTimeout, timeoutDiags := data.Timeouts.Create(ctx, defaultTimeout)
	resp.Diagnostics.Append(timeoutDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
	r.apply(ctx, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	resp.Diagnostics.Append(
		setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)
}

func (r *helmChartProxyResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var data helmChartProxyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	updateTimeout, timeoutDiags := data.Timeouts.Update(ctx, defaultTimeout)
	resp.Diagnostics.Append(timeoutDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	r.apply(ctx, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	resp.Diagnostics.Append(
		setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)
}

func (r *helmChartProxyResource) apply(
	ctx context.Context,
	data *helmChartProxyResourceModel,
	diags *diag.Diagnostics,
) {
	client, d := r.pd.objectClient(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	proxyGVK, releaseGVK, err := r.kinds(ctx, client)
	if err != nil {
		diags.AddError("Resolving HelmChartProxy", err.Error())
		return
	}
	in, d := r.inputs(ctx, data, proxyGVK.GroupVersion().String())
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	if err := in.validate(); err != nil {
		diags.AddError("Invalid helm chart proxy", err.Error())
		return
	}
	conditions, timeout, d := objectWaitFromModel(ctx, data.Wait, helmChartProxyWaitDefaults)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	obj := renderHelmChartProxy(in)
	data.Manifest = types.StringValue(manifestYAML(obj))
	if _, err := client.Apply(ctx, obj, false); err != nil {
		diags.AddError("Applying HelmChartProxy", err.Error())
		return
	}
	live, releases, err := r.waitReleases(
		ctx,
		client,
		proxyGVK,
		releaseGVK,
		in.Namespace,
		in.Name,
		conditions,
		timeout,
	)
	if err != nil {
		diags.AddError("Waiting for helm releases", err.Error())
		return
	}
	data.ID = types.StringValue(fmt.Sprintf("%s/%s", in.Namespace, in.Name))
	diags.Append(r.fill(ctx, data, live, releases)...)
}

func (r *helmChartProxyResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var data helmChartProxyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The identity follows from the state, whatever the read finds.
	resp.Diagnostics.Append(
		setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	proxyGVK, releaseGVK, err := r.kinds(ctx, client)
	if errors.Is(err, capi.ErrKindNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Resolving HelmChartProxy", err.Error())
		return
	}
	obj, found, d := readLive(
		ctx,
		client,
		proxyGVK,
		data.Namespace.ValueString(),
		data.Name.ValueString(),
	)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	releases, _, err := r.releases(
		ctx,
		client,
		releaseGVK,
		data.Namespace.ValueString(),
		data.Name.ValueString(),
	)
	if err != nil {
		resp.Diagnostics.AddError("Reading helm releases", err.Error())
		return
	}
	resp.Diagnostics.Append(r.setFromLive(ctx, &data, readLiveHelmChartProxy(obj))...)
	resp.Diagnostics.Append(r.fill(ctx, &data, obj, releases)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	resp.Diagnostics.Append(
		setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)
}

func (r *helmChartProxyResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var data helmChartProxyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	deleteTimeout, timeoutDiags := data.Timeouts.Delete(ctx, defaultTimeout)
	resp.Diagnostics.Append(timeoutDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	_, timeout, d := objectWaitFromModel(ctx, data.Wait, helmChartProxyWaitDefaults)
	resp.Diagnostics.Append(d...)
	proxyGVK, releaseGVK, err := r.kinds(ctx, client)
	if errors.Is(err, capi.ErrKindNotFound) {
		return // CAAPH is gone, and its objects with it
	}
	if err != nil {
		resp.Diagnostics.AddError("Resolving HelmChartProxy", err.Error())
		return
	}
	namespace, name := data.Namespace.ValueString(), data.Name.ValueString()
	if err := client.Delete(ctx, proxyGVK, namespace, name); err != nil {
		resp.Diagnostics.AddError("Deleting HelmChartProxy", err.Error())
		return
	}
	err = client.WaitForDeletion(ctx, proxyGVK, namespace, name, capi.WaitOptions{Timeout: timeout})
	if err == nil {
		return
	}
	// CAAPH uninstalls each release through its cluster's kubeconfig; a
	// release whose cluster is gone can never finish.
	orphans, orphanErr := r.orphanedReleases(ctx, client, releaseGVK, namespace, name)
	if orphanErr != nil || len(orphans) == 0 {
		resp.Diagnostics.AddError("Deleting HelmChartProxy", err.Error())
		return
	}
	if !data.ForceDelete.ValueBool() {
		resp.Diagnostics.AddError(
			"Deleting HelmChartProxy",
			fmt.Sprintf(
				"%s\nThe releases on %s cannot be uninstalled because those clusters no longer exist. Set force_delete = true to remove their finalizers.",
				err.Error(),
				strings.Join(orphanNames(orphans), ", "),
			),
		)
		return
	}
	for i := range orphans {
		if err := r.removeFinalizers(
			ctx,
			client,
			releaseGVK,
			namespace,
			orphans[i].GetName(),
		); err != nil {
			resp.Diagnostics.AddError("Removing release finalizer", err.Error())
			return
		}
	}
	if err := client.WaitForDeletion(
		ctx,
		proxyGVK,
		namespace,
		name,
		capi.WaitOptions{Timeout: 2 * time.Minute},
	); err != nil {
		if err := r.removeFinalizers(ctx, client, proxyGVK, namespace, name); err != nil {
			resp.Diagnostics.AddError("Removing HelmChartProxy finalizer", err.Error())
			return
		}
		if err := client.WaitForDeletion(
			ctx,
			proxyGVK,
			namespace,
			name,
			capi.WaitOptions{Timeout: time.Minute},
		); err != nil {
			resp.Diagnostics.AddError("Deleting HelmChartProxy", err.Error())
		}
	}
}

// orphanedReleases returns the proxy's releases whose Cluster no longer exists.
func (r *helmChartProxyResource) orphanedReleases(
	ctx context.Context,
	client *capi.ObjectClient,
	releaseGVK k8sschema.GroupVersionKind,
	namespace, name string,
) ([]unstructured.Unstructured, error) {
	_, list, err := r.releases(ctx, client, releaseGVK, namespace, name)
	if err != nil {
		return nil, err
	}
	catalog, err := client.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	core, err := catalog.CoreVersion()
	if err != nil {
		return nil, err
	}
	clusterGVK := k8sschema.GroupVersionKind{
		Group:   "cluster.x-k8s.io",
		Version: core,
		Kind:    "Cluster",
	}
	var orphans []unstructured.Unstructured
	for i := range list {
		cluster, _, _ := unstructured.NestedString(list[i].Object, "spec", "clusterRef", "name")
		if cluster == "" {
			continue
		}
		_, err := client.Get(ctx, clusterGVK, namespace, cluster)
		if errors.Is(err, capi.ErrObjectNotFound) {
			orphans = append(orphans, list[i])
		}
	}
	return orphans, nil
}

func orphanNames(list []unstructured.Unstructured) []string {
	names := make([]string, 0, len(list))
	for i := range list {
		cluster, _, _ := unstructured.NestedString(list[i].Object, "spec", "clusterRef", "name")
		names = append(names, cluster)
	}
	sort.Strings(names)
	return names
}

// removeFinalizers clears an object's finalizers.
func (r *helmChartProxyResource) removeFinalizers(
	ctx context.Context,
	client *capi.ObjectClient,
	gvk k8sschema.GroupVersionKind,
	namespace, name string,
) error {
	_, err := client.Patch(
		ctx,
		gvk,
		namespace,
		name,
		k8stypes.MergePatchType,
		[]byte(`{"metadata":{"finalizers":null}}`),
	)
	if errors.Is(err, capi.ErrObjectNotFound) {
		return nil
	}
	return err
}

// ImportState takes `namespace/name`.
func (r *helmChartProxyResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	importedID, d := importID(ctx, req, namespacedImportID)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	parts := strings.Split(strings.TrimSpace(importedID), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected namespace/name, got %q.", importedID),
		)
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	proxyGVK, releaseGVK, err := r.kinds(ctx, client)
	if err != nil {
		resp.Diagnostics.AddError("Resolving HelmChartProxy", err.Error())
		return
	}
	obj, err := client.Get(ctx, proxyGVK, parts[0], parts[1])
	if err != nil {
		resp.Diagnostics.AddError("HelmChartProxy not found", err.Error())
		return
	}
	releases, _, err := r.releases(ctx, client, releaseGVK, parts[0], parts[1])
	if err != nil {
		resp.Diagnostics.AddError("Reading helm releases", err.Error())
		return
	}
	live := readLiveHelmChartProxy(obj)
	data := helmChartProxyResourceModel{
		Timeouts: timeoutsNullValue(),
		ID: types.StringValue(
			importedID,
		),
		Name:            types.StringValue(parts[1]),
		Namespace:       types.StringValue(parts[0]),
		Cluster:         types.StringNull(),
		ClusterSelector: types.MapNull(types.StringType),
		Chart: types.ObjectNull(
			helmChartAttrTypes(),
		),
		Release: types.ObjectNull(helmChartReleaseAttrTypes()),
		Options: types.ObjectNull(
			helmOptionsAttrTypes(),
		),
		Credentials: types.ObjectNull(helmCredentialsAttrTypes()),
		TLS:         types.ObjectNull(helmTLSAttrTypes()),
		Wait: types.ObjectNull(
			objectWaitAttrTypes(),
		),
		ForceDelete: types.BoolValue(false),
		Manifest:    types.StringNull(),
	}
	// Prefer the one-cluster form when the selector is a cluster name.
	if len(live.ClusterSelector) == 1 && live.ClusterSelector[clusterNameLabel] != "" {
		data.Cluster = types.StringValue(live.ClusterSelector[clusterNameLabel])
	}
	resp.Diagnostics.Append(r.setFromLive(ctx, &data, live)...)
	resp.Diagnostics.Append(r.fill(ctx, &data, obj, releases)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	resp.Diagnostics.Append(
		setIdentity(ctx, resp.Identity, namespacedIdentity(data.Namespace, data.Name))...)
}
