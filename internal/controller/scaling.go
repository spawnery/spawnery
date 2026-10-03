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

// ScalingInputs is everything the sizing decision needs.
//
// PodHash only selects which stale server retires and whether the changeover
// has begun; it never enters provisionalCapacity or readyFree. Capacity that
// counted only current-spec servers would, on any spec edit, find nothing
// running and order a full replacement set up to maxReplicas.
type ScalingInputs struct {
	Views []ServerView
	// MinReplicas is read through floor(), never directly: a boost adds to it.
	MinReplicas int32
	// Boost is what live ScaleBoost objects add to the floor, and to nothing
	// else. maxReplicas still applies after it.
	Boost       int32
	MaxReplicas int32
	// SpareSlots is the free player capacity the group keeps available.
	SpareSlots int32
	// MaxPlayers is the capacity of a single server of this group.
	MaxPlayers int32
	// PlayableSlots is spec.playableSlots, 0 when unset.
	PlayableSlots int32
	// Stabilization is how long a server must have been empty before it may
	// be removed for lack of demand.
	Stabilization time.Duration
	// PendingCreates is how many servers the reconciler has created and the
	// cache has not shown yet.
	PendingCreates int32
	// PendingDeletes are the servers whose removal it has already asked for
	// and the cache still shows.
	PendingDeletes map[string]bool
	// PodHash is podspec.DesiredServerHash for the group as it stands now. A
	// digest rather than metadata.generation, because a generation moves on
	// every spec field, minReplicas included.
	PodHash string
	// ChangeoverRefused withholds the cold start: the network's changeover
	// budget is spent by other groups, or an earlier stage is still changing
	// over. Demand is still answered.
	ChangeoverRefused bool
	// MaxUnavailable is spec.update.maxUnavailable: how many servers this
	// update may have unavailable at once.
	MaxUnavailable int32
	// PendingRetires are the servers this reconciler has asked to retire and
	// the cache has not shown yet.
	PendingRetires map[string]bool
	// MinAvailable is spec.update.minAvailable: how many servers stay
	// joinable while stale ones remain. 0 is no floor.
	MinAvailable int32
	// WhenEmpty is spec.update.strategy WhenEmpty: only stale servers known
	// to be empty are retired.
	WhenEmpty bool
}

// floor is MinReplicas plus live boosts. Both the create rule and the guard
// against shedding below the floor must read the same number.
func (in ScalingInputs) floor() int32 { return in.MinReplicas + in.Boost }

// capacity is what one server brings before it has reported anything.
func (in ScalingInputs) capacity() int32 {
	if in.PlayableSlots > 0 {
		return in.PlayableSlots
	}
	return in.MaxPlayers
}

// SizeDecision is what the group does about its size this pass.
type SizeDecision struct {
	Create int32
	// CreateOrdinals names the ordinals a persistent group is missing, lowest
	// first. Empty for an ephemeral group.
	CreateOrdinals []int32
	Delete         []string
	// Retire names the stale servers to put into soft drain now. Never in the
	// same pass as Delete. Condemn may come in the same pass, but never names
	// the same server.
	Retire []string
	// Wanted is how many servers the spare-slot rule asked for, before the
	// ceiling.
	Wanted int32
	// Surplus is how many servers the ceiling asked to have removed, whether
	// or not that many could be nominated.
	Surplus int32
	// Limited is true while maxReplicas is holding capacity back.
	Limited bool
	// ColdStartBlocked: Limited because the ceiling refused a changeover's cold
	// start. Wanted is 0 then, as it is when nothing is short.
	ColdStartBlocked bool
	// ChangeoverWaiting is a cold start or a stale takedown withheld by the
	// network's changeover budget or an earlier stage.
	ChangeoverWaiting bool
	// FloorHeld is true when a changeover retirement was declined only
	// because it would leave fewer than MinAvailable joinable servers.
	FloorHeld bool
	// Joinable is the count FloorHeld was judged on.
	Joinable int32
	// FloorBlocked is true when FloorHeld and the extra server the floor
	// needs cannot be built because the group is at maxReplicas.
	FloorBlocked bool
	// Condemn names the servers whose node is departing. They are deleted
	// unconditionally: not bounded by Surplus, not held back by MinReplicas.
	Condemn []string
	// DeleteReason is the event reason for the whole Delete batch.
	DeleteReason string
	// Conflicts names the ordinals more than one server carries; the operator
	// cannot choose between two worlds, so DecidePersistentSize refuses to act
	// on them.
	Conflicts []OrdinalConflict
}

