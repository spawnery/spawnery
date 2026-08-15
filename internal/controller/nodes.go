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
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	if node == nil {
		return false
	}
	if node.Spec.Unschedulable {
		return true
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if slices.Contains(taintKeys, taint.Key) {
			return true
		}
	}
	return false
}

// nodeDeparting resolves a pod's node and asks IsDeparting about it.
//
// Every failure answers false. An empty name is an unscheduled pod, which is
// on no node; a Get that fails is a node we cannot read, and a group must not
// be emptied on the strength of a cache miss. The next reconcile asks again,
// so a false answer here costs at most a delay and never a wrong deletion.
// ServerGroupReconciler watches Node and maps an event onto the groups with
// pods on it, so that delay is the time a cordon or taint takes to reach the
// cache rather than a whole resync interval.
func nodeDeparting(ctx context.Context, reader client.Reader, nodeName string, taintKeys []string) bool {
	if nodeName == "" {
		return false
	}
	node := &corev1.Node{}
	if err := reader.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return false
	}
	return IsDeparting(node, taintKeys)
}
