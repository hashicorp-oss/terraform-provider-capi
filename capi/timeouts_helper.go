// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Default operation timeouts, overridden by a resource's `timeouts`.
const (
	// defaultTimeout bounds an object apply, its readiness wait and its
	// deletion.
	defaultTimeout = 10 * time.Minute
	// defaultClusterCreateTimeout covers a self-managed create end to end:
	// the bootstrap cluster, providers, the first control plane, the
	// handover, the move and full readiness on bare metal.
	defaultClusterCreateTimeout = 2 * time.Hour
	// defaultClusterUpdateTimeout covers provider upgrades and a rollout.
	defaultClusterUpdateTimeout = 60 * time.Minute
	// defaultClusterDeleteTimeout covers the reverse pivot of a self-managed
	// cluster and the release of every machine.
	defaultClusterDeleteTimeout = 2 * time.Hour
)

// timeoutsNullValue returns a null timeouts.Value with the attribute types
// every resource declares (create, update, delete), for a model built
// outside configuration (import).
func timeoutsNullValue() timeouts.Value {
	return timeouts.Value{
		Object: types.ObjectNull(map[string]attr.Type{
			"create": types.StringType,
			"delete": types.StringType,
			"update": types.StringType,
		}),
	}
}

// remaining is how long an operation bounded by ctx (its `timeouts` entry)
// has left, for waits inside it; fallback when ctx has no deadline (Read).
func remaining(ctx context.Context, fallback time.Duration) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return fallback
	}
	if d := time.Until(deadline); d > 0 {
		return d
	}
	return time.Millisecond
}
