// Copyright IBM Corp. 2021, 2026
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
)

// CAPI annotations and labels the node group resource writes.
const (
	autoscalerMinSizeAnnotation = "cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size"
	autoscalerMaxSizeAnnotation = "cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size"
	clusterNameLabel            = "cluster.x-k8s.io/cluster-name"
	deploymentNameLabel         = "cluster.x-k8s.io/deployment-name"
	topologyDeploymentLabel     = "topology.cluster.x-k8s.io/deployment-name"
)

// objectRef is a reference to a template, as the template resources export it.
type objectRef struct {
	APIGroup, APIVersion, Kind, Name, Namespace string
}

// nodeGroupInputs is everything a MachineDeployment (direct mode) or a
// topology entry (ClusterClass mode) is rendered from, after the cluster's
// own values filled the defaults.
type nodeGroupInputs struct {
	Cluster, Namespace, Name string
	// CoreVersion is the core API version of the Cluster object.
	CoreVersion string
	// Replicas is nil when the cluster autoscaler owns the count.
	Replicas         *int64
	AutoscalingMin   *int64
	AutoscalingMax   *int64
	Version          string
	Infrastructure   *objectRef
	Bootstrap        *objectRef
	Class            string
	Variables        map[string]any
	FailureDomain    string
	Labels           map[string]string
	Annotations      map[string]string
	Strategy         string
	MaxSurge         *int64
	MaxUnavailable   *int64
	NodeDrain        *time.Duration
	NodeVolumeDetach *time.Duration
	NodeDeletion     *time.Duration
}

// topologyMode reports whether the group is an entry of a ClusterClass
// topology rather than its own MachineDeployment.
func (in nodeGroupInputs) topologyMode() bool { return in.Class != "" }

// contractRefs reports whether references use the v1beta2 shape
// ({apiGroup, kind, name}) rather than the legacy one ({apiVersion, kind,
// name, namespace}).
func (in nodeGroupInputs) contractRefs() bool {
	return version.CompareKubeAwareVersionStrings(in.CoreVersion, "v1beta2") >= 0
}

// validate checks what can be checked without a cluster.
func (in nodeGroupInputs) validate() error {
	var errs []error
	if in.topologyMode() {
		if in.Infrastructure != nil || in.Bootstrap != nil {
			errs = append(errs, errors.New("class and template are exclusive: a ClusterClass worker class brings its own templates"))
		}
	} else {
		if in.Infrastructure == nil || in.Bootstrap == nil {
			errs = append(errs, errors.New("template.infrastructure and template.bootstrap are required without a class"))
		}
		if in.Version == "" {
			errs = append(errs, errors.New("version is required without a class (the cluster has no topology version to default from)"))
		}
	}
	if in.Replicas != nil && (in.AutoscalingMin != nil || in.AutoscalingMax != nil) {
		errs = append(errs, errors.New("replicas and autoscaling are exclusive: the cluster autoscaler owns the count when autoscaling is set"))
	}
	if (in.AutoscalingMin == nil) != (in.AutoscalingMax == nil) {
		errs = append(errs, errors.New("autoscaling needs both min and max"))
	}
	if in.AutoscalingMin != nil && in.AutoscalingMax != nil && *in.AutoscalingMin > *in.AutoscalingMax {
		errs = append(errs, errors.New("autoscaling.min must not exceed autoscaling.max"))
	}
	switch in.Strategy {
	case "", "RollingUpdate", "OnDelete":
	default:
		errs = append(errs, fmt.Errorf("rollout.strategy %q is not RollingUpdate or OnDelete", in.Strategy))
	}
	return errors.Join(errs...)
}

// metadataMaps returns the labels and annotations of the group's template
// metadata: the user's, the selector labels (direct mode), and, when the
// metadata is the topology entry's (the topology controller copies it onto
// the MachineDeployment itself), the autoscaler annotations.
func (in nodeGroupInputs) metadataMaps(selector, autoscaler bool) (map[string]any, map[string]any) {
	labels := map[string]any{}
	for k, v := range in.Labels {
		labels[k] = v
	}
	if selector {
		labels[clusterNameLabel] = in.Cluster
		labels[deploymentNameLabel] = in.Name
	}
	annotations := map[string]any{}
	for k, v := range in.Annotations {
		annotations[k] = v
	}
	if autoscaler {
		for k, v := range in.autoscalerAnnotations() {
			annotations[k] = v
		}
	}
	return labels, annotations
}

