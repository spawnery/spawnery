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
	// Stale is true if the count cannot be trusted. Stale counts as occupied.
	Stale bool
	// Generation is the group generation this server was created from.
	Generation int64
	// CreatedAt is the creation timestamp of the Server object.
	CreatedAt time.Time
}

// Occupied reports whether the pod of this server must be treated as carrying
// players. It is the exact rule the Server controller labels pods with
// (podspec.LabelOccupied), and the group's PodDisruptionBudget is sized from
// it: a stale count counts as occupied, whatever the phase. Counting fewer
// pods than carry the label would hand the eviction API a disruption to spend
// on a pod that still has players on it.
func (v ServerView) Occupied() bool {
	return v.Stale || v.Players > 0
}

// mayHavePlayers is the narrower question the deletion candidate selection
// asks: could this particular server be carrying players right now?
//
// A count we cannot trust hides players only on a server the proxies actually
// route to. A Pending or Starting server has no agent stream yet, so its count
// is stale by construction; treating that as "occupied" would make every
// server that never came up permanently undeletable and leave a group unable
// to ever shrink. It stays conservative where it matters: any server that is
// registered with the proxies, or reports players at all, is off limits.
func (v ServerView) mayHavePlayers() bool {
	return v.Players > 0 || (v.Stale && v.tookPlayers())
}

// leaving reports whether the server is already on its way out, so the group
// must not count it as a candidate again.
func (v ServerView) leaving() bool {
	return v.Phase == phase.Draining || v.Phase == phase.Terminating
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

// tookPlayers reports whether the server was ever able to hold players. Only a
// Ready server is registered with the proxies.
func (v ServerView) tookPlayers() bool {
	return v.Phase == phase.Ready
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
