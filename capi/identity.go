// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Resource identities: what names an object on the management cluster,
// for an `import` block with `identity` (Terraform 1.12 and later). Each
// identity maps onto the resource's import ID, so both imports run the same
// code.

// namespacedIdentityModel identifies a namespaced object by name.
type namespacedIdentityModel struct {
	Namespace types.String `tfsdk:"namespace"`
	Name      types.String `tfsdk:"name"`
}

func namespacedIdentitySchema() identityschema.Schema {
	return identityschema.Schema{
		Attributes: map[string]identityschema.Attribute{
			"namespace": identityschema.StringAttribute{
				Description:       "Namespace of the object. Default `default`.",
				OptionalForImport: true,
			},
			"name": identityschema.StringAttribute{
				Description:       "Name of the object.",
				RequiredForImport: true,
			},
		},
	}
}

func namespacedIdentity(namespace, name types.String) namespacedIdentityModel {
	return namespacedIdentityModel{Namespace: defaultNamespace(namespace), Name: name}
}

// namespacedImportID is the `namespace/name` import ID of an identity.
func namespacedImportID(id namespacedIdentityModel) string {
	return defaultNamespace(id.Namespace).ValueString() + "/" + id.Name.ValueString()
}

// nodeGroupIdentityModel identifies a node group by its cluster.
type nodeGroupIdentityModel struct {
	Namespace types.String `tfsdk:"namespace"`
	Cluster   types.String `tfsdk:"cluster"`
	Name      types.String `tfsdk:"name"`
}

func nodeGroupIdentitySchema() identityschema.Schema {
	return identityschema.Schema{
		Attributes: map[string]identityschema.Attribute{
			"namespace": identityschema.StringAttribute{
				Description:       "Namespace of the Cluster. Default `default`.",
				OptionalForImport: true,
			},
			"cluster": identityschema.StringAttribute{
				Description:       "Name of the Cluster.",
				RequiredForImport: true,
			},
			"name": identityschema.StringAttribute{
				Description:       "Group name: the MachineDeployment's, or the topology entry's.",
				RequiredForImport: true,
			},
		},
	}
}

func nodeGroupIdentity(namespace, cluster, name types.String) nodeGroupIdentityModel {
	return nodeGroupIdentityModel{
		Namespace: defaultNamespace(namespace),
		Cluster:   cluster,
		Name:      name,
	}
}

func nodeGroupImportID(id nodeGroupIdentityModel) string {
	return defaultNamespace(
		id.Namespace,
	).ValueString() +
		"/" + id.Cluster.ValueString() + "/" + id.Name.ValueString()
}

// providerIdentityModel identifies a clusterctl provider.
type providerIdentityModel struct {
	Type types.String `tfsdk:"type"`
	Name types.String `tfsdk:"name"`
}

func providerIdentitySchema() identityschema.Schema {
	return identityschema.Schema{
		Attributes: map[string]identityschema.Attribute{
			"type": identityschema.StringAttribute{
				Description:       "Provider type: core, infrastructure, bootstrap, control-plane, ipam or addon.",
				RequiredForImport: true,
			},
			"name": identityschema.StringAttribute{
				Description:       "Provider name as clusterctl knows it.",
				RequiredForImport: true,
			},
		},
	}
}

func providerIdentity(typ, name types.String) providerIdentityModel {
	return providerIdentityModel{Type: typ, Name: name}
}

func providerImportID(id providerIdentityModel) string {
	return id.Type.ValueString() + "/" + id.Name.ValueString()
}

// objectIdentityModel identifies any object by kind.
type objectIdentityModel struct {
	Kind      types.String `tfsdk:"kind"`
	Namespace types.String `tfsdk:"namespace"`
	Name      types.String `tfsdk:"name"`
}

func objectIdentitySchema() identityschema.Schema {
	return identityschema.Schema{
		Attributes: map[string]identityschema.Attribute{
			"kind": identityschema.StringAttribute{
				Description:       "Kind of the object.",
				RequiredForImport: true,
			},
			"namespace": identityschema.StringAttribute{
				Description:       "Namespace of the object. Default `default`.",
				OptionalForImport: true,
			},
			"name": identityschema.StringAttribute{
				Description:       "Name of the object.",
				RequiredForImport: true,
			},
		},
	}
}

func objectIdentity(kind, namespace, name types.String) objectIdentityModel {
	return objectIdentityModel{Kind: kind, Namespace: defaultNamespace(namespace), Name: name}
}

func objectImportID(id objectIdentityModel) string {
	return id.Kind.ValueString() + ":" + defaultNamespace(
		id.Namespace,
	).ValueString() +
		"/" + id.Name.ValueString()
}

// defaultNamespace is the namespace a null or empty value stands for.
func defaultNamespace(ns types.String) types.String {
	if ns.IsNull() || ns.IsUnknown() || ns.ValueString() == "" {
		return types.StringValue("default")
	}
	return ns
}

// setIdentity records a resource's identity next to its state; responses
// built outside the framework (unit tests) carry none.
func setIdentity(ctx context.Context, identity *tfsdk.ResourceIdentity, v any) diag.Diagnostics {
	if identity == nil {
		return nil
	}
	return identity.Set(ctx, v)
}

// importID is the ID an import works from: the one given, or for an import
// by identity the same ID built from the identity.
func importID[T any](
	ctx context.Context,
	req resource.ImportStateRequest,
	format func(T) string,
) (string, diag.Diagnostics) {
	if req.ID != "" || req.Identity == nil {
		return req.ID, nil
	}
	var id T
	diags := req.Identity.Get(ctx, &id)
	if diags.HasError() {
		return "", diags
	}
	return format(id), diags
}
