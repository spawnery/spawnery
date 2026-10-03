/*
Copyright paul_wtf.

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

// IsDeparting: cordoned, or carrying a -drain-taint key with an effect that
// repels pods. A PreferNoSchedule taint would let the replacement land on the
// same node and be condemned again, forever.
func IsDeparting(node *corev1.Node, taintKeys []string) bool {
	departing, _ := departingWithHint(node, taintKeys)
	return departing
}

// wellKnownDrainTaints are only warned about, never acted on: acting would tie
// node drains to another project's key names. The warning catches a forgotten
// -drain-taint flag.
var wellKnownDrainTaints = map[string]string{
	"ToBeDeletedByClusterAutoscaler": "cluster-autoscaler",
	"karpenter.sh/disrupted":         "Karpenter",
	"karpenter.sh/disruption":        "Karpenter",
}

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

// nodeDeparting answers false on every failure: a cache miss must not empty a
// group, and the Node watch asks again soon.
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
		// A log line, not a condition: the operator's flags are no group owner's to fix.
		warnedMissingDrainTaint.Do(nodeName, func() {
			log.FromContext(ctx).Info(
				"a node carries a drain taint this operator was not configured for; its pods will not be moved",
				"node", nodeName, "project", hint, "flag", "-drain-taint",
				"remedy", "pass -drain-taint with that project's key, or cordon the node")
		})
	}
	return departing
}

// Never pruned: bounded by the cluster's node count.
var warnedMissingDrainTaint once

// blockedReplacement says why a group cannot rebuild what it condemns off a
// departing node (backoff, unusable Network). It still condemns, since those
// players are evicted anyway; the message says the group will run short.
type blockedReplacement struct {
	Reason string
	// A backoff window ends on its own; a broken Network waits for a person.
	Bounded bool
}

func drainingCondition(nodeNames []string) metav1.Condition {
	return drainingConditionBlocked(nodeNames, blockedReplacement{})
}

func drainingConditionBlocked(nodeNames []string, blocked blockedReplacement) metav1.Condition {
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
	if blocked.Reason != "" {
		ends := "until that is fixed"
		if blocked.Bounded {
			ends = "until that clears on its own"
		}
		cond.Message += fmt.Sprintf(
			"; this group cannot build replacements for them because %s, so it will run below its "+
				"size %s", blocked.Reason, ends)
	}
	return cond
}

// once is sync.Once per key. f runs under the lock so racing callers with the
// same key produce one call.
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
