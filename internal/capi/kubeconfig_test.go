// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

// testCA returns a self-signed CA as PEM certificate and key.
func testCA(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubernetes"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestParseRootSecrets(t *testing.T) {
	crt, key := testCA(t)
	bundle := "cluster:\n  id: x\ncerts:\n  k8s:\n    crt: " + base64.StdEncoding.EncodeToString(crt) + "\n    key: " + base64.StdEncoding.EncodeToString(key) + "\n"
	root, err := ParseRootSecrets("prod", map[string]string{"prod-talos/bundle": bundle})
	if err != nil || string(root.CertPEM) != string(crt) || string(root.KeyPEM) != string(key) {
		t.Fatalf("talos bundle: %v %v", root, err)
	}
	root, err = ParseRootSecrets("prod", map[string]string{"prod-ca/tls.crt": string(crt), "prod-ca/tls.key": string(key)})
	if err != nil || string(root.CertPEM) != string(crt) {
		t.Fatalf("kubeadm ca: %v %v", root, err)
	}
	if _, err := ParseRootSecrets("prod", map[string]string{"other/x": "y"}); !errors.Is(err, ErrNoRootCredential) {
		t.Fatalf("err = %v, want ErrNoRootCredential", err)
	}
	if _, err := ParseRootSecrets("prod", map[string]string{"prod-talos/bundle": "certs: {k8s: {crt: '!!', key: '!!'}}"}); err == nil {
		t.Fatal("a bundle that is not base64 must fail")
	}
}

func TestDeriveKubeconfig(t *testing.T) {
	crt, key := testCA(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	raw, err := DeriveKubeconfig(&RootCredential{CertPEM: crt, KeyPEM: key}, "https://10.0.0.160:6443", "prod", 2*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "prod" || cfg.Clusters["prod"].Server != "https://10.0.0.160:6443" || string(cfg.Clusters["prod"].CertificateAuthorityData) != string(crt) {
		t.Errorf("kubeconfig = %+v", cfg)
	}
	user := cfg.AuthInfos["terraform-provider-capi"]
	block, _ := pem.Decode(user.ClientCertificateData)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caBlock, _ := pem.Decode(crt)
	ca, _ := x509.ParseCertificate(caBlock.Bytes)
	if err := cert.CheckSignatureFrom(ca); err != nil {
		t.Errorf("the client certificate must be signed by the cluster CA: %v", err)
	}
	if cert.Subject.Organization[0] != "system:masters" || !cert.NotAfter.Equal(now.Add(2*time.Hour)) {
		t.Errorf("subject %v, notAfter %v", cert.Subject, cert.NotAfter)
	}
	if _, err := x509.ParseECPrivateKey(mustDecodePEM(t, user.ClientKeyData)); err != nil {
		t.Errorf("client key: %v", err)
	}

	info, err := ParseKubeconfig(raw)
	if err != nil || info.Server != "https://10.0.0.160:6443" || len(info.ClientCertificateData) == 0 || len(info.ClientKeyData) == 0 || info.Token != "" {
		t.Errorf("ParseKubeconfig = %+v, %v", info, err)
	}

	if _, err := DeriveKubeconfig(&RootCredential{CertPEM: crt, KeyPEM: key}, "", "prod", time.Hour, now); err == nil {
		t.Error("an endpoint is required")
	}
	if _, err := DeriveKubeconfig(nil, "https://x", "prod", time.Hour, now); !errors.Is(err, ErrNoRootCredential) {
		t.Errorf("err = %v", err)
	}
	if _, err := ParseKubeconfig([]byte("not: [yaml")); err == nil {
		t.Error("garbage must fail")
	}
}

func mustDecodePEM(t *testing.T, data []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("not PEM")
	}
	return block.Bytes
}
