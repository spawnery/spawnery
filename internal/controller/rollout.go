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
	"sort"
	"time"
)

// ProxyView is what the rollout decision needs to know about one proxy pod.
// It is deliberately not a corev1.Pod: the rules are about staleness,
// occupancy and intent, and a view keeps them testable without a cluster.
type ProxyView struct {
	Name         string
	Stale        bool
	Ready        bool
	Draining     bool
	Players      int32
	PlayersStale bool
	CreatedAt    time.Time
}

// RolloutDecision is what one pass should do: how many pods to create, and
// which pods to mark as going. Drain carries names rather than indices,
// because after this milestone a pod's fate no longer depends on its
// position.
type RolloutDecision struct {
	Create int32
	Drain  []string
}

// DecideRollout decides one pass: how many pods to create, and which pods to
// mark as going.
//
// The target size is replicas plus a surge of 1 while any pod is stale --
// including one that is already draining. Dropping the surge the moment a pod
// is marked would leave the group one over its target, and the surplus rule
// below would mark a second pod on the same pass: a rolling update that
// drains the whole group at once, which is exactly what one-at-a-time
// forbids.
func DecideRollout(pods []ProxyView, replicas int32) RolloutDecision {
	var stale, draining int32
	for _, p := range pods {
		if p.Stale {
			stale++
		}
		if p.Draining {
			draining++
		}
	}

	var surge int32
	if stale > 0 {
		surge = 1
	}
	target := replicas + surge
	total := int32(len(pods))

	if total < target {
		return RolloutDecision{Create: target - total}
	}

	// One at a time. The cycle advances only when the deletion loop removes
	// the draining pod, which happens when it is empty or when its deadline
	// expires.
	if draining > 0 {
		return RolloutDecision{}
	}

	if total > target {
		return RolloutDecision{Drain: pick(pods, total-target)}
	}

	// At target with stale pods left: mark one, but only while the group has
	// a ready pod to spare -- stale or current alike, because what protects
	// players is ready capacity and not which generation supplies it.
	if stale > 0 && readyBeyond(pods, replicas) {
		return RolloutDecision{Drain: pick(staleOnly(pods), 1)}
	}
	return RolloutDecision{}
}

// readyBeyond reports whether a stale pod may safely go: the group already
// has a ready pod to spare, stale and current counted the same, so retiring
// one stale pod cannot drop ready capacity below replicas.
func readyBeyond(pods []ProxyView, replicas int32) bool {
	var readyTotal int32
	for _, p := range pods {
		if p.Ready && !p.Draining {
			readyTotal++
		}
	}
	return readyTotal > replicas
}

func staleOnly(pods []ProxyView) []ProxyView {
	out := make([]ProxyView, 0, len(pods))
	for _, p := range pods {
		if p.Stale {
			out = append(out, p)
		}
	}
	return out
}

// pick returns the n pods that should go, in order. Stale before current,
// because a stale pod has to go regardless and taking a current one first
// would drain two pods for one replacement. Then fewest players, because the
// emptiest finishes soonest and disconnects fewest people at the deadline. An
// untrusted count sorts last on the repository's own rule -- unknown counts
// as occupied, since a pod whose agent stream is down may hold players nobody
// can see.
//
// Age breaks what is left, newest first, and the direction is not symmetry: it
// is the same guess the rule this function replaced was making, that an older
// proxy has had longer to collect players. That guess is worth nothing between
// two known counts -- equally empty is equally empty, whoever got there first
// -- and it is the only thing there is when every count is untrusted, which is
// an operator that has just restarted or a fleet whose agent streams are all
// down. Marking the oldest there picks the pod most likely to be occupied,
// which then reads as occupied, which holds the drain open to the full
// deadline before disconnecting whoever was on it. So age points the way it
// always did, and a later reader should not flip it back for looking
// arbitrary: it is a stand-in for the occupancy the operator cannot see.
//
// It also keeps the order deterministic, so a test can name the pod it expects
// rather than counting survivors.
func pick(pods []ProxyView, n int32) []string {
	candidates := append([]ProxyView(nil), pods...)
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Stale != b.Stale {
			return a.Stale
		}
		if a.PlayersStale != b.PlayersStale {
			return !a.PlayersStale
		}
		if a.Players != b.Players {
			return a.Players < b.Players
		}
		return a.CreatedAt.After(b.CreatedAt)
	})
	out := make([]string, 0, n)
	for i := int32(0); i < n && int(i) < len(candidates); i++ {
		out = append(out, candidates[i].Name)
	}
	return out
}
