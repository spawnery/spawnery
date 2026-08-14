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
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// ready builds a Ready server with a fresh report. ServerView.Generation is
// left at zero throughout this file: the sizing rules do not read it, and a
// test that set it would suggest they did.
func ready(name string, players, slots int32) ServerView {
	return ServerView{
		Name: name, Phase: phase.Ready, Players: players, Slots: slots,
		WasRegistered: true,
	}
}

// starting builds a server that has a pod and has never reported: no slots, and
// a count that is therefore stale.
func starting(name string) ServerView {
	return ServerView{Name: name, Phase: phase.Starting, Stale: true}
}

func TestDecideSizeCreatesTheFloor(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 2, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 2 {
		t.Errorf("Create = %d, want 2 to reach the floor", got.Create)
	}
}

func TestDecideSizeCreditsCapacityThatIsOrderedButNotArrived(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ScalingInputs
		want int32
	}{
		{
			// The whole point of the milestone: a server that has a pod and has
			// not reported yet is capacity on its way, not a hole to fill.
			name: "a starting server covers the spare slots",
			in: ScalingInputs{
				Views:       []ServerView{starting("a")},
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
			},
			want: 0,
		},
		{
			name: "a create the cache has not shown yet counts the same way",
			in: ScalingInputs{
				MinReplicas: 1, MaxReplicas: 10,
				SpareSlots: 40, MaxPlayers: 100, PendingCreates: 1,
			},
			want: 0,
		},
		{
			// A server that reported once and then went quiet is not capacity:
			// unknown counts as occupied throughout this repository.
			name: "a server whose count went stale credits nothing",
			in: ScalingInputs{
				Views: []ServerView{{
					Name: "a", Phase: phase.Ready, Slots: 100, Stale: true,
					WasRegistered: true,
				}},
				MinReplicas: 1, MaxReplicas: 10,
				SpareSlots: 40, MaxPlayers: 100,
			},
			want: 1,
		},
		{
			// 4a does not roll updates, so its capacity arithmetic does not
			// read the generation at all, and 4b keeps it that way: a rule
			// that credited only current-generation servers would order a
			// full replacement set on every spec edit.
			//
			// The group is mid-changeover here - one stale server, one of the
			// current generation - so coldStart does not fire and the only
			// thing deciding the outcome is whether the stale server's twenty
			// free slots are counted. They are: 20 + 20 meets the forty spare
			// slots exactly. Drop the stale server's contribution and this
			// wants one create instead of none, which is what makes the case
			// load-bearing rather than decorative.
			name: "a server of another generation still credits its capacity",
			in: ScalingInputs{
				Views: []ServerView{
					{
						Name: "stale", Phase: phase.Ready, Slots: 100, Players: 80,
						WasRegistered: true, Generation: 3,
					},
					{
						Name: "current", Phase: phase.Ready, Slots: 100, Players: 80,
						WasRegistered: true, Generation: 4,
					},
				},
				Generation:  4,
				MinReplicas: 1, MaxReplicas: 10,
				SpareSlots: 40, MaxPlayers: 100,
			},
			want: 0,
		},
		{
			name: "a draining server credits nothing",
			in: ScalingInputs{
				Views:       []ServerView{{Name: "a", Phase: phase.Draining, Slots: 100}},
				MinReplicas: 0, MaxReplicas: 10,
				SpareSlots: 40, MaxPlayers: 100,
			},
			want: 1,
		},
		{
			name: "a server pending deletion credits nothing",
			in: ScalingInputs{
				Views:       []ServerView{ready("a", 0, 100)},
				MinReplicas: 0, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
				PendingDeletes: map[string]bool{"a": true},
			},
			want: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideSize(tc.in).Create; got != tc.want {
				t.Errorf("Create = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDecideSizeRoundsTheShortfallUp(t *testing.T) {
	for _, tc := range []struct {
		name       string
		free       int32
		spare      int32
		wantCreate int32
	}{
		{"no shortfall", 100, 40, 0},
		{"exactly at the mark", 40, 40, 0},
		{"one slot short orders one server", 39, 40, 1},
		{"a shortfall of exactly one server orders one", 0, 100, 1},
		{"one slot more orders two", 0, 101, 2},
		{"a large shortfall orders the ceiling of the quotient", 0, 250, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideSize(ScalingInputs{
				Views:       []ServerView{ready("a", 100-tc.free, 100)},
				MinReplicas: 1, MaxReplicas: 10,
				SpareSlots: tc.spare, MaxPlayers: 100,
			})
			if got.Create != tc.wantCreate {
				t.Errorf("Create = %d, want %d", got.Create, tc.wantCreate)
			}
		})
	}
}

func TestDecideSizeReportsTheCeilingHoldingCapacityBack(t *testing.T) {
	in := ScalingInputs{
		Views:       []ServerView{ready("a", 100, 100), ready("b", 100, 100)},
		MinReplicas: 1, MaxReplicas: 2,
		SpareSlots: 40, MaxPlayers: 100,
	}
	got := DecideSize(in)
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 at the ceiling", got.Create)
	}
	if got.Wanted != 1 {
		t.Errorf("Wanted = %d, want 1 — the rule asked for one before the ceiling cut it", got.Wanted)
	}
	if !got.Limited {
		t.Error("Limited = false, want true: the ceiling is holding capacity back")
	}
	if got.ColdStartBlocked {
		t.Error("ColdStartBlocked = true, want false: this is the ordinary shortfall, not a refused cold start")
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: a group short of capacity does not also shrink", got.Delete)
	}

	in.MaxReplicas = 5
	if got := DecideSize(in); got.Limited || got.Create != 1 {
		t.Errorf("with room: Create = %d, Limited = %v, want 1 and false", got.Create, got.Limited)
	}
}

func TestDecideSizeShrinksToALoweredCeilingWithoutWaiting(t *testing.T) {
	// A lowered maxReplicas is an instruction, not a suggestion: the
	// stabilization window does not apply. SelectDeletionCandidates still
	// refuses any server that may be carrying players, which is what keeps
	// this safe.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			ready("a", 0, 100), ready("b", 0, 100), ready("c", 5, 100),
		},
		MinReplicas: 1, MaxReplicas: 2,
		SpareSlots: 40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 {
		t.Fatalf("Delete = %v, want exactly one name", got.Delete)
	}
	if got.Delete[0] == "c" {
		t.Error("nominated the occupied server — core invariant broken")
	}
	if got.Surplus != 1 {
		t.Errorf("Surplus = %d, want 1", got.Surplus)
	}
}

