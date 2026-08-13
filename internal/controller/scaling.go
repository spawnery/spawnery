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
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// ScalingInputs is everything the sizing decision needs. Like the other
// decisions in this package it is a value type, so the rules are pure and
// table-tested without a cluster.
// Nothing here is the group's generation, and that is deliberate. Every edit to
// a ServerGroup spec raises it, so a scale-up rule that only credited servers of
// the current generation would order a full replacement set on the next
// five-second pass after any edit — a rolling update without maxUnavailable,
// without soft drain and without the guarantee of one ready server of the new
// generation. Those rules, and with them the generation, arrive in milestone 4b.
type ScalingInputs struct {
	// Views is what the cache shows of the group's servers.
	Views []ServerView
	// MinReplicas is the floor the group is held at.
	MinReplicas int32
	// MaxReplicas is the ceiling it may not pass.
	MaxReplicas int32
	// SpareSlots is the free player capacity the group keeps available.
	SpareSlots int32
	// MaxPlayers is the capacity of a single server of this group.
	MaxPlayers int32
	// Stabilization is how long a server must have been empty before it may
	// be removed for lack of demand.
	Stabilization time.Duration
	// PendingCreates is how many servers the reconciler has created and the
	// cache has not shown yet.
	PendingCreates int32
	// PendingDeletes are the servers whose removal it has already asked for
	// and the cache still shows.
	PendingDeletes map[string]bool
}

// SizeDecision is what the group does about its size this pass.
type SizeDecision struct {
	// Create is how many servers to create now.
	Create int32
	// Delete names the servers to remove now.
	Delete []string
	// Wanted is how many servers the spare-slot rule asked for, before the
	// ceiling. Wanted > Create is the definition of Limited.
	Wanted int32
	// Surplus is how many servers the ceiling asked to have removed, whether
	// or not that many could be nominated.
	Surplus int32
	// Limited is true while maxReplicas is holding capacity back.
	Limited bool
}

// provisionalCapacity is one server's contribution to the figure the scale-up
// rule reads. It is deliberately not AggregateGroup's FreeSlots.
//
// A server created now is not Ready for tens of seconds and contributes nothing
// to FreeSlots. At a five-second resync a scaler reading FreeSlots would see the
// same shortfall six to twelve times and order the same replacement each time,
// until maxReplicas stopped it. That is not an edge case — it is what every
// scale-up would do.
//
// So capacity that has been ordered counts before it arrives. Slots == 0 is what
// separates a server still starting up, which has never reported, from one whose
// agent went quiet, which has: the first is credited in full, the second not at
// all, because unknown counts as occupied everywhere in this repository.
//
// status.freeSlots keeps AggregateGroup's meaning — Ready servers of the current
// generation — because that is what its CRD field documents and what the rolling
// update of milestone 4b needs. Two numbers, two purposes; they must not be
// unified.
func provisionalCapacity(v ServerView, maxPlayers int32) int32 {
	if !v.countsTowardSize() {
		return 0
	}
	if v.Slots == 0 {
		return maxPlayers
	}
	if v.Stale {
		return 0
	}
	if free := v.Slots - v.Players; free > 0 {
		return free
	}
	return 0
}

// deletable is the candidate pool: what the cache shows, minus the servers
// whose removal this reconciler has already asked for. Leaving them in would
// let one pass nominate the same server the previous pass already deleted, and
// count it twice against the surplus.
func deletable(in ScalingInputs) []ServerView {
	if len(in.PendingDeletes) == 0 {
		return in.Views
	}
	out := make([]ServerView, 0, len(in.Views))
	for _, v := range in.Views {
		if !in.PendingDeletes[v.Name] {
			out = append(out, v)
		}
	}
	return out
}

