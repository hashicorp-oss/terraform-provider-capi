// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
)

// ClusterctlTemplateGenerator generates cluster templates using the clusterctl client library.
type ClusterctlTemplateGenerator struct {
	configPath string
}

// NewClusterctlTemplateGenerator creates a new template generator. An empty
// configPath lets clusterctl fall back to its own default config file resolution.
func NewClusterctlTemplateGenerator(configPath string) *ClusterctlTemplateGenerator {
	return &ClusterctlTemplateGenerator{configPath: configPath}
}

// Generate generates a cluster template YAML using clusterctl.
func (t *ClusterctlTemplateGenerator) Generate(ctx context.Context, cluster *Cluster, opts TemplateOptions) ([]byte, error) {
	// The template comes from the infrastructure provider's release, so the
	// client must resolve the same provider names and URLs that init used.
	client, err := newClusterctlClient(ctx, t.configPath, opts.Providers, opts.Variables)
	if err != nil {
		return nil, &CAPIError{
			Operation: "generate-template",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: creating clusterctl client: %v", ErrTemplateGenerate, err),
		}
	}

	templateOpts := clusterctlclient.GetClusterTemplateOptions{
		Kubeconfig: clusterctlclient.Kubeconfig{
			Path: cluster.KubeconfigPath,
		},
		ClusterName:       opts.ClusterName,
		TargetNamespace:   opts.Namespace,
		KubernetesVersion: opts.KubernetesVersion,
	}

	if opts.ControlPlaneMachineCount != nil {
		templateOpts.ControlPlaneMachineCount = opts.ControlPlaneMachineCount
	}

	if opts.WorkerMachineCount != nil {
		templateOpts.WorkerMachineCount = opts.WorkerMachineCount
	}

	switch {
	case opts.Source != "":
		templateOpts.URLSource = &clusterctlclient.URLSourceOptions{URL: templateURL(opts.Source)}
	case opts.InfrastructureProvider != "":
		templateOpts.ProviderRepositorySource = &clusterctlclient.ProviderRepositorySourceOptions{
			InfrastructureProvider: opts.InfrastructureProvider,
			Flavor:                 opts.Flavor,
		}
	}

	template, err := client.GetClusterTemplate(ctx, templateOpts)
	if err != nil {
		return nil, &CAPIError{
			Operation: "generate-template",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: %v", ErrTemplateGenerate, err),
		}
	}

	yamlBytes, err := template.Yaml()
	if err != nil {
		return nil, &CAPIError{
			Operation: "generate-template",
			Cluster:   cluster.Name,
			Err:       fmt.Errorf("%w: rendering YAML: %v", ErrTemplateGenerate, err),
		}
	}

	return yamlBytes, nil
}

// templateURL normalizes a template source for clusterctl. URLs (file://,
// https://) pass through; a bare path is made absolute so it does not depend
// on the provider process's working directory.
func templateURL(source string) string {
	if strings.Contains(source, "://") {
		return source
	}
	if abs, err := filepath.Abs(source); err == nil {
		return abs
	}
	return source
}
