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
	// CreateOrdinals names the ordinals a persistent group is missing, lowest
	// first. Empty for an ephemeral group, which builds servers by count and
	// not by identity -- that is what Create carries instead.
	CreateOrdinals []int32
	// Delete names the servers to remove now.
	Delete []string
	// Retire names the stale servers to put into soft drain now. Never the
	// same server as Delete, and never in the same pass as one: retirement is
	// how a server leaves during a changeover, deletion is how it leaves for
	// lack of demand, and decideSize's chain of early returns takes one of
	// those branches or the other.
	//
	// Only half of that carries over to Condemn, which is worth saying here
	// because this is the sentence somebody reasoning about Condemn will
	// reach for. Retire and Condemn never name the same server either —
	// selectRetirement excludes a condemned one, and its own comment says why
	// — but they do occur in the same pass, on different servers. DecideSize
	// attaches Condemn alongside whichever branch decideSize returned,
	// precisely because a node drain answers to none of those branches. Same
	// server: no. Same pass: yes.
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
	// Condemn names the servers whose node is departing. They are deleted
	// unconditionally: not bounded by Surplus, not held back by MinReplicas,
	// and all of them in one pass. It is a separate field from Delete so the
	// two reasons never share a number — Delete is the scale-down nomination
	// and Surplus is what the ceiling asked for, and a node drain is about
	// neither.
	Condemn []string
	// DeleteReason is the event reason for Delete. The contract is one reason
	// for the whole batch, not one per name.
	DeleteReason string
	// Conflicts names the ordinals more than one server carries. Only the
	// persistent rule fills it -- an ephemeral group has no ordinals -- and it
	// is a report rather than an instruction: the operator cannot choose
	// between two worlds, so DecidePersistentSize refuses to act on such an
	// ordinal and this is what says so on the group.
	Conflicts []OrdinalConflict
}

