// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
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
var providerMapAttrs = []string{
	"core",
	"infrastructure",
	"bootstrap",
	"control_plane",
	"ipam",
	"addon",
}

var nullJSON = json.RawMessage("null")

// upgradeV0JSON rewrites the flat v0 state into the v1 shape (nested
// objects, "name:version" provider strings, an addons list) and carries it
// on to the current version.
func upgradeV0JSON(raw []byte) ([]byte, error) {
	var v0 map[string]json.RawMessage
	if err := json.Unmarshal(raw, &v0); err != nil {
		return nil, fmt.Errorf("decoding v0 state: %w", err)
	}
	get := func(key string) json.RawMessage {
		if v, ok := v0[key]; ok && len(v) > 0 {
			return v
		}
		return nullJSON
	}
	obj := func(fields map[string]json.RawMessage) json.RawMessage {
		b, _ := json.Marshal(fields)
		return b
	}
	providerObj := func(key string, extra map[string]json.RawMessage) json.RawMessage {
		if string(get(key)) == "null" {
			if len(extra) == 0 {
				return nullJSON
			}
			fields := map[string]json.RawMessage{"provider": nullJSON}
			for k, v := range extra {
				fields[k] = v
			}
			return obj(fields)
		}
		fields := map[string]json.RawMessage{"provider": get(key)}
		for k, v := range extra {
			fields[k] = v
		}
		return obj(fields)
	}
	ns := get("target_namespace")
	if string(ns) == "null" {
		ns = json.RawMessage(`"default"`)
	}
	v1 := map[string]json.RawMessage{
		"name":               get("name"),
		"kubernetes_version": get("kubernetes_version"),
		"flavor":             get("flavor"),
		"id":                 get("id"),
		"management": obj(map[string]json.RawMessage{
			"kubeconfig": get(
				"management_kubeconfig",
			),
			"skip_init":    get("skip_init"),
			"self_managed": get("self_managed"),
			"namespace":    ns,
		}),
		"infrastructure": providerObj("infrastructure_provider", nil),
		"bootstrap":      providerObj("bootstrap_provider", nil),
		"control_plane": providerObj(
			"control_plane_provider",
			map[string]json.RawMessage{"machine_count": get("control_plane_machine_count")},
		),
		"core":    providerObj("core_provider", nil),
		"workers": nullJSON,
		"addons":  nullJSON,
		"wait": obj(
			map[string]json.RawMessage{"enabled": get("wait_for_ready"), "timeout": nullJSON},
		),
		"output": obj(
			map[string]json.RawMessage{"kubeconfig_path": get("kubeconfig_path")},
		),
		"status": obj(map[string]json.RawMessage{
			"endpoint": get(
				"endpoint",
			),
			"kubeconfig":     get("kubeconfig"),
			"ca_certificate": get("cluster_ca_certificate"),
			"description": get(
				"cluster_description",
			),
			"bootstrap_cluster": get("bootstrap_cluster_name"),
		}),
	}
	if wc := get("worker_machine_count"); string(wc) != "null" {
		v1["workers"] = obj(map[string]json.RawMessage{"machine_count": wc})
	}
	b, err := json.Marshal(v1)
	if err != nil {
		return nil, err
	}
	return upgradeV1JSON(b)
}

// upgradeV1JSON rewrites raw v1 state (objects holding a "name:version"
// provider string plus an addons list) into the current state: first the v2
// shape (provider maps and topology), then v3, v4 and v5. Attributes that
// did not change shape pass through untouched.
func upgradeV1JSON(raw []byte) ([]byte, error) {
	v2, err := upgradeV1ToV2JSON(raw)
	if err != nil {
		return nil, err
	}
	return upgradeV2JSON(v2)
}

// upgradeV2JSON rewrites raw v2 state into the current state: the v3 rewrite,
// then v4 and v5.
func upgradeV2JSON(raw []byte) ([]byte, error) {
	v3, err := upgradeV2ToV3JSON(raw)
	if err != nil {
		return nil, err
	}
	return upgradeV3JSON(v3)
}

// upgradeV3JSON rewrites raw v3 state into the current state: the v4
// rewrite, then v5.
func upgradeV3JSON(raw []byte) ([]byte, error) {
	v4, err := upgradeV3ToV4JSON(raw)
	if err != nil {
		return nil, err
	}
	return upgradeV4JSON(v4)
}

