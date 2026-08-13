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

	"github.com/spawnery/spawnery/internal/phase"
)

// ServerView is everything the group logic needs about one server. It is a
// value type on purpose: the selection rules are pure and table-tested.
type ServerView struct {
	// Name of the Server object.
	Name string
	// Phase is its current state machine position.
	Phase phase.Phase
	// Players is the last reported count.
	Players int32
	// Slots is the reported capacity.
	Slots int32
	// EmptyFor is how long this server has been reporting zero players. Like
	// agent.Snapshot.EmptyFor it decides nothing on its own: a server that was
	// never empty carries zero here too, so every rule that reads it also asks
	// Players == 0 && !Stale.
	EmptyFor time.Duration
	// Stale is true if the count cannot be trusted. Stale counts as occupied.
	Stale bool
	// WasRegistered is true if this server was registered with the proxies at
	// some point in the life of its current pod. It is read from
	// status.wasRegistered, never derived from the phase: a server that lost
	// its probe is back in Starting with its players still connected, because
	// deregistering stops new joins without moving anyone off.
	WasRegistered bool
	// SessionsGone is true if the pod reached a terminal state or disappeared.
	// Either way the process is down and its players went with it — the same
	// reasoning the state machine uses when it refuses to drain a terminal pod.
	SessionsGone bool
	// Generation is the group generation this server was created from.
	Generation int64
	// Retire is spec.retire: the group has asked this server to retire. It
	// is the single signal for the update's maxUnavailable budget, and it
	// survives the escalation to Draining that maxStaleSeconds can force —
	// which is what tells that drain apart from one a scale-down started.
	Retire bool
	// CreatedAt is the creation timestamp of the Server object.
	CreatedAt time.Time
	// FailedAt is status.failedAt: when this server entered phase Failed.
	// Zero for a server that has not. The backoff counts a failure from this
	// rather than from the moment it observed one, so a window cannot be
	// extended by being looked at.
	FailedAt time.Time
	// ReadySince is status.readySince: when this server last entered phase
	// Ready. Zero for one that never did — and the Server controller clears
	// it on every exit from Ready, so a Failed server carries no ReadySince.
	ReadySince time.Time
}

// isOccupied is the single occupancy rule of the system. Both sides of the
// PodDisruptionBudget are computed from it: the Server controller labels pods
// with it (syncOccupiedLabel) and the ServerGroup controller sizes the budget's
// minAvailable from it (ServerView.Occupied). The two have to agree pod for
// pod. Counting fewer pods than carry the label hands the eviction API a
// disruption to spend on a pod that still has players; counting a pod the label
// has released pins minAvailable above a budget that can never be met, and
// kubectl drain then wedges on a pod nobody is on. They used to be two
// implementations kept in step by a comment, and they drifted.
//
// players > 0 is the plain case. A count we cannot trust hides players only
// where players could be, and that takes the server having been registered with
// the proxies at some point in the life of this pod, because nobody is ever
// routed to one that was not. That is what keeps a live server whose agent went
// quiet protected.
//
// sessionsGone overrides all of it, including a non-zero count. A pod that
// reached a terminal state, or disappeared, took every session it held down
// with it — the same reasoning the state machine uses when it refuses to drain
// a terminal pod. The last count the registry saw is no evidence to the
// contrary: nothing tells the registry to forget a pod, so a server that
// crashed with seven players on it goes on reporting seven for as long as the
// object is retained. Treating that as occupied protects nobody, while the
// budget built from it refuses to release the pod: with currentHealthy below
// desiredHealthy the eviction API will not touch an unhealthy pod either, and
// an operator's kubectl drain never finishes on that node — for the whole
// failed-retention window, an hour by default.
func isOccupied(players int32, stale, wasRegistered, sessionsGone bool) bool {
	if sessionsGone {
		return false
	}
	return players > 0 || (stale && wasRegistered)
}

// Occupied reports whether the pod of this server must be treated as carrying
// players. The group's PodDisruptionBudget is sized from it.
func (v ServerView) Occupied() bool {
	return isOccupied(v.Players, v.Stale, v.WasRegistered, v.SessionsGone)
}