// autoscalerAnnotations are the cluster autoscaler's bounds, read from the
// MachineDeployment object itself. Empty without autoscaling.
func (in nodeGroupInputs) autoscalerAnnotations() map[string]string {
	if in.AutoscalingMin == nil || in.AutoscalingMax == nil {
		return nil
	}
	return map[string]string{
		autoscalerMinSizeAnnotation: fmt.Sprint(*in.AutoscalingMin),
		autoscalerMaxSizeAnnotation: fmt.Sprint(*in.AutoscalingMax),
	}
}

// clusterClassRef returns the name and namespace of a Cluster's
// ClusterClass (v1beta2 classRef, else the legacy class and classNamespace);
// empty without a topology.
func clusterClassRef(cluster *unstructured.Unstructured) (name, namespace string) {
	if name, _, _ = unstructured.NestedString(cluster.Object, "spec", "topology", "classRef", "name"); name != "" {
		namespace, _, _ = unstructured.NestedString(cluster.Object, "spec", "topology", "classRef", "namespace")
		return name, namespace
	}
	name, _, _ = unstructured.NestedString(cluster.Object, "spec", "topology", "class")
	namespace, _, _ = unstructured.NestedString(cluster.Object, "spec", "topology", "classNamespace")
	return name, namespace
}

// ref renders a template reference in the contract's shape.
func (in nodeGroupInputs) ref(r *objectRef) map[string]any {
	if in.contractRefs() {
		return map[string]any{"apiGroup": r.APIGroup, "kind": r.Kind, "name": r.Name}
	}
	ns := r.Namespace
	if ns == "" {
		ns = in.Namespace
	}
	return map[string]any{"apiVersion": r.APIVersion, "kind": r.Kind, "name": r.Name, "namespace": ns}
}

// rollout renders the rollout strategy: v1beta2 `rollout.strategy`, or the
// legacy `strategy`. Nil when nothing is set.
func (in nodeGroupInputs) rollout() map[string]any {
	if in.Strategy == "" && in.MaxSurge == nil && in.MaxUnavailable == nil {
		return nil
	}
	strategy := map[string]any{}
	typ := in.Strategy
	if typ == "" {
		typ = "RollingUpdate"
	}
	strategy["type"] = typ
	if typ == "RollingUpdate" && (in.MaxSurge != nil || in.MaxUnavailable != nil) {
		ru := map[string]any{}
		if in.MaxSurge != nil {
			ru["maxSurge"] = *in.MaxSurge
		}
		if in.MaxUnavailable != nil {
			ru["maxUnavailable"] = *in.MaxUnavailable
		}
		strategy["rollingUpdate"] = ru
	}
	return strategy
}

// deletion renders the node timeouts: v1beta2 `deletion.*TimeoutSeconds`,
// or the legacy durations on the machine spec. Nil when nothing is set.
func (in nodeGroupInputs) deletion() map[string]any {
	if in.NodeDrain == nil && in.NodeVolumeDetach == nil && in.NodeDeletion == nil {
		return nil
	}
	out := map[string]any{}
	set := func(seconds, legacy string, d *time.Duration) {
		if d == nil {
			return
		}
		if in.contractRefs() {
			out[seconds] = int64(d.Seconds())
		} else {
			out[legacy] = d.String()
		}
	}
	set("nodeDrainTimeoutSeconds", "nodeDrainTimeout", in.NodeDrain)
	set("nodeVolumeDetachTimeoutSeconds", "nodeVolumeDetachTimeout", in.NodeVolumeDetach)
	set("nodeDeletionTimeoutSeconds", "nodeDeletionTimeout", in.NodeDeletion)
	return out
}

// renderMachineDeployment builds the MachineDeployment object of a direct
// mode node group.
func renderMachineDeployment(in nodeGroupInputs) *unstructured.Unstructured {
	gvk := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: in.CoreVersion, Kind: "MachineDeployment"}
	labels, annotations := in.metadataMaps(true, false)
	meta := map[string]any{"labels": labels}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	machineSpec := map[string]any{
		"clusterName":       in.Cluster,
		"version":           in.Version,
		"bootstrap":         map[string]any{"configRef": in.ref(in.Bootstrap)},
		"infrastructureRef": in.ref(in.Infrastructure),
	}
	if in.FailureDomain != "" {
		machineSpec["failureDomain"] = in.FailureDomain
	}
	spec := map[string]any{
		"clusterName": in.Cluster,
		"selector":    map[string]any{"matchLabels": map[string]any{clusterNameLabel: in.Cluster, deploymentNameLabel: in.Name}},
		"template":    map[string]any{"metadata": meta, "spec": machineSpec},
	}
	if in.Replicas != nil {
		spec["replicas"] = *in.Replicas
	}
	if rollout := in.rollout(); rollout != nil {
		if in.contractRefs() {
			spec["rollout"] = map[string]any{"strategy": rollout}
		} else {
			spec["strategy"] = rollout
		}
	}
	if deletion := in.deletion(); deletion != nil {
		if in.contractRefs() {
			spec["deletion"] = deletion
		} else {
			for k, v := range deletion {
				machineSpec[k] = v
			}
		}
	}
	obj := newObject(gvk, in.Namespace, in.Name, map[string]string{clusterNameLabel: in.Cluster}, in.autoscalerAnnotations())
	obj.Object["spec"] = spec
	return obj
}