// upgradeV4JSON rewrites raw v4 state into v5, the EKS shape: the namespace
// moves to the top level, the six provider maps under management.providers,
// cluster_class and topology become class or control_plane (the control
// plane's machine template ref is unknown to the old state and left null,
// so the next plan asks for a capi_machine_template ref), the workers and
// output go (node groups are capi_machine_deployment, the kubeconfig is
// ephemeral.capi_kubeconfig), wait.enabled becomes wait.conditions, and the
// status gains its phase.
func upgradeV4JSON(raw []byte) ([]byte, error) {
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decoding v4 state: %w", err)
	}
	get := func(m map[string]json.RawMessage, key string) json.RawMessage {
		if v, ok := m[key]; ok && len(v) > 0 {
			return v
		}
		return nullJSON
	}
	obj := func(fields map[string]json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(fields)
	}

	mgmt, err := rawObject(state["management"])
	if err != nil {
		return nil, fmt.Errorf("management: %w", err)
	}
	if mgmt == nil {
		mgmt = map[string]json.RawMessage{}
	}
	namespace := get(mgmt, "namespace")
	if string(namespace) == "null" {
		namespace = json.RawMessage(`"default"`)
	}
	state["namespace"] = namespace

	// The provider maps move under management.providers.
	providers := map[string]json.RawMessage{}
	anyProvider := false
	for _, attr := range providerMapAttrs {
		providers[attr] = get(state, attr)
		if string(providers[attr]) != "null" {
			anyProvider = true
		}
		delete(state, attr)
	}
	if anyProvider {
		if mgmt["providers"], err = obj(providers); err != nil {
			return nil, err
		}
	} else {
		mgmt["providers"] = nullJSON
	}
	delete(mgmt, "kubeconfig")
	delete(mgmt, "skip_init")
	delete(mgmt, "uninstall")
	delete(mgmt, "namespace")
	if _, ok := mgmt["self_managed"]; !ok {
		mgmt["self_managed"] = json.RawMessage("false")
	}
	bs, err := rawObject(mgmt["bootstrap"])
	if err != nil {
		return nil, fmt.Errorf("management.bootstrap: %w", err)
	}
	if bs != nil {
		bs["on_failure"] = json.RawMessage(`"resume"`)
		if mgmt["bootstrap"], err = obj(bs); err != nil {
			return nil, err
		}
	} else {
		mgmt["bootstrap"] = nullJSON
	}
	if state["management"], err = obj(mgmt); err != nil {
		return nil, err
	}

	// Generation mode.
	firstKey := func(raw json.RawMessage) json.RawMessage {
		m, err := rawObject(raw)
		if err != nil || len(m) == 0 {
			return nullJSON
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b, _ := json.Marshal(keys[0])
		return b
	}
	replicas, machineSpec := nullJSON, nullJSON
	if topology, err := rawObject(state["topology"]); err == nil && topology != nil {
		if cp, err := rawObject(topology["control_plane"]); err == nil && cp != nil {
			replicas = get(cp, "replicas")
			var infraYAML string
			_ = json.Unmarshal(get(cp, "infrastructure"), &infraYAML)
			if strings.TrimSpace(infraYAML) != "" {
				var spec map[string]any
				if err := yaml.Unmarshal([]byte(infraYAML), &spec); err != nil {
					return nil, fmt.Errorf("topology.control_plane.infrastructure: %w", err)
				}
				if machineSpec, err = dynamicJSON(normalizeJSON(spec)); err != nil {
					return nil, fmt.Errorf("topology.control_plane.infrastructure: %w", err)
				}
			}
		}
	}
	var clusterClass bool
	_ = json.Unmarshal(get(state, "cluster_class"), &clusterClass)
	if clusterClass {
		if state["class"], err = obj(
			map[string]json.RawMessage{"name": get(state, "name"), "namespace": nullJSON},
		); err != nil {
			return nil, err
		}
		state["infrastructure"] = nullJSON
		state["control_plane"] = nullJSON
		if string(replicas) != "null" {
			if state["control_plane"], err = obj(
				map[string]json.RawMessage{
					"provider":       nullJSON,
					"replicas":       replicas,
					"infrastructure": nullJSON,
					"machine_spec":   nullJSON,
					"spec":           nullJSON,
				},
			); err != nil {
				return nil, err
			}
		}
	} else {
		state["class"] = nullJSON
		if state["infrastructure"], err = obj(
			map[string]json.RawMessage{
				"provider": firstKey(providers["infrastructure"]),
				"spec":     nullJSON,
			},
		); err != nil {
			return nil, err
		}
		cpProvider := firstKey(providers["control_plane"])
		if string(cpProvider) == "null" {
			cpProvider = json.RawMessage(`"kubeadm"`)
		}
		// The machine template the old state generated itself carries on as
		// machine_spec: same spec, same hash-named template, no rollout.
		if state["control_plane"], err = obj(
			map[string]json.RawMessage{
				"provider":       cpProvider,
				"replicas":       replicas,
				"infrastructure": nullJSON,
				"machine_spec":   machineSpec,
				"spec":           nullJSON,
			},
		); err != nil {
			return nil, err
		}
	}
	delete(state, "cluster_class")
	delete(state, "topology")
	delete(state, "output")

	// Wait: enabled becomes the default condition list.
	wait, err := rawObject(state["wait"])
	if err != nil {
		return nil, fmt.Errorf("wait: %w", err)
	}
	if wait != nil {
		enabled := true
		_ = json.Unmarshal(get(wait, "enabled"), &enabled)
		conditions := json.RawMessage(`["Available"]`)
		if !enabled {
			conditions = json.RawMessage(`[]`)
		}
		if state["wait"], err = obj(
			map[string]json.RawMessage{"conditions": conditions},
		); err != nil {
			return nil, err
		}
	}
	// wait.timeout became the operation timeouts, which are configuration
	// only: a configured duration moves to `timeouts` in the configuration.
	state["timeouts"] = nullJSON

	// Status: the kubeconfig is gone, the phase is ready.
	status, err := rawObject(state["status"])
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	if status == nil {
		status = map[string]json.RawMessage{}
	}
	delete(status, "kubeconfig")
	status["phase"] = json.RawMessage(`"ready"`)
	status["error"] = nullJSON
	status["conditions"] = nullJSON
	for _, key := range []string{"endpoint", "ca_certificate", "description", "bootstrap_cluster"} {
		if _, ok := status[key]; !ok {
			status[key] = nullJSON
		}
	}
	if state["status"], err = obj(status); err != nil {
		return nil, err
	}
	return json.Marshal(state)
}

