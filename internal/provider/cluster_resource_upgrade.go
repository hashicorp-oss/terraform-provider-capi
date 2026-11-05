// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
)

// providerAttrNames lists every attribute of the provider object. The JSON
// handed back to Terraform must carry all of them (null when unset) or it
// will not decode against the schema.
var providerAttrNames = []string{
	"version", "fetch_config", "config_variables", "secret_config_variables",
	"deployment", "manager", "additional_manifests", "manifest_patches", "patches",
	"resource_config",
}

// providerMapAttrs are the six provider maps.
var providerMapAttrs = []string{"core", "infrastructure", "bootstrap", "control_plane", "ipam", "addon"}

var nullJSON = json.RawMessage("null")

// upgradeV1JSON rewrites raw v1 state (objects holding a "name:version"
// provider string plus an addons list) into the current state: first the v2
// shape (provider maps and topology), then v3, then v4. Attributes that did
// not change shape pass through untouched.
func upgradeV1JSON(raw []byte) ([]byte, error) {
	v2, err := upgradeV1ToV2JSON(raw)
	if err != nil {
		return nil, err
	}
	return upgradeV2JSON(v2)
}

// upgradeV2JSON rewrites raw v2 state into the current state: the v3 rewrite,
// then v4.
func upgradeV2JSON(raw []byte) ([]byte, error) {
	v3, err := upgradeV2ToV3JSON(raw)
	if err != nil {
		return nil, err
	}
	return upgradeV3JSON(v3)
}

// upgradeV3JSON rewrites raw v3 state into v4: the Talos bootstrapper, its
// in-place mode, the cni block and management.addons are gone. A cluster
// bootstrapped in place, or by a Talos node, cannot be carried over: its
// bootstrap node is a cluster member this provider no longer knows how to
// manage, so the state must be dropped and the cluster imported.
func upgradeV3JSON(raw []byte) ([]byte, error) {
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decoding v3 state: %w", err)
	}
	delete(state, "cni")

	mgmt, err := rawObject(state["management"])
	if err != nil {
		return nil, fmt.Errorf("management: %w", err)
	}
	if mgmt != nil {
		delete(mgmt, "addons")
		bs, err := rawObject(mgmt["bootstrap"])
		if err != nil {
			return nil, fmt.Errorf("management.bootstrap: %w", err)
		}
		if bs != nil {
			var typ, mode string
			_ = json.Unmarshal(bs["type"], &typ)
			_ = json.Unmarshal(bs["mode"], &mode)
			name := ""
			_ = json.Unmarshal(state["name"], &name)
			if typ == "talos" || mode == "in_place" {
				return nil, fmt.Errorf("cluster %q was bootstrapped with management.bootstrap.type = \"talos\" or mode = \"in_place\", which this version no longer supports: remove it from state (terraform state rm) and import it as a cluster managed through its own kubeconfig", name)
			}
			delete(bs, "mode")
			delete(bs, "state_dir")
			delete(bs, "talos")
			if mgmt["bootstrap"], err = json.Marshal(bs); err != nil {
				return nil, err
			}
		}
		if state["management"], err = json.Marshal(mgmt); err != nil {
			return nil, err
		}
	}
	return json.Marshal(state)
}

