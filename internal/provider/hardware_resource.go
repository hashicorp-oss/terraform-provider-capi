// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi"
	"github.com/tinkerbell-community/terraform-provider-capi/internal/capi/bmc"
)

var (
	_ resource.Resource                = &HardwareResource{}
	_ resource.ResourceWithConfigure   = &HardwareResource{}
	_ resource.ResourceWithImportState = &HardwareResource{}
)

// NewHardwareResource returns the capi_hardware resource.
func NewHardwareResource() resource.Resource {
	return &HardwareResource{}
}

// HardwareResource is one bare-metal machine registered with Tinkerbell on
// the management cluster: its Hardware, and, with a BMC, the rufio Machine
// and the credentials Secret, which have no life without each other. It is
// the inventory entry of capi_cluster as its own resource, the way
// aws_ec2_host is a dedicated machine.
type HardwareResource struct {
	pd *providerData
	// power opens a BMC client for force delete; replaced in tests.
	power func(creds bmc.Credentials) bmc.Client
}

// HardwareResourceModel is the capi_hardware model.
type HardwareResourceModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	Namespace types.String `tfsdk:"namespace"`
	Cluster   types.String `tfsdk:"cluster"`
	Network   types.Object `tfsdk:"network"`
	Disk      types.Object `tfsdk:"disk"`
	BMC       types.Object `tfsdk:"bmc"`
	Labels    types.Map    `tfsdk:"labels"`
	Adopted   types.Bool   `tfsdk:"adopted"`
	Force     types.Bool   `tfsdk:"force"`
	Manifest  types.String `tfsdk:"manifest"`
	Status    types.Object `tfsdk:"status"`
}

// HardwareStatusModel is the status block.
type HardwareStatusModel struct {
	Owner          types.String `tfsdk:"owner"`
	OwnerNamespace types.String `tfsdk:"owner_namespace"`
	Provisioned    types.Bool   `tfsdk:"provisioned"`
	AllowPXE       types.Bool   `tfsdk:"allow_pxe"`
}

func hardwareStatusAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"owner": types.StringType, "owner_namespace": types.StringType, "provisioned": types.BoolType, "allow_pxe": types.BoolType}
}

func (r *HardwareResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hardware"
}

func (r *HardwareResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One bare-metal machine registered with Tinkerbell on the management cluster the provider block names: a `Hardware`, and with `bmc` a rufio `Machine` and its credentials `Secret` (`<name>-bmc-auth`), which have no life without each other. The Tinkerbell stack (tink, rufio) must serve those kinds. Delete refuses while a TinkerbellMachine owns the Hardware, naming it; `force` powers the machine off through its BMC first. Import with `namespace/name`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name": schema.StringAttribute{
				MarkdownDescription: "Hostname; the object names.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"namespace": schema.StringAttribute{
				MarkdownDescription: "Namespace: the one CAPT looks for Hardware in, the cluster's. Default `default`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("default"),
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"cluster": schema.StringAttribute{
				MarkdownDescription: "Cluster name stamped as the `cluster.x-k8s.io/cluster-name` label.",
				Optional:            true,
			},
			"network": schema.SingleNestedAttribute{
				MarkdownDescription: "The primary interface.",
				Required:            true,
				Attributes: map[string]schema.Attribute{
					"mac_address": schema.StringAttribute{MarkdownDescription: "MAC address (`aa:bb:cc:dd:ee:01`); the agent ID.", Required: true},
					"ip_address":  schema.StringAttribute{MarkdownDescription: "Address the DHCP server hands out.", Optional: true},
					"netmask":     schema.StringAttribute{Optional: true},
					"gateway":     schema.StringAttribute{Optional: true},
					"nameservers": schema.ListAttribute{ElementType: types.StringType, Optional: true},
					"vlan_id":     schema.StringAttribute{Optional: true},
				},
			},
			"disk": schema.SingleNestedAttribute{
				MarkdownDescription: "The install disk.",
				Optional:            true,
				Attributes:          map[string]schema.Attribute{"device": schema.StringAttribute{MarkdownDescription: "Device path (`/dev/sda`).", Required: true}},
			},
			"bmc": schema.SingleNestedAttribute{
				MarkdownDescription: "Out-of-band management. Redfish with basic auth by default; Intel AMT when the port is 16992 or 16993.",
				Optional:            true,
				Attributes: map[string]schema.Attribute{
					"address":  schema.StringAttribute{MarkdownDescription: "`host`, `host:port` or `scheme://host:port`.", Required: true},
					"username": schema.StringAttribute{Required: true},
					"password": schema.StringAttribute{Required: true, Sensitive: true},
				},
			},
			"labels": metadataMapAttribute("Labels on the Hardware, which a machine template's `hardwareAffinity` selects by (`type = \"cp\"`)."),
			"adopted": schema.BoolAttribute{
				MarkdownDescription: "The machine already runs the cluster: mark the Hardware provisioned and keep it off PXE, so CAPT adopts the node rather than re-imaging it.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"force": schema.BoolAttribute{
				MarkdownDescription: "On destroy, power the machine off through its BMC and delete the objects even while a TinkerbellMachine owns the Hardware.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			"manifest": schema.StringAttribute{MarkdownDescription: "The applied objects as YAML, the Secret's credentials redacted.", Computed: true},
			"status": schema.SingleNestedAttribute{
				MarkdownDescription: "What CAPT has done with the Hardware.",
				Computed:            true,
				Attributes: map[string]schema.Attribute{
					"owner":           schema.StringAttribute{MarkdownDescription: "The TinkerbellMachine that claimed it, if any.", Computed: true},
					"owner_namespace": schema.StringAttribute{Computed: true},
					"provisioned":     schema.BoolAttribute{MarkdownDescription: "The provisioning workflow ran (or `adopted` set it).", Computed: true},
					"allow_pxe":       schema.BoolAttribute{Computed: true},
				},
			},
		},
	}
}