func TestDecideSizeNeverNominatesAServerAlreadyBeingRemoved(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			ready("a", 0, 100), ready("b", 0, 100), ready("c", 0, 100),
		},
		MinReplicas: 1, MaxReplicas: 2,
		SpareSlots: 40, MaxPlayers: 100,
		// Set, so the demand rule of task 4 finds no stabilized candidate and
		// this test keeps asserting only what its name says.
		Stabilization:  5 * time.Minute,
		PendingDeletes: map[string]bool{"a": true},
	})
	for _, name := range got.Delete {
		if name == "a" {
			t.Fatal("nominated a server whose deletion has already been asked for")
		}
	}
	// a is gone from the count, so b and c are already at the ceiling of 2.
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none once the pending removal is counted", got.Delete)
	}
}

// TestDecideSizeShortOfCapacityStillObeysALoweredCeiling pins the fixed point
// the whole-branch review found: three servers against a ceiling of one is an
// instruction, and the group is also 700 slots short. Before the fix the
// shortfall returned first and the surplus branch was unreachable, so the
// group stood above its ceiling for ever while publishing that it wanted more.
func TestDecideSizeShortOfCapacityStillObeysALoweredCeiling(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			ready("a", 0, 100), ready("b", 0, 100), ready("c", 0, 100),
		},
		MinReplicas: 1, MaxReplicas: 1,
		SpareSlots: 1000, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d at the ceiling, want 0", got.Create)
	}
	if got.Wanted != 7 || !got.Limited {
		t.Errorf("Wanted = %d, Limited = %v; want 7 and true — the group is still short "+
			"while it shrinks, and has to say so", got.Wanted, got.Limited)
	}
	if got.Surplus != 2 || len(got.Delete) != 2 {
		t.Errorf("Surplus = %d, Delete = %v; want 2 and two names", got.Surplus, got.Delete)
	}
}

// TestDecideSizeShortOfCapacityDoesNotShrinkForLackOfDemand is the other half:
// an idle server past its window sits beside a full one and the group is short.
// The demand rule would remove the idle one; the shortfall says the opposite,
// and the shortfall wins.
func TestDecideSizeShortOfCapacityDoesNotShrinkForLackOfDemand(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			ready("full", 100, 100), empty("idle", 100, time.Hour),
		},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots: 200, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want 1", got.Create)
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v while the group is short, want none", got.Delete)
	}
}

// TestDecideSizeDoesNotLetALeavingServerHoldTheFloor gives the group exactly
// as much room as its floor needs, so counting the draining server toward the
// size — instead of only toward nothing, as countsTowardSize says — is the
// difference between ordering the replacement and running one short for the
// whole drain.
func TestDecideSizeDoesNotLetALeavingServerHoldTheFloor(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{{Name: "a", Phase: phase.Draining, Slots: 100}},
		MinReplicas: 1, MaxReplicas: 1,
		SpareSlots: 0, MaxPlayers: 100,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want 1: a server on its way out does not hold the floor", got.Create)
	}
}

// empty builds a Ready, empty server that has been empty for d.
func empty(name string, slots int32, d time.Duration) ServerView {
	v := ready(name, 0, slots)
	v.EmptyFor = d
	return v
}

func TestDecideSizeWaitsForTheStabilizationWindow(t *testing.T) {
	in := ScalingInputs{
		Views: []ServerView{
			empty("a", 100, 4*time.Minute),
			empty("b", 100, 4*time.Minute),
		},
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	}
	if got := DecideSize(in); len(got.Delete) != 0 {
		t.Errorf("Delete = %v before the window elapsed, want none", got.Delete)
	}

	in.Views[0].EmptyFor = 5 * time.Minute
	in.Views[1].EmptyFor = 5 * time.Minute
	got := DecideSize(in)
	if len(got.Delete) != 1 {
		t.Fatalf("Delete = %v, want exactly one — one per pass", got.Delete)
	}
}