// renderTopologyEntry builds the entry of Cluster.spec.topology.workers
// .machineDeployments the node group owns, and the Cluster patch that
// carries it for a server-side apply under the group's own field manager.
func renderTopologyEntry(in nodeGroupInputs) (entry map[string]any, patch *unstructured.Unstructured) {
	entry = map[string]any{"name": in.Name, "class": in.Class}
	if in.Replicas != nil {
		entry["replicas"] = *in.Replicas
	}
	if in.FailureDomain != "" {
		entry["failureDomain"] = in.FailureDomain
	}
	labels, annotations := in.metadataMaps(false, true)
	meta := map[string]any{}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	if len(meta) > 0 {
		entry["metadata"] = meta
	}
	if len(in.Variables) > 0 {
		keys := make([]string, 0, len(in.Variables))
		for k := range in.Variables {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		overrides := make([]any, 0, len(keys))
		for _, k := range keys {
			overrides = append(overrides, map[string]any{"name": k, "value": in.Variables[k]})
		}
		entry["variables"] = map[string]any{"overrides": overrides}
	}
	if rollout := in.rollout(); rollout != nil {
		if in.contractRefs() {
			entry["rollout"] = map[string]any{"strategy": rollout}
		} else {
			entry["strategy"] = rollout
		}
	}
	if deletion := in.deletion(); deletion != nil {
		if in.contractRefs() {
			entry["deletion"] = deletion
		} else {
			for k, v := range deletion {
				entry[k] = v
			}
		}
	}
	gvk := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: in.CoreVersion, Kind: "Cluster"}
	patch = newObject(gvk, in.Namespace, in.Cluster, nil, nil)
	patch.Object["spec"] = map[string]any{"topology": map[string]any{"workers": map[string]any{"machineDeployments": []any{entry}}}}
	return entry, patch
}

// renderTopologyRemoval is the Cluster apply that drops the group's entry:
// an apply naming no entries releases the one this field manager owned.
func renderTopologyRemoval(in nodeGroupInputs) *unstructured.Unstructured {
	gvk := k8sschema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: in.CoreVersion, Kind: "Cluster"}
	patch := newObject(gvk, in.Namespace, in.Cluster, nil, nil)
	patch.Object["spec"] = map[string]any{"topology": map[string]any{"workers": map[string]any{"machineDeployments": []any{}}}}
	return patch
}

// nodeGroupFieldManager is the field manager of a topology entry: one per
// group, so each owns exactly its list item.
func nodeGroupFieldManager(name string) string {
	return "terraform-provider-capi/machine-deployment/" + name
}

// clusterFacts is what the node group needs from the live Cluster object.
type clusterFacts struct {
	CoreVersion    string
	TopologyMode   bool
	Version        string
	Classes        []string
	FailureDomains []string
}

// readClusterFacts extracts them from a Cluster object and its ClusterClass
// (nil when the cluster has none).
func readClusterFacts(cluster, class *unstructured.Unstructured) clusterFacts {
	facts := clusterFacts{CoreVersion: cluster.GroupVersionKind().Version}
	if topology, ok, _ := unstructured.NestedMap(cluster.Object, "spec", "topology"); ok && len(topology) > 0 {
		facts.TopologyMode = true
		facts.Version, _, _ = unstructured.NestedString(cluster.Object, "spec", "topology", "version")
	}
	if fds, ok, _ := unstructured.NestedSlice(cluster.Object, "status", "failureDomains"); ok {
		for _, fd := range fds {
			if s, ok := fd.(string); ok {
				facts.FailureDomains = append(facts.FailureDomains, s)
			}
		}
	} else if fds, ok, _ := unstructured.NestedMap(cluster.Object, "status", "failureDomains"); ok {
		for name := range fds {
			facts.FailureDomains = append(facts.FailureDomains, name)
		}
		sort.Strings(facts.FailureDomains)
	}
	if class != nil {
		mds, _, _ := unstructured.NestedSlice(class.Object, "spec", "workers", "machineDeployments")
		for _, md := range mds {
			if m, ok := md.(map[string]any); ok {
				if name, _ := m["class"].(string); name != "" {
					facts.Classes = append(facts.Classes, name)
				}
			}
		}
		sort.Strings(facts.Classes)
	}
	return facts
}