func (r *HardwareResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type", fmt.Sprintf("Expected *providerData, got: %T.", req.ProviderData))
		return
	}
	r.pd = pd
	if r.power == nil {
		r.power = func(creds bmc.Credentials) bmc.Client {
			return bmc.New(creds, log.New(os.Stderr, "[bmc] ", log.LstdFlags))
		}
	}
}

// --- model conversion ---

// machine converts the model to the inventory machine the renderer takes.
func (r *HardwareResource) machine(ctx context.Context, data *HardwareResourceModel) (capi.InventoryMachine, diag.Diagnostics) {
	var diags diag.Diagnostics
	m := capi.InventoryMachine{Hostname: data.Name.ValueString()}
	if !data.Network.IsNull() && !data.Network.IsUnknown() {
		var n NetworkModel
		diags.Append(data.Network.As(ctx, &n, basetypes.ObjectAsOptions{})...)
		m.IP, m.Netmask, m.Gateway, m.MAC, m.VLANID = n.IPAddress.ValueString(), n.Netmask.ValueString(), n.Gateway.ValueString(), n.MACAddress.ValueString(), n.VLANID.ValueString()
		ns, d := stringList(ctx, n.Nameservers)
		diags.Append(d...)
		m.Nameservers = ns
	}
	if !data.Disk.IsNull() && !data.Disk.IsUnknown() {
		var d DiskModel
		diags.Append(data.Disk.As(ctx, &d, basetypes.ObjectAsOptions{})...)
		m.Disk = d.Device.ValueString()
	}
	if !data.BMC.IsNull() && !data.BMC.IsUnknown() {
		var b BMCModel
		diags.Append(data.BMC.As(ctx, &b, basetypes.ObjectAsOptions{})...)
		m.BMC = &capi.InventoryBMC{Address: b.Address.ValueString(), Username: b.Username.ValueString(), Password: b.Password.ValueString()}
	}
	labels, d := stringMap(ctx, data.Labels)
	diags.Append(d...)
	m.Labels = labels
	return m, diags
}

// hardwareFacts is what a live Hardware (and its BMC objects) report.
type hardwareFacts struct {
	Cluster        string
	IP, Netmask    string
	Gateway, MAC   string
	Nameservers    []string
	VLANID         string
	Disk           string
	Labels         map[string]string
	Owner          string
	OwnerNamespace string
	Provisioned    bool
	AllowPXE       bool
	Adopted        bool
}

