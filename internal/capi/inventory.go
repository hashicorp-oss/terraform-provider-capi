// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"encoding/base64"
	"fmt"
	"maps"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Tinkerbell object identities the inventory renders. The API versions are
// those of the Tinkerbell stack's CRDs (tink, rufio), which are installed as an
// addon rather than discovered from clusterctl provider labels.
const (
	tinkHardwareAPIVersion = "tinkerbell.org/v1alpha1"
	tinkHardwareKind       = "Hardware"
	rufioAPIGroup          = "bmc.tinkerbell.org"
	rufioMachineAPIVersion = rufioAPIGroup + "/v1alpha1"
	rufioMachineKind       = "Machine"

	// hardwareProvisionedAnnotation is CAPT's marker for Hardware whose
	// workflow already ran. Pre-setting it on the bootstrap node's Hardware
	// makes CAPT adopt the running node instead of re-imaging it.
	hardwareProvisionedAnnotation = "v1alpha1.tinkerbell.org/provisioned"

	// defaultLeaseTime is the DHCP lease the Hardware advertises, in seconds.
	defaultLeaseTime = int64(86400)

	// Intel AMT listens on these ports; a BMC address naming one of them
	// selects rufio's IntelAMT provider instead of Redfish.
	amtPortPlain = 16992
	amtPortTLS   = 16993
)

// InventoryMachine is one bare-metal machine registered with Tinkerbell.
type InventoryMachine struct {
	Hostname    string
	IP          string
	Netmask     string
	Gateway     string
	MAC         string
	Nameservers []string
	VLANID      string
	Disk        string
	// BMC is nil for a machine without out-of-band management.
	BMC    *InventoryBMC
	Labels map[string]string
}

// InventoryBMC is a machine's BMC endpoint and credentials. Address accepts
// host, host:port, or scheme://host:port, like inventory.machine[].bmc.address.
type InventoryBMC struct {
	Address  string
	Username string
	Password string
}

// InventoryOptions configures InventoryManifests.
type InventoryOptions struct {
	// Namespace is where every object lands: the cluster namespace, which is
	// where CAPT looks for Hardware to claim.
	Namespace string
	// ClusterName is stamped as the cluster-name label on every object.
	ClusterName string
	// BootstrapMachine is the hostname of a machine already running the
	// cluster (an adopted node). Its Hardware is marked provisioned and
	// excluded from PXE so CAPT adopts the node rather than re-imaging it.
	// Empty means every machine is a normal netboot target.
	BootstrapMachine string
	Machines         []InventoryMachine
}

// CAPT's marks on a Hardware it has claimed for a TinkerbellMachine.
const (
	// HardwareOwnerNameLabel names the TinkerbellMachine that claimed the Hardware.
	HardwareOwnerNameLabel = "v1alpha1.tinkerbell.org/ownerName"
	// HardwareOwnerNamespaceLabel is that machine's namespace.
	HardwareOwnerNamespaceLabel = "v1alpha1.tinkerbell.org/ownerNamespace"
	// HardwareProvisionedAnnotation is hardwareProvisionedAnnotation, exported.
	HardwareProvisionedAnnotation = hardwareProvisionedAnnotation
	// HardwareAPIVersion and HardwareKind identify Tinkerbell Hardware.
	HardwareAPIVersion = tinkHardwareAPIVersion
	HardwareKind       = tinkHardwareKind
	// RufioMachineAPIVersion and RufioMachineKind identify a rufio Machine.
	RufioMachineAPIVersion = rufioMachineAPIVersion
	RufioMachineKind       = rufioMachineKind
)

