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
// including one that is already draining. What surge outliving the mark buys
// is the create branch, and it is worth deriving rather than repeating,
// because the design's §3.2 gives a different reason and that reason does not
// hold against the rules in the order they ship.
//
// §3.2 argues that dropping the surge the moment a pod is marked leaves the
// group at replicas + 1 against a target of replicas, so the surplus rule
// marks a second pod and the group drains at once. It does not: the
// one-at-a-time guard sits ahead of the surplus rule here, the marked pod is
// draining, and the guard returns before that rule is reached. That path is
// closed with surge or without it.
//
// The create branch is what is not closed, because it is checked before the
// guard. With the surge dropped, a group whose surge pod dies while the old
// one is still draining stands at replicas pods against a target of replicas,
// builds no replacement, and drops to replicas - 1 ready the moment the
// draining pod goes. Surge outliving the mark rebuilds it in place instead.
// TestDecideRollout's "the surge pod dying mid-drain is replaced, because
// surge outlives the mark" is that case exactly.
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

	// At target with stale pods left: mark one while the group has a ready pod
	// to spare -- stale or current alike, because what protects players is
	// ready capacity and not which generation supplies it -- or while a stale
	// pod is not serving at all, because retiring what serves nobody costs
	// nobody anything.
	//
	// The second clause is not a relaxation of the first, it is the same rule
	// applied to the pod that would actually go. A pod the kubelet does not
	// call Ready is behind no Service endpoint, so readyBeyond counts it as
	// zero and marking it lowers ready capacity by zero -- and pick sorts
	// not-Ready ahead of Ready among stale candidates, so the pod returned
	// under this clause is that pod and not a ready one standing next to it.
	//
	// Without the clause a stale pod that stays unready holds the gate shut
	// against its own replacement: it counts towards stale, so it holds the
	// surge open and keeps total at target, and it contributes nothing to
	// readyTotal, so it cannot supply the spare capacity the first clause asks
	// for. Every branch then declines, and nothing this function does changes
	// the state they decline on -- the rollout stops on the pass that reaches
	// it and only something outside here (the pod recovering, or an operator
	// deleting it by hand) starts it again. The ordinary way in is the one
	// that makes it worth a paragraph: an operator bumps spec.image because a
	// proxy is crashlooping, and the crashlooping proxy deadlocks the roll
	// issued to replace it.
	if stale > 0 && (readyBeyond(pods, replicas) || anyStaleNotReady(pods)) {
		return RolloutDecision{Drain: pick(staleOnly(pods), 1)}
	}
	return RolloutDecision{}
}

// anyStaleNotReady reports whether some stale pod is not serving: not Ready,
// and not already going.
//
// Draining is excluded so that this counts the same population readyBeyond
// does -- both ask about pods still in service, and a predicate that answered
// about a pod already on its way out would not be about capacity at all. Note
// staleOnly does not filter Draining, so a draining pod can be handed to pick;
// what keeps the two in step is that the draining > 0 guard returns before
// this branch is reached, so neither this predicate nor that pick ever sees a
// draining pod on a live path.
func anyStaleNotReady(pods []ProxyView) bool {
	for _, p := range pods {
		if p.Stale && !p.Ready && !p.Draining {
			return true
		}
	}
	return false
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
// would drain two pods for one replacement. Then not-Ready before Ready,
// because a pod the kubelet does not call Ready is behind no Service
// endpoint: nobody can reach it, nobody new will, and retiring it lowers the
// group's ready capacity by exactly zero. That makes it the cheapest thing
// here to retire, in the surplus branch as much as the at-target one, and it
// is what lets the at-target gate allow a mark on the strength of an unready
// stale pod: the gate decides whether, this decides which, and the gate's
// second disjunct is exact because this clause guarantees the pod it gets
// back is the unready one it reasoned about. It is above the player clauses
// deliberately: a fallen pod's last reported count is what it held
// before it fell over, not what it holds now, so sorting it by that figure
// would rank it on a number that has stopped meaning anything.
//
// Then fewest players, because the emptiest finishes soonest and disconnects
// fewest people at the deadline. An untrusted count sorts last on the
// repository's own rule -- unknown counts as occupied, since a pod whose
// agent stream is down may hold players nobody can see.
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
		if a.Ready != b.Ready {
			return !a.Ready
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