// OrdinalConflict is one ordinal and every server carrying it.
type OrdinalConflict struct {
	Ordinal int32
	// Servers are the colliding names, sorted. At least two.
	Servers []string
}

// provisionalCapacity is one server's contribution to the figure the scale-up
// rule reads, deliberately not AggregateGroup's FreeSlots: a new server is not
// Ready for tens of seconds, and a scaler reading FreeSlots would order the
// same replacement on every resync until maxReplicas stopped it. So ordered
// capacity counts before it arrives. Slots == 0 separates a server still
// starting, credited in full, from one whose agent went quiet, credited zero.
func provisionalCapacity(v ServerView, capacity int32) int32 {
	if !v.countsTowardSize() {
		return 0
	}
	// The door before the phase: a door-closed server that lost its probe would
	// otherwise get the Slots == 0 credit below, and a server nobody can join is
	// not capacity on its way.
	if v.JoinsClosed {
		return 0
	}
	// Fresh counts tell a deregistered server apart from one the operator has not
	// heard from since its own restart, which the credit below is for.
	if v.WasRegistered && !v.Registered && !v.Stale {
		return 0
	}
	// A server whose pod is gone reads like one that never reported, and Stale
	// cannot tell them apart: a starting server is stale too. SessionsGone also
	// reads true for a resync while the informer lags a fresh pod, which errs
	// toward one server too many rather than one too few.
	if v.SessionsGone {
		return 0
	}
	if v.Slots == 0 {
		return capacity
	}
	if v.Stale {
		return 0
	}
	return v.freeSeats()
}

// deletable is the candidate pool: what the cache shows, minus the servers
// whose removal has already been asked for, and minus retiring ones. A retiring
// server is tested by spec.retire as well as by reservation: observe()
// satisfies the reservation as soon as spec.retire shows, while the phase still
// reads Ready, and the demand rule would otherwise turn the soft drain into a
// hard delete. expectations is keyed by name, so a delete reservation would
// also overwrite the retire one and free its maxUnavailable slot.
func deletable(in ScalingInputs) []ServerView {
	out := make([]ServerView, 0, len(in.Views))
	for _, v := range in.Views {
		// A condemned server would land in Delete and Condemn at once.
		if in.PendingDeletes[v.Name] || in.PendingRetires[v.Name] || v.Retire || v.Condemned || v.Hold {
			continue
		}
		out = append(out, v)
	}
	return out
}