func TestDecideSizeHoldsTheFloor(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{empty("a", 100, time.Hour)},
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v at the floor, want none", got.Delete)
	}

	// The case above cannot tell the floor from the spare: removing the only
	// server would leave no free slots at all, so feasibility blocks it too.
	// With a spare of zero, nothing but the floor can stop the removal.
	got = DecideSize(ScalingInputs{
		Views: []ServerView{
			empty("a", 100, time.Hour), empty("b", 100, time.Hour),
		},
		MinReplicas: 2, MaxReplicas: 10,
		SpareSlots: 0, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v with the group already at a floor of 2, want none", got.Delete)
	}
}

func TestDecideSizeKeepsEnoughFreeSlotsAfterTheRemoval(t *testing.T) {
	// Two empty servers, spare 150: removing either leaves 100 free, which is
	// short. Nothing may go, even though both have waited out the window.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			empty("a", 100, time.Hour),
			empty("b", 100, time.Hour),
		},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots: 150, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: the removal would fall below spareSlots", got.Delete)
	}
}

// TestDecideSizeTestsEachCandidateOnItsOwn pins that an infeasible head of the
// candidate list does not hide a feasible tail.
//
// Both servers are empty and past the window, so both are candidates.
// SelectDeletionCandidates puts servers that never took players first, so
// "fresh" is the head. Free slots are 100 + 30 = 130: removing "fresh" leaves
// 30, short of the 40 spare, while removing "small" leaves 100. A rule that
// tested only the head would delete nothing.
func TestDecideSizeTestsEachCandidateOnItsOwn(t *testing.T) {
	fresh := empty("fresh", 100, time.Hour)
	fresh.WasRegistered = false
	small := empty("small", 30, time.Hour)

	got := DecideSize(ScalingInputs{
		Views:       []ServerView{fresh, small},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 || got.Delete[0] != "small" {
		t.Errorf("Delete = %v, want [small]: removing fresh would leave 30 free slots, short of 40", got.Delete)
	}
}

func TestDecideSizeNeverRemovesAServerWithAnUnreliableCount(t *testing.T) {
	stale := empty("a", 100, time.Hour)
	stale.Stale = true

	got := DecideSize(ScalingInputs{
		Views:       []ServerView{stale, empty("b", 100, time.Hour)},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots: 0, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 || got.Delete[0] != "b" {
		t.Fatalf("Delete = %v, want [b]: a server whose player count cannot be "+
			"trusted is never removed, and the one beside it still can be", got.Delete)
	}
}

// TestDecideSizeDoesNotCountUntrustedCapacityAsFree pins the other half of the
// staleness rule. A stale server is never removed — that is tested elsewhere —
// but its capacity must also not be counted as free, or a removal somewhere
// else in the group passes a spare check on slots nobody can vouch for.
func TestDecideSizeDoesNotCountUntrustedCapacityAsFree(t *testing.T) {
	untrusted := empty("untrusted", 100, time.Hour)
	untrusted.Stale = true

	in := ScalingInputs{
		Views:       []ServerView{untrusted, empty("b", 100, time.Hour)},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	}
	if got := DecideSize(in); len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: only b's 100 slots are trustworthy, and "+
			"removing b would leave nothing at all against a spare of 40", got.Delete)
	}

	// The same group with the count trusted again: 200 free slots, and removing
	// one still leaves 100 against a spare of 40.
	in.Views[0].Stale = false
	if got := DecideSize(in); len(got.Delete) != 1 {
		t.Errorf("Delete = %v once the count is trustworthy, want exactly one", got.Delete)
	}
}

// staleReady builds a Ready server of an older generation.
func staleReady(name string, players, slots int32, gen int64) ServerView {
	v := ready(name, players, slots)
	v.Generation = gen
	return v
}

func TestDecideSizeColdStartsAReplacementForAStaleGroup(t *testing.T) {
	// Two stale servers with plenty of free capacity between them. The
	// spare-slot rule is satisfied and would create nothing, so without the
	// cold start no server of the new generation ever exists, nothing may
	// retire, and the update never begins.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("a", 60, 100, 3),
			staleReady("b", 60, 100, 3),
		},
		Generation:  4,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want exactly 1 to start the changeover", got.Create)
	}
}

func TestDecideSizeColdStartsOnlyOnce(t *testing.T) {
	// The replacement is on order but has not reached the cache. Firing
	// again here would create one server per five-second pass for the whole
	// boot.
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{staleReady("a", 60, 100, 3)},
		Generation: 4, PendingCreates: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 while the cold start is outstanding", got.Create)
	}
}

func TestDecideSizeDoesNotColdStartWhenAReplacementIsAlreadyUp(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("a", 60, 100, 3),
			ready("b", 0, 100), // generation 0 == current
		},
		Generation:  0,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 when the current generation is already up", got.Create)
	}
}

func TestDecideSizeReportsAColdStartTheCeilingRefuses(t *testing.T) {
	// A group pinned at maxReplicas has no room for the one extra server the
	// changeover needs, so the update cannot begin. Stalling is right — the
	// ceiling is an instruction — but it must not stall silently, and
	// ScalingLimited is the condition that already exists to say so.
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{staleReady("a", 0, 100, 3)},
		Generation:  4,
		MinReplicas: 1, MaxReplicas: 1, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 at the ceiling", got.Create)
	}
	if !got.Limited {
		t.Error("Limited = false, want true so the stalled changeover is visible")
	}
	if !got.ColdStartBlocked {
		t.Error("ColdStartBlocked = false, want true: Wanted is 0 here same as the unlimited case, " +
			"and ColdStartBlocked is the only field that tells them apart")
	}
	if got.Wanted != 0 {
		t.Errorf("Wanted = %d, want 0 — this case is defined by Wanted staying 0 despite Limited", got.Wanted)
	}
}

