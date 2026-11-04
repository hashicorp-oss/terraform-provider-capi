// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package capi

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/tree"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	treeFirstElem = "├─"
	treeLastElem  = "└─"
	treePipe      = "│ "
)

// RenderObjectTree renders a clusterctl describe tree as text, one row per
// object: its summary condition (Available when the object has one, else
// Ready), the reason, how long the condition has held, and the message. It
// is modeled on clusterctl's own printer without colors and replica
// counters, so status.description reads like `clusterctl describe cluster`.
func RenderObjectTree(t *tree.ObjectTree, now time.Time) string {
	if t == nil || t.GetRoot() == nil {
		return ""
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tREASON\tSINCE\tMESSAGE")
	addTreeRows(w, t, t.GetRoot(), "", now)
	_ = w.Flush()
	return strings.TrimRight(b.String(), "\n")
}

// addTreeRows writes the row for obj and, recursively, its children in
// clusterctl's order: higher z-order first, then by name.
func addTreeRows(w io.Writer, t *tree.ObjectTree, obj ctrlclient.Object, prefix string, now time.Time) {
	status, reason, since, message := treeRowCondition(obj, now)
	if tree.IsGroupObject(obj) {
		items := strings.Split(tree.GetGroupItems(obj), tree.GroupItemsSeparator)
		if len(items) > 2 {
			message = "See " + strings.Join(items[:2], tree.GroupItemsSeparator) + ", ..."
		} else {
			message = "See " + strings.Join(items, tree.GroupItemsSeparator)
		}
	}
	lines := strings.Split(message, "\n")
	fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\n", prefix, treeRowName(obj), status, reason, since, lines[0])
	continuation := strings.ReplaceAll(strings.ReplaceAll(prefix, treeFirstElem, treePipe), treeLastElem, "  ")
	for _, extra := range lines[1:] {
		fmt.Fprintf(w, "%s\t\t\t\t%s\n", continuation, extra)
	}

	children := t.GetObjectsByParent(obj.GetUID())
	sort.SliceStable(children, func(i, j int) bool {
		zi, zj := tree.GetZOrder(children[i]), tree.GetZOrder(children[j])
		if zi == zj {
			return treeRowName(children[i]) < treeRowName(children[j])
		}
		return zi > zj
	})
	for i, child := range children {
		addTreeRows(w, t, child, childPrefix(prefix, i, len(children)), now)
	}
}

// childPrefix carries the tree lines of prefix down one level and adds the
// branch for child i of n.
func childPrefix(prefix string, i, n int) string {
	next := strings.ReplaceAll(prefix, treeFirstElem, treePipe)
	next = strings.ReplaceAll(next, treeLastElem, "  ")
	if i < n-1 {
		return next + treeFirstElem
	}
	return next + treeLastElem
}

// treeRowName names an object the way clusterctl does: group objects by
// count and kind, virtual objects by their meta name, everything else as
// Kind/name, prefixed with the meta name (ClusterInfrastructure,
// ControlPlane, ...) when the tree assigned one.
func treeRowName(obj ctrlclient.Object) string {
	if tree.IsGroupObject(obj) {
		items := strings.Split(tree.GetGroupItems(obj), tree.GroupItemsSeparator)
		kind := strings.TrimSuffix(obj.GetObjectKind().GroupVersionKind().Kind, "Group")
		return fmt.Sprintf("%d %ss...", len(items), kind)
	}
	if tree.IsVirtualObject(obj) {
		if meta := tree.GetMetaName(obj); meta != "" {
			return meta
		}
		return obj.GetName()
	}
	name := obj.GetObjectKind().GroupVersionKind().Kind + "/" + obj.GetName()
	if meta := tree.GetMetaName(obj); meta != "" {
		name = meta + " - " + name
	}
	if !obj.GetDeletionTimestamp().IsZero() {
		name = "!! DELETED !! " + name
	}
	return name
}

// treeRowCondition picks the condition that summarizes an object: Available
// where it exists (Cluster, MachineDeployment, control planes), else Ready.
func treeRowCondition(obj ctrlclient.Object, now time.Time) (status, reason, since, message string) {
	var c *metav1.Condition
	if available := tree.GetAvailableCondition(obj); available != nil {
		c = available
	} else if ready := tree.GetReadyCondition(obj); ready != nil {
		c = ready
	}
	if c == nil {
		return "", "", "", ""
	}
	since = ""
	if !c.LastTransitionTime.IsZero() {
		since = duration.HumanDuration(now.Sub(c.LastTransitionTime.Time))
	}
	return string(c.Status), c.Reason, since, c.Message
}
