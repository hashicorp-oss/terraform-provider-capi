// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"fmt"
	"net/netip"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// metaNetworkKey is Talos's META key for the metal platform network
// configuration (MetalNetworkPlatformConfig), what `talosctl meta write 0xa`
// writes. Talos layers it under the machine config, so identity written here
// survives a machine-config apply that no longer carries it.
const metaNetworkKey uint8 = 0x0a

// NetworkPin is the identity written to META so an adopted node keeps its
// hostname and address through in-place config updates.
type NetworkPin struct {
	Hostname string
	// Address is the node address with prefix length (10.1.40.35/16).
	Address     string
	Gateway     string
	Nameservers []string
	// MAC selects the link when the address is not yet on one.
	MAC string
}

// platformNetworkDocument renders the META document in the shape the
// talosmeta Tinkerbell action writes for CAPT-provisioned nodes (every entry on
// the platform layer), so adopted and provisioned nodes carry the same
// configuration source.
func platformNetworkDocument(pin NetworkPin, linkName string) ([]byte, error) {
	prefix, err := netip.ParsePrefix(pin.Address)
	if err != nil {
		return nil, fmt.Errorf("parsing pinned address %q: %w", pin.Address, err)
	}
	family := "inet4"
	if prefix.Addr().Is6() {
		family = "inet6"
	}
	doc := map[string]any{
		"addresses": []any{map[string]any{
			"address": prefix.String(), "linkName": linkName, "family": family,
			"scope": "global", "flags": "permanent", "layer": "platform",
		}},
		"links":       []any{map[string]any{"name": linkName, "up": true, "layer": "platform"}},
		"routes":      []any{},
		"hostnames":   []any{},
		"resolvers":   []any{},
		"timeServers": []any{},
		"operators":   []any{},
		"externalIPs": []any{},
	}
	if pin.Gateway != "" {
		gw, err := netip.ParseAddr(pin.Gateway)
		if err != nil {
			return nil, fmt.Errorf("parsing gateway %q: %w", pin.Gateway, err)
		}
		priority := 1024
		if gw.Is6() {
			priority = 2048
		}
		doc["routes"] = []any{map[string]any{
			"family": family, "gateway": gw.String(), "outLinkName": linkName, "table": "main",
			"priority": priority, "scope": "global", "type": "unicast", "protocol": "static", "layer": "platform",
		}}
	}
	if pin.Hostname != "" {
		doc["hostnames"] = []any{map[string]any{"hostname": pin.Hostname, "layer": "platform"}}
	}
	if len(pin.Nameservers) > 0 {
		servers := make([]any, 0, len(pin.Nameservers))
		for _, ns := range pin.Nameservers {
			if _, err := netip.ParseAddr(strings.TrimSpace(ns)); err != nil {
				return nil, fmt.Errorf("parsing nameserver %q: %w", ns, err)
			}
			servers = append(servers, strings.TrimSpace(ns))
		}
		doc["resolvers"] = []any{map[string]any{"dnsServers": servers, "layer": "platform"}}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("rendering platform network config: %w", err)
	}
	return out, nil
}

// pickLinkName chooses the link the pin belongs to: the link that already
// carries the address, else the link with the pin's MAC. addrLinks maps
// address (without prefix) to link name; macLinks maps lower-case MAC to link
// name.
func pickLinkName(pin NetworkPin, addrLinks, macLinks map[string]string) (string, error) {
	prefix, err := netip.ParsePrefix(pin.Address)
	if err != nil {
		return "", fmt.Errorf("parsing pinned address %q: %w", pin.Address, err)
	}
	if name := addrLinks[prefix.Addr().String()]; name != "" {
		return name, nil
	}
	if mac := strings.ToLower(strings.TrimSpace(pin.MAC)); mac != "" {
		if name := macLinks[mac]; name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("no link carries %s or has MAC %q", prefix.Addr(), pin.MAC)
}
