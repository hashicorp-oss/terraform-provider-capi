// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
	yamlprocessor "sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	"sigs.k8s.io/yaml"
)

// BuildComponentsAlterFn creates a ComponentsAlterFn that applies
// capi-operator-style customizations from a ProviderConfig to provider
// component objects. This mirrors the operator's customizeObjectsFn +
// applyPatches pipeline, implemented natively without operator dependency.
func BuildComponentsAlterFn(p ProviderConfig) repository.ComponentsAlterFn {
	return func(objs []unstructured.Unstructured) ([]unstructured.Unstructured, error) {
		var err error

		// 1. Apply deployment customizations (mirrors operator's customizeObjectsFn).
		if p.Deployment != nil {
			objs, err = customizeDeployment(objs, p.Deployment)
			if err != nil {
				return nil, fmt.Errorf("customizing deployment: %w", err)
			}
		}

		// 2. Apply manager customizations (mirrors operator's manager arg injection).
		if p.Manager != nil {
			objs, err = customizeManager(objs, p.Manager)
			if err != nil {
				return nil, fmt.Errorf("customizing manager: %w", err)
			}
		}

		// 3. Apply manifest patches — RFC 7396 merge patches (mirrors operator's applyPatches for manifestPatches).
		if len(p.ManifestPatches) > 0 {
			objs, err = applyManifestPatches(objs, p.ManifestPatches)
			if err != nil {
				return nil, fmt.Errorf("applying manifest patches: %w", err)
			}
		}

		// 4. Apply targeted patches — strategic merge / RFC6902 with selectors
		// (mirrors operator's applyPatches for patches).
		if len(p.Patches) > 0 {
			objs, err = applyTargetedPatches(objs, p.Patches)
			if err != nil {
				return nil, fmt.Errorf("applying targeted patches: %w", err)
			}
		}

		// 5. Append additional manifests.
		if p.AdditionalManifests != "" {
			extra, parseErr := parseMultiDocYAML([]byte(p.AdditionalManifests))
			if parseErr != nil {
				return nil, fmt.Errorf("parsing additional manifests: %w", parseErr)
			}
			objs = append(objs, extra...)
		}

		return objs, nil
	}
}

// customizeDeployment finds the controller-manager Deployment and applies
// DeploymentConfig overrides. Mirrors capi-operator's customizeObjectsFn.
func customizeDeployment(
	objs []unstructured.Unstructured,
	cfg *DeploymentConfig,
) ([]unstructured.Unstructured, error) {
	for i := range objs {
		if objs[i].GetKind() != "Deployment" {
			continue
		}
		if !strings.Contains(objs[i].GetName(), "controller-manager") {
			continue
		}

		if cfg.Replicas != nil {
			if err := unstructured.SetNestedField(
				objs[i].Object,
				*cfg.Replicas,
				"spec",
				"replicas",
			); err != nil {
				return nil, fmt.Errorf("setting replicas: %w", err)
			}
		}

		if len(cfg.NodeSelector) > 0 {
			ns := make(map[string]any, len(cfg.NodeSelector))
			for k, v := range cfg.NodeSelector {
				ns[k] = v
			}
			if err := unstructured.SetNestedField(
				objs[i].Object,
				ns,
				"spec",
				"template",
				"spec",
				"nodeSelector",
			); err != nil {
				return nil, fmt.Errorf("setting nodeSelector: %w", err)
			}
		}

		if cfg.ServiceAccountName != "" {
			if err := unstructured.SetNestedField(
				objs[i].Object,
				cfg.ServiceAccountName,
				"spec",
				"template",
				"spec",
				"serviceAccountName",
			); err != nil {
				return nil, fmt.Errorf("setting serviceAccountName: %w", err)
			}
		}

		if len(cfg.Containers) > 0 {
			if err := customizeContainers(&objs[i], cfg.Containers); err != nil {
				return nil, fmt.Errorf("customizing containers: %w", err)
			}
		}
	}
	return objs, nil
}