// clampReport bounds what an agent reports about itself by the capacity the
// operator handed its group.
//
// Registry.ReportPlayers rejects a player count above the reported slots, but
// checks the slots against nothing, because it does not know which group a pod
// belongs to. Here the two meet. From milestone 4a the reported slots feed the
// group's scaling decision, so one pod reporting slots: 1000000 at zero players
// would make its whole group look permanently spacious and suppress every
// scale-up for every server in it — an effect reaching across pod boundaries,
// which is exactly what the agent channel's design rules out everywhere else.
//
// The players are cut to the clamped capacity rather than to maxPlayers, so a
// group whose servers legitimately report a smaller capacity than the group's
// bound stays consistent with itself.
func clampReport(players, slots, maxPlayers int32) (int32, int32) {
	// Floored at one rather than zero, and not because a group is ever
	// configured with a capacity of zero: the CRD forbids it. This is the
	// invariant the function must carry on its own — a clamp that could turn a
	// positive player count into zero would take an occupied server out of
	// Occupied(), and with it out of the budget's minAvailable, while the pod
	// label computed from the unclamped snapshot still says it is occupied.
	// candidates.go's isOccupied comment says what that costs.
	if maxPlayers < 1 {
		maxPlayers = 1
	}
	if slots > maxPlayers {
		slots = maxPlayers
	}
	if players > slots {
		players = slots
	}
	return players, slots
}

// mayHavePlayers is the question the deletion candidate selection asks: could
// this particular server be carrying players right now?
//
// A count we cannot trust hides players only on a server the proxies actually
// route to, which is what status.wasRegistered records. Applying staleness to
// a server that was never registered would make every server that failed to
// come up permanently undeletable and leave a group unable to ever shrink.
//
// Unlike Occupied it does not exempt a server whose sessions are gone. That
// exemption exists to release a dead pod from the budget; here it would only
// buy the right to nominate a server that countsTowardSize already excludes,
// so leaving it out costs nothing and keeps the nomination the conservative
// one of the two.
func (v ServerView) mayHavePlayers() bool {
	return v.Players > 0 || (v.Stale && v.WasRegistered)
}

// leaving reports whether the server is already on its way out, so the group
// must not count it as a candidate again.
//
// Retiring is in here for a second reason beyond nomination: dropping out of
// the group's size is exactly what makes the spare-slot rule order a
// replacement for a server a rolling update has retired. The generation never
// enters the capacity arithmetic; this does the work instead.
func (v ServerView) leaving() bool {
	return v.Phase == phase.Draining || v.Phase == phase.Terminating || v.Phase == phase.Retiring
}

// countsTowardSize reports whether this server holds the group at its floor.
//
// A server that is already leaving does not, and neither does a Failed one: it
// is deregistered from the proxies, no player can join it, and the Server
// controller keeps it for the group's failed retention — an hour by default —
// so that somebody can look at it. Counting it would leave the group below its
// floor for that whole hour with nothing a player could join, which is the
// opposite of what the retention is for.
func (v ServerView) countsTowardSize() bool {
	return !v.leaving() && v.Phase != phase.Failed
}

// tookPlayers reports whether the server was ever able to hold players. That
// is status.wasRegistered and not the phase: a server sitting in Starting
// after a readiness loss took players for as long as it was Ready, and
// removing it disturbs sessions that a never-registered server does not have.
func (v ServerView) tookPlayers() bool {
	return v.WasRegistered
}

// SelectDeletionCandidates nominates up to count servers for removal.
//
// It never nominates a server that may be carrying players, and a stale count
// on a registered server counts as carrying players — one server too many
// beats one kicked player. Servers that never took players go first, then the
// youngest, so that long-lived sessions on older instances are disturbed last.
// Servers that do not count toward the group's size — leaving or Failed ones —
// are never nominated: removing them would not shrink the group.
func SelectDeletionCandidates(views []ServerView, count int) []string {
	if count <= 0 {
		return nil
	}

	eligible := make([]ServerView, 0, len(views))
	for _, v := range views {
		if !v.countsTowardSize() || v.mayHavePlayers() {
			continue
		}
		eligible = append(eligible, v)
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].tookPlayers() != eligible[j].tookPlayers() {
			return !eligible[i].tookPlayers()
		}
		if !eligible[i].CreatedAt.Equal(eligible[j].CreatedAt) {
			return eligible[i].CreatedAt.After(eligible[j].CreatedAt)
		}
		return eligible[i].Name < eligible[j].Name
	})

	if count > len(eligible) {
		count = len(eligible)
	}
	if count == 0 {
		return nil
	}

	names := make([]string, 0, count)
	for _, v := range eligible[:count] {
		names = append(names, v.Name)
	}
	return names
}