// readHardware flattens a Hardware object.
func readHardware(hw *unstructured.Unstructured) hardwareFacts {
	f := hardwareFacts{Labels: map[string]string{}, AllowPXE: true}
	for k, v := range hw.GetLabels() {
		switch k {
		case clusterNameLabel:
			f.Cluster = v
		case capi.HardwareOwnerNameLabel:
			f.Owner = v
		case capi.HardwareOwnerNamespaceLabel:
			f.OwnerNamespace = v
		default:
			f.Labels[k] = v
		}
	}
	_, f.Provisioned = hw.GetAnnotations()[capi.HardwareProvisionedAnnotation]
	ifaces, _, _ := unstructured.NestedSlice(hw.Object, "spec", "interfaces")
	if len(ifaces) > 0 {
		if iface, ok := ifaces[0].(map[string]any); ok {
			f.MAC, _, _ = unstructured.NestedString(iface, "dhcp", "mac")
			f.IP, _, _ = unstructured.NestedString(iface, "dhcp", "ip", "address")
			f.Netmask, _, _ = unstructured.NestedString(iface, "dhcp", "ip", "netmask")
			f.Gateway, _, _ = unstructured.NestedString(iface, "dhcp", "ip", "gateway")
			f.Nameservers, _, _ = unstructured.NestedStringSlice(iface, "dhcp", "name_servers")
			f.VLANID, _, _ = unstructured.NestedString(iface, "dhcp", "vlan_id")
			if allow, ok, _ := unstructured.NestedBool(iface, "netboot", "allowPXE"); ok {
				f.AllowPXE = allow
			}
		}
	}
	disks, _, _ := unstructured.NestedSlice(hw.Object, "spec", "disks")
	if len(disks) > 0 {
		if d, ok := disks[0].(map[string]any); ok {
			f.Disk, _ = d["device"].(string)
		}
	}
	f.Adopted = f.Provisioned && !f.AllowPXE
	return f
}

// setFromLive writes the configured attributes from the live objects. Only
// the labels the configuration names are compared.
func (r *HardwareResource) setFromLive(ctx context.Context, data *HardwareResourceModel, f hardwareFacts, bmcLive *BMCModel) diag.Diagnostics {
	var diags diag.Diagnostics
	data.Cluster = keepString(data.Cluster, f.Cluster)
	var n NetworkModel
	if !data.Network.IsNull() {
		diags.Append(data.Network.As(ctx, &n, basetypes.ObjectAsOptions{})...)
	}
	n.MACAddress = types.StringValue(f.MAC)
	n.IPAddress, n.Netmask, n.Gateway, n.VLANID = keepString(n.IPAddress, f.IP), keepString(n.Netmask, f.Netmask), keepString(n.Gateway, f.Gateway), keepString(n.VLANID, f.VLANID)
	n.Nameservers = types.ListNull(types.StringType)
	if len(f.Nameservers) > 0 {
		v, d := types.ListValueFrom(ctx, types.StringType, f.Nameservers)
		diags.Append(d...)
		n.Nameservers = v
	}
	nv, d := types.ObjectValueFrom(ctx, networkAttrTypes(), n)
	diags.Append(d...)
	data.Network = nv
	data.Disk = types.ObjectNull(diskAttrTypes())
	if f.Disk != "" {
		dv, d := types.ObjectValueFrom(ctx, diskAttrTypes(), DiskModel{Device: types.StringValue(f.Disk)})
		diags.Append(d...)
		data.Disk = dv
	}
	if bmcLive != nil {
		bv, d := types.ObjectValueFrom(ctx, bmcAttrTypes(), *bmcLive)
		diags.Append(d...)
		data.BMC = bv
	} else {
		data.BMC = types.ObjectNull(bmcAttrTypes())
	}
	if !data.Labels.IsNull() {
		configured, d := stringMap(ctx, data.Labels)
		diags.Append(d...)
		live := map[string]string{}
		for k := range configured {
			if v, ok := f.Labels[k]; ok {
				live[k] = v
			}
		}
		lv, d := types.MapValueFrom(ctx, types.StringType, live)
		diags.Append(d...)
		data.Labels = lv
	}
	data.Adopted = types.BoolValue(f.Adopted)
	st, d := types.ObjectValueFrom(ctx, hardwareStatusAttrTypes(), HardwareStatusModel{
		Owner: stringOrNull(f.Owner), OwnerNamespace: stringOrNull(f.OwnerNamespace), Provisioned: types.BoolValue(f.Provisioned), AllowPXE: types.BoolValue(f.AllowPXE),
	})
	diags.Append(d...)
	data.Status = st
	return diags
}