// upgradeV3ToV4JSON rewrites raw v3 state into v4: the Talos bootstrapper, its
// in-place mode, the cni block and management.addons are gone. A cluster
// bootstrapped in place, or by a Talos node, cannot be carried over: its
// bootstrap node is a cluster member this provider no longer knows how to
// manage, so the state must be dropped and the cluster imported.
func upgradeV3ToV4JSON(raw []byte) ([]byte, error) {
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
				return nil, fmt.Errorf(
					"cluster %q was bootstrapped with management.bootstrap.type = \"talos\" or mode = \"in_place\", which this version no longer supports: remove it from state (terraform state rm) and import it as a cluster managed through its own kubeconfig",
					name,
				)
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
		if workers != nil && len(workers["machine_deployments"]) > 0 &&
			string(workers["machine_deployments"]) != "null" {
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
func providerEntryJSON(
	providerRaw json.RawMessage,
	extra map[string]json.RawMessage,
) (string, json.RawMessage, error) {
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
	b, _ := json.Marshal(map[string]any{"machine_deployments": []any{md}})
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

// dynamicJSON encodes a Go value as the JSON a dynamic attribute holds in
// state: {"value": v, "type": T}, with objects typed per key and lists as
// tuples, the way the framework stores them.
func dynamicJSON(v any) (json.RawMessage, error) {
	typ, err := dynamicTypeJSON(v)
	if err != nil {
		return nil, err
	}
	value, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"value": value, "type": typ})
}

func dynamicTypeJSON(v any) (json.RawMessage, error) {
	switch t := v.(type) {
	case nil:
		return json.RawMessage(`"dynamic"`), nil
	case string:
		return json.RawMessage(`"string"`), nil
	case bool:
		return json.RawMessage(`"bool"`), nil
	case float64, int64, int, json.Number:
		return json.RawMessage(`"number"`), nil
	case map[string]any:
		attrs := map[string]json.RawMessage{}
		for k, val := range t {
			at, err := dynamicTypeJSON(val)
			if err != nil {
				return nil, err
			}
			attrs[k] = at
		}
		b, err := json.Marshal(attrs)
		if err != nil {
			return nil, err
		}
		return json.Marshal([]json.RawMessage{json.RawMessage(`"object"`), b})
	case []any:
		elems := make([]json.RawMessage, 0, len(t))
		for _, val := range t {
			et, err := dynamicTypeJSON(val)
			if err != nil {
				return nil, err
			}
			elems = append(elems, et)
		}
		b, err := json.Marshal(elems)
		if err != nil {
			return nil, err
		}
		return json.Marshal([]json.RawMessage{json.RawMessage(`"tuple"`), b})
	}
	return nil, fmt.Errorf("unsupported value %T in a dynamic attribute", v)
}
