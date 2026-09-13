// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
)

// providerAttrNames lists every attribute of the v2 provider object. The JSON
// handed back to Terraform must carry all of them (null when unset) or it
// will not decode against the schema.
var providerAttrNames = []string{
	"version", "fetch_config", "config_variables", "secret_config_variables",
	"deployment", "manager", "additional_manifests", "manifest_patches", "patches",
}

var nullJSON = json.RawMessage("null")

// upgradeV1JSON rewrites raw v1 state (objects holding a "name:version"
// provider string plus an addons list) into v2 state (provider maps and
// topology). Attributes that did not change shape pass through untouched.
func upgradeV1JSON(raw []byte) ([]byte, error) {
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
	state["cni"] = nullJSON
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