// readyContribution is the free capacity one server has right now; a removal
// is judged against arrived capacity, not ordered. It is AggregateGroup's
// formula without the spec filter, which would make every scale-down
// impossible from the moment the spec is edited.
func readyContribution(v ServerView) int32 {
	if v.Phase != phase.Ready || v.Stale || !v.Registered || v.JoinsClosed {
		return 0
	}
	return v.freeSeats()
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
// current spec before anything can retire. Retirement needs a staying Ready
// current server; when every server is stale, nothing retires, nothing leaves
// the size count and the spare-slot rule creates nothing. This unconditional
// create breaks that deadlock and is why a changeover costs at most one extra
// server. A Starting current server suppresses it; a Failed one does not, the
// per-group backoff gates the create instead.
func coldStart(in ScalingInputs) bool {
	if in.PendingCreates > 0 {
		// A create issued under the previous spec can delay the cold start by one
		// pass, until the cache shows it or its reservation expires.
		return false
	}
	var stale, current int
	for _, v := range in.Views {
		if v.Hold {
			continue
		}
		if in.PendingDeletes[v.Name] {
			continue
		}
		if !staleSpec(v, in.PodHash) {
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
// It cannot reuse SelectDeletionCandidates, which refuses any server that may
// carry players; those are exactly the ones a changeover has to retire. Empty
// servers first, then the oldest; one per pass. Under WhenEmpty only servers
// known to be empty qualify.
//
// With a floor, the first candidate whose retirement keeps MinAvailable
// joinable servers goes; the bool reports a decline only the floor caused,
// which licenses decideSize's extra server.
func selectRetirement(in ScalingInputs) (string, bool) {
	var (
		readyCurrent bool
		unavailable  int32
		stale        []ServerView
	)
	for _, v := range in.Views {
		// spec.retire survives the escalation to Draining that maxStaleSeconds
		// forces, so a forced drain keeps its budget slot.
		if v.Retire || in.PendingRetires[v.Name] {
			unavailable++
			continue
		}
		if !staleSpec(v, in.PodHash) {
			// A server nominated for deletion or condemned is not a replacement: a
			// retirement cannot be taken back, so waiting beats retiring against a
			// server that is going away.
			if v.Phase == phase.Ready && !in.PendingDeletes[v.Name] && !v.Condemned {
				readyCurrent = true
			}
			continue
		}
		// A condemned server already has a delete reservation, which expectRetired
		// would overwrite, and the drain takes it anyway.
		if v.Phase == phase.Ready && !in.PendingDeletes[v.Name] && !v.Condemned && !v.Hold {
			if in.WhenEmpty && (v.Players != 0 || v.Stale) {
				continue
			}
			stale = append(stale, v)
		}
	}
	// The CRD's default of 1 never applies when spec.update itself is absent,
	// and 0 would silently block every retirement.
	budget := in.MaxUnavailable
	if budget < 1 {
		budget = 1
	}
	// Required for every group, not only fallback targets: a ServerGroup cannot
	// tell whether a ProxyGroup names it.
	if !readyCurrent || unavailable >= budget || len(stale) == 0 {
		return "", false
	}
	sort.SliceStable(stale, func(i, j int) bool {
		// A stale count that last read zero may be hiding players.
		if ei, ej := stale[i].Players == 0 && !stale[i].Stale, stale[j].Players == 0 && !stale[j].Stale; ei != ej {
			return ei
		}
		if !stale[i].CreatedAt.Equal(stale[j].CreatedAt) {
			return stale[i].CreatedAt.Before(stale[j].CreatedAt)
		}
		return stale[i].Name < stale[j].Name
	})
	if in.MinAvailable < 1 {
		return stale[0].Name, false
	}
	open := joinableCount(in)
	for _, v := range stale {
		if !joinable(in, v) || open-1 >= in.MinAvailable {
			return v.Name, false
		}
	}
	return "", true
}

// joinable reports whether a player can be sent to this server now: Ready,
// in the proxies' tables, door open, and not already on its way out by any
// route. Either generation.
func joinable(in ScalingInputs, v ServerView) bool {
	return v.Phase == phase.Ready && v.Registered && !v.JoinsClosed &&
		!v.Retire && !in.PendingRetires[v.Name] && !in.PendingDeletes[v.Name] && !v.Condemned
}

func joinableCount(in ScalingInputs) int32 {
	var n int32
	for _, v := range in.Views {
		if joinable(in, v) {
			n++
		}
	}
	return n
}

// currentStarting reports whether a server of the current generation is on
// its way up, which is the extra server the floor already asked for.
func currentStarting(in ScalingInputs) bool {
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] || staleSpec(v, in.PodHash) {
			continue
		}
		if v.Phase == phase.Pending || v.Phase == phase.Starting {
			return true
		}
	}
	return false
}

// staleRemains reports whether the changeover still has stale capacity to
// shed: the same set coldStart counts. Failed or leaving stale servers are
// already going, and counting them would suspend scale-downs for the whole
// failed retention.
func staleRemains(in ScalingInputs) bool {
	for _, v := range in.Views {
		if v.Hold {
			continue
		}
		if in.PendingDeletes[v.Name] {
			continue
		}
		if staleSpec(v, in.PodHash) && v.countsTowardSize() {
			return true
		}
	}
	return false
}

// DecideSize is the group's sizing rule plus condemnation, which rides
// alongside: a node drain answers to none of decideSize's branches, so none
// may decline or bound it.
func DecideSize(in ScalingInputs) SizeDecision {
	decision := decideSize(in)
	decision.Condemn = condemned(in)
	return decision
}

// condemned names every server whose node is departing and whose removal has
// not already been reserved. Nil when there are none.
func condemned(in ScalingInputs) []string {
	var out []string
	for _, v := range in.Views {
		if v.Condemned && !in.PendingDeletes[v.Name] {
			out = append(out, v.Name)
		}
	}
	return out
}

// decideSize is the group's sizing rule. The order is capacity, then the
// ceiling, then demand, so a group short of capacity never also shrinks in the
// same pass.
func decideSize(in ScalingInputs) SizeDecision {
	alive := in.PendingCreates
	capacity := in.capacity()
	provisional := in.PendingCreates * capacity
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] {
			continue
		}
		if v.countsTowardSize() {
			alive++
		}
		provisional += provisionalCapacity(v, capacity)
	}

	var wanted int32
	if capacity > 0 && provisional < in.SpareSlots {
		gap := in.SpareSlots - provisional
		wanted = (gap + capacity - 1) / capacity
	}

	create := wanted
	if floor := in.floor() - alive; floor > create {
		create = floor
	}
	// demanded excludes the cold start: a refused cold start has decided
	// nothing, a refused shortfall has.
	demanded := create
	cold := coldStart(in)
	waiting := cold && in.ChangeoverRefused
	if waiting {
		cold = false
	}
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
	// A cold start the ceiling refuses stalls the changeover, and ScalingLimited
	// has to say so.
	coldBlocked := cold && granted < 1
	limited := wanted > granted || coldBlocked
	coldOnly := coldBlocked && demanded < 1

	if create > 0 {
		if granted > 0 {
			return SizeDecision{Create: granted, Wanted: wanted, Limited: limited, ColdStartBlocked: coldBlocked, ChangeoverWaiting: waiting}
		}
		// No room to grow, but a lowered maxReplicas must still be carried out. The
		// demand removal below stays forbidden: the group just said it is short.
		if surplus := alive - in.MaxReplicas; surplus > 0 {
			return SizeDecision{
				Wanted:            wanted,
				Limited:           limited,
				ColdStartBlocked:  coldBlocked,
				Surplus:           surplus,
				Delete:            SelectDeletionCandidates(deletable(in), int(surplus)),
				ChangeoverWaiting: waiting,
			}
		}
		if !coldOnly {
			return SizeDecision{Wanted: wanted, Limited: limited, ColdStartBlocked: coldBlocked, ChangeoverWaiting: waiting}
		}
		// Only the refused cold start: fall through so the demand rule can free the
		// room it needs, or the stall is permanent. It can only take an empty stale
		// server; the changeover filter holds the current spec out.
	}

	if surplus := alive - in.MaxReplicas; surplus > 0 {
		return SizeDecision{
			Surplus:           surplus,
			Delete:            SelectDeletionCandidates(deletable(in), int(surplus)),
			ChangeoverWaiting: waiting,
		}
	}

	// Retirement, after the ceiling and before demand: a pass that retires has
	// already decided how this group loses a server.
	name, floorHeld := selectRetirement(in)
	if name != "" {
		return SizeDecision{Retire: []string{name}, ChangeoverWaiting: waiting}
	}
	var floorOpen int32
	var floorBlocked bool
	if floorHeld {
		floorOpen = joinableCount(in)
		if in.PendingCreates == 0 && !currentStarting(in) {
			if alive < in.MaxReplicas {
				return SizeDecision{Create: 1, FloorHeld: true, Joinable: floorOpen, ChangeoverWaiting: waiting}
			}
			floorBlocked = true
		}
	}

	// Demand. Never while a create is outstanding.
	if in.PendingCreates == 0 && alive > in.floor() {
		pool := deletable(in)
		free := readyFree(pool)
		// During a changeover only stale servers are demand candidates; otherwise,
		// while maxUnavailable is spent, the demand rule would delete the cold
		// start's own replacement (youngest first) and coldStart would rebuild it.
		// The spec may enter deletion candidacy but not the capacity arithmetic: here
		// it can only hold a removal back, there it would order creates.
		changeover := staleRemains(in)
		open := joinableCount(in)

		// Under WhenEmpty an occupied stale server may never empty, so current
		// servers become candidates once no stale one is, short of the last Ready one.
		var readyCurrent int32
		for _, v := range pool {
			if !staleSpec(v, in.PodHash) && v.Phase == phase.Ready {
				readyCurrent++
			}
		}
		collect := func(current bool) []ServerView {
			eligible := make([]ServerView, 0, len(pool))
			for _, v := range pool {
				if changeover && staleSpec(v, in.PodHash) == current {
					continue
				}
				if current && v.Phase == phase.Ready && readyCurrent < 2 {
					continue
				}
				if changeover && in.MinAvailable > 0 && joinable(in, v) && open-1 < in.MinAvailable {
					continue
				}
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
			return eligible
		}
		eligible := collect(false)
		if len(eligible) == 0 && changeover && in.WhenEmpty {
			eligible = collect(true)
		}
		if names := SelectDeletionCandidates(eligible, 1); len(names) > 0 {
			return SizeDecision{Delete: names, ChangeoverWaiting: waiting,
				FloorHeld: floorHeld, Joinable: floorOpen, FloorBlocked: floorBlocked}
		}
	}

	// Nothing was decided, but a refused or withheld cold start still has to
	// reach the operator.
	return SizeDecision{Wanted: wanted, Limited: limited || floorBlocked, ColdStartBlocked: coldBlocked,
		ChangeoverWaiting: waiting, FloorHeld: floorHeld, Joinable: floorOpen, FloorBlocked: floorBlocked}
}