// HardwareObjects renders one machine's objects in apply order: the BMC auth
// Secret and the rufio Machine when it has a BMC, then the Hardware. With
// adopted set, the Hardware is marked provisioned and excluded from PXE, so
// CAPT adopts the node it already runs rather than re-imaging it.
func HardwareObjects(
	namespace, clusterName string,
	m InventoryMachine,
	adopted bool,
) ([]unstructured.Unstructured, error) {
	if m.Hostname == "" {
		return nil, fmt.Errorf("hardware without a hostname")
	}
	opts := InventoryOptions{Namespace: namespace, ClusterName: clusterName}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if adopted {
		opts.BootstrapMachine = m.Hostname
	}
	var objs []unstructured.Unstructured
	if m.BMC != nil {
		secret, machine, err := bmcObjects(opts, m)
		if err != nil {
			return nil, fmt.Errorf("hardware %s: %w", m.Hostname, err)
		}
		objs = append(objs, secret, machine)
	}
	return append(objs, hardwareObject(opts, m)), nil
}

// BMCSecretName is the basic-auth Secret rufio reads a machine's BMC
// credentials from.
func BMCSecretName(hostname string) string { return bmcSecretName(hostname) }

// InventoryManifests renders, per machine, the BMC auth Secret, the rufio
// Machine and the Tinkerbell Hardware as one multi-document YAML stream in
// apply order. Machines are emitted sorted by hostname so the output is stable.
func InventoryManifests(opts InventoryOptions) ([]byte, error) {
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	machines := append([]InventoryMachine(nil), opts.Machines...)
	sort.Slice(machines, func(i, j int) bool { return machines[i].Hostname < machines[j].Hostname })

	var objs []unstructured.Unstructured
	for _, m := range machines {
		if m.Hostname == "" {
			return nil, fmt.Errorf("inventory machine without a hostname")
		}
		if m.BMC != nil {
			secret, machine, err := bmcObjects(opts, m)
			if err != nil {
				return nil, fmt.Errorf("inventory machine %s: %w", m.Hostname, err)
			}
			objs = append(objs, secret, machine)
		}
		objs = append(objs, hardwareObject(opts, m))
	}
	return renderManifest(objs)
}

// bmcSecretName is the basic-auth Secret rufio reads the BMC credentials from.
func bmcSecretName(hostname string) string { return hostname + "-bmc-auth" }

// bmcObjects renders the credentials Secret and the rufio Machine.
func bmcObjects(
	opts InventoryOptions,
	m InventoryMachine,
) (unstructured.Unstructured, unstructured.Unstructured, error) {
	host, port, scheme, err := parseBMCAddress(m.BMC.Address)
	if err != nil {
		return unstructured.Unstructured{}, unstructured.Unstructured{}, err
	}

	secret := inventoryObject(opts, "v1", "Secret", bmcSecretName(m.Hostname))
	secret.Object["type"] = "kubernetes.io/basic-auth"
	secret.Object["data"] = map[string]any{
		"username": base64.StdEncoding.EncodeToString([]byte(m.BMC.Username)),
		"password": base64.StdEncoding.EncodeToString([]byte(m.BMC.Password)),
	}

	connection := map[string]any{
		"host":        host,
		"insecureTLS": true,
		"authSecretRef": map[string]any{
			"name":      bmcSecretName(m.Hostname),
			"namespace": opts.Namespace,
		},
		"providerOptions": providerOptions(port, scheme),
	}
	if port > 0 {
		connection["port"] = int64(port)
	}
	machine := inventoryObject(opts, rufioMachineAPIVersion, rufioMachineKind, m.Hostname)
	machine.Object["spec"] = map[string]any{"connection": connection}
	return secret, machine, nil
}

// providerOptions pins rufio to one bmclib provider. Intel AMT is selected by
// its well-known ports; everything else is driven over Redfish with basic
// auth, which spares KVM-class BMCs (NanoKVM, JetKVM) the full provider probe
// that times out their sessions.
func providerOptions(port int, scheme string) map[string]any {
	if port == amtPortPlain || port == amtPortTLS {
		hostScheme := "http"
		if port == amtPortTLS || scheme == "https" {
			hostScheme = "https"
		}
		return map[string]any{
			"preferredOrder": []any{"IntelAMT"},
			"intelAMT":       map[string]any{"port": int64(port), "hostScheme": hostScheme},
		}
	}
	redfish := map[string]any{"useBasicAuth": true}
	if port > 0 {
		redfish["port"] = int64(port)
	}
	return map[string]any{
		"preferredOrder": []any{"gofish"},
		"redfish":        redfish,
	}
}

