/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// IsDeparting reports whether this node is on its way out of service, and the
// pods on it should be moved before somebody else moves them the hard way.
//
// Two ways in. spec.unschedulable is what kubectl cordon and kubectl drain
// set, and it is the criterion the master design names (§5.1); it is hardwired
// and not configurable. A taint key from the operator's -drain-taint list is
// the second, for autoscalers that taint before they cordon.
//
// The effect is part of the taint test and not decoration. A PreferNoSchedule
// taint does not stop the scheduler putting the replacement pod back on this
// same node: we would condemn a pod, rebuild it here, condemn that one next
// pass, and rotate for as long as the taint stands. Restricting the match to
// the two effects that actually repel a pod closes that loop by construction
// rather than with a guard somewhere downstream.
//
// A nil node is not departing. The caller reaches that case when a node cannot
// be read at all, and failing towards "not departing" keeps an unreadable Node
// from emptying a group; the watch and the resync bring the answer back within
// seconds.
func IsDeparting(node *corev1.Node, taintKeys []string) bool {
	departing, _ := departingWithHint(node, taintKeys)
	return departing
}

// wellKnownDrainTaints are keys other projects use to mark a node they are
// about to remove. This operator does not act on them and will not: a default
// that reacted to another project's taint key would couple it to a vocabulary
// that project is free to rename, which is exactly the coupling a configurable
// -drain-taint list exists to avoid.
//
// Noticing is a different thing from reacting, and the coupling it risks is
// different too. A key that gets renamed here costs a warning that stops
// appearing; a key that got renamed in the list above would cost a node drain
// that stops working. So this is a list of things to mention, and nothing
// downstream branches on it.
//
// What it answers is the one thing an operator running an autoscaler cannot
// find out from here: whether they forgot the flag. An unset -drain-taint and
// a genuinely quiet cluster look identical from inside this operator -- until
// a node turns up carrying a taint that plainly means "this node is going" and
// is not in the list it was told about.
var wellKnownDrainTaints = map[string]string{
	"ToBeDeletedByClusterAutoscaler": "cluster-autoscaler",
	"karpenter.sh/disrupted":         "Karpenter",
	"karpenter.sh/disruption":        "Karpenter",
}

// departingWithHint is IsDeparting plus the name of a project whose drain
// taint this node carries and this operator was not configured for. The hint
// is empty whenever the node is departing by a route this operator does
// honour, because there is then nothing missing to report.
func departingWithHint(node *corev1.Node, taintKeys []string) (bool, string) {
	if node == nil {
		return false, ""
	}
	if node.Spec.Unschedulable {
		return true, ""
	}
	var hint string
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if slices.Contains(taintKeys, taint.Key) {
			return true, ""
		}
		if project, known := wellKnownDrainTaints[taint.Key]; known && hint == "" {
			hint = project
		}
	}
	return false, hint
}

// nodeDeparting resolves a pod's node and asks IsDeparting about it.
//
// Every failure answers false. An empty name is an unscheduled pod, which is
// on no node; a Get that fails is a node we cannot read, and a group must not
// be emptied on the strength of a cache miss. The next reconcile asks again,
// so a false answer here costs at most a delay and never a wrong deletion.
// ServerGroupReconciler and ProxyGroupReconciler each watch Node and map an
// event onto the groups with pods on it, so that delay is the time a cordon
// or taint takes to reach the cache rather than a whole resync interval.
func nodeDeparting(ctx context.Context, reader client.Reader, nodeName string, taintKeys []string) bool {
	if nodeName == "" {
		return false
	}
	node := &corev1.Node{}
	if err := reader.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return false
	}
	departing, hint := departingWithHint(node, taintKeys)
	if hint != "" {
		// Once per node, not once per pod on it and not once per pass. This
		// runs for every pod of every group on every reconcile, so an
		// ungated log would be several lines a second for as long as the
		// node stood -- and the thing it reports does not change while it
		// stands.
		//
		// A log line and not a condition: what is wrong is the operator's own
		// flags, which belong to whoever deployed it, and no group's status is
		// the place to report a mistake no group's owner can fix.
		warnedMissingDrainTaint.Do(nodeName, func() {
			log.FromContext(ctx).Info(
				"a node carries a drain taint this operator was not configured for; its pods will not be moved",
				"node", nodeName, "project", hint, "flag", "-drain-taint",
				"remedy", "pass -drain-taint with that project's key, or cordon the node")
		})
	}
	return departing
}

// warnedMissingDrainTaint remembers which nodes have already been reported, so
// the warning above is one line per node rather than one per pod per pass.
//
// Never pruned. The keys are node names, so the set is bounded by the nodes
// this cluster has ever had that carried an unconfigured drain taint -- which
// is at most the cluster's node count, and in the case this exists for is one
// or two. A node that comes back after its taint was cleared is not warned
// about again, which is the right way round: the flag is still missing, and
// the line already stands in the log.
var warnedMissingDrainTaint once

// drainingCondition builds the NodeDraining condition from the names of
// departing nodes carrying at least one of this group's live pods. Both
// ServerGroupReconciler.Reconcile and ProxyGroupReconciler.reconcileReplicas
// call this over names collected from a fact they have already computed this
// pass -- ServerView.Condemned and reconcileReplicas's own per-pod
// nodeDeparting check, respectively -- rather than asking nodeDeparting about
// any pod a second time. Duplicates and empty names (an unresolvable pod) are
// tolerated here so neither caller has to dedupe or filter before calling.
func drainingCondition(nodeNames []string) metav1.Condition {
	cond := metav1.Condition{
		Type:    spawneryv1alpha1.ConditionNodeDraining,
		Status:  metav1.ConditionFalse,
		Reason:  spawneryv1alpha1.ReasonNoNodesDraining,
		Message: "no pods are on nodes that are on their way out of service",
	}
	seen := make(map[string]bool, len(nodeNames))
	names := make([]string, 0, len(nodeNames))
	for _, n := range nodeNames {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	if len(names) == 0 {
		return cond
	}
	sort.Strings(names)
	cond.Status = metav1.ConditionTrue
	cond.Reason = spawneryv1alpha1.ReasonNodeDraining
	cond.Message = fmt.Sprintf("pods are on node(s) %s, which are on their way out of service",
		strings.Join(names, ", "))
	return cond
}

// once is a set of keys each of which runs its function the first time only.
//
// sync.Once is per value and this needs per key; sync.Map plus LoadOrStore
// would run the function on every caller that raced the first. This is the
// small, obvious version: one mutex, one map, and the function called under it
// so two goroutines with the same key produce one call.
type once struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (o *once) Do(key string, f func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen[key] {
		return
	}
	if o.seen == nil {
		o.seen = map[string]bool{}
	}
	o.seen[key] = true
	f()
}
