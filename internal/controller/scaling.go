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

// ScalingInputs is everything the sizing decision needs. Like the other
// decisions in this package it is a value type, so the rules are pure and
// table-tested without a cluster.
// The group's generation is here, and it is confined to one job. It selects
// which stale server retires and whether the changeover has begun; it never
// enters provisionalCapacity or readyFree. A scale-up rule that credited only
// servers of the current generation would find, the instant any field of the
// group's spec changed, that nothing running counts — and would order a full
// replacement set up to maxReplicas on the next five-second pass. Keeping the
// arithmetic generation-blind is what makes that impossible rather than merely
// braked.
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
	// Generation is the group's current metadata.generation. A view whose
	// Generation differs is stale.
	//
	// It decides only *which* server retires, never how many get built: the
	// capacity arithmetic below stays generation-blind, exactly as milestone
	// 4a left it. See the type comment above.
	Generation int64
	// MaxUnavailable is spec.update.maxUnavailable: how many servers this
	// update may have unavailable at once.
	MaxUnavailable int32
	// PendingRetires are the servers this reconciler has asked to retire and
	// the cache has not shown yet.
	PendingRetires map[string]bool
}

// SizeDecision is what the group does about its size this pass.
type SizeDecision struct {
	// Create is how many servers to create now.
	Create int32
	// Delete names the servers to remove now.
	Delete []string
	// Retire names the stale servers to put into soft drain now. Never the
	// same server as Delete, and never in the same pass: retirement is how a
	// server leaves during a changeover, deletion is how it leaves for lack
	// of demand.
	Retire []string
	// Wanted is how many servers the spare-slot rule asked for, before the
	// ceiling. Limited is true either because Wanted exceeds Create — the
	// ordinary shortfall — or because the ceiling refused the one server a
	// generation changeover needs to begin, in which case Wanted stays 0 and
	// ColdStartBlocked, not Wanted, is what says why.
	Wanted int32
	// Surplus is how many servers the ceiling asked to have removed, whether
	// or not that many could be nominated.
	Surplus int32
	// Limited is true while maxReplicas is holding capacity back.
	Limited bool
	// ColdStartBlocked is true when Limited is set because the ceiling
	// refused a changeover's cold start rather than because of an ordinary
	// capacity shortfall. Wanted is 0 in both that case and the unlimited
	// "nothing is short" case, so it cannot distinguish them on its own —
	// this field is the explicit signal the caller that builds the
	// operator-facing ScalingLimited message needs to tell them apart.
	ColdStartBlocked bool
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
//
// Reserved retirements are held out for a second, sharper reason: expectations
// is keyed by name, so a delete reservation recorded for a server that already
// has a retire reservation overwrites it, and the retirement stops counting
// against maxUnavailable while the patch is still in flight — which lets the
// next pass spend the budget a second time. That is reachable rather than
// theoretical. A pass that reserves a retirement returns there, so the two
// never meet within one pass; but on the pass after, the reservation makes
// selectRetirement decline while the server still shows Ready and empty in the
// cache, and the demand rule below is happy to nominate exactly it. Excluding
// it here is also the plainly right answer on its own terms: a server this
// group has just asked to retire is already on its way out, and deleting it
// instead is the harder of the two removals.
//
// spec.retire is tested as well as the reservation, and the reservation alone
// is not enough. observe() satisfies a retire expectation the moment the cache
// shows spec.retire, but the phase is written by the Server controller in a
// second write that necessarily lands later — so there is an ordinary pass, not
// a race, in which the view reads Retire: true, Phase: Ready and holds no
// reservation at all. Filtering only the reservation puts that server straight
// back into the demand pool, where an empty stale server is the preferred
// candidate; the soft drain the group just asked for becomes a hard delete, for
// exactly the class of server the nomination prefers. Retirement is how a
// server leaves during a changeover: once it has been asked, no other rule gets
// to take it.
//
// There is no fast path for the reservation-free case any more, because
// spec.retire lives on the views rather than in a map and has to be looked at
// either way. The pool is one allocation per pass over a handful of servers.
func deletable(in ScalingInputs) []ServerView {
	out := make([]ServerView, 0, len(in.Views))
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] || in.PendingRetires[v.Name] || v.Retire {
			continue
		}
		out = append(out, v)
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

// coldStart reports whether the group must create the first server of its
// current generation before anything can retire.
//
// Retirement needs a ready server of the current generation to exist. When
// every server is stale none does, so nothing may retire, so nothing drops out
// of the size count, so the spare-slot rule creates nothing — a deadlock that
// only an unconditional create breaks. This is the one create in the system
// that is not answering demand, and it is why the changeover costs at most one
// extra server.
//
// It is suppressed while a Failed server of the *current* generation is being
// retained. Without that, a broken new image fails, drops out of
// countsTowardSize, and is re-created on the next five-second pass forever. The
// retention window (an hour by default) becomes the interval: one attempt, the
// old generation serving undisturbed, one corpse to diagnose from. A stale
// failure does not suppress it — the old generation's corpse says nothing about
// whether the new image works.
//
// This is deliberately not backoff. The floor rule has the same loop and keeps
// it; giving it a real per-group backoff with a Degraded condition is its own
// milestone.
func coldStart(in ScalingInputs) bool {
	if in.PendingCreates > 0 {
		// A pending create is always of the current generation, by
		// construction: createServer stamps group.Generation on it.
		return false
	}
	var stale, current int
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] {
			continue
		}
		if v.Generation == in.Generation {
			// A retained failure counts here even though countsTowardSize
			// excludes it, and that is the suppression.
			if v.Phase == phase.Failed || v.countsTowardSize() {
				current++
			}
			continue
		}
		if v.countsTowardSize() {
			stale++
		}
	}
	return stale > 0 && current == 0
}