// upgradeV2ToV3JSON rewrites raw v2 state into v3: template and flavor go away,
// cluster_class and network appear, every provider entry gains resource_config,
// and the topology entries gain their machine template specs.
func upgradeV2ToV3JSON(raw []byte) ([]byte, error) {
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decoding v2 state: %w", err)
	}
	delete(state, "template")
	delete(state, "flavor")
	state["cluster_class"] = json.RawMessage("false")
	state["network"] = nullJSON
	if _, ok := state["manifests"]; !ok {
		state["manifests"] = nullJSON
	}
	if err := upgradeInventoryAndAddonsJSON(state); err != nil {
		return nil, err
	}

	for _, attr := range providerMapAttrs {
		entries, err := rawObject(state[attr])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", attr, err)
		}
		if entries == nil {
			state[attr] = nullJSON
			continue
		}
		for name, raw := range entries {
			entry, err := rawObject(raw)
			if err != nil {
				return nil, fmt.Errorf("%s[%s]: %w", attr, name, err)
			}
			if entry == nil {
				continue
			}
			for _, a := range providerAttrNames {
				if _, ok := entry[a]; !ok {
					entry[a] = nullJSON
				}
			}
			b, err := json.Marshal(entry)
			if err != nil {
				return nil, err
			}
			entries[name] = b
		}
		b, err := json.Marshal(entries)
		if err != nil {
			return nil, err
		}
		state[attr] = b
	}

	topology, err := rawObject(state["topology"])
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	if topology != nil {
		cp, err := rawObject(topology["control_plane"])
		if err != nil {
			return nil, fmt.Errorf("topology.control_plane: %w", err)
		}
		if cp != nil {
			if _, ok := cp["infrastructure"]; !ok {
				cp["infrastructure"] = nullJSON
			}
			if topology["control_plane"], err = json.Marshal(cp); err != nil {
				return nil, err
			}
		}
		workers, err := rawObject(topology["workers"])
		if err != nil {
			return nil, fmt.Errorf("topology.workers: %w", err)
		}
		if workers != nil && len(workers["machine_deployments"]) > 0 && string(workers["machine_deployments"]) != "null" {
			var mds []map[string]json.RawMessage
			if err := json.Unmarshal(workers["machine_deployments"], &mds); err != nil {
				return nil, fmt.Errorf("topology.workers.machine_deployments: %w", err)
			}
			for _, md := range mds {
				for _, a := range []string{"infrastructure", "bootstrap"} {
					if _, ok := md[a]; !ok {
						md[a] = nullJSON
					}
				}
			}
			if workers["machine_deployments"], err = json.Marshal(mds); err != nil {
				return nil, err
			}
			if topology["workers"], err = json.Marshal(workers); err != nil {
				return nil, err
			}
		}
		if state["topology"], err = json.Marshal(topology); err != nil {
			return nil, err
		}
	}

	return json.Marshal(state)
}

// upgradeV1ToV2JSON is the v1 -> v2 rewrite (provider maps and topology).
func upgradeV1ToV2JSON(raw []byte) ([]byte, error) {
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decoding v1 state: %w", err)
	}

	cpCount, workerCount := nullJSON, nullJSON

	for _, attr := range []string{"infrastructure", "bootstrap", "control_plane", "core"} {
		obj, err := rawObject(state[attr])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", attr, err)
		}
		if obj == nil {
			state[attr] = nullJSON
			continue
		}
		if attr == "control_plane" {
			if mc, ok := obj["machine_count"]; ok {
				cpCount = mc
			}
		}
		m, err := providerMapJSON(obj["provider"])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", attr, err)
		}
		state[attr] = m
	}

	workers, err := rawObject(state["workers"])
	if err != nil {
		return nil, fmt.Errorf("workers: %w", err)
	}
	if workers != nil {
		if mc, ok := workers["machine_count"]; ok {
			workerCount = mc
		}
	}
	delete(state, "workers")

	addonMap := map[string]json.RawMessage{}
	if len(state["addons"]) > 0 && string(state["addons"]) != "null" {
		var addons []map[string]json.RawMessage
		if err := json.Unmarshal(state["addons"], &addons); err != nil {
			return nil, fmt.Errorf("addons: %w", err)
		}
		for _, a := range addons {
			name, entry, err := providerEntryJSON(a["provider"], a)
			if err != nil {
				return nil, fmt.Errorf("addons: %w", err)
			}
			addonMap[name] = entry
		}
	}
	delete(state, "addons")
	if len(addonMap) == 0 {
		state["addon"] = nullJSON
	} else {
		b, err := json.Marshal(addonMap)
		if err != nil {
			return nil, err
		}
		state["addon"] = b
	}
	state["ipam"] = nullJSON
	state["template"] = nullJSON

	topology := map[string]json.RawMessage{
		"control_plane": controlPlaneTopologyJSON(cpCount),
		"workers":       workersTopologyJSON(workerCount),
	}
	if string(topology["control_plane"]) == "null" && string(topology["workers"]) == "null" {
		state["topology"] = nullJSON
	} else {
		b, err := json.Marshal(topology)
		if err != nil {
			return nil, err
		}
		state["topology"] = b
	}

	return json.Marshal(state)
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// providerMapJSON builds {name: providerObject} from a "name:version" string.
func providerMapJSON(providerRaw json.RawMessage) (json.RawMessage, error) {
	name, entry, err := providerEntryJSON(providerRaw, nil)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{name: entry})
}

