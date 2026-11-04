// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package talos

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

func TestPlatformNetworkDocument_MatchesTalosmetaShape(t *testing.T) {
	doc, err := platformNetworkDocument(NetworkPin{
		Hostname: "cp-1", Address: "10.1.40.35/16", Gateway: "10.0.0.1", Nameservers: []string{"10.0.0.1", "1.1.1.1"},
	}, "enp1s0")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(doc, &got); err != nil {
		t.Fatalf("document is not YAML: %v\n%s", err, doc)
	}
	addr := asMap(t, asSlice(t, got["addresses"])[0])
	if addr["address"] != "10.1.40.35/16" || addr["linkName"] != "enp1s0" || addr["layer"] != "platform" || addr["family"] != "inet4" {
		t.Errorf("address = %v", addr)
	}
	route := asMap(t, asSlice(t, got["routes"])[0])
	if route["gateway"] != "10.0.0.1" || route["outLinkName"] != "enp1s0" || route["table"] != "main" || route["priority"] != 1024 {
		t.Errorf("route = %v", route)
	}
	if _, hasDst := route["dst"]; hasDst {
		t.Error("the default route carries no dst, like talosmeta")
	}
	host := asMap(t, asSlice(t, got["hostnames"])[0])
	if host["hostname"] != "cp-1" || host["layer"] != "platform" {
		t.Errorf("hostname = %v", host)
	}
	res := asMap(t, asSlice(t, got["resolvers"])[0])
	if len(asSlice(t, res["dnsServers"])) != 2 {
		t.Errorf("resolvers = %v", res)
	}
	for _, key := range []string{"links", "timeServers", "operators", "externalIPs"} {
		if _, ok := got[key]; !ok {
			t.Errorf("document must carry %q", key)
		}
	}
}

func TestPlatformNetworkDocument_Minimal(t *testing.T) {
	doc, err := platformNetworkDocument(NetworkPin{Address: "10.0.0.5/24"}, "eth0")
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if strings.Contains(s, "gateway") || strings.Contains(s, "dnsServers") || !strings.Contains(s, "hostnames: []") {
		t.Errorf("minimal pin must have no route, resolver or hostname:\n%s", s)
	}
}

func TestPlatformNetworkDocument_RejectsBadInput(t *testing.T) {
	for _, pin := range []NetworkPin{
		{Address: "10.0.0.5"},
		{Address: "10.0.0.5/24", Gateway: "gw"},
		{Address: "10.0.0.5/24", Nameservers: []string{"dns.example"}},
	} {
		if _, err := platformNetworkDocument(pin, "eth0"); err == nil {
			t.Errorf("pin %+v must be rejected", pin)
		}
	}
}

func TestPickLinkName(t *testing.T) {
	pin := NetworkPin{Address: "10.0.0.5/24", MAC: "AA:BB:CC:DD:EE:01"}
	if got, _ := pickLinkName(pin, map[string]string{"10.0.0.5": "enp1s0"}, map[string]string{"aa:bb:cc:dd:ee:01": "eth9"}); got != "enp1s0" {
		t.Errorf("address match wins, got %q", got)
	}
	if got, _ := pickLinkName(pin, nil, map[string]string{"aa:bb:cc:dd:ee:01": "eth9"}); got != "eth9" {
		t.Errorf("MAC fallback, got %q", got)
	}
	if _, err := pickLinkName(pin, nil, nil); err == nil {
		t.Error("no match must be an error")
	}
}
