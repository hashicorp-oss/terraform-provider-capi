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
	"fmt"
	"math/big"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

// ErrNoRootCredential is returned when a secrets map carries neither a Talos
// bundle nor a kubeadm CA key pair.
var ErrNoRootCredential = errors.New("no root credential in the cluster secrets")

// RootCredential is the Kubernetes CA key pair of a cluster, the one
// credential from which any other can be derived.
type RootCredential struct {
	CertPEM []byte
	KeyPEM  []byte
}

// talosBundle is the slice of a Talos secrets bundle this provider reads.
type talosBundle struct {
	Certs struct {
		K8s struct {
			Crt string `json:"crt"`
			Key string `json:"key"`
		} `json:"k8s"`
	} `json:"certs"`
}

// ParseRootSecrets finds the cluster CA in the secrets persisted for a
// cluster (see Manager.rootSecrets): the Talos machine-secrets bundle under
// "<name>-talos/bundle" (its certs.k8s pair, base64-encoded PEM), else the
// kubeadm CA under "<name>-ca/tls.crt" and "<name>-ca/tls.key".
func ParseRootSecrets(name string, secrets map[string]string) (*RootCredential, error) {
	if bundle, ok := secrets[name+"-talos/bundle"]; ok && bundle != "" {
		var b talosBundle
		if err := yaml.Unmarshal([]byte(bundle), &b); err != nil {
			return nil, fmt.Errorf("parsing the Talos secrets bundle: %w", err)
		}
		crt, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b.Certs.K8s.Crt))
		if err != nil {
			return nil, fmt.Errorf("decoding the Talos bundle's Kubernetes CA certificate: %w", err)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b.Certs.K8s.Key))
		if err != nil {
			return nil, fmt.Errorf("decoding the Talos bundle's Kubernetes CA key: %w", err)
		}
		if len(crt) == 0 || len(key) == 0 {
			return nil, fmt.Errorf(
				"%w: the Talos bundle has no certs.k8s pair",
				ErrNoRootCredential,
			)
		}
		return &RootCredential{CertPEM: crt, KeyPEM: key}, nil
	}
	crt, key := secrets[name+"-ca/tls.crt"], secrets[name+"-ca/tls.key"]
	if crt != "" && key != "" {
		return &RootCredential{CertPEM: []byte(crt), KeyPEM: []byte(key)}, nil
	}
	return nil, ErrNoRootCredential
}

// DeriveKubeconfig issues a client certificate for the cluster-admin group,
// valid for ttl from now, signed by the cluster CA, and returns a kubeconfig
// for server. It is what a self-managed cluster uses to reach itself, since
// no other authority holds a credential for it.
func DeriveKubeconfig(
	root *RootCredential,
	server, clusterName string,
	ttl time.Duration,
	now time.Time,
) ([]byte, error) {
	if root == nil {
		return nil, ErrNoRootCredential
	}
	if server == "" {
		return nil, errors.New("deriving a kubeconfig: the API server endpoint is required")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	caBlock, _ := pem.Decode(root.CertPEM)
	if caBlock == nil {
		return nil, errors.New("deriving a kubeconfig: the CA certificate is not PEM")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("deriving a kubeconfig: parsing the CA certificate: %w", err)
	}
	caKey, err := parsePrivateKey(root.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("deriving a kubeconfig: parsing the CA key: %w", err)
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("deriving a kubeconfig: generating the client key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("deriving a kubeconfig: generating a serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "terraform-provider-capi",
			Organization: []string{"system:masters"},
		},
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(ttl),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("deriving a kubeconfig: signing the client certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		return nil, fmt.Errorf("deriving a kubeconfig: encoding the client key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if clusterName == "" {
		clusterName = "cluster"
	}
	user := "terraform-provider-capi"
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: root.CertPEM,
	}
	cfg.AuthInfos[user] = &clientcmdapi.AuthInfo{
		ClientCertificateData: certPEM,
		ClientKeyData:         keyPEM,
	}
	cfg.Contexts[clusterName] = &clientcmdapi.Context{Cluster: clusterName, AuthInfo: user}
	cfg.CurrentContext = clusterName
	return clientcmd.Write(*cfg)
}

// parsePrivateKey accepts the PEM key encodings CAs are issued with.
func parsePrivateKey(keyPEM []byte) (any, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("unsupported key type %q", block.Type)
}

// KubeconfigConnection is what a kubeconfig's current context connects with.
type KubeconfigConnection struct {
	Server                   string
	CertificateAuthorityData []byte
	ClientCertificateData    []byte
	ClientKeyData            []byte
	Token                    string
}

// ParseKubeconfig returns the connection details of a kubeconfig's current
// context (the first context when none is current).
func ParseKubeconfig(raw []byte) (*KubeconfigConnection, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	name := cfg.CurrentContext
	if name == "" {
		for n := range cfg.Contexts {
			name = n
			break
		}
	}
	ctx, ok := cfg.Contexts[name]
	if !ok {
		return nil, errors.New("parsing kubeconfig: no context")
	}
	cluster, ok := cfg.Clusters[ctx.Cluster]
	if !ok {
		return nil, fmt.Errorf("parsing kubeconfig: context %q names no cluster", name)
	}
	out := &KubeconfigConnection{
		Server:                   cluster.Server,
		CertificateAuthorityData: cluster.CertificateAuthorityData,
	}
	if user, ok := cfg.AuthInfos[ctx.AuthInfo]; ok {
		out.ClientCertificateData = user.ClientCertificateData
		out.ClientKeyData = user.ClientKeyData
		out.Token = user.Token
	}
	return out, nil
}