// checkAgainstCluster validates the inputs against what the cluster offers.
func (in nodeGroupInputs) checkAgainstCluster(facts clusterFacts) error {
	var errs []error
	if in.topologyMode() != facts.TopologyMode {
		if facts.TopologyMode {
			errs = append(errs, fmt.Errorf("cluster %s uses a ClusterClass: set class to one of its worker classes instead of template", in.Cluster))
		} else {
			errs = append(errs, fmt.Errorf("cluster %s has no topology: set template instead of class", in.Cluster))
		}
	}
	if in.topologyMode() && len(facts.Classes) > 0 && !contains(facts.Classes, in.Class) {
		errs = append(errs, fmt.Errorf("class %q is not a worker class of the cluster's ClusterClass (%s)", in.Class, strings.Join(facts.Classes, ", ")))
	}
	if in.FailureDomain != "" && len(facts.FailureDomains) > 0 && !contains(facts.FailureDomains, in.FailureDomain) {
		errs = append(errs, fmt.Errorf("failure_domain %q is not one of the cluster's failure domains (%s)", in.FailureDomain, strings.Join(facts.FailureDomains, ", ")))
	}
	return errors.Join(errs...)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// nodeGroupStatus is what a live MachineDeployment reports.
type nodeGroupStatus struct {
	ObjectName        string
	Phase             string
	Replicas          int64
	ReadyReplicas     int64
	AvailableReplicas int64
	UpToDateReplicas  int64
	Conditions        []conditionFact
}

type conditionFact struct {
	Type, Status, Reason, Message string
}

// readNodeGroupStatus flattens a MachineDeployment's status.
func readNodeGroupStatus(md *unstructured.Unstructured) nodeGroupStatus {
	st := nodeGroupStatus{ObjectName: md.GetName()}
	st.Phase, _, _ = unstructured.NestedString(md.Object, "status", "phase")
	st.Replicas, _, _ = unstructured.NestedInt64(md.Object, "status", "replicas")
	st.ReadyReplicas, _, _ = unstructured.NestedInt64(md.Object, "status", "readyReplicas")
	st.AvailableReplicas, _, _ = unstructured.NestedInt64(md.Object, "status", "availableReplicas")
	st.UpToDateReplicas, _, _ = unstructured.NestedInt64(md.Object, "status", "upToDateReplicas")
	raw, _, _ := unstructured.NestedSlice(md.Object, "status", "conditions")
	for _, c := range raw {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(m, "type")
		status, _, _ := unstructured.NestedString(m, "status")
		reason, _, _ := unstructured.NestedString(m, "reason")
		message, _, _ := unstructured.NestedString(m, "message")
		st.Conditions = append(st.Conditions, conditionFact{Type: typ, Status: status, Reason: reason, Message: message})
	}
	return st
}

// liveNodeGroup is what Read compares the configuration against: the
// values of a live MachineDeployment (direct) or topology entry.
type liveNodeGroup struct {
	Replicas       *int64
	Version        string
	FailureDomain  string
	Infrastructure string
	Bootstrap      string
	Class          string
}

// readLiveMachineDeployment reads the configured values back from a
// MachineDeployment.
func readLiveMachineDeployment(md *unstructured.Unstructured) liveNodeGroup {
	var out liveNodeGroup
	if r, ok, _ := unstructured.NestedInt64(md.Object, "spec", "replicas"); ok {
		out.Replicas = &r
	}
	out.Version, _, _ = unstructured.NestedString(md.Object, "spec", "template", "spec", "version")
	out.FailureDomain, _, _ = unstructured.NestedString(md.Object, "spec", "template", "spec", "failureDomain")
	out.Infrastructure, _, _ = unstructured.NestedString(md.Object, "spec", "template", "spec", "infrastructureRef", "name")
	out.Bootstrap, _, _ = unstructured.NestedString(md.Object, "spec", "template", "spec", "bootstrap", "configRef", "name")
	return out
}

// findTopologyEntry returns the group's entry of a Cluster's topology, or
// nil when absent.
func findTopologyEntry(cluster *unstructured.Unstructured, name string) map[string]any {
	mds, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "topology", "workers", "machineDeployments")
	for _, md := range mds {
		if m, ok := md.(map[string]any); ok && m["name"] == name {
			return m
		}
	}
	return nil
}

