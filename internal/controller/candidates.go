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
	"sort"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// ServerView is everything the group logic needs about one server, as a value
// so the selection rules stay pure.
type ServerView struct {
	Name string
	// Ordinal is spec.ordinal, the index a persistent server's claim is named
	// from. Nil for an ephemeral server.
	Ordinal *int32
	// Number is spec.number. Zero for a server created before the field existed.
	Number  int32
	Phase   phase.Phase
	Players int32
	Slots   int32
	// Playable is how many of Slots count as capacity; 0 reads as Slots.
	Playable int32
	// EmptyFor is zero also for a server that was never empty, so every reader
	// also checks Players == 0 && !Stale.
	EmptyFor time.Duration
	// Stale is true if the count cannot be trusted. Stale counts as occupied.
	Stale bool
	// WasRegistered is status.wasRegistered, never derived from the phase: a
	// server that lost its probe is back in Starting with its players still on.
	WasRegistered bool

	// Registered is whether the proxies have this server right now, which
	// decides whether its empty seats are capacity.
	Registered bool
	// JoinsClosed: the server wants no new players but keeps the ones it has,
	// and stays registered meanwhile.
	JoinsClosed bool
	// SessionsGone is true if the pod reached a terminal state or disappeared.
	SessionsGone bool
	// Generation is the group generation this server was created from.
	Generation int64
	// PodHash is spec.podHash. Empty means the server predates the field and
	// is adopted rather than replaced.
	PodHash string
	// Retire is spec.retire. It survives the escalation to Draining that
	// maxStaleSeconds forces, which tells that drain apart from a scale-down.
	Retire bool
	// Hold is spec.hold: nothing automatic removes this server.
	Hold bool
	// Condemned is true when the pod sits on a node leaving service.
	Condemned bool
	// NodeName is pod.spec.nodeName, for the NodeDraining condition.
	NodeName  string
	CreatedAt time.Time
	// FailedAt is status.failedAt. The backoff counts from it rather than from
	// when it saw the failure, so looking cannot extend a window.
	FailedAt time.Time
	// ReadySince is status.readySince, cleared on every exit from Ready.
	ReadySince time.Time
	// ResizePending is status.storageResizePending: the pod must restart before
	// the filesystem follows the grown volume.
	ResizePending bool
	// ResizeError is status.storageResizeError, or empty.
	ResizeError string
}

// staleSpec compares render hashes. Either side empty means "do not compare":
// an empty view hash predates spec.podHash, and treating it as stale would
// retire every server on upgrade; an empty want comes from a pass without a
// usable Network, which must not become a fleet changeover.
func staleSpec(v ServerView, want string) bool {
	if want == "" || v.PodHash == "" {
		return false
	}
	return v.PodHash != want
}

// isOccupied is the occupancy rule for a server pod. Both the occupied pod
// label (syncOccupiedLabel) and the PodDisruptionBudget's minAvailable
// (ServerView.Occupied) come from it, and they must agree pod for pod, or the
// eviction API either removes a pod with players or never lets kubectl drain
// finish. Proxy pods carry the same label from proxyOccupied; the LabelRole
// term in reconcilePDB's selector keeps them out of this budget.
//
// A stale count hides players only on a server that was ever registered,
// since nobody is routed to one that was not. sessionsGone overrides even a
// non-zero count: the registry never forgets a crashed pod's last count, and
// counting it would block drains for the whole failed retention.
func isOccupied(players int32, stale, wasRegistered, sessionsGone bool) bool {
	if sessionsGone {
		return false
	}
	return players > 0 || (stale && wasRegistered)
}

// Occupied reports whether this server's pod counts toward the group's
// PodDisruptionBudget.
func (v ServerView) Occupied() bool {
	return isOccupied(v.Players, v.Stale, v.WasRegistered, v.SessionsGone)
}