func TestDecideSizeDoesNotShrinkWhileACreateIsOutstanding(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			empty("a", 100, time.Hour), empty("b", 100, time.Hour),
		},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
		PendingCreates: 1,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v while a create is outstanding, want none", got.Delete)
	}
}

func TestDecideSizeRetiresAStaleServerOnceAReplacementIsReady(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("old", 60, 100, 3),
			ready("new", 0, 100),
		},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Errorf("Retire = %v, want [old]", got.Retire)
	}
}

func TestDecideSizeRetiresAServerThatStillHasPlayers(t *testing.T) {
	// The point of soft drain, and the reason retirement cannot reuse
	// SelectDeletionCandidates: that function excludes exactly these
	// servers, and these are the ones that have to go.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("old", 99, 100, 3),
			ready("new", 0, 100),
		},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 {
		t.Fatalf("Retire = %v, want the occupied stale server", got.Retire)
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none — retirement is not deletion", got.Delete)
	}
}

func TestDecideSizeDoesNotRetireWithoutAReadyReplacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  ServerView
	}{
		{"the replacement is still starting", starting("new")},
		{"the replacement failed", ServerView{Name: "new", Phase: phase.Failed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideSize(ScalingInputs{
				Views:      []ServerView{staleReady("old", 60, 100, 3), tc.new},
				Generation: 0, MaxUnavailable: 1,
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
			})
			if len(got.Retire) != 0 {
				t.Errorf("Retire = %v, want none until a replacement is Ready", got.Retire)
			}
		})
	}
}

func TestDecideSizeRespectsTheUpdateBudget(t *testing.T) {
	// One is already retiring. maxUnavailable is 1, so the second stale
	// server waits.
	retiring := staleReady("first", 5, 100, 3)
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{retiring, staleReady("second", 5, 100, 3), ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none while the budget is spent", got.Retire)
	}
}

func TestDecideSizeCountsAForcedDrainAgainstTheBudget(t *testing.T) {
	// maxStaleSeconds escalated a retirement into a real drain. spec.retire
	// stays true across that, so it keeps occupying the budget it started
	// in — the server is still unavailable because of this update.
	forced := staleReady("first", 5, 100, 3)
	forced.Phase = phase.Draining
	forced.Retire = true
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{forced, staleReady("second", 5, 100, 3), ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — the forced drain still holds the budget", got.Retire)
	}
}

func TestDecideSizeDoesNotCountAScaleDownDrainAgainstTheUpdateBudget(t *testing.T) {
	// The complement, and the reason spec.retire exists rather than the
	// phase being read: a server draining because demand fell was not made
	// unavailable by this update and must not spend its budget.
	unrelated := staleReady("shrinking", 0, 100, 3)
	unrelated.Phase = phase.Draining // Retire stays false
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{unrelated, staleReady("old", 5, 100, 3), ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Errorf("Retire = %v, want [old]", got.Retire)
	}
}

func TestDecideSizeCountsAReservedRetirementAgainstTheBudget(t *testing.T) {
	// The patch is out, the cache has not shown it. Counting only what the
	// cache shows would spend the budget twice.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("first", 5, 100, 3),
			staleReady("second", 5, 100, 3),
			ready("new", 0, 100),
		},
		Generation: 0, MaxUnavailable: 1,
		PendingRetires: map[string]bool{"first": true},
		MinReplicas:    1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none while a reservation stands", got.Retire)
	}
}

