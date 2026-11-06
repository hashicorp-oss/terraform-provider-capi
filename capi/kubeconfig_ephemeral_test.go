// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func testClusterCA(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubernetes"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(240 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		), string(
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		)
}

// A derived kubeconfig asks for no renewal (the resource has no Renew, and a
// renewed certificate could not reach provider blocks anyway) and outlasts a
// long apply by default.
func TestKubeconfigEphemeral_DerivedOutlastsTheRun(t *testing.T) {
	ctx := context.Background()
	e := &kubeconfigEphemeralResource{}
	schemaResp := &ephemeral.SchemaResponse{}
	e.Schema(ctx, ephemeral.SchemaRequest{}, schemaResp)

	crt, key := testClusterCA(t)
	secrets, d := providerSecretsToDynamic(
		ctx,
		map[string]string{"prod-ca/tls.crt": crt, "prod-ca/tls.key": key},
	)
	if d.HasError() {
		t.Fatal(d)
	}
	st := tfsdk.State{
		Schema: schemaResp.Schema,
		Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
	}
	if d := st.Set(ctx, &kubeconfigEphemeralModel{
		Cluster:              types.StringValue("prod"),
		Namespace:            types.StringNull(),
		Secrets:              secrets,
		Endpoint:             types.StringValue("https://10.1.40.100:6443"),
		TTL:                  types.StringNull(),
		Raw:                  types.StringNull(),
		Host:                 types.StringNull(),
		ClusterCACertificate: types.StringNull(),
		ClientCertificate:    types.StringNull(),
		ClientKey:            types.StringNull(),
		Token:                types.StringNull(),
	}); d.HasError() {
		t.Fatal(d)
	}
	resp := &ephemeral.OpenResponse{
		Result: tfsdk.EphemeralResultData{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
		},
	}
	e.Open(
		ctx,
		ephemeral.OpenRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: st.Raw}},
		resp,
	)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if !resp.RenewAt.IsZero() {
		t.Errorf(
			"RenewAt = %s: Terraform would call a Renew the resource does not implement",
			resp.RenewAt,
		)
	}
	var out kubeconfigEphemeralModel
	if d := resp.Result.Get(ctx, &out); d.HasError() {
		t.Fatal(d)
	}
	block, _ := pem.Decode([]byte(out.ClientCertificate.ValueString()))
	if block == nil {
		t.Fatalf("no client certificate in %q", out.ClientCertificate.ValueString())
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(cert.NotAfter); left < 7*time.Hour {
		t.Errorf("the certificate is valid for %s only", left.Round(time.Minute))
	}
}