// customizeContainers applies container overrides to a Deployment's pod spec.
func customizeContainers(obj *unstructured.Unstructured, overrides []ContainerConfig) error {
	containers, found, err := unstructured.NestedSlice(
		obj.Object,
		"spec",
		"template",
		"spec",
		"containers",
	)
	if err != nil || !found {
		return nil
	}

	for _, override := range overrides {
		for ci, c := range containers {
			cMap, ok := c.(map[string]any)
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(cMap, "name")
			if name != override.Name {
				continue
			}

			if override.ImageURL != "" {
				cMap["image"] = override.ImageURL
			}
			if len(override.Command) > 0 {
				cmd := make([]any, len(override.Command))
				for i, v := range override.Command {
					cmd[i] = v
				}
				cMap["command"] = cmd
			}
			if len(override.Args) > 0 {
				args := containerArgs(cMap)
				for k, v := range override.Args {
					args = setArg(args, k, v)
				}
				cMap["args"] = args
			}
			containers[ci] = cMap
		}
	}

	return unstructured.SetNestedSlice(
		obj.Object,
		containers,
		"spec",
		"template",
		"spec",
		"containers",
	)
}

// customizeManager finds the manager container in the controller-manager
// Deployment and injects manager configuration as container args.
// Mirrors capi-operator's customizeManagerContainer.
func customizeManager(
	objs []unstructured.Unstructured,
	cfg *ManagerConfig,
) ([]unstructured.Unstructured, error) {
	for i := range objs {
		if objs[i].GetKind() != "Deployment" ||
			!strings.Contains(objs[i].GetName(), "controller-manager") {
			continue
		}

		containers, found, err := unstructured.NestedSlice(
			objs[i].Object,
			"spec",
			"template",
			"spec",
			"containers",
		)
		if err != nil || !found {
			continue
		}

		for ci, c := range containers {
			cMap, ok := c.(map[string]any)
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(cMap, "name")
			if name != "manager" {
				continue
			}

			args := containerArgs(cMap)

			if cfg.ProfilerAddress != "" {
				args = setArg(args, "--profiler-address", cfg.ProfilerAddress)
			}
			if cfg.MaxConcurrentReconciles != nil {
				args = setArg(
					args,
					"--max-concurrent-reconciles",
					fmt.Sprintf("%d", *cfg.MaxConcurrentReconciles),
				)
			}
			if cfg.Verbosity != nil {
				args = setArg(args, "--v", fmt.Sprintf("%d", *cfg.Verbosity))
			}
			if len(cfg.FeatureGates) > 0 {
				gates := make([]string, 0, len(cfg.FeatureGates))
				for k, v := range cfg.FeatureGates {
					gates = append(gates, fmt.Sprintf("%s=%t", k, v))
				}
				args = setArg(args, "--feature-gates", strings.Join(gates, ","))
			}
			for k, v := range cfg.AdditionalArgs {
				args = setArg(args, k, v)
			}

			cMap["args"] = args
			containers[ci] = cMap
		}

		if err := unstructured.SetNestedSlice(
			objs[i].Object,
			containers,
			"spec",
			"template",
			"spec",
			"containers",
		); err != nil {
			return nil, err
		}
	}
	return objs, nil
}

// applyManifestPatches applies RFC 7396 JSON merge patches to all objects.
// Each patch string is parsed as JSON/YAML and merged into every object.
// Mirrors capi-operator's manifestPatches behavior.
func applyManifestPatches(
	objs []unstructured.Unstructured,
	patches []string,
) ([]unstructured.Unstructured, error) {
	for i := range objs {
		objJSON, err := json.Marshal(objs[i].Object)
		if err != nil {
			return nil, fmt.Errorf(
				"marshaling object %s/%s: %w",
				objs[i].GetKind(),
				objs[i].GetName(),
				err,
			)
		}

		for _, patchStr := range patches {
			patchJSON, err := yaml.YAMLToJSON([]byte(patchStr))
			if err != nil {
				return nil, fmt.Errorf("converting patch to JSON: %w", err)
			}
			objJSON, err = jsonpatch.MergePatch(objJSON, patchJSON)
			if err != nil {
				return nil, fmt.Errorf(
					"applying merge patch to %s/%s: %w",
					objs[i].GetKind(),
					objs[i].GetName(),
					err,
				)
			}
		}

		var patched map[string]any
		if err := json.Unmarshal(objJSON, &patched); err != nil {
			return nil, fmt.Errorf("unmarshaling patched object: %w", err)
		}
		objs[i].Object = patched
	}
	return objs, nil
}