// readyContribution is the free capacity one server actually has right now:
// arrived, unlike provisionalCapacity, because a removal must be judged against
// capacity that exists rather than capacity that is on order.
//
// It is AggregateGroup's formula without the generation filter, and not a call
// to AggregateGroup, for the reason ScalingInputs gives: filtering by generation
// would make every scale-down impossible from the moment anyone edits the
// group's spec, because the whole group would read as stale and contribute
// nothing.
func readyContribution(v ServerView) int32 {
	if v.Phase != phase.Ready || v.Stale {
		return 0
	}
	if free := v.Slots - v.Players; free > 0 {
		return free
	}
	return 0
}

// readyFree is the group's arrived free capacity, the total the feasibility
// test subtracts one candidate's share from.
func readyFree(views []ServerView) int32 {
	var free int32
	for _, v := range views {
		free += readyContribution(v)
	}
	return free
}

// DecideSize is the group's sizing rule.
//
// The order matters and is the design's, not an accident: capacity first, then
// the ceiling, then demand. A group that is short of capacity never also
// shrinks in the same pass.
func DecideSize(in ScalingInputs) SizeDecision {
	alive := in.PendingCreates
	provisional := in.PendingCreates * in.MaxPlayers
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] {
			continue
		}
		if v.countsTowardSize() {
			alive++
		}
		provisional += provisionalCapacity(v, in.MaxPlayers)
	}

	var wanted int32
	if in.MaxPlayers > 0 && provisional < in.SpareSlots {
		gap := in.SpareSlots - provisional
		wanted = (gap + in.MaxPlayers - 1) / in.MaxPlayers
	}

	create := wanted
	if floor := in.MinReplicas - alive; floor > create {
		create = floor
	}
	room := in.MaxReplicas - alive
	if room < 0 {
		room = 0
	}
	granted := create
	if granted > room {
		granted = room
	}
	limited := wanted > granted

	if create > 0 {
		if granted > 0 {
			return SizeDecision{Create: granted, Wanted: wanted, Limited: limited}
		}
		// No room to grow. Being short of capacity is not a reprieve from the
		// ceiling: a lowered maxReplicas is an instruction, and a group that
		// cannot answer its shortfall must still carry it out — while saying
		// that it is short, which is why Wanted and Limited travel with the
		// removal. What the shortfall does forbid is the removal below, which
		// runs for lack of demand: that one would take away capacity the group
		// has just said it needs.
		if surplus := alive - in.MaxReplicas; surplus > 0 {
			return SizeDecision{
				Wanted:  wanted,
				Limited: limited,
				Surplus: surplus,
				Delete:  SelectDeletionCandidates(deletable(in), int(surplus)),
			}
		}
		return SizeDecision{Wanted: wanted, Limited: limited}
	}

	if surplus := alive - in.MaxReplicas; surplus > 0 {
		return SizeDecision{
			Surplus: surplus,
			Delete:  SelectDeletionCandidates(deletable(in), int(surplus)),
		}
	}

	// Demand. Never in the same pass as a create — reaching here means the
	// group is not short of capacity, but an outstanding create says capacity
	// is on its way, and removing a server against that is a decision made on
	// two different readings of the same moment.
	if in.PendingCreates == 0 && alive > in.MinReplicas {
		pool := deletable(in)
		free := readyFree(pool)

		eligible := make([]ServerView, 0, len(pool))
		for _, v := range pool {
			// EmptyFor decides nothing on its own: a server that was never
			// empty carries zero here too, and Stabilization may be zero.
			if v.Players != 0 || v.Stale || v.EmptyFor < in.Stabilization {
				continue
			}
			// Each candidate on its own, so an infeasible head of the list does
			// not hide a feasible tail.
			if free-readyContribution(v) < in.SpareSlots {
				continue
			}
			eligible = append(eligible, v)
		}
		// One per pass: every removal costs a drain cycle, and the five-second
		// resync converges quickly enough.
		if names := SelectDeletionCandidates(eligible, 1); len(names) > 0 {
			return SizeDecision{Delete: names}
		}
	}

	return SizeDecision{}
}