// selectRetirement nominates one stale server for soft drain, or nothing.
//
// It cannot reuse SelectDeletionCandidates: that function refuses any server
// that may be carrying players, and those are precisely the ones a changeover
// has to retire. This is not a loosening of that rule — a retiring server is
// still never nominated for deletion — it is a different question asked of a
// narrower set: Ready, stale, not already retiring.
//
// Empty servers first, because retiring one costs nobody anything, then the
// oldest, so the longest-lived sessions are disturbed last. Ties by name, so
// two passes over the same state agree.
//
// One per pass. Every retirement is a replacement's worth of work, and the
// five-second resync converges quickly enough.
func selectRetirement(in ScalingInputs) string {
	var (
		readyCurrent bool
		unavailable  int32
		stale        []ServerView
	)
	for _, v := range in.Views {
		// spec.retire is the single budget signal. It survives the
		// escalation to Draining that maxStaleSeconds can force, so a
		// forced drain keeps holding the slot it started in — while a drain
		// that a scale-down or a deletion began never had it.
		if v.Retire || in.PendingRetires[v.Name] {
			unavailable++
			continue
		}
		if v.Generation == in.Generation {
			// A server already nominated for deletion is not a replacement.
			// The ceiling branch runs before this one, applies no changeover
			// filter, and sorts never-took-players first then youngest — which
			// is the cold-start replacement exactly — so a lowered maxReplicas
			// can take the new server while the cache still shows it Ready.
			// Retiring a stale server on the strength of that is deregistering
			// capacity against a replacement on its way out, and a retirement
			// is not retractable by the next pass. coldStart and staleRemains
			// both skip PendingDeletes before counting anything; this is the
			// same rule.
			if v.Phase == phase.Ready && !in.PendingDeletes[v.Name] {
				readyCurrent = true
			}
			continue
		}
		if v.Phase == phase.Ready && !in.PendingDeletes[v.Name] {
			stale = append(stale, v)
		}
	}
	// A zero budget can only mean "unset", so it is floored at one rather
	// than read as "never retire".
	//
	// The CRD gives spec.update.maxUnavailable a default of 1 and a minimum
	// of 1, so no group can legitimately present 0 — but spec.update itself
	// is optional and no CEL rule requires it, and a nil parent means the
	// child's default never applies. A group whose operator never wrote an
	// update policy therefore arrives here with 0, and without this floor it
	// would decline every retirement forever: no error, no condition, no
	// event, just a changeover that never starts. Because the CRD forbids a
	// real 0, defaulting inside a pure function is unambiguous here in a way
	// it usually is not.
	//
	// The accessor at the call site applies the same default. Both being
	// right is correct rather than redundant: this function is also reached
	// by tests and by any future caller that builds ScalingInputs itself, and
	// the silence of the failure is what makes one defence too few.
	budget := in.MaxUnavailable
	if budget < 1 {
		budget = 1
	}
	// At least one ready server of the current generation, for every group
	// and not only for fallback targets: a ServerGroup cannot tell whether a
	// ProxyGroup names it, and learning to would cost a watch and a cache
	// that can be wrong for a distinction that only permits emptying a
	// non-fallback group faster.
	if !readyCurrent || unavailable >= budget || len(stale) == 0 {
		return ""
	}
	sort.SliceStable(stale, func(i, j int) bool {
		// Empty means empty and known to be: unknown counts as occupied
		// everywhere in this repository, and a stale count on a server that
		// last reported zero may be hiding players. Without !Stale this
		// comparator *prefers* exactly those servers, doing the opposite of
		// the rule it justifies itself by — retiring an empty server costs
		// nobody anything, but retiring one that only looks empty costs the
		// sessions the ordering exists to disturb last.
		if ei, ej := stale[i].Players == 0 && !stale[i].Stale, stale[j].Players == 0 && !stale[j].Stale; ei != ej {
			return ei
		}
		if !stale[i].CreatedAt.Equal(stale[j].CreatedAt) {
			return stale[i].CreatedAt.Before(stale[j].CreatedAt)
		}
		return stale[i].Name < stale[j].Name
	})
	return stale[0].Name
}