// applyTargetedPatches applies patches with target selectors.
// Each patch is applied only to objects matching its target selector.
// Supports RFC 7396 merge patches (default) and RFC 6902 JSON patches
// (detected by array syntax). Mirrors capi-operator's targeted patch behavior.
func applyTargetedPatches(
	objs []unstructured.Unstructured,
	patches []PatchConfig,
) ([]unstructured.Unstructured, error) {
	for _, pc := range patches {
		if pc.Patch == "" {
			continue
		}
		patchJSON, err := yaml.YAMLToJSON([]byte(pc.Patch))
		if err != nil {
			return nil, fmt.Errorf("converting patch to JSON: %w", err)
		}

		for i := range objs {
			if !matchesTarget(&objs[i], pc.Target) {
				continue
			}

			objJSON, err := json.Marshal(objs[i].Object)
			if err != nil {
				return nil, fmt.Errorf("marshaling object: %w", err)
			}

			var patched []byte
			// Detect RFC 6902 (array) vs RFC 7396 (object) by first byte.
			trimmed := bytes.TrimSpace(patchJSON)
			if len(trimmed) > 0 && trimmed[0] == '[' {
				p, err := jsonpatch.DecodePatch(patchJSON)
				if err != nil {
					return nil, fmt.Errorf("decoding RFC6902 patch: %w", err)
				}
				patched, err = p.Apply(objJSON)
				if err != nil {
					return nil, fmt.Errorf(
						"applying RFC6902 patch to %s/%s: %w",
						objs[i].GetKind(),
						objs[i].GetName(),
						err,
					)
				}
			} else {
				patched, err = jsonpatch.MergePatch(objJSON, patchJSON)
				if err != nil {
					return nil, fmt.Errorf(
						"applying merge patch to %s/%s: %w",
						objs[i].GetKind(),
						objs[i].GetName(),
						err,
					)
				}
			}

			var patchedObj map[string]any
			if err := json.Unmarshal(patched, &patchedObj); err != nil {
				return nil, fmt.Errorf("unmarshaling patched object: %w", err)
			}
			objs[i].Object = patchedObj
		}
	}
	return objs, nil
}

// matchesTarget checks whether an object matches a PatchSelector.
// A nil target matches all objects.
func matchesTarget(obj *unstructured.Unstructured, target *PatchSelector) bool {
	if target == nil {
		return true
	}
	gvk := obj.GroupVersionKind()
	if target.Group != "" && gvk.Group != target.Group {
		return false
	}
	if target.Version != "" && gvk.Version != target.Version {
		return false
	}
	if target.Kind != "" && gvk.Kind != target.Kind {
		return false
	}
	if target.Name != "" && obj.GetName() != target.Name {
		return false
	}
	if target.Namespace != "" && obj.GetNamespace() != target.Namespace {
		return false
	}
	if target.LabelSelector != "" {
		selector, err := labels.Parse(target.LabelSelector)
		if err != nil {
			return false
		}
		if !selector.Matches(labels.Set(obj.GetLabels())) {
			return false
		}
	}
	return true
}

// parseMultiDocYAML parses a multi-document YAML string into unstructured objects.
func parseMultiDocYAML(data []byte) ([]unstructured.Unstructured, error) {
	var result []unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if obj.Object != nil {
			result = append(result, obj)
		}
	}
	return result, nil
}

// containerArgs extracts the args slice from a container map as []interface{}.
func containerArgs(cMap map[string]any) []any {
	raw, ok := cMap["args"]
	if !ok {
		return nil
	}
	args, ok := raw.([]any)
	if !ok {
		return nil
	}
	return args
}

// setArg sets or replaces a --key=value argument in an args list.
func setArg(args []any, key, value string) []any {
	prefix := key + "="
	if !strings.HasPrefix(key, "-") {
		prefix = "--" + key + "="
	}
	for i, a := range args {
		if s, ok := a.(string); ok && strings.HasPrefix(s, prefix) {
			args[i] = prefix + value
			return args
		}
	}
	return append(args, prefix+value)
}

// --- Custom Processor ---

// customProcessor wraps the default SimpleProcessor to inject additional
// template variables during component YAML processing. Variables from
// ConfigVariables and SecretConfigVariables take precedence over the
// default resolver (clusterctl config / env vars).
type customProcessor struct {
	base yamlprocessor.Processor
	vars map[string]string
}