// maxRetainedFailures is how many Failed servers a group keeps for diagnosis.
//
// A Failed server holds its pod, and its full resource request, for the whole
// of spec.failedRetentionSeconds — an hour by default. A broken image does not
// take that hour to fail: MaxContainerRestarts plus the kubelet's backoff gets
// there in a minute or two, and the group creates a replacement on the next
// five-second pass. Left uncapped that is dozens of retained servers and pods
// per floor replica before the first one expires.
//
// One retained failure is enough to diagnose from, and it is the retention
// window that then bounds the footprint rather than the failure rate. Proper
// backoff, a Degraded condition and giving up on repeated failures belong to
// milestone 4; this is only the bound that keeps the cluster usable until then.
const maxRetainedFailures = 1

// selectFailedForPruning names the Failed servers beyond the retention cap.
//
// The newest generation's failures come first, and within one generation the
// oldest failure is the one kept.
//
// Oldest-within-a-generation is the original rule and its reason is unchanged:
// the first failure after a change is the one that says what broke, and every
// later one is the same story again, told by a server that started against an
// already-broken world. The generation ordering is that same reason applied to
// the largest change a group can undergo. A generation bump is a change, so the
// first failure after it — the current generation's earliest — is the one that
// says what broke; the previous generation's corpse says nothing about the new
// image.
//
// It is load-bearing as well as truer. coldStart is suppressed only while a
// Failed server of the *current* generation is retained (design §3.7), and that
// suppression is the whole of 4b's guard against a broken new image being
// re-created every five seconds. With maxRetainedFailures at 1, a purely
// oldest-first rule keeps any inherited older failure and prunes the current
// generation's — deleting the one thing doing the suppressing, on the same
// reconcile that observes it, so the loop runs at time-to-fail instead of once
// per retention window, and the corpse the operator is left with is the one that
// says nothing. Preferring the newest generation is what gives coldStart a
// suppression this function cannot take away.
//
// Generations are compared numerically rather than against the group's current
// one, which keeps this a pure two-argument selector: a Server is stamped with
// its group's generation when it is created and a group's generation only ever
// increases, so the highest generation among the failures is the current one
// whenever a current-generation failure exists.
//
// Servers already on their way out are left alone — they are being removed
// anyway, and counting them would let a second failure through while the first
// drains.
func selectFailedForPruning(views []ServerView, keep int) []string {
	failed := make([]ServerView, 0, len(views))
	for _, v := range views {
		if v.Phase == phase.Failed {
			failed = append(failed, v)
		}
	}
	if len(failed) <= keep {
		return nil
	}

	sort.SliceStable(failed, func(i, j int) bool {
		if failed[i].Generation != failed[j].Generation {
			return failed[i].Generation > failed[j].Generation
		}
		if !failed[i].CreatedAt.Equal(failed[j].CreatedAt) {
			return failed[i].CreatedAt.Before(failed[j].CreatedAt)
		}
		return failed[i].Name < failed[j].Name
	})

	names := make([]string, 0, len(failed)-keep)
	for _, v := range failed[keep:] {
		names = append(names, v.Name)
	}
	return names
}

// occupiedPods is the absolute number of pods the group's PodDisruptionBudget
// has to keep available. Every phase is counted, including the servers that are
// draining: their pods still carry the occupied label until the last player is
// off, so leaving them out would lower minAvailable below the number of pods
// the selector matches — and that difference is exactly one eviction the API
// would then permit on a pod that still has players.
func occupiedPods(views []ServerView) int32 {
	var n int32
	for _, v := range views {
		if v.Occupied() {
			n++
		}
	}
	return n
}

// GroupTotals is the aggregated status of a group.
type GroupTotals struct {
	// Replicas is the number of Server objects.
	Replicas int32
	// ReadyReplicas is how many are in phase Ready.
	ReadyReplicas int32
	// OnlinePlayers is the sum of players, whatever their generation.
	OnlinePlayers int32
	// FreeSlots counts only Ready servers of the current generation with a
	// fresh player count. Stale generations are excluded on purpose: without
	// that, a rolling update would never create replacements, because the old
	// servers' free slots would satisfy the scaler forever.
	FreeSlots int32
}

// AggregateGroup sums the views up for the group status.
func AggregateGroup(views []ServerView, generation int64) GroupTotals {
	var t GroupTotals
	for _, v := range views {
		t.Replicas++
		if v.Phase == phase.Ready {
			t.ReadyReplicas++
		}
		if !v.Stale {
			t.OnlinePlayers += v.Players
		}
		if v.Phase == phase.Ready && v.Generation == generation && !v.Stale {
			free := v.Slots - v.Players
			if free > 0 {
				t.FreeSlots += free
			}
		}
	}
	return t
}