// OrdinalConflict is one ordinal and every server carrying it.
type OrdinalConflict struct {
	Ordinal int32
	// Servers are the colliding names, sorted. At least two.
	Servers []string
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
	// Before the Slots == 0 credit below, and not merged into it: a server
	// whose pod is gone reads exactly like one that has never reported, and
	// only this flag tells them apart. Testing Stale here instead would be a
	// regression — a genuinely starting server is stale too, and crediting it
	// zero is the runaway this function exists to prevent.
	// SessionsGone reads true for one resync on a server that has genuinely
	// just started, whenever the informer's cache has not yet shown a pod the
	// API server already created: it is
	// `status.PodName != "" && (!podFound || podTerminal(pod))`, and the name
	// is stamped before the cache catches up. Such a server is credited zero
	// rather than its full maxPlayers, so the sum reads low, `wanted` reads
	// high, and the group over-creates for a pass or two.
	//
	// That is the safer of the two directions and it is chosen deliberately: a
	// group with a server too many costs money, a group with a server too few
	// costs joins. isOccupied has carried the same lag for the same reason
	// since before 4b; what 4b changed is that a scaling decision reads it too.
	if v.SessionsGone {
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
		// Condemned is skipped for the same reason as Retire: this server is
		// already leaving by another route, and naming it here would put it in
		// Delete and Condemn at once.
		if in.PendingDeletes[v.Name] || in.PendingRetires[v.Name] || v.Retire || v.Condemned {
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
// Retirement needs a ready server of the current generation that is staying to
// exist — selectRetirement discounts one nominated for deletion or condemned by
// its node, for the reasons given there. When every server is stale none does,
// so nothing may retire, so nothing drops out of the size count, so the
// spare-slot rule creates nothing — a deadlock that only an unconditional
// create breaks. This is the one create in the system that is not answering
// demand, and it is why the changeover costs at most one extra server.
//
// This function's own test is countsTowardSize rather than Ready, so the two
// agree about a condemned server — leaving() covers it, so it does not suppress
// a cold start — and differ about a Starting one, which suppresses the cold
// start without yet licensing a retirement. That gap is intended: the group
// waits the few seconds for it to become Ready rather than building another.
//
// A Failed server of the current generation does not suppress this. It used to
// — milestone 4b's stopgap against a broken image being recreated every five
// seconds — and the per-group backoff replaced it: the same loop is now bounded
// by a window that starts at seconds and grows only if the failures keep
// coming, rather than by a flat retention hour after any single failure.
// DecideSize still returns Create >= 1 whenever coldStart applies — neither
// this function nor DecideSize does the bounding. It happens on execution, at
// the gate in servergroup_controller.go (`if backoff.MayCreate`), which skips
// the create loop entirely while a window is open.
func coldStart(in ScalingInputs) bool {
	if in.PendingCreates > 0 {
		// A pending create is of the current generation in every pass but one:
		// createServer stamps group.Generation on it. The exception is a create
		// issued under generation N that is still outstanding when the pass
		// reads N+1 — then this declines a cold start on the strength of a
		// server that will arrive stale. It costs one pass and no more: either
		// the cache shows the server, which makes it a stale view like any
		// other, or the reservation reaches its TTL. Nothing is created or
		// deleted on the strength of it, which is why the code does not try to
		// tell the two apart.
		return false
	}
	var stale, current int
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] {
			continue
		}
		if v.Generation == in.Generation {
			if v.countsTowardSize() {
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
// narrower set: Ready, stale, not already retiring, not already nominated for
// deletion, and not condemned.
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
			//
			// Condemned is that same rule reached by the other route. A
			// condemned server is nominated for deletion in this very pass, by
			// Condemn rather than by Delete, so counting it as the replacement
			// a changeover waits for licenses a retirement against a server
			// that is itself being drained off a departing node. It is the
			// clause the nomination branch below carries, and it is here for
			// the same reason: the two branches ask the same question of a
			// server, and a reader who found the clause on one and not the
			// other could not reconstruct why.
			//
			// The two failure directions are not symmetric, and this picks the
			// safe one deliberately. With the clause, retirement declines more
			// often: a changeover that happens to coincide with a node drain
			// waits for a replacement that is not on the departing node, which
			// costs the changeover time and costs no player a connection.
			// Without it, a stale server is retired against a replacement that
			// is going away, and a retirement cannot be taken back — so the
			// group deregisters capacity it still needs. Slower beats
			// irreversible.
			if v.Phase == phase.Ready && !in.PendingDeletes[v.Name] && !v.Condemned {
				readyCurrent = true
			}
			continue
		}
		// Condemned is excluded for the reason deletable() excludes it, running
		// the other way. A condemned server is named by Condemn in this same
		// pass, and the caller reserves a delete for it; retiring it as well
		// would have expectRetired overwrite that reservation in the
		// name-keyed expectations map moments after it was made. It would also
		// spend a maxUnavailable slot on a server the node drain is taking
		// anyway — spec.retire holds that slot for the whole of the drain, so
		// the changeover would lose a slot it never got any work out of. A
		// server already leaving by one route may not also be spent from the
		// update budget.
		if v.Phase == phase.Ready && !in.PendingDeletes[v.Name] && !v.Condemned {
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
	// ServerGroup.UpdateMaxUnavailable (api/v1alpha1/servergroup_types.go),
	// the accessor that fills ScalingInputs.MaxUnavailable at the call site,
	// applies the same default. Both being right is correct rather than
	// redundant: this function is also reached by tests and by any future
	// caller that builds ScalingInputs itself, and the silence of the
	// failure is what makes one defence too few.
	budget := in.MaxUnavailable
	if budget < 1 {
		budget = 1
	}
	// At least one ready server of the current generation that is staying —
	// not one already nominated for deletion, and not one condemned by its
	// node — for every group and not only for fallback targets: a ServerGroup
	// cannot tell whether a ProxyGroup names it, and learning to would cost a
	// watch and a cache that can be wrong for a distinction that only permits
	// emptying a non-fallback group faster.
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

// DecideSize is the group's sizing rule, plus the one removal that is not a
// sizing decision at all.
//
// Condemnation rides alongside the size decision rather than inside it. The
// chain in decideSize is an ordered set of early returns — capacity, then the
// ceiling, then demand — and a node drain answers to none of those three: the
// node is leaving with or without the group's consent, so no branch may
// decline it and no branch may bound it. Attaching it to whichever decision
// comes back keeps that independence visible and keeps the chain unchanged.
func DecideSize(in ScalingInputs) SizeDecision {
	decision := decideSize(in)
	decision.Condemn = condemned(in)
	return decision
}

// condemned names every server whose node is departing and whose removal has
// not already been reserved. Nil when there are none, so a caller can tell
// "nothing to condemn" from "an empty list was built".
func condemned(in ScalingInputs) []string {
	var out []string
	for _, v := range in.Views {
		if v.Condemned && !in.PendingDeletes[v.Name] {
			out = append(out, v.Name)
		}
	}
	return out
}

// decideSize is the group's sizing rule.
//
// The order matters and is the design's, not an accident: capacity first, then
// the ceiling, then demand. A group that is short of capacity never also
// shrinks in the same pass.
func decideSize(in ScalingInputs) SizeDecision {
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
	// demanded is what this group would build with no changeover running: the
	// spare-slot rule and the floor, before the cold start is added on top. The
	// difference is load-bearing below — a cold start the ceiling refuses has
	// decided nothing, while an ordinary shortfall it refuses has.
	demanded := create
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
	// A refused cold start that is the only thing this pass wanted to build.
	// See the fall-through in the create block below: this pass has decided
	// nothing, so it must not return as though it had.
	coldOnly := coldBlocked && demanded < 1

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
		if !coldOnly {
			return SizeDecision{Wanted: wanted, Limited: limited, ColdStartBlocked: coldBlocked}
		}
		// The refused cold start, and nothing else asking for a server: no
		// create was granted and there is no surplus to shed, so this pass has
		// decided nothing at all. Returning here is what made the stall
		// permanent. A group at its ceiling with an idle stale server beside it
		// would refuse to delete the very server the changeover exists to
		// remove — the demand rule below sheds it happily when no changeover is
		// running — and would then tell the operator that the way out is a
		// higher maxReplicas. Every later pass read the same state and decided
		// the same thing. Falling through lets the demand rule free the room the
		// cold start was refused for, and the next pass starts the changeover.
		//
		// Falling through is safe on both counts. It cannot delete a server that
		// may be carrying players: it reaches the one removal the chain below
		// can make through the same two independent guards as every other, the
		// eligibility loop's Players != 0 || Stale and SelectDeletionCandidates'
		// mayHavePlayers. And it cannot delete something this pass should have
		// kept: demanded < 1 says neither the spare-slot rule nor the floor
		// asked for a server, so the group is not short; the eligibility loop
		// still tests feasibility against spareSlots one candidate at a time;
		// alive > MinReplicas still guards the floor; and only a *stale* server
		// can be taken, because a cold start means no current-generation server
		// counts toward the size, which is exactly when staleRemains is true and
		// the changeover filter holds the current generation out. The retirement
		// branch cannot fire here at all, for the same reason: it needs a Ready
		// server of the current generation, and a cold start is the statement
		// that there is none.
		//
		// The stall stays visible. With nothing to shed, the chain below decides
		// nothing either and the final return carries Limited and
		// ColdStartBlocked exactly as this branch used to.
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

	// Nothing was decided. Every path that reaches here with create == 0 has
	// wanted == 0 and neither flag set, so this is the empty decision it always
	// was. The one path that does not is the refused cold start that fell
	// through and found nothing to shed — and that is the stall the operator has
	// to be told about, so the flags travel out with it.
	return SizeDecision{Wanted: wanted, Limited: limited, ColdStartBlocked: coldBlocked}
}