func newCustomProcessor(configVars, secretVars map[string]string) yamlprocessor.Processor {
	vars := make(map[string]string, len(configVars)+len(secretVars))
	for k, v := range configVars {
		vars[k] = v
	}
	for k, v := range secretVars {
		vars[k] = v
	}
	if len(vars) == 0 {
		return yamlprocessor.NewSimpleProcessor()
	}
	return &customProcessor{
		base: yamlprocessor.NewSimpleProcessor(),
		vars: vars,
	}
}

func (p *customProcessor) GetTemplateName(version, flavor string) string {
	return p.base.GetTemplateName(version, flavor)
}

func (p *customProcessor) GetClusterClassTemplateName(version, name string) string {
	return p.base.GetClusterClassTemplateName(version, name)
}

func (p *customProcessor) GetVariables(raw []byte) ([]string, error) {
	return p.base.GetVariables(raw)
}

func (p *customProcessor) GetVariableMap(raw []byte) (map[string]*string, error) {
	return p.base.GetVariableMap(raw)
}

func (p *customProcessor) Process(
	raw []byte,
	resolver func(string) (string, error),
) ([]byte, error) {
	wrappedResolver := func(key string) (string, error) {
		if v, ok := p.vars[key]; ok {
			return v, nil
		}
		return resolver(key)
	}
	return p.base.Process(raw, wrappedResolver)
}

// --- Repository Client Wrapping ---

// customizingRepoClient wraps a repository.Client to intercept Components().Get()
// and apply post-processing customizations via AlterComponents.
type customizingRepoClient struct {
	repository.Client
	alterFn repository.ComponentsAlterFn
	key     ProviderKey
}

func (c *customizingRepoClient) Components() repository.ComponentsClient {
	return &customizingComponentsClient{
		ComponentsClient: c.Client.Components(),
		alterFn:          c.alterFn,
	}
}

// customizingComponentsClient wraps ComponentsClient.Get to apply AlterComponents
// after the standard processing pipeline (variable substitution, namespace fixing,
// label injection) has completed.
type customizingComponentsClient struct {
	repository.ComponentsClient
	alterFn repository.ComponentsAlterFn
}

func (c *customizingComponentsClient) Get(
	ctx context.Context,
	options repository.ComponentsOptions,
) (repository.Components, error) {
	comps, err := c.ComponentsClient.Get(ctx, options)
	if err != nil {
		return nil, err
	}
	if err := repository.AlterComponents(comps, c.alterFn); err != nil {
		return nil, fmt.Errorf("applying component customizations: %w", err)
	}
	return comps, nil
}

// NewCustomizingRepoFactory creates a RepositoryClientFactory that wraps the
// default repository client with provider-specific customizations. Providers
// are matched by type and name, so bootstrap "talos" and control-plane "talos"
// are configured independently. For matching providers it:
//  1. Injects a custom Processor that adds ConfigVariables/SecretConfigVariables
//  2. Wraps the ComponentsClient to apply deployment/manager/patch customizations
//
// Other providers pass through to the default factory unchanged.
func NewCustomizingRepoFactory(
	configClient config.Client,
	customizations map[ProviderKey]ProviderConfig,
) clusterctlclient.RepositoryClientFactory {
	return func(ctx context.Context, input clusterctlclient.RepositoryClientFactoryInput) (repository.Client, error) {
		var (
			p   ProviderConfig
			key ProviderKey
			ok  bool
		)
		if typ, known := ProviderTypeFromClusterctl(input.Provider.Type()); known {
			key = ProviderKey{Type: typ, Name: input.Provider.Name()}
			p, ok = customizations[key]
		}

		var repoOpts []repository.Option
		if ok && (len(p.ConfigVariables) > 0 || len(p.SecretConfigVariables) > 0) {
			repoOpts = append(repoOpts, repository.InjectYamlProcessor(
				newCustomProcessor(p.ConfigVariables, p.SecretConfigVariables),
			))
		}

		repoClient, err := repository.New(ctx, input.Provider, configClient, repoOpts...)
		if err != nil {
			return nil, err
		}

		needsAlter := ok && (p.Deployment != nil || p.Manager != nil ||
			len(p.ManifestPatches) > 0 || len(p.Patches) > 0 || p.AdditionalManifests != "")
		if needsAlter {
			return &customizingRepoClient{
				Client:  repoClient,
				alterFn: BuildComponentsAlterFn(p),
				key:     key,
			}, nil
		}
		return repoClient, nil
	}
}