// clampReport bounds an agent's reported slots by its group's capacity.
// Registry.ReportPlayers cannot, not knowing the group, and one pod reporting
// huge slots would suppress scale-up for the whole group.
func clampReport(players, slots, maxPlayers int32) (int32, int32) {
	// Floored at one even though the CRD forbids zero: clamping a positive
	// player count to zero would drop an occupied server from Occupied() while
	// its pod label, computed unclamped, still says occupied.
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

func playableSeats(reported int32, spec *int32, slots int32) int32 {
	playable := slots
	switch {
	case reported > 0:
		playable = reported
	case spec != nil:
		playable = *spec
	}
	if playable > slots {
		playable = slots
	}
	if playable < 1 && slots > 0 {
		playable = 1
	}
	return playable
}

func (v ServerView) freeSeats() int32 {
	playable := v.Playable
	if playable <= 0 || playable > v.Slots {
		playable = v.Slots
	}
	if free := playable - v.Players; free > 0 {
		return free
	}
	return 0
}

// mayHavePlayers: staleness counts only on a registered server, or a server
// that never came up would be undeletable. Unlike Occupied it does not exempt
// SessionsGone; countsTowardSize already excludes those.
func (v ServerView) mayHavePlayers() bool {
	return v.Players > 0 || (v.Stale && v.WasRegistered)
}

// leavingByPhase covers the phases reached only after something, not
// necessarily this reconciler, asked the server to go. expectationDelete
// needs exactly this, without Condemned.
func (v ServerView) leavingByPhase() bool {
	return v.Phase == phase.Draining || v.Phase == phase.Terminating || v.Phase == phase.Retiring
}

// leaving reports whether the server is already on its way out. Retiring and
// Condemned servers drop out of the group's size here, which is what makes the
// spare-slot rule order their replacements in the same pass.
func (v ServerView) leaving() bool {
	return v.leavingByPhase() || v.Condemned
}

// countsTowardSize reports whether this server holds the group at its floor.
// Failed and Finished servers do not: they take no players but are retained
// (Failed for an hour by default), and counting them would hold the group below
// its floor meanwhile.
func (v ServerView) countsTowardSize() bool {
	return !v.leaving() && !phase.Terminal(v.Phase)
}

// tookPlayers is status.wasRegistered, not the phase: a server back in
// Starting after a readiness loss may still carry sessions.
func (v ServerView) tookPlayers() bool {
	return v.WasRegistered
}

// SelectDeletionCandidates nominates up to count servers for removal.
//
// It never nominates a server that may be carrying players, and a stale count
// on a registered server counts as carrying players. Servers that never took
// players go first, then the youngest, so long-lived sessions are disturbed
// last.
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

// maxRetainedFailures caps the Failed servers a group keeps for diagnosis.
// Each holds its pod for failedRetentionSeconds, and a broken image fails much
// faster than that, so uncapped a group piles up dozens. The creation rate is
// bounded separately by DecideBackoff.
const maxRetainedFailures = 1

// selectFailedForPruning names the Failed servers beyond the retention cap.
//
// The newest generation's failures come first, and within one generation the
// oldest failure is kept: the first failure after a change says what broke.
// Generations compare numerically, since a group's only increases.
//
// Leaving servers are skipped, Condemned ones included: size() deletes those
// as NodeDraining in the same pass, and r.Delete does not mark the local
// object, so they would be deleted twice.
func selectFailedForPruning(views []ServerView, keep int) []string {
	failed := make([]ServerView, 0, len(views))
	for _, v := range views {
		if v.Phase == phase.Failed && !v.leaving() {
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
		// creationTimestamp has second resolution; replicas that fail together
		// are usually created in the same second.
		if !failed[i].FailedAt.Equal(failed[j].FailedAt) {
			return failed[i].FailedAt.Before(failed[j].FailedAt)
		}
		return failed[i].Name < failed[j].Name
	})

	names := make([]string, 0, len(failed)-keep)
	for _, v := range failed[keep:] {
		names = append(names, v.Name)
	}
	return names
}

// occupiedPods is the PodDisruptionBudget's minAvailable. Draining servers
// count too: their pods keep the occupied label until the last player is off.
func occupiedPods(views []ServerView) int32 {
	var n int32
	for _, v := range views {
		if v.Occupied() {
			n++
		}
	}
	return n
}

// GroupTotals is the aggregated status of a group. Replicas, ReadyReplicas and
// OnlinePlayers count every server, old spec included, because they report
// what is serving; FreeSlots is the scaler's input and counts only the current
// spec, or old servers' seats would stall a rolling update forever.
type GroupTotals struct {
	Replicas      int32
	ReadyReplicas int32
	OnlinePlayers int32
	FreeSlots     int32
}

// AggregateGroup sums the views up for the group status. An empty podHash
// (Network unusable) compares nothing, so every Ready server contributes:
// publishing the capacity that exists beats publishing zero.
func AggregateGroup(views []ServerView, podHash string) GroupTotals {
	var t GroupTotals
	for _, v := range views {
		t.Replicas++
		if v.Phase == phase.Ready {
			t.ReadyReplicas++
		}
		if !v.Stale {
			t.OnlinePlayers += v.Players
		}
		if v.Phase == phase.Ready && v.Registered && !v.JoinsClosed &&
			!staleSpec(v, podHash) && !v.Stale {
			t.FreeSlots += v.freeSeats()
		}
	}
	return t
}