func TestDecideSizeRetiresEmptyServersFirstThenTheOldest(t *testing.T) {
	base := time.Now()
	older := staleReady("older", 5, 100, 3)
	older.CreatedAt = base
	newer := staleReady("newer", 5, 100, 3)
	newer.CreatedAt = base.Add(time.Minute)
	empty := staleReady("empty", 0, 100, 3)
	empty.CreatedAt = base.Add(2 * time.Minute)

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{newer, older, empty, ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "empty" {
		t.Fatalf("Retire = %v, want [empty] — an empty server costs nobody anything", got.Retire)
	}

	got = DecideSize(ScalingInputs{
		Views:      []ServerView{newer, older, ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "older" {
		t.Errorf("Retire = %v, want [older] when none is empty", got.Retire)
	}
}

func TestDecideSizeDoesNotRetireAndShrinkInOnePass(t *testing.T) {
	// 4a's precedent: two removals decided in one pass are two decisions
	// taken on two readings of the same moment. Retirement comes first and
	// the pass ends there.
	idle := staleReady("idle", 0, 100, 3)
	idle.EmptyFor = time.Hour
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{idle, staleReady("busy", 5, 100, 3), ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: time.Minute,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none in a pass that retires", got.Delete)
	}
	if len(got.Retire) != 1 {
		t.Errorf("Retire = %v, want one", got.Retire)
	}
}

// TestDecideSizeDoesNotDeleteAServerWhoseRetirementIsReserved pins the answer
// to the question task 4 carried forward: expectations is keyed by name, so a
// delete reservation recorded for a server that already has a retire
// reservation overwrites it, the retirement stops counting against
// maxUnavailable while its patch is still in flight, and the pass after that
// spends the budget a second time.
//
// The sequence is reachable, and this is the pass that reaches it. A pass that
// nominates a retirement returns there, so the two decisions never meet within
// one pass — but on the pass after, the standing reservation is exactly what
// makes selectRetirement decline, while the cache still shows the server Ready
// and long empty, which is everything the demand rule asks for. "old" is the
// only stabilized candidate here, so the demand rule has no other way to
// spend the pass.
func TestDecideSizeDoesNotDeleteAServerWhoseRetirementIsReserved(t *testing.T) {
	old := staleReady("old", 0, 100, 3)
	old.EmptyFor = time.Hour
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{old, ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		PendingRetires: map[string]bool{"old": true},
		MinReplicas:    1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — the reservation already spends the budget", got.Retire)
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: deleting a server whose retirement is "+
			"reserved overwrites that reservation and hands the budget back", got.Delete)
	}
}

// TestDecideSizeRetiresTheStaleServerRatherThanDeletingTheColdStart pins the
// answer to the question task 5 carried forward: the cold-start server becomes
// Ready, and while nothing has retired yet it is empty, it is surplus, and
// deleting it re-triggers the cold start on the next pass, which creates
// another.
//
// The retirement rule is what closes that loop, and it closes it by ordering
// rather than by any test of its own: the pass finds a stale server to retire
// and returns there, so the demand rule never runs. On the next pass the stale
// server is Retiring, leaving() drops it out of the size, and the fresh server
// is no longer surplus.
//
// The fixture is arranged so the demand rule would otherwise take exactly the
// wrong server. Both have been empty past the window, so both are candidates;
// SelectDeletionCandidates sorts the youngest first among servers that took
// players, and the cold-start server is the younger — so without the branch
// above it, the group deletes the replacement and keeps the server the update
// is trying to get rid of.
func TestDecideSizeRetiresTheStaleServerRatherThanDeletingTheColdStart(t *testing.T) {
	base := time.Now()
	old := staleReady("old", 0, 100, 3)
	old.EmptyFor = time.Hour
	old.CreatedAt = base
	fresh := ready("new", 0, 100)
	fresh.EmptyFor = time.Hour
	fresh.CreatedAt = base.Add(time.Hour)

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{old, fresh},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Errorf("Retire = %v, want [old]", got.Retire)
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none — deleting the cold-start server re-triggers "+
			"the cold start and the group builds the same server again", got.Delete)
	}
}

// TestDecideSizeDoesNotDeleteTheColdStartWhileTheRetirementBudgetIsSpent is the
// other half of the test above, and the one the ordering does not cover.
//
// There, nothing had retired yet, so the pass nominated a retirement and
// returned before the demand rule ran. Here one stale server is already
// retiring and maxUnavailable is 1, so selectRetirement correctly declines and
// the pass falls through to demand with a stale server still standing. The
// fixture is the state that was probed against the implementation as task 6
// first committed it, which answered Retire=[] Delete=[new]: the group deleted
// the cold start's own replacement, leaving no current-generation server, so
// coldStart fired again on the next pass.
//
// The victim was preferred rather than merely permitted — "old2" is a stale,
// empty server sitting in the same candidate list, and it loses the
// youngest-first sort to the fresh replacement purely on age. So the assertion
// is in two parts: the replacement must survive, and the stale server is what
// the group ought to shed instead.
func TestDecideSizeDoesNotDeleteTheColdStartWhileTheRetirementBudgetIsSpent(t *testing.T) {
	base := time.Now()
	retiring := staleReady("old1", 40, 100, 3)
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	retiring.CreatedAt = base
	idle := staleReady("old2", 0, 100, 3)
	idle.EmptyFor = time.Hour
	idle.CreatedAt = base.Add(time.Minute)
	fresh := ready("new", 0, 100)
	fresh.EmptyFor = time.Hour
	fresh.CreatedAt = base.Add(time.Hour)

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{retiring, idle, fresh},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none while the budget is spent", got.Retire)
	}
	for _, name := range got.Delete {
		if name == "new" {
			t.Errorf("Delete = %v, want no current-generation server — deleting the "+
				"replacement leaves none of its generation and re-triggers the cold start", got.Delete)
		}
	}
	if len(got.Delete) != 1 || got.Delete[0] != "old2" {
		t.Errorf("Delete = %v, want [old2] — while a changeover is in progress the "+
			"group sheds stale capacity first", got.Delete)
	}
}

