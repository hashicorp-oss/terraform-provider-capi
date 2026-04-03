# CAPI Management Components Implementation Guide

## Overview

This document provides step-by-step instructions for implementing and extending the CAPI management components in the Terraform provider. The architecture mirrors the [EKS Anywhere](https://github.com/aws/eks-anywhere) CAPI management workflow.

## Quick Reference

### File Layout

```
internal/capi/
├── interfaces.go    # All component interfaces
├── types.go         # Cluster, options, result types
├── errors.go        # Typed error types
├── manager.go       # Workflow orchestrator
├── bootstrap.go     # Kind bootstrap cluster (Bootstrapper)
├── installer.go     # clusterctl init (Installer)
├── template.go      # clusterctl generate (TemplateGenerator)
├── applier.go       # kubectl apply/delete (Applier)
├── mover.go         # clusterctl move (Mover)
├── waiter.go        # kubectl wait (Waiter)
├── info.go          # kubeconfig + describe (KubeconfigRetriever, ClusterDescriber)
├── mock_test.go     # Test doubles for all interfaces
├── manager_test.go  # Manager orchestration tests
└── docker/
    └── provider.go  # Docker provider defaults
```

## Step 1: Creating a New Component

### 1.1 Define the Interface

Add your interface to `internal/capi/interfaces.go`:

```go
// MyComponent does X.
// Modeled after EKS Anywhere's <equivalent>.
type MyComponent interface {
    DoSomething(ctx context.Context, cluster *Cluster, opts MyOptions) error
}
```

### 1.2 Define Options Type

Add your options to `internal/capi/types.go`:

```go
type MyOptions struct {
    Param1 string
    Param2 int
}
```

### 1.3 Implement the Concrete Type

Create `internal/capi/mycomponent.go`:

```go
type ConcreteMyComponent struct {
    // dependencies
}

func NewConcreteMyComponent() *ConcreteMyComponent {
    return &ConcreteMyComponent{}
}

func (c *ConcreteMyComponent) DoSomething(ctx context.Context, cluster *Cluster, opts MyOptions) error {
    // implementation
    return nil
}
```

### 1.4 Add a Functional Option

In `internal/capi/manager.go`:

```go
func WithMyComponent(c MyComponent) ManagerOption {
    return func(m *Manager) { m.myComponent = c }
}
```

### 1.5 Wire Into the Workflow

In `Manager.CreateCluster()` (or `DeleteCluster()`), add the step in the correct position in the workflow sequence.

### 1.6 Create Test Mock

In `internal/capi/mock_test.go`:

```go
type MockMyComponent struct {
    DoSomethingFunc  func(ctx context.Context, cluster *Cluster, opts MyOptions) error
    DoSomethingCalls []struct {
        Cluster *Cluster
        Opts    MyOptions
    }
}

func (m *MockMyComponent) DoSomething(ctx context.Context, cluster *Cluster, opts MyOptions) error {
    m.DoSomethingCalls = append(m.DoSomethingCalls, struct {
        Cluster *Cluster
        Opts    MyOptions
    }{cluster, opts})
    if m.DoSomethingFunc != nil {
        return m.DoSomethingFunc(ctx, cluster, opts)
    }
    return nil
}
```

## Step 2: Testing Strategy

### Unit Tests (Manager Level)

Test the Manager orchestration with mocked components:

```go
func TestManager_CreateCluster_MyScenario(t *testing.T) {
    myComponent := &MockMyComponent{}
    mgr := NewManager(WithMyComponent(myComponent))

    result, err := mgr.CreateCluster(ctx, CreateClusterOptions{...})

    // Assert calls were made correctly
    if len(myComponent.DoSomethingCalls) != 1 { ... }
}
```

### Error Recovery Tests

Always test that bootstrap cleanup happens on failure:

```go
func TestManager_CreateCluster_MyComponentFailure_CleansUpBootstrap(t *testing.T) {
    bootstrapper := &MockBootstrapper{}
    myComponent := &MockMyComponent{
        DoSomethingFunc: func(...) error {
            return fmt.Errorf("failed")
        },
    }
    mgr := NewManager(WithBootstrapper(bootstrapper), WithMyComponent(myComponent))

    _, err := mgr.CreateCluster(ctx, CreateClusterOptions{...})
    if err == nil { t.Fatal("expected error") }

    // Verify bootstrap cleanup
    if len(bootstrapper.DeleteCalls) != 1 { t.Fatal("expected cleanup") }
}
```

### Integration Tests

For components interacting with real infrastructure, use build tags:

```go
//go:build integration

func TestKindBootstrapper_Create_RealCluster(t *testing.T) {
    // Tests that actually create Docker containers
}
```

## Step 3: Exposing in Terraform

### 3.1 Add Schema Attribute

In `internal/provider/cluster_resource.go`, add to the Schema:

```go
"my_attribute": schema.StringAttribute{
    MarkdownDescription: "Description of what this does",
    Optional:            true,
},
```

### 3.2 Add to Resource Model

```go
type ClusterResourceModel struct {
    // ...existing fields...
    MyAttribute types.String `tfsdk:"my_attribute"`
}
```

### 3.3 Map to CreateClusterOptions

In the `Create` method:

```go
if !data.MyAttribute.IsNull() {
    createOpts.MyParam = data.MyAttribute.ValueString()
}
```

## Step 4: Docker Integration Testing

### Prerequisites

```bash
# Install required tools
brew install kind kubectl
```

### Running Docker-based Tests

```bash
# Unit tests (fast, mocked)
go test ./internal/capi/... -v

# Integration tests (creates real kind clusters)
go test ./internal/capi/... -v -tags=integration -timeout=30m

# Terraform acceptance tests
TF_ACC=1 go test ./internal/provider/... -v -timeout=60m
```

## EKS Anywhere Reference

The workflow maps to these EKS Anywhere source files:

| This Provider | EKS Anywhere Source |
|---|---|
| `Manager.CreateCluster()` | `pkg/workflows/management/create.go` → `Run()` |
| `KindBootstrapper.Create()` | `pkg/workflows/management/create_bootstrap.go` → `createBootStrapClusterTask` |
| `ClusterctlInstaller.Init()` | `pkg/workflows/management/create_install_capi.go` → `installCAPIComponentsTask` |
| `ClusterctlTemplateGenerator.Generate()` + `KubectlApplier.Apply()` | `pkg/workflows/management/create_workload.go` → `createWorkloadClusterTask` |
| `ClusterctlMover.Move()` | `pkg/workflows/management/create_move_capi.go` → `moveClusterManagementTask` |
| `KindBootstrapper.Delete()` | cleanup phase in `Run()` |
| `KubectlWaiter` | `pkg/clustermanager/cluster_manager.go` → `waitForCAPI`, `waitForNodesReady` |