// hardwareObject renders the Tinkerbell Hardware for a machine.
func hardwareObject(opts InventoryOptions, m InventoryMachine) unstructured.Unstructured {
	hw := inventoryObject(opts, tinkHardwareAPIVersion, tinkHardwareKind, m.Hostname)
	labels := hw.GetLabels()
	maps.Copy(labels, m.Labels)
	hw.SetLabels(labels)

	bootstrap := opts.BootstrapMachine != "" && m.Hostname == opts.BootstrapMachine
	if bootstrap {
		hw.SetAnnotations(map[string]string{hardwareProvisionedAnnotation: "true"})
	}

	dhcp := map[string]any{
		"mac":        m.MAC,
		"hostname":   m.Hostname,
		"lease_time": defaultLeaseTime,
	}
	if m.IP != "" {
		ip := map[string]any{"address": m.IP}
		if m.Netmask != "" {
			ip["netmask"] = m.Netmask
		}
		if m.Gateway != "" {
			ip["gateway"] = m.Gateway
		}
		dhcp["ip"] = ip
	}
	if len(m.Nameservers) > 0 {
		dhcp["name_servers"] = toAnySlice(m.Nameservers)
	}
	if m.VLANID != "" {
		dhcp["vlan_id"] = m.VLANID
	}

	spec := map[string]any{
		"agentID": m.MAC,
		"metadata": map[string]any{
			"instance": map[string]any{"id": m.MAC, "hostname": m.Hostname},
		},
		"interfaces": []any{map[string]any{
			"dhcp": dhcp,
			"netboot": map[string]any{
				"allowPXE":      !bootstrap,
				"allowWorkflow": true,
			},
		}},
	}
	if m.Disk != "" {
		spec["disks"] = []any{map[string]any{"device": m.Disk}}
	}
	if m.BMC != nil {
		spec["bmcRef"] = map[string]any{
			"apiGroup": rufioAPIGroup,
			"kind":     rufioMachineKind,
			"name":     m.Hostname,
		}
	}
	hw.Object["spec"] = spec
	return hw
}

// inventoryObject returns a namespaced object carrying the cluster-name label.
func inventoryObject(
	opts InventoryOptions,
	apiVersion, kind, name string,
) unstructured.Unstructured {
	obj := unstructured.Unstructured{Object: map[string]any{}}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetName(name)
	obj.SetNamespace(opts.Namespace)
	labels := map[string]string{}
	if opts.ClusterName != "" {
		labels[clusterNameLabel] = opts.ClusterName
	}
	obj.SetLabels(labels)
	return obj
}

// parseBMCAddress splits host, host:port, or scheme://host:port.
func parseBMCAddress(address string) (host string, port int, scheme string, err error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", 0, "", fmt.Errorf("bmc address is empty")
	}
	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil {
			return "", 0, "", fmt.Errorf("parsing bmc address %q: %w", address, err)
		}
		host = u.Hostname()
		if host == "" {
			return "", 0, "", fmt.Errorf("bmc address %q has no host", address)
		}
		if p := u.Port(); p != "" {
			if port, err = strconv.Atoi(p); err != nil {
				return "", 0, "", fmt.Errorf("parsing bmc port in %q: %w", address, err)
			}
		}
		return host, port, strings.ToLower(u.Scheme), nil
	}
	if h, p, err := net.SplitHostPort(address); err == nil {
		port, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, "", fmt.Errorf("parsing bmc port in %q: %w", address, err)
		}
		return h, port, "", nil
	}
	return address, 0, "", nil
}