// TestDecideSizeStillShedsAStaleServerForLackOfDemand is the guard on the rule
// above: it holds current-generation servers out of the demand rule, and it
// must hold nothing else out. A rule that also stopped stale servers being
// shed would be worse than the bug it fixes — the group would keep paying for
// capacity nobody is using for the whole of a changeover.
//
// The budget is spent, so retirement declines and demand runs. The
// current-generation server has players and was never a candidate, so the only
// thing this can measure is whether the empty stale server is still deletable.
func TestDecideSizeStillShedsAStaleServerForLackOfDemand(t *testing.T) {
	retiring := staleReady("old1", 40, 100, 3)
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	idle := staleReady("old2", 0, 100, 3)
	idle.EmptyFor = time.Hour

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{retiring, idle, ready("new", 60, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 || got.Delete[0] != "old2" {
		t.Errorf("Delete = %v, want [old2] — an empty stale server past its window "+
			"is still shed for lack of demand during a changeover", got.Delete)
	}
}

// TestDecideSizeDeletesForLackOfDemandWhenNoStaleServerRemains is the
// regression check on the ordinary case, which is every pass outside a
// changeover: with no stale server anywhere, the demand rule is exactly what
// milestone 4a left, and the empty current-generation server is deleted.
func TestDecideSizeDeletesForLackOfDemandWhenNoStaleServerRemains(t *testing.T) {
	idle := ready("idle", 0, 100)
	idle.EmptyFor = time.Hour

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{ready("busy", 60, 100), idle},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 || got.Delete[0] != "idle" {
		t.Errorf("Delete = %v, want [idle] — with no changeover in progress the "+
			"demand rule is untouched", got.Delete)
	}
}

// TestDecideSizeDoesNotDeleteAServerAlreadyShowingSpecRetire is the other half
// of TestDecideSizeDoesNotDeleteAServerWhoseRetirementIsReserved, and the half
// the reservation does not cover.
//
// observe() satisfies a retire expectation the moment the cache shows
// spec.retire, while the phase is written by the Server controller in a second
// write that lands later. So the ordinary sequence — not a race — puts the
// group in exactly this state for a pass or more: the view reads Retire: true
// and Phase: Ready, and PendingRetires is already empty. selectRetirement
// counts the server against the budget and declines, the pass falls through to
// demand, and if the pool were filtered on the reservation alone the server
// would be right back in it — the preferred candidate, since the nomination
// prefers empty servers and an empty server is what the demand rule wants too.
// The soft drain would become a hard delete.
func TestDecideSizeDoesNotDeleteAServerAlreadyShowingSpecRetire(t *testing.T) {
	old := staleReady("old", 0, 100, 3)
	old.EmptyFor = time.Hour
	old.Retire = true // the patch landed; the phase write has not

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{old, ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — this server is already retiring", got.Retire)
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: a server that has been asked to retire "+
			"leaves by soft drain, not by deletion", got.Delete)
	}
}

// TestDecideSizeDoesNotRetireOnAReplacementNominatedForDeletion pins the input
// to the "one ready server of the current generation" guard.
//
// The guard is not about how many current-generation servers the cache lists,
// it is about whether one of them will still be there. A replacement this
// reconciler has already asked to delete will not be, and the ceiling branch
// makes that the preferred outcome rather than a rare one: it runs before
// retirement, applies no changeover filter, and sorts never-took-players first
// then youngest — which is the cold-start replacement exactly. Retiring a stale
// server on the strength of it deregisters capacity that has nothing to fall
// back on, and a retirement is not retractable by the next pass.
func TestDecideSizeDoesNotRetireOnAReplacementNominatedForDeletion(t *testing.T) {
	booting := starting("booting") // current generation, not Ready yet

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{staleReady("old", 60, 100, 3), ready("new", 0, 100), booting},
		Generation: 0, MaxUnavailable: 1,
		PendingDeletes: map[string]bool{"new": true},
		MinReplicas:    1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — the only ready server of the current "+
			"generation is already nominated for deletion", got.Retire)
	}
}

// TestDecideSizeTreatsAnUnsetUpdateBudgetAsOne pins a decision that would
// otherwise be invisible in both directions.
//
// spec.update is optional and no CEL rule requires it, so a nil parent is the
// ordinary state of a group whose operator never wrote an update policy — and a
// nil parent means maxUnavailable's CRD default of 1 never applies and 0
// arrives here. Read literally that is "no budget", and the group would decline
// every retirement forever with no error, no condition and no event. Because
// the CRD's Minimum=1 means a real 0 cannot exist, a zero can only mean unset,
// and the floor is safe.
//
// The second half is what stops the floor being read as "unset means
// unlimited": one is one.
func TestDecideSizeTreatsAnUnsetUpdateBudgetAsOne(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{staleReady("old", 60, 100, 3), ready("new", 0, 100)},
		Generation:  0, // MaxUnavailable deliberately unset
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Fatalf("Retire = %v, want [old] — an unset maxUnavailable is one, not zero: "+
			"a group with no spec.update block must still roll", got.Retire)
	}

	retiring := staleReady("first", 5, 100, 3)
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	got = DecideSize(ScalingInputs{
		Views:       []ServerView{retiring, staleReady("second", 5, 100, 3), ready("new", 0, 100)},
		Generation:  0, // MaxUnavailable deliberately unset
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — the floor is one concurrent retirement, "+
			"not an unbounded budget", got.Retire)
	}
}