// staleRemains reports whether the changeover still has stale capacity to shed.
//
// It is the same set coldStart counts as stale: a different generation, no
// deletion already reserved, and still counting toward the group's size. A
// stale server that is Failed, Draining or Terminating is not something the
// changeover is still racing to remove — it is gone or going — and counting it
// would suspend ordinary scale-downs of current-generation servers for the
// whole failed-retention window, an hour by default.
func staleRemains(in ScalingInputs) bool {
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] {
			continue
		}
		if v.Generation != in.Generation && v.countsTowardSize() {
			return true
		}
	}
	return false
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
	cold := coldStart(in)
	if cold && create < 1 {
		create = 1
	}
	room := in.MaxReplicas - alive
	if room < 0 {
		room = 0
	}
	granted := create
	if granted > room {
		granted = room
	}
	// A cold start the ceiling refuses is a changeover that cannot begin.
	// Stalling is correct — a lowered maxReplicas is an instruction — but it
	// must be visible, and ScalingLimited is the condition that says exactly
	// "the ceiling is holding capacity back". coldBlocked is carried apart
	// from limited because Wanted is 0 in this case exactly as it is when
	// nothing is short — it is the only field that lets the message the
	// operator sees name which of the two is actually happening.
	coldBlocked := cold && granted < 1
	limited := wanted > granted || coldBlocked

	if create > 0 {
		if granted > 0 {
			return SizeDecision{Create: granted, Wanted: wanted, Limited: limited, ColdStartBlocked: coldBlocked}
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
				Wanted:           wanted,
				Limited:          limited,
				ColdStartBlocked: coldBlocked,
				Surplus:          surplus,
				Delete:           SelectDeletionCandidates(deletable(in), int(surplus)),
			}
		}
		return SizeDecision{Wanted: wanted, Limited: limited, ColdStartBlocked: coldBlocked}
	}

	if surplus := alive - in.MaxReplicas; surplus > 0 {
		return SizeDecision{
			Surplus: surplus,
			Delete:  SelectDeletionCandidates(deletable(in), int(surplus)),
		}
	}

	// Retirement, after the ceiling and before demand. The ceiling is an
	// instruction and outranks a changeover; demand does not, because a pass
	// that retires has already decided how this group loses a server and a
	// second removal would be a second decision on the same reading.
	if name := selectRetirement(in); name != "" {
		return SizeDecision{Retire: []string{name}}
	}

	// Demand. Never in the same pass as a create — reaching here means the
	// group is not short of capacity, but an outstanding create says capacity
	// is on its way, and removing a server against that is a decision made on
	// two different readings of the same moment.
	if in.PendingCreates == 0 && alive > in.MinReplicas {
		pool := deletable(in)
		free := readyFree(pool)
		// While a changeover is in progress the group sheds stale capacity
		// first: a current-generation server is not a demand candidate while
		// any stale server remains.
		//
		// The retirement branch above closes this case only while the budget
		// is free, because a pass that retires returns there. When
		// maxUnavailable is already spent — the long-lived case, since a soft
		// drain on an occupied server is exactly what holds the budget — that
		// branch declines and the pass falls through to here with stale
		// servers still standing. Without this filter the demand rule then
		// deletes the cold start's own replacement, and it *prefers* it:
		// SelectDeletionCandidates sorts youngest-first among servers that
		// took players, so the fresh server loses to the stale one beside it
		// on age alone. With no current-generation server left, coldStart
		// fires on the next pass and builds another, for as long as the budget
		// stays spent — up to the whole of maxStaleSeconds. Skipping the
		// current generation closes that loop, because the last
		// current-generation server is never a candidate, and it fixes the
		// backwards preference in the same stroke.
		//
		// This is the one place the generation enters a decision other than
		// "which server retires", and the reason it is allowed here while
		// being forbidden in provisionalCapacity, readyContribution and
		// readyFree is the direction the error can run in. A generation filter
		// in the capacity arithmetic makes running servers stop counting the
		// instant any field of the spec changes, so the group orders a full
		// replacement set: runaway *creates*, up to maxReplicas, which is the
		// failure that disconnects players. A generation filter in deletion
		// candidacy can only make *fewer* servers deletable. It can hold a
		// removal back; it cannot create anything. The numbers above stay
		// generation-blind exactly as 4a left them.
		changeover := staleRemains(in)

		eligible := make([]ServerView, 0, len(pool))
		for _, v := range pool {
			// The changeover rule: stale capacity goes first. See above.
			if changeover && v.Generation == in.Generation {
				continue
			}
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