// readBMC reads the rufio Machine and the credentials Secret back.
func (r *HardwareResource) readBMC(ctx context.Context, client *capi.ObjectClient, namespace, name string) (*BMCModel, error) {
	machine, err := client.Get(ctx, gvkFor(capi.RufioMachineAPIVersion, capi.RufioMachineKind), namespace, name)
	if errors.Is(err, capi.ErrObjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	host, _, _ := unstructured.NestedString(machine.Object, "spec", "connection", "host")
	address := host
	if port, ok, _ := unstructured.NestedInt64(machine.Object, "spec", "connection", "port"); ok && port > 0 {
		address = fmt.Sprintf("%s:%d", host, port)
	}
	out := &BMCModel{Address: types.StringValue(address), Username: types.StringNull(), Password: types.StringNull()}
	secret, err := client.Get(ctx, capi.SecretGVK, namespace, capi.BMCSecretName(name))
	if err != nil && !errors.Is(err, capi.ErrObjectNotFound) {
		return nil, err
	}
	if secret != nil {
		data, _, _ := unstructured.NestedStringMap(secret.Object, "data")
		if u, err := base64.StdEncoding.DecodeString(data["username"]); err == nil {
			out.Username = types.StringValue(string(u))
		}
		if p, err := base64.StdEncoding.DecodeString(data["password"]); err == nil {
			out.Password = types.StringValue(string(p))
		}
	}
	return out, nil
}

// addressesMatch reports whether a configured BMC address names the same
// endpoint as the live one, which carries only host and port.
func addressesMatch(configured, live string) bool {
	trim := func(s string) string {
		if i := strings.Index(s, "://"); i >= 0 {
			s = s[i+3:]
		}
		return strings.TrimSuffix(strings.TrimSpace(s), "/")
	}
	return trim(configured) == trim(live)
}

// --- CRUD ---

func (r *HardwareResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data HardwareResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *HardwareResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data, prior HardwareResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &data, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	// A BMC removed from the configuration takes its objects with it.
	if data.BMC.IsNull() && !prior.BMC.IsNull() {
		client, d := r.pd.objectClient(ctx)
		resp.Diagnostics.Append(d...)
		if !d.HasError() {
			r.deleteBMC(ctx, client, data.Namespace.ValueString(), data.Name.ValueString(), &resp.Diagnostics)
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *HardwareResource) apply(ctx context.Context, data *HardwareResourceModel, diags *diag.Diagnostics) {
	client, d := r.pd.objectClient(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	m, d := r.machine(ctx, data)
	diags.Append(d...)
	if diags.HasError() {
		return
	}
	objs, err := capi.HardwareObjects(data.Namespace.ValueString(), data.Cluster.ValueString(), m, data.Adopted.ValueBool())
	if err != nil {
		diags.AddError("Invalid hardware", err.Error())
		return
	}
	var manifest []string
	for i := range objs {
		obj := &objs[i]
		if _, err := client.Apply(ctx, obj, false); err != nil {
			diags.AddError("Applying "+obj.GetKind()+" "+obj.GetName(), err.Error())
			return
		}
		shown := obj.DeepCopy()
		if shown.GetKind() == "Secret" {
			shown.Object["data"] = map[string]any{"username": "(redacted)", "password": "(redacted)"}
		}
		manifest = append(manifest, manifestYAML(shown))
	}
	data.ID = types.StringValue(fmt.Sprintf("%s/%s", data.Namespace.ValueString(), data.Name.ValueString()))
	data.Manifest = types.StringValue(strings.Join(manifest, "---\n"))
	hw := &objs[len(objs)-1]
	live, err := client.Get(ctx, hw.GroupVersionKind(), hw.GetNamespace(), hw.GetName())
	if err != nil {
		diags.AddError("Reading Hardware", err.Error())
		return
	}
	f := readHardware(live)
	st, d := types.ObjectValueFrom(ctx, hardwareStatusAttrTypes(), HardwareStatusModel{
		Owner: stringOrNull(f.Owner), OwnerNamespace: stringOrNull(f.OwnerNamespace), Provisioned: types.BoolValue(f.Provisioned), AllowPXE: types.BoolValue(f.AllowPXE),
	})
	diags.Append(d...)
	data.Status = st
}

func (r *HardwareResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data HardwareResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	namespace, name := data.Namespace.ValueString(), data.Name.ValueString()
	hw, found, d := readLive(ctx, client, gvkFor(capi.HardwareAPIVersion, capi.HardwareKind), namespace, name)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	bmcLive, err := r.readBMC(ctx, client, namespace, name)
	if err != nil {
		resp.Diagnostics.AddError("Reading BMC objects", err.Error())
		return
	}
	// Keep the configured address form when it names the live endpoint.
	if bmcLive != nil && !data.BMC.IsNull() {
		var configured BMCModel
		resp.Diagnostics.Append(data.BMC.As(ctx, &configured, basetypes.ObjectAsOptions{})...)
		if addressesMatch(configured.Address.ValueString(), bmcLive.Address.ValueString()) {
			bmcLive.Address = configured.Address
		}
	}
	resp.Diagnostics.Append(r.setFromLive(ctx, &data, readHardware(hw), bmcLive)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *HardwareResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data HardwareResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	namespace, name := data.Namespace.ValueString(), data.Name.ValueString()
	hwGVK := gvkFor(capi.HardwareAPIVersion, capi.HardwareKind)
	hw, found, d := readLive(ctx, client, hwGVK, namespace, name)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	if found {
		f := readHardware(hw)
		if f.Owner != "" {
			if !data.Force.ValueBool() {
				resp.Diagnostics.AddError("Hardware in use", fmt.Sprintf("Hardware %s/%s is owned by TinkerbellMachine %s/%s. Delete that machine's node group first, or set force = true to power the machine off and delete anyway.", namespace, name, f.OwnerNamespace, f.Owner))
				return
			}
			if !data.BMC.IsNull() {
				var b BMCModel
				resp.Diagnostics.Append(data.BMC.As(ctx, &b, basetypes.ObjectAsOptions{})...)
				if err := r.power(bmc.Credentials{Address: b.Address.ValueString(), Username: b.Username.ValueString(), Password: b.Password.ValueString()}).PowerOff(ctx); err != nil {
					resp.Diagnostics.AddError("Powering off "+name, err.Error())
					return
				}
			}
		}
		resp.Diagnostics.Append(deleteAndWait(ctx, client, hwGVK, namespace, name, 2*time.Minute)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	r.deleteBMC(ctx, client, namespace, name, &resp.Diagnostics)
}

// deleteBMC removes the rufio Machine and the credentials Secret.
func (r *HardwareResource) deleteBMC(ctx context.Context, client *capi.ObjectClient, namespace, name string, diags *diag.Diagnostics) {
	if err := client.Delete(ctx, gvkFor(capi.RufioMachineAPIVersion, capi.RufioMachineKind), namespace, name); err != nil {
		diags.AddError("Deleting rufio Machine "+name, err.Error())
	}
	if err := client.Delete(ctx, capi.SecretGVK, namespace, capi.BMCSecretName(name)); err != nil {
		diags.AddError("Deleting Secret "+capi.BMCSecretName(name), err.Error())
	}
}

// ImportState takes `namespace/name`.
func (r *HardwareResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected namespace/name, got %q.", req.ID))
		return
	}
	client, d := r.pd.objectClient(ctx)
	resp.Diagnostics.Append(d...)
	if d.HasError() {
		return
	}
	hw, err := client.Get(ctx, gvkFor(capi.HardwareAPIVersion, capi.HardwareKind), parts[0], parts[1])
	if err != nil {
		resp.Diagnostics.AddError("Hardware not found", err.Error())
		return
	}
	bmcLive, err := r.readBMC(ctx, client, parts[0], parts[1])
	if err != nil {
		resp.Diagnostics.AddError("Reading BMC objects", err.Error())
		return
	}
	f := readHardware(hw)
	data := HardwareResourceModel{
		ID: types.StringValue(req.ID), Name: types.StringValue(parts[1]), Namespace: types.StringValue(parts[0]),
		Cluster: types.StringNull(), Network: types.ObjectNull(networkAttrTypes()), Disk: types.ObjectNull(diskAttrTypes()), BMC: types.ObjectNull(bmcAttrTypes()),
		Force: types.BoolValue(false), Manifest: types.StringNull(),
	}
	labels, d := types.MapValueFrom(ctx, types.StringType, f.Labels)
	resp.Diagnostics.Append(d...)
	data.Labels = labels
	resp.Diagnostics.Append(r.setFromLive(ctx, &data, f, bmcLive)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