// TestDecideSizeDoesNotRetireAnUntrustedCountFirst is the empty-first rule
// meeting the invariant the rest of the package obeys: unknown counts as
// occupied.
//
// Ready && Stale is a real combination — losing the count is not losing the
// probe — and a stale server whose last report was zero may be carrying
// players. Preferring it for retirement inverts the rule's own justification:
// retiring an empty server costs nobody anything, but this one is only empty as
// far as anyone can see. The older server with a count that can be trusted is
// what the ordering asks for instead.
func TestDecideSizeDoesNotRetireAnUntrustedCountFirst(t *testing.T) {
	base := time.Now()
	busy := staleReady("busy", 5, 100, 3)
	busy.CreatedAt = base
	quiet := staleReady("quiet", 0, 100, 3)
	quiet.Stale = true // last reported zero, and cannot be believed
	quiet.CreatedAt = base.Add(time.Minute)

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{quiet, busy, ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "busy" {
		t.Errorf("Retire = %v, want [busy] — a stale count is not an empty server, "+
			"so the oldest goes first and nothing jumps the queue on an unknown", got.Retire)
	}
}

// TestDecideSizeDoesNotSuspendDemandForAStaleServerThatIsAlreadyGone pins
// staleRemains' countsTowardSize condition.
//
// A stale server that is Failed, Draining or Terminating is not capacity the
// changeover is still racing to remove — it is gone or going, and no rule is
// going to shed it again. Counting it as "stale capacity remains" would hold
// every current-generation server out of the demand rule for as long as it is
// retained, which for a Failed server is the whole failed-retention window, an
// hour by default. It is also the set coldStart counts, and the two have to
// agree: the oscillation the changeover filter closes needs coldStart to
// re-fire, so a filter live in states coldStart is not would suspend
// scale-downs where the loop cannot happen.
//
// The discriminating fixture is the one the suite lacked: the corpse is the
// *only* stale server, so staleRemains is false solely because of this
// condition, and there is a demand-eligible current-generation server for it to
// suspend.
func TestDecideSizeDoesNotSuspendDemandForAStaleServerThatIsAlreadyGone(t *testing.T) {
	for _, gone := range []phase.Phase{phase.Failed, phase.Draining, phase.Terminating} {
		t.Run(string(gone), func(t *testing.T) {
			corpse := staleReady("corpse", 0, 100, 3)
			corpse.Phase = gone
			idle := ready("idle", 0, 100)
			idle.EmptyFor = time.Hour

			got := DecideSize(ScalingInputs{
				Views:      []ServerView{corpse, idle, ready("busy", 60, 100)},
				Generation: 0, MaxUnavailable: 1,
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
				Stabilization: 5 * time.Minute,
			})
			if len(got.Delete) != 1 || got.Delete[0] != "idle" {
				t.Errorf("Delete = %v, want [idle] — a stale server that is already "+
					"gone is not stale capacity the changeover still has to shed, and "+
					"treating it as such suspends ordinary scale-downs for the whole "+
					"of its retention", got.Delete)
			}
		})
	}
}

// TestDecideSizeShedsAnIdleStaleServerToMakeRoomForARefusedColdStart is the
// fixed point that used to be permanent. A group at its ceiling with an idle
// stale server beside it refused to delete the very server the changeover
// exists to remove, and told the operator to raise maxReplicas instead. The
// control case below is what makes that a defect rather than a design
// consequence: the same views and knobs with no changeover shed the same server
// happily.
func TestDecideSizeShedsAnIdleStaleServerToMakeRoomForARefusedColdStart(t *testing.T) {
	views := []ServerView{
		staleReady("a", 60, 100, 3),
		func() ServerView {
			v := staleReady("b", 0, 100, 3)
			v.EmptyFor = 10 * time.Minute
			return v
		}(),
	}
	in := ScalingInputs{
		Views:      views,
		Generation: 4, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 2, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	}

	got := DecideSize(in)
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 — the ceiling refuses the cold start", got.Create)
	}
	if len(got.Delete) != 1 || got.Delete[0] != "b" {
		t.Fatalf("Delete = %v, want [b]: the pass granted nothing, so it has decided "+
			"nothing, and shedding the idle stale server is what frees the room the "+
			"cold start was refused for", got.Delete)
	}

	// The control. Without the changeover the group already sheds b, which is
	// why refusing to shed it *because* of a changeover is the wrong way round.
	in.Generation = 3
	if control := DecideSize(in); len(control.Delete) != 1 || control.Delete[0] != "b" {
		t.Fatalf("control Delete = %v, want [b] — the fixture no longer measures the "+
			"difference the changeover makes", control.Delete)
	}
}

// TestDecideSizeStillReportsARefusedColdStartWithNothingToShed is the other half
// of the finding above: falling through must not cost the stall its visibility.
// When the pass genuinely has nothing to shed there is no way out but a higher
// ceiling, and ScalingLimited has to say so.
func TestDecideSizeStillReportsARefusedColdStartWithNothingToShed(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("a", 60, 100, 3),
			staleReady("b", 60, 100, 3),
		},
		Generation: 4, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 2, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none — both servers have players on them", got.Delete)
	}
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 at the ceiling", got.Create)
	}
	if !got.Limited || !got.ColdStartBlocked {
		t.Errorf("Limited = %v, ColdStartBlocked = %v, want both true: with nothing to "+
			"shed the changeover really is stalled on maxReplicas and the operator "+
			"has to be told", got.Limited, got.ColdStartBlocked)
	}
}