// readLiveTopologyEntry reads the configured values back from a topology entry.
func readLiveTopologyEntry(entry map[string]any) liveNodeGroup {
	var out liveNodeGroup
	if r, ok, _ := unstructured.NestedInt64(entry, "replicas"); ok {
		out.Replicas = &r
	}
	out.FailureDomain, _, _ = unstructured.NestedString(entry, "failureDomain")
	out.Class, _, _ = unstructured.NestedString(entry, "class")
	return out
}

// machineFailure is one machine of a group that is not yet ready.
type machineFailure struct {
	Name, Phase string
	Conditions  []conditionFact
	// Deleting is set when the machine is draining or detaching volumes;
	// Timeout then names the timeout that applies.
	Deleting bool
	Timeout  string
}

// String renders the failure the way the wait error reports it.
func (f machineFailure) String() string {
	var b strings.Builder
	b.WriteString(f.Name)
	if f.Phase != "" {
		b.WriteString(" phase=" + f.Phase)
	}
	for _, c := range f.Conditions {
		b.WriteString(" " + c.Type + "=" + c.Status)
		if c.Reason != "" {
			b.WriteString(" (" + c.Reason + ")")
		}
		if c.Message != "" {
			b.WriteString(": " + c.Message)
		}
	}
	if f.Deleting && f.Timeout != "" {
		b.WriteString(" [" + f.Timeout + "]")
	}
	return b.String()
}

// readMachineFailures reports, for each machine of a group, what keeps it
// from being ready: the phase, the conditions that are not True (v1beta2
// conditions carry the controller's explanation), and, for a machine being
// deleted, the drain or volume-detach timeout that bounds the wait. Ready
// machines are skipped.
func readMachineFailures(machines []unstructured.Unstructured) []machineFailure {
	var out []machineFailure
	for i := range machines {
		m := &machines[i]
		f := machineFailure{Name: m.GetName()}
		f.Phase, _, _ = unstructured.NestedString(m.Object, "status", "phase")
		st := readNodeGroupStatus(m)
		ready := false
		for _, c := range st.Conditions {
			switch {
			case c.Type == "Ready" && c.Status == "True":
				ready = true
			case c.Type == "Deleting" && c.Status == "True":
				// True while in progress; its message says what the
				// deletion is waiting for.
				f.Conditions = append(f.Conditions, c)
			case c.Status != "True" && c.Type != "Paused":
				f.Conditions = append(f.Conditions, c)
			}
		}
		if m.GetDeletionTimestamp() != nil || f.Phase == "Deleting" {
			f.Deleting = true
			f.Timeout = machineDeletionTimeout(m, st.Conditions)
		}
		if ready && !f.Deleting {
			continue
		}
		out = append(out, f)
	}
	return out
}

// machineDeletionTimeout names the timeout that applies to a deleting
// machine: the drain timeout while draining, the volume-detach timeout
// while waiting for volumes, else the node-deletion timeout.
func machineDeletionTimeout(m *unstructured.Unstructured, conditions []conditionFact) string {
	pick := func(contract, legacy, label string) string {
		if secs, ok, _ := unstructured.NestedInt64(m.Object, "spec", "deletion", contract); ok {
			return fmt.Sprintf("%s %ds", label, secs)
		}
		if s, _, _ := unstructured.NestedString(m.Object, "spec", legacy); s != "" {
			return label + " " + s
		}
		return label + " default"
	}
	for _, c := range conditions {
		switch c.Type {
		case "Deleting", "DrainingSucceeded":
			if strings.Contains(strings.ToLower(c.Reason+c.Message), "volume") {
				return pick("nodeVolumeDetachTimeoutSeconds", "nodeVolumeDetachTimeout", "node_volume_detach timeout")
			}
			if strings.Contains(strings.ToLower(c.Reason+c.Message), "drain") {
				return pick("nodeDrainTimeoutSeconds", "nodeDrainTimeout", "node_drain timeout")
			}
		case "VolumeDetachSucceeded":
			return pick("nodeVolumeDetachTimeoutSeconds", "nodeVolumeDetachTimeout", "node_volume_detach timeout")
		}
	}
	return pick("nodeDeletionTimeoutSeconds", "nodeDeletionTimeout", "node_deletion timeout")
}

// describeMachineFailures renders the failures as one line per machine.
func describeMachineFailures(failures []machineFailure) string {
	if len(failures) == 0 {
		return ""
	}
	lines := make([]string, 0, len(failures))
	for _, f := range failures {
		lines = append(lines, "  "+f.String())
	}
	return "machines not ready:\n" + strings.Join(lines, "\n")
}