// providerEntryJSON returns the provider name and a full v2 provider object,
// copying any matching attributes from extra (a v1 addons element).
func providerEntryJSON(providerRaw json.RawMessage, extra map[string]json.RawMessage) (string, json.RawMessage, error) {
	var providerStr string
	if err := json.Unmarshal(providerRaw, &providerStr); err != nil {
		return "", nil, fmt.Errorf("provider must be a string: %w", err)
	}
	name, version := splitNameVersion(providerStr)
	if name == "" {
		return "", nil, fmt.Errorf("empty provider name in %q", providerStr)
	}

	entry := map[string]json.RawMessage{}
	for _, a := range providerAttrNames {
		entry[a] = nullJSON
		if v, ok := extra[a]; ok && len(v) > 0 {
			entry[a] = v
		}
	}
	if version != "" {
		b, err := json.Marshal(version)
		if err != nil {
			return "", nil, err
		}
		entry["version"] = b
	}

	// v1 fetch_config had only url/oci; v2 adds owner/repository.
	fc, err := rawObject(entry["fetch_config"])
	if err != nil {
		return "", nil, fmt.Errorf("fetch_config: %w", err)
	}
	if fc != nil {
		for _, k := range []string{"owner", "repository", "url", "oci"} {
			if _, ok := fc[k]; !ok {
				fc[k] = nullJSON
			}
		}
		b, err := json.Marshal(fc)
		if err != nil {
			return "", nil, err
		}
		entry["fetch_config"] = b
	}

	b, err := json.Marshal(entry)
	if err != nil {
		return "", nil, err
	}
	return name, b, nil
}

func controlPlaneTopologyJSON(count json.RawMessage) json.RawMessage {
	if len(count) == 0 || string(count) == "null" {
		return nullJSON
	}
	b, _ := json.Marshal(map[string]json.RawMessage{"replicas": count})
	return b
}

func workersTopologyJSON(count json.RawMessage) json.RawMessage {
	if len(count) == 0 || string(count) == "null" {
		return nullJSON
	}
	md := map[string]json.RawMessage{
		"name": json.RawMessage(`"md-0"`), "class": nullJSON, "replicas": count,
		"failure_domain": nullJSON, "metadata": nullJSON,
	}
	b, _ := json.Marshal(map[string]interface{}{"machine_deployments": []interface{}{md}})
	return b
}

// upgradeInventoryAndAddonsJSON adds the attributes that joined schema
// version 3 without a bump: inventory.manage (defaults true),
// management.uninstall (false), management.bootstrap.kairos (null) and
// management.bootstrap.addons.prerequisites (null). Absent objects stay absent.
func upgradeInventoryAndAddonsJSON(state map[string]json.RawMessage) error {
	inv, err := rawObject(state["inventory"])
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	if inv != nil {
		if _, ok := inv["manage"]; !ok {
			inv["manage"] = json.RawMessage("true")
		}
		if state["inventory"], err = json.Marshal(inv); err != nil {
			return err
		}
	}

	mgmt, err := rawObject(state["management"])
	if err != nil {
		return fmt.Errorf("management: %w", err)
	}
	if mgmt == nil {
		return nil
	}
	if _, ok := mgmt["uninstall"]; !ok {
		mgmt["uninstall"] = json.RawMessage("false")
	}
	bs, err := rawObject(mgmt["bootstrap"])
	if err != nil {
		return fmt.Errorf("management.bootstrap: %w", err)
	}
	if bs != nil {
		if _, ok := bs["kairos"]; !ok {
			bs["kairos"] = nullJSON
		}
		addons, err := rawObject(bs["addons"])
		if err != nil {
			return fmt.Errorf("management.bootstrap.addons: %w", err)
		}
		if addons != nil {
			if _, ok := addons["prerequisites"]; !ok {
				addons["prerequisites"] = nullJSON
			}
			if bs["addons"], err = json.Marshal(addons); err != nil {
				return err
			}
		}
		if mgmt["bootstrap"], err = json.Marshal(bs); err != nil {
			return err
		}
	}
	if state["management"], err = json.Marshal(mgmt); err != nil {
		return err
	}
	return nil
}