// TestDecideSizeDoesNotShedForARealShortfallEvenWhenTheColdStartIsRefused is
// the other half of the guard the fall-through above depends on: `demanded <
// 1`, captured before the cold start's own bump, is what tells "this pass
// wanted nothing anyway" apart from "this pass is genuinely short and the
// cold start ate the only slot the ceiling had". Dropping `&& demanded < 1`
// from `coldOnly` passes the rest of the package, because the per-candidate
// feasibility test in the demand rule usually catches a real shortfall on its
// own — `provisional >= readyFree(pool)` for an ordinary view. It does not
// here: x is Ready with SessionsGone, so provisionalCapacity credits it 0
// while readyContribution, which has no SessionsGone test, credits it in
// full. The spare-slot rule reads 10 free (from c alone) against 100 wanted
// and asks for a server; the feasibility test reads x's phantom 100 and finds
// room to shed c instead. Both x and c are stale (generation 3, one
// generation behind), so the cold start is refused at the ceiling and the
// fall-through is exactly what is being exercised.
func TestDecideSizeDoesNotShedForARealShortfallEvenWhenTheColdStartIsRefused(t *testing.T) {
	x := ready("x", 0, 100)
	x.Generation = 3
	x.SessionsGone = true
	x.EmptyFor = 10 * time.Minute

	c := ready("c", 0, 10)
	c.Generation = 3
	c.EmptyFor = 10 * time.Minute

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{x, c},
		Generation: 4, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 2, SpareSlots: 100, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 — the ceiling refuses the cold start", got.Create)
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: the group is short of capacity (demanded == "+
			"1), and shedding c would take away capacity it just said it needs", got.Delete)
	}
	if !got.Limited {
		t.Errorf("Limited = %v, want true — the shortfall is real and stays visible", got.Limited)
	}
}

// TestDecideSizeOverhangIsMaxUnavailableServersNotOne pins what the design's
// "at most one extra server" claim is actually worth. maxReplicas bounds alive,
// leaving() holds Retiring out of alive, so every concurrent retirement buys the
// spare-slot rule one server above the ceiling. At maxUnavailable 2 the group
// holds maxReplicas + 2 Server objects, and design §3.3 and acceptance
// criterion 5 are true only at the default of 1.
//
// The two retirements here are set up directly on the fixture (already
// Retiring, Retire: true), so this test reaches its assertions through the
// create path and never calls selectRetirement — MaxUnavailable is inert to
// this particular outcome. That is fine: this test is about the overhang
// bound, not the budget rule that lets a second retirement start.
// TestDecideSizeGrantsASecondRetirementAtBudgetTwo covers that rule.
func TestDecideSizeOverhangIsMaxUnavailableServersNotOne(t *testing.T) {
	retiring := func(name string) ServerView {
		v := staleReady(name, 50, 100, 3)
		v.Phase = phase.Retiring
		v.Retire = true
		return v
	}
	const ceiling = 3
	views := []ServerView{
		retiring("r1"), retiring("r2"),
		ready("a", 100, 100), ready("b", 100, 100),
	}
	got := DecideSize(ScalingInputs{
		Views:      views,
		Generation: 0, MaxUnavailable: 2,
		MinReplicas: 1, MaxReplicas: ceiling, SpareSlots: 100, MaxPlayers: 100,
	})
	if got.Create != 1 {
		t.Fatalf("Create = %d, want 1: two retiring servers are out of alive, so the "+
			"ceiling of %d still has room for one more", got.Create, ceiling)
	}
	if overhang := int32(len(views)) + got.Create - ceiling; overhang != 2 {
		t.Errorf("overhang = %d Server objects above maxReplicas, want 2 — the bound "+
			"is maxUnavailable, not one, and the design says one", overhang)
	}
}

// TestDecideSizeGrantsASecondRetirementAtBudgetTwo is the budget-2 boundary
// that TestDecideSizeRespectsTheUpdateBudget never exercises: that test (and
// every other MaxUnavailable >= 2 fixture in this package, see
// TestDecideSizeOverhangIsMaxUnavailableServersNotOne) only shows the create
// path with the retirements already in flight. This is the same fixture as
// TestDecideSizeRespectsTheUpdateBudget, one retirement already in flight and
// one stale candidate waiting, but at MaxUnavailable: 2 rather than 1 — the
// budget selectRetirement actually reads.
func TestDecideSizeGrantsASecondRetirementAtBudgetTwo(t *testing.T) {
	retiring := staleReady("first", 5, 100, 3)
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	views := []ServerView{retiring, staleReady("second", 5, 100, 3), ready("new", 0, 100)}

	got := DecideSize(ScalingInputs{
		Views:      views,
		Generation: 0, MaxUnavailable: 2,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "second" {
		t.Fatalf("Retire = %v, want [second]: one retirement already in flight leaves "+
			"one slot free of a budget of 2", got.Retire)
	}

	// The boundary the case above crosses: the identical fixture at the
	// default budget of 1 declines, as TestDecideSizeRespectsTheUpdateBudget
	// already pins.
	got = DecideSize(ScalingInputs{
		Views:      views,
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none at budget 1 — the same fixture that grants a "+
			"nomination at budget 2 must decline it here", got.Retire)
	}
}

func TestProvisionalCapacityDoesNotCreditAServerWhosePodIsGone(t *testing.T) {
	// Slots == 0 means two different things: a server that has never
	// reported (capacity on its way, credit it) and one whose pod vanished
	// (nothing there, credit nothing). SessionsGone is what separates them.
	gone := ServerView{Name: "a", Phase: phase.Starting, Stale: true, SessionsGone: true}
	if got := provisionalCapacity(gone, 100); got != 0 {
		t.Errorf("provisionalCapacity = %d, want 0 for a server whose pod is gone", got)
	}
}

func TestProvisionalCapacityStillCreditsAStartingServer(t *testing.T) {
	// The guard against the obvious wrong fix: a starting server is stale
	// and has never reported too, and crediting it zero brings back the
	// runaway scale-up 4a built this rule to stop.
	if got := provisionalCapacity(starting("a"), 100); got != 100 {
		t.Errorf("provisionalCapacity = %d, want the full 100 for a starting server", got)
	}
}
