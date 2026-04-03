// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	clusterctlclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
)

// ClusterctlTemplateGenerator generates cluster templates using the clusterctl client library.
type ClusterctlTemplateGenerator struct {
	configPath string
}

// NewClusterctlTemplateGenerator creates a new template generator.
func NewClusterctlTemplateGenerator(configPath string) *ClusterctlTemplateGenerator {
	if configPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			configPath = filepath.Join(home, ".cluster-api")
		}
	}
	return &ClusterctlTemplateGenerator{configPath: configPath}
}

// Generate generates a cluster template YAML using clusterctl.
func (t *ClusterctlTemplateGenerator) Generate(ctx context.Context, cluster *Cluster, opts TemplateOptions) ([]byte, error) {
	client, err := clusterctlclient.New(ctx, t.configPath)
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

	if opts.InfrastructureProvider != "" {
		templateOpts.ProviderRepositorySource = &clusterctlclient.ProviderRepositorySourceOptions{
			InfrastructureProvider: opts.InfrastructureProvider,
			Flavor:                opts.Flavor,
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
