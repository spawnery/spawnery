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
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// ready leaves PodHash empty, which staleSpec reads as "adopt", so it is never stale.
func ready(name string, players, slots int32) ServerView {
	return ServerView{
		Name: name, Phase: phase.Ready, Players: players, Slots: slots,
		WasRegistered: true, Registered: true,
	}
}

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
			// 20 + 20 meets the 40 spare exactly; without the stale server's credit this wants a create.
			name: "a server of another generation still credits its capacity",
			in: ScalingInputs{
				Views: []ServerView{
					{
						Name: "stale", Phase: phase.Ready, Slots: 100, Players: 80,
						WasRegistered: true, Registered: true, PodHash: "old",
					},
					{
						Name: "current", Phase: phase.Ready, Slots: 100, Players: 80,
						WasRegistered: true, Registered: true, PodHash: "current",
					},
				},
				PodHash:     "current",
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
	// SelectDeletionCandidates still refuses any server that may be carrying players.
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
		// Set so the demand rule finds no stabilized candidate.
		Stabilization:  5 * time.Minute,
		PendingDeletes: map[string]bool{"a": true},
	})
	for _, name := range got.Delete {
		if name == "a" {
			t.Fatal("nominated a server whose deletion has already been asked for")
		}
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none once the pending removal is counted", got.Delete)
	}
}

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

// SpareSlots is 0, so only the floor can be short and a Create can only come from it.
func TestDecideSizeDoesNotLetAFinishedServerHoldTheFloor(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{{Name: "a", Phase: phase.Finished, Slots: 100}},
		MinReplicas: 1, MaxReplicas: 1,
		SpareSlots: 0, MaxPlayers: 100,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want 1: a Finished server does not hold the floor", got.Create)
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
	// Removing either leaves 100 free against a spare of 150.
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

// "fresh" sorts first; removing it leaves 30 free against a spare of 40, removing "small" leaves 100.
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

// Otherwise a removal elsewhere passes the spare check on slots nobody can vouch for.
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

	// Trusted again: removing one still leaves 100 against a spare of 40.
	in.Views[0].Stale = false
	if got := DecideSize(in); len(got.Delete) != 1 {
		t.Errorf("Delete = %v once the count is trustworthy, want exactly one", got.Delete)
	}
}

// The hash is only compared for equality, so any distinct string models a changeover.
func staleReady(name string, players, slots int32, hash string) ServerView {
	v := ready(name, players, slots)
	v.PodHash = hash
	return v
}

func TestDecideSizeColdStartsAReplacementForAStaleGroup(t *testing.T) {
	// The spare-slot rule alone would create nothing, so the update would never begin.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("a", 60, 100, "old"),
			staleReady("b", 60, 100, "old"),
		},
		PodHash:     "current",
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want exactly 1 to start the changeover", got.Create)
	}
}

func TestDecideSizeColdStartsOnlyOnce(t *testing.T) {
	// The replacement is on order but has not reached the cache.
	got := DecideSize(ScalingInputs{
		Views:   []ServerView{staleReady("a", 60, 100, "old")},
		PodHash: "current", PendingCreates: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 while the cold start is outstanding", got.Create)
	}
}

func TestDecideSizeDoesNotColdStartWhenAReplacementIsAlreadyUp(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("a", 60, 100, "old"),
			ready("b", 0, 100), // generation 0 == current
		},
		PodHash:     "current",
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 when the current generation is already up", got.Create)
	}
}

func TestDecideSizeReportsAColdStartTheCeilingRefuses(t *testing.T) {
	// Stalling at the ceiling is right, but ScalingLimited must say so.
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{staleReady("a", 0, 100, "old")},
		PodHash:     "current",
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

// A refused changeover is not a ceiling refusal: ColdStartBlocked and Limited stay false.
func TestDecideSizeWithholdsTheColdStartWhileNotAdmitted(t *testing.T) {
	in := ScalingInputs{
		Views:       []ServerView{staleReady("a", 0, 100, "old")},
		PodHash:     "current",
		MinReplicas: 1, MaxReplicas: 3, SpareSlots: 40, MaxPlayers: 100,
		ChangeoverRefused: true,
	}
	got := DecideSize(in)
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 — the changeover budget withholds the cold start", got.Create)
	}
	if !got.ChangeoverWaiting {
		t.Error("ChangeoverWaiting = false, want true")
	}
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — nothing may retire without a replacement", got.Retire)
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none", got.Delete)
	}
	if got.ColdStartBlocked {
		t.Error("ColdStartBlocked = true, want false — the ceiling never refused anything here")
	}
	if got.Limited {
		t.Error("Limited = true, want false — the budget's refusal is not the ceiling's")
	}

	in.ChangeoverRefused = false
	got = DecideSize(in)
	if got.Create != 1 {
		t.Errorf("Create = %d, want 1 — today's cold start once the budget admits it", got.Create)
	}
	if got.ChangeoverWaiting {
		t.Error("ChangeoverWaiting = true, want false — the changeover was admitted")
	}
}

func TestDecideSizeStillAnswersDemandWhileWaiting(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("a", 60, 100, "old"),
			staleReady("b", 100, 100, "old"),
		},
		PodHash:     "current",
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 60, MaxPlayers: 100,
		ChangeoverRefused: true,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want 1 — the spare-slot shortfall is answered even while waiting", got.Create)
	}
	if !got.ChangeoverWaiting {
		t.Error("ChangeoverWaiting = false, want true")
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
			staleReady("old", 60, 100, "old"),
			ready("new", 0, 100),
		},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Errorf("Retire = %v, want [old]", got.Retire)
	}
}

func TestDecideSizeRetiresAServerThatStillHasPlayers(t *testing.T) {
	// SelectDeletionCandidates excludes exactly these servers, so retirement cannot reuse it.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("old", 99, 100, "old"),
			ready("new", 0, 100),
		},
		PodHash: "current", MaxUnavailable: 1,
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
				Views:   []ServerView{staleReady("old", 60, 100, "old"), tc.new},
				PodHash: "current", MaxUnavailable: 1,
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
			})
			if len(got.Retire) != 0 {
				t.Errorf("Retire = %v, want none until a replacement is Ready", got.Retire)
			}
		})
	}
}

func TestDecideSizeRespectsTheUpdateBudget(t *testing.T) {
	retiring := staleReady("first", 5, 100, "old")
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	got := DecideSize(ScalingInputs{
		Views:   []ServerView{retiring, staleReady("second", 5, 100, "old"), ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none while the budget is spent", got.Retire)
	}
}

func TestDecideSizeCountsAForcedDrainAgainstTheBudget(t *testing.T) {
	// spec.retire stays true after maxStaleSeconds escalates to a drain, so it still spends the budget.
	forced := staleReady("first", 5, 100, "old")
	forced.Phase = phase.Draining
	forced.Retire = true
	got := DecideSize(ScalingInputs{
		Views:   []ServerView{forced, staleReady("second", 5, 100, "old"), ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — the forced drain still holds the budget", got.Retire)
	}
}

func TestDecideSizeDoesNotCountAScaleDownDrainAgainstTheUpdateBudget(t *testing.T) {
	// A server draining because demand fell was not made unavailable by this update.
	unrelated := staleReady("shrinking", 0, 100, "old")
	unrelated.Phase = phase.Draining // Retire stays false
	got := DecideSize(ScalingInputs{
		Views:   []ServerView{unrelated, staleReady("old", 5, 100, "old"), ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Errorf("Retire = %v, want [old]", got.Retire)
	}
}

func TestDecideSizeCountsAReservedRetirementAgainstTheBudget(t *testing.T) {
	// The patch is out but the cache has not shown it.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("first", 5, 100, "old"),
			staleReady("second", 5, 100, "old"),
			ready("new", 0, 100),
		},
		PodHash: "current", MaxUnavailable: 1,
		PendingRetires: map[string]bool{"first": true},
		MinReplicas:    1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none while a reservation stands", got.Retire)
	}
}

func TestDecideSizeRetiresEmptyServersFirstThenTheOldest(t *testing.T) {
	base := time.Now()
	older := staleReady("older", 5, 100, "old")
	older.CreatedAt = base
	newer := staleReady("newer", 5, 100, "old")
	newer.CreatedAt = base.Add(time.Minute)
	empty := staleReady("empty", 0, 100, "old")
	empty.CreatedAt = base.Add(2 * time.Minute)

	got := DecideSize(ScalingInputs{
		Views:   []ServerView{newer, older, empty, ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "empty" {
		t.Fatalf("Retire = %v, want [empty] — an empty server costs nobody anything", got.Retire)
	}

	got = DecideSize(ScalingInputs{
		Views:   []ServerView{newer, older, ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "older" {
		t.Errorf("Retire = %v, want [older] when none is empty", got.Retire)
	}
}

func TestDecideSizeDoesNotRetireAndShrinkInOnePass(t *testing.T) {
	// Retirement comes first and ends the pass.
	idle := staleReady("idle", 0, 100, "old")
	idle.EmptyFor = time.Hour
	got := DecideSize(ScalingInputs{
		Views:   []ServerView{idle, staleReady("busy", 5, 100, "old"), ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
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

// expectations is keyed by name, so a delete reservation would overwrite the retire reservation and spend the budget twice.
func TestDecideSizeDoesNotDeleteAServerWhoseRetirementIsReserved(t *testing.T) {
	old := staleReady("old", 0, 100, "old")
	old.EmptyFor = time.Hour
	got := DecideSize(ScalingInputs{
		Views:   []ServerView{old, ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
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

// Both are demand candidates and the cold-start server sorts first; only retiring first keeps it.
func TestDecideSizeRetiresTheStaleServerRatherThanDeletingTheColdStart(t *testing.T) {
	base := time.Now()
	old := staleReady("old", 0, 100, "old")
	old.EmptyFor = time.Hour
	old.CreatedAt = base
	fresh := ready("new", 0, 100)
	fresh.EmptyFor = time.Hour
	fresh.CreatedAt = base.Add(time.Hour)

	got := DecideSize(ScalingInputs{
		Views:   []ServerView{old, fresh},
		PodHash: "current", MaxUnavailable: 1,
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

// With the budget spent the pass falls through to demand, where "old2" must go rather than the younger replacement.
func TestDecideSizeDoesNotDeleteTheColdStartWhileTheRetirementBudgetIsSpent(t *testing.T) {
	base := time.Now()
	retiring := staleReady("old1", 40, 100, "old")
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	retiring.CreatedAt = base
	idle := staleReady("old2", 0, 100, "old")
	idle.EmptyFor = time.Hour
	idle.CreatedAt = base.Add(time.Minute)
	fresh := ready("new", 0, 100)
	fresh.EmptyFor = time.Hour
	fresh.CreatedAt = base.Add(time.Hour)

	got := DecideSize(ScalingInputs{
		Views:   []ServerView{retiring, idle, fresh},
		PodHash: "current", MaxUnavailable: 1,
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

// Holding stale servers out of demand too would pay for idle capacity for the whole changeover.
func TestDecideSizeStillShedsAStaleServerForLackOfDemand(t *testing.T) {
	retiring := staleReady("old1", 40, 100, "old")
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	idle := staleReady("old2", 0, 100, "old")
	idle.EmptyFor = time.Hour

	got := DecideSize(ScalingInputs{
		Views:   []ServerView{retiring, idle, ready("new", 60, 100)},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 || got.Delete[0] != "old2" {
		t.Errorf("Delete = %v, want [old2] — an empty stale server past its window "+
			"is still shed for lack of demand during a changeover", got.Delete)
	}
}

func TestDecideSizeDeletesForLackOfDemandWhenNoStaleServerRemains(t *testing.T) {
	idle := ready("idle", 0, 100)
	idle.EmptyFor = time.Hour

	got := DecideSize(ScalingInputs{
		Views:   []ServerView{ready("busy", 60, 100), idle},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 || got.Delete[0] != "idle" {
		t.Errorf("Delete = %v, want [idle] — with no changeover in progress the "+
			"demand rule is untouched", got.Delete)
	}
}

// The cache shows spec.retire before the phase moves, and PendingRetires is already empty.
func TestDecideSizeDoesNotDeleteAServerAlreadyShowingSpecRetire(t *testing.T) {
	old := staleReady("old", 0, 100, "old")
	old.EmptyFor = time.Hour
	old.Retire = true // the patch landed; the phase write has not

	got := DecideSize(ScalingInputs{
		Views:   []ServerView{old, ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
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

// The ceiling branch prefers deleting the cold-start replacement, and a retirement cannot be taken back.
func TestDecideSizeDoesNotRetireOnAReplacementNominatedForDeletion(t *testing.T) {
	booting := starting("booting") // current generation, not Ready yet

	got := DecideSize(ScalingInputs{
		Views:   []ServerView{staleReady("old", 60, 100, "old"), ready("new", 0, 100), booting},
		PodHash: "current", MaxUnavailable: 1,
		PendingDeletes: map[string]bool{"new": true},
		MinReplicas:    1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — the only ready server of the current "+
			"generation is already nominated for deletion", got.Retire)
	}
}

// spec.update is optional and the CRD minimum is 1, so 0 means unset; read literally it would block every retirement.
func TestDecideSizeTreatsAnUnsetUpdateBudgetAsOne(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{staleReady("old", 60, 100, "old"), ready("new", 0, 100)},
		PodHash:     "current", // MaxUnavailable deliberately unset
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Fatalf("Retire = %v, want [old] — an unset maxUnavailable is one, not zero: "+
			"a group with no spec.update block must still roll", got.Retire)
	}

	retiring := staleReady("first", 5, 100, "old")
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	got = DecideSize(ScalingInputs{
		Views:       []ServerView{retiring, staleReady("second", 5, 100, "old"), ready("new", 0, 100)},
		PodHash:     "current", // MaxUnavailable deliberately unset
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — the floor is one concurrent retirement, "+
			"not an unbounded budget", got.Retire)
	}
}

// Unknown counts as occupied: a stale zero may be carrying players.
func TestDecideSizeDoesNotRetireAnUntrustedCountFirst(t *testing.T) {
	base := time.Now()
	busy := staleReady("busy", 5, 100, "old")
	busy.CreatedAt = base
	quiet := staleReady("quiet", 0, 100, "old")
	quiet.Stale = true // last reported zero, and cannot be believed
	quiet.CreatedAt = base.Add(time.Minute)

	got := DecideSize(ScalingInputs{
		Views:   []ServerView{quiet, busy, ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "busy" {
		t.Errorf("Retire = %v, want [busy] — a stale count is not an empty server, "+
			"so the oldest goes first and nothing jumps the queue on an unknown", got.Retire)
	}
}

// Counting a gone stale server would suspend demand for the whole failed-retention window.
func TestDecideSizeDoesNotSuspendDemandForAStaleServerThatIsAlreadyGone(t *testing.T) {
	for _, gone := range []phase.Phase{phase.Failed, phase.Draining, phase.Terminating} {
		t.Run(string(gone), func(t *testing.T) {
			corpse := staleReady("corpse", 0, 100, "old")
			corpse.Phase = gone
			idle := ready("idle", 0, 100)
			idle.EmptyFor = time.Hour

			got := DecideSize(ScalingInputs{
				Views:   []ServerView{corpse, idle, ready("busy", 60, 100)},
				PodHash: "current", MaxUnavailable: 1,
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

func TestDecideSizeShedsAnIdleStaleServerToMakeRoomForARefusedColdStart(t *testing.T) {
	views := []ServerView{
		staleReady("a", 60, 100, "old"),
		func() ServerView {
			v := staleReady("b", 0, 100, "old")
			v.EmptyFor = 10 * time.Minute
			return v
		}(),
	}
	in := ScalingInputs{
		Views:   views,
		PodHash: "current", MaxUnavailable: 1,
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

	// The control: without the changeover the group sheds b as well.
	in.PodHash = "old"
	if control := DecideSize(in); len(control.Delete) != 1 || control.Delete[0] != "b" {
		t.Fatalf("control Delete = %v, want [b] — the fixture no longer measures the "+
			"difference the changeover makes", control.Delete)
	}
}

func TestDecideSizeStillReportsARefusedColdStartWithNothingToShed(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("a", 60, 100, "old"),
			staleReady("b", 60, 100, "old"),
		},
		PodHash: "current", MaxUnavailable: 1,
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

// x is Ready with SessionsGone: provisionalCapacity credits it 0 but the feasibility test credits 100,
// so only `demanded < 1` in coldOnly stops c being shed.
func TestDecideSizeDoesNotShedForARealShortfallEvenWhenTheColdStartIsRefused(t *testing.T) {
	x := ready("x", 0, 100)
	x.PodHash = "old"
	x.SessionsGone = true
	x.EmptyFor = 10 * time.Minute

	c := ready("c", 0, 10)
	c.PodHash = "old"
	c.EmptyFor = 10 * time.Minute

	got := DecideSize(ScalingInputs{
		Views:   []ServerView{x, c},
		PodHash: "current", MaxUnavailable: 1,
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

// Each concurrent retirement buys the spare-slot rule one server above maxReplicas.
func TestDecideSizeOverhangIsMaxUnavailableServersNotOne(t *testing.T) {
	retiring := func(name string) ServerView {
		v := staleReady(name, 50, 100, "old")
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
		Views:   views,
		PodHash: "current", MaxUnavailable: 2,
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

func TestDecideSizeGrantsASecondRetirementAtBudgetTwo(t *testing.T) {
	retiring := staleReady("first", 5, 100, "old")
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	views := []ServerView{retiring, staleReady("second", 5, 100, "old"), ready("new", 0, 100)}

	got := DecideSize(ScalingInputs{
		Views:   views,
		PodHash: "current", MaxUnavailable: 2,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "second" {
		t.Fatalf("Retire = %v, want [second]: one retirement already in flight leaves "+
			"one slot free of a budget of 2", got.Retire)
	}

	got = DecideSize(ScalingInputs{
		Views:   views,
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none at budget 1 — the same fixture that grants a "+
			"nomination at budget 2 must decline it here", got.Retire)
	}
}

func TestProvisionalCapacityDoesNotCreditAServerWhosePodIsGone(t *testing.T) {
	// SessionsGone separates a never-reported server from one whose pod vanished.
	gone := ServerView{Name: "a", Phase: phase.Starting, Stale: true, SessionsGone: true}
	if got := provisionalCapacity(gone, 100); got != 0 {
		t.Errorf("provisionalCapacity = %d, want 0 for a server whose pod is gone", got)
	}
}

func TestProvisionalCapacityStillCreditsAStartingServer(t *testing.T) {
	// Crediting a starting server zero would bring back the runaway scale-up.
	if got := provisionalCapacity(starting("a"), 100); got != 100 {
		t.Errorf("provisionalCapacity = %d, want the full 100 for a starting server", got)
	}
}

// The door is read before the phase: crediting a door-closed server that left Ready would
// stop the group building the server waiting players need. One server too many is the cheap error.
func TestProvisionalCapacityReadsTheDoorBeforeThePhase(t *testing.T) {
	// Never reported, so Slots == 0: the shape otherwise credited in full.
	closed := ServerView{Name: "a", Phase: phase.Starting, JoinsClosed: true}
	if got := provisionalCapacity(closed, 100); got != 0 {
		t.Errorf("provisionalCapacity = %d, want 0: a server that shut its door has no "+
			"seat to offer whether or not it is Ready", got)
	}

	closed.JoinsClosed = false
	if got := provisionalCapacity(closed, 100); got != 100 {
		t.Errorf("provisionalCapacity = %d, want 100 once the door is open again", got)
	}
}

func TestDecideSizeCondemns(t *testing.T) {
	t.Run("a condemned server is named even with no surplus", func(t *testing.T) {
		in := ScalingInputs{
			Views: []ServerView{
				{Name: "a", Phase: phase.Ready, Slots: 10, Players: 3},
				{Name: "b", Phase: phase.Ready, Slots: 10, Players: 3, Condemned: true},
			},
			MinReplicas: 2, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		if len(got.Condemn) != 1 || got.Condemn[0] != "b" {
			t.Fatalf("Condemn = %v, want [b]", got.Condemn)
		}
		if got.Surplus != 0 {
			t.Fatalf("Surplus = %d, want 0: a node drain is not a scale-down", got.Surplus)
		}
	})

	t.Run("minReplicas does not hold a condemned server back", func(t *testing.T) {
		in := ScalingInputs{
			Views:       []ServerView{{Name: "a", Phase: phase.Ready, Slots: 10, Condemned: true}},
			MinReplicas: 1, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		if len(got.Condemn) != 1 || got.Condemn[0] != "a" {
			t.Fatalf("Condemn = %v, want [a]", got.Condemn)
		}
	})

	t.Run("the replacement is asked for in the same pass", func(t *testing.T) {
		in := ScalingInputs{
			Views:       []ServerView{{Name: "a", Phase: phase.Ready, Slots: 10, Condemned: true}},
			MinReplicas: 1, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		if got.Create < 1 {
			t.Fatalf("Create = %d, want at least 1", got.Create)
		}
	})

	t.Run("all condemned servers go in one pass", func(t *testing.T) {
		in := ScalingInputs{
			Views: []ServerView{
				{Name: "a", Phase: phase.Ready, Slots: 10, Condemned: true},
				{Name: "b", Phase: phase.Ready, Slots: 10, Condemned: true},
				{Name: "c", Phase: phase.Ready, Slots: 10},
			},
			MinReplicas: 3, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		if len(got.Condemn) != 2 {
			t.Fatalf("Condemn = %v, want two names", got.Condemn)
		}
	})

	t.Run("Delete and Condemn never name the same server", func(t *testing.T) {
		in := ScalingInputs{
			Views: []ServerView{
				{Name: "a", Phase: phase.Ready, Slots: 10, Condemned: true},
				{Name: "b", Phase: phase.Ready, Slots: 10},
				{Name: "c", Phase: phase.Ready, Slots: 10},
			},
			MinReplicas: 1, MaxReplicas: 2, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		for _, d := range got.Delete {
			for _, c := range got.Condemn {
				if d == c {
					t.Fatalf("%q is in both Delete and Condemn", d)
				}
			}
		}
	})

	t.Run("no condemned servers leaves Condemn nil", func(t *testing.T) {
		in := ScalingInputs{
			Views:       []ServerView{{Name: "a", Phase: phase.Ready, Slots: 10}},
			MinReplicas: 1, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		if got := DecideSize(in); got.Condemn != nil {
			t.Fatalf("Condemn = %v, want nil", got.Condemn)
		}
	})

	t.Run("a condemned stale server is not also nominated for retirement", func(t *testing.T) {
		// A retirement in the same pass would overwrite Condemn's delete reservation in the name-keyed expectations map.
		in := ScalingInputs{
			Views: []ServerView{
				func() ServerView {
					v := staleReady("old", 60, 100, "old")
					v.Condemned = true
					return v
				}(),
				ready("new", 0, 100),
			},
			PodHash: "current", MaxUnavailable: 1,
			MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		}
		got := DecideSize(in)
		if len(got.Retire) != 0 {
			t.Errorf("Retire = %v, want none: %q is condemned and may not also be retired", got.Retire, "old")
		}
		if len(got.Condemn) != 1 || got.Condemn[0] != "old" {
			t.Errorf("Condemn = %v, want [old]: the node drain still takes it", got.Condemn)
		}
	})

	t.Run("a condemned current-generation server is not the replacement a retirement waits for", func(t *testing.T) {
		// Retiring against a replacement on a departing node cannot be taken back.
		// "warming" (Starting) suppresses the cold start without satisfying readyCurrent itself.
		in := ScalingInputs{
			Views: []ServerView{
				staleReady("old", 60, 100, "old"),
				{Name: "warming", Phase: phase.Starting},
				func() ServerView {
					v := ready("new", 0, 100)
					v.Condemned = true
					return v
				}(),
			},
			PodHash: "current", MaxUnavailable: 1,
			MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		}
		got := DecideSize(in)
		if got.Create != 0 {
			t.Fatalf("Create = %d, want 0: a create means decideSize returned before the "+
				"retirement branch and this case tests nothing", got.Create)
		}
		if len(got.Retire) != 0 {
			t.Errorf("Retire = %v, want none: the only Ready current-generation server is "+
				"condemned, so it is not a replacement to retire against", got.Retire)
		}
		if len(got.Condemn) != 1 || got.Condemn[0] != "new" {
			t.Errorf("Condemn = %v, want [new]: the node drain still takes it", got.Condemn)
		}
	})

	t.Run("a condemned server already reserved for delete is not named again", func(t *testing.T) {
		in := ScalingInputs{
			Views:       []ServerView{{Name: "a", Phase: phase.Ready, Slots: 10, Condemned: true}},
			MinReplicas: 1, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
			PendingDeletes: map[string]bool{"a": true},
		}
		if got := DecideSize(in); len(got.Condemn) != 0 {
			t.Fatalf("Condemn = %v, want none: %q already has a reserved delete", got.Condemn, "a")
		}
	})
}

func TestDecideSizeCapacityEditRetiresNothing(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:          []ServerView{ready("a", 10, 100), ready("b", 10, 100)},
		MinReplicas:    2,
		MaxReplicas:    10,
		SpareSlots:     40,
		MaxPlayers:     100,
		PodHash:        "current",
		MaxUnavailable: 1,
	})
	if len(got.Retire) != 0 {
		t.Fatalf("a capacity edit retired %v, want nothing", got.Retire)
	}
}

// Guards the case above: a rule that retired nothing would satisfy it.
func TestDecideSizeImageEditStillRetires(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:          []ServerView{staleReady("a", 10, 100, "old"), ready("b", 10, 100)},
		MinReplicas:    2,
		MaxReplicas:    10,
		SpareSlots:     40,
		MaxPlayers:     100,
		PodHash:        "current",
		MaxUnavailable: 1,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "a" {
		t.Fatalf("Retire = %v, want exactly [a]", got.Retire)
	}
}

// Otherwise the first reconcile after an operator upgrade is a full fleet changeover.
func TestDecideSizeAdoptsHashlessServers(t *testing.T) {
	a, b := ready("a", 10, 100), ready("b", 10, 100)
	a.PodHash, b.PodHash = "", ""
	got := DecideSize(ScalingInputs{
		Views:          []ServerView{a, b},
		MinReplicas:    2,
		MaxReplicas:    10,
		SpareSlots:     40,
		MaxPlayers:     100,
		PodHash:        "fresh",
		MaxUnavailable: 1,
	})
	if len(got.Retire) != 0 {
		t.Fatalf("hashless servers were retired: %v", got.Retire)
	}
}

func TestABoostRaisesTheFloor(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100,
		Boost:   2,
		PodHash: "current",
	})
	if got.Create != 3 {
		t.Errorf("Create = %d, want 3: a floor of 1 plus a boost of 2", got.Create)
	}
}

func TestTheCeilingStillBindsAgainstABoost(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 1, MaxReplicas: 2,
		SpareSlots: 40, MaxPlayers: 100,
		Boost:   50,
		PodHash: "current",
	})
	if got.Create > 2 {
		t.Errorf("Create = %d, want at most the ceiling of 2", got.Create)
	}
}

func TestABoostOfZeroChangesNothing(t *testing.T) {
	base := ScalingInputs{MinReplicas: 2, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100, PodHash: "current"}
	boosted := base
	boosted.Boost = 0

	if with, without := DecideSize(boosted), DecideSize(base); with.Create != without.Create {
		t.Errorf("a zero boost changed the decision: %d vs %d", with.Create, without.Create)
	}
}

func TestABoostAlsoHoldsCapacityAgainstAScaleDown(t *testing.T) {
	// A boost that reached the create rule but not this one would build servers and shed them next pass.
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{ready("a", 0, 100), ready("b", 0, 100), ready("c", 0, 100)},
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 0,
		Boost:         2,
		PodHash:       "current",
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: three servers is exactly the boosted floor", got.Delete)
	}
}

func TestAGroupWhoseEveryServerIsPlayingBuildsARoom(t *testing.T) {
	// With one closed server rounding hides the miscount; it takes every server shut before 156 free seats clear the bar.
	in := ScalingInputs{
		MaxReplicas: 10,
		MaxPlayers:  80,
		SpareSlots:  80,
		Views: []ServerView{
			{Name: "a", Phase: phase.Ready, Registered: true, JoinsClosed: true, Players: 2, Slots: 80},
			{Name: "b", Phase: phase.Ready, Registered: true, JoinsClosed: true, Players: 2, Slots: 80},
		},
		PendingDeletes: map[string]bool{},
	}

	if got := decideSize(in).Create; got < 1 {
		t.Errorf("create = %d, want at least 1 — nobody can join either running round", got)
	}
}

// The open, empty server is the only one anybody can join.
func TestDemandDoesNotShedTheOnlyJoinableServer(t *testing.T) {
	a := ready("a", 2, 80)
	a.JoinsClosed = true
	b := ready("b", 2, 80)
	b.JoinsClosed = true
	in := ScalingInputs{
		MinReplicas: 1, MaxReplicas: 10, MaxPlayers: 80, SpareSlots: 80,
		Stabilization: time.Minute,
		Views:         []ServerView{a, b, empty("c", 80, 5*time.Minute)},
	}

	got := DecideSize(in)

	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: c is the only server whose door is open", got.Delete)
	}
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0: c's 80 seats satisfy the spare", got.Create)
	}
}

func TestReadyContributionReadsTheSameDoorAsAggregateGroup(t *testing.T) {
	closed := ready("a", 2, 80)
	closed.JoinsClosed = true
	if got := readyContribution(closed); got != 0 {
		t.Errorf("readyContribution = %d, want 0 behind a closed door", got)
	}
	dropped := ready("a", 2, 80)
	dropped.Registered = false
	if got := readyContribution(dropped); got != 0 {
		t.Errorf("readyContribution = %d, want 0 for a server the proxies do not have", got)
	}
	if got := readyContribution(ready("a", 2, 80)); got != 78 {
		t.Errorf("readyContribution = %d, want 78 for an open, registered server", got)
	}
}

// A server that lost its probe with its stream up is deregistered; its seats are unreachable.
func TestProvisionalCapacityDoesNotCreditAServerTheProxiesDropped(t *testing.T) {
	dropped := ServerView{
		Name: "a", Phase: phase.Starting, Slots: 80, Players: 5,
		WasRegistered: true, Registered: false,
	}
	if got := provisionalCapacity(dropped, 80); got != 0 {
		t.Errorf("provisionalCapacity = %d, want 0 for a server the proxies dropped", got)
	}
	dropped.Registered = true
	if got := provisionalCapacity(dropped, 80); got != 75 {
		t.Errorf("provisionalCapacity = %d, want 75 once the proxies have it again", got)
	}
}

// After an operator restart every count is stale, not missing seats: crediting nothing built one extra server per group.
func TestProvisionalCapacityStillCreditsAServerARestartedOperatorHasNotHeardFrom(t *testing.T) {
	unheard := ServerView{
		Name: "a", Phase: phase.Starting, Stale: true,
		WasRegistered: true, Registered: false,
	}
	if got := provisionalCapacity(unheard, 80); got != 80 {
		t.Errorf("provisionalCapacity = %d, want the full 80 for a server the operator has not heard from since it started", got)
	}
}

// floorInputs is a changeover with room: two stale servers carrying players,
// one Ready replacement, budget for two retirements.
func floorInputs(minAvailable int32, views ...ServerView) ScalingInputs {
	return ScalingInputs{
		Views:   views,
		PodHash: "current", MaxUnavailable: 2, MinAvailable: minAvailable,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	}
}

func TestDecideSizeKeepsTheFloorOfJoinableServers(t *testing.T) {
	got := DecideSize(floorInputs(3,
		staleReady("old1", 10, 100, "old"),
		staleReady("old2", 10, 100, "old"),
		ready("new", 0, 100),
	))
	if len(got.Retire) != 0 {
		t.Fatalf("Retire = %v, want none: three joinable, floor three", got.Retire)
	}
	if got.Create != 1 || !got.FloorHeld || got.Joinable != 3 {
		t.Errorf("Create = %d FloorHeld = %v Joinable = %d, want one extra server for the floor of 3 joinable",
			got.Create, got.FloorHeld, got.Joinable)
	}
}

func TestDecideSizeRetiresAboveTheFloor(t *testing.T) {
	got := DecideSize(floorInputs(2,
		staleReady("old1", 10, 100, "old"),
		staleReady("old2", 10, 100, "old"),
		ready("new", 0, 100),
	))
	if len(got.Retire) != 1 || got.Retire[0] != "old1" || got.FloorHeld {
		t.Errorf("Retire = %v FloorHeld = %v, want [old1]: two stay joinable", got.Retire, got.FloorHeld)
	}
}

func TestDecideSizeRetiresAClosedDoorWithoutTouchingTheFloor(t *testing.T) {
	closed := staleReady("zzz", 10, 100, "old")
	closed.JoinsClosed = true
	got := DecideSize(floorInputs(2,
		staleReady("old2", 10, 100, "old"),
		closed,
		ready("new", 0, 100),
	))
	if len(got.Retire) != 1 || got.Retire[0] != "zzz" {
		t.Errorf("Retire = %v, want [zzz]: it is not joinable, so retiring it keeps the two that are", got.Retire)
	}
}

func TestDecideSizeBuildsOnlyOneExtraServerAtATime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   []ServerView
		pending int32
	}{
		{"the extra server is starting", []ServerView{starting("surge")}, 0},
		{"the extra server is pending", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := floorInputs(3, append([]ServerView{
				staleReady("old1", 10, 100, "old"),
				staleReady("old2", 10, 100, "old"),
				ready("new", 0, 100),
			}, tc.extra...)...)
			in.PendingCreates = tc.pending
			got := DecideSize(in)
			if got.Create != 0 || len(got.Retire) != 0 || !got.FloorHeld {
				t.Errorf("Create = %d Retire = %v FloorHeld = %v, want nothing new while the extra server comes up",
					got.Create, got.Retire, got.FloorHeld)
			}
		})
	}
}

func TestDecideSizeReportsTheFloorBlockedAtTheCeiling(t *testing.T) {
	busy := ready("busy", 20, 100)
	busy.JoinsClosed = true
	in := floorInputs(3,
		staleReady("old1", 10, 100, "old"),
		staleReady("old2", 10, 100, "old"),
		ready("new", 0, 100),
		busy,
	)
	in.MaxReplicas = 4
	got := DecideSize(in)
	if got.Create != 0 || len(got.Retire) != 0 {
		t.Fatalf("Create = %d Retire = %v, want nothing: at the ceiling with the floor reached", got.Create, got.Retire)
	}
	if !got.FloorHeld || !got.FloorBlocked || !got.Limited {
		t.Errorf("FloorHeld = %v FloorBlocked = %v Limited = %v, want all true", got.FloorHeld, got.FloorBlocked, got.Limited)
	}
}

func TestDecideSizeHoldsTheFloorAgainstScaleDownDuringAChangeover(t *testing.T) {
	retiring := staleReady("old1", 40, 100, "old")
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	idle := staleReady("old2", 0, 100, "old")
	idle.EmptyFor = time.Hour
	in := floorInputs(2, retiring, idle, ready("new", 60, 100))
	in.MaxUnavailable = 1
	got := DecideSize(in)
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: old2 and new are the two joinable servers the floor keeps", got.Delete)
	}
}

func TestDecideSizeIgnoresTheFloorOutsideAChangeover(t *testing.T) {
	a := ready("a", 0, 100)
	a.EmptyFor = time.Hour
	b := ready("b", 0, 100)
	b.EmptyFor = time.Hour
	got := DecideSize(floorInputs(5, a, b))
	if len(got.Delete) != 1 {
		t.Errorf("Delete = %v, want one: nothing is stale, so the floor does not apply", got.Delete)
	}
}

func TestDecideSizeDoesNotCountAReservedRetirementAsJoinable(t *testing.T) {
	in := floorInputs(2,
		staleReady("old1", 10, 100, "old"),
		staleReady("old2", 10, 100, "old"),
		ready("new", 0, 100),
	)
	in.PendingRetires = map[string]bool{"old1": true}
	got := DecideSize(in)
	if len(got.Retire) != 0 || got.Create != 1 {
		t.Errorf("Retire = %v Create = %d, want no retirement and one extra server: "+
			"old1 is already going, so old2 and new are the only joinable two", got.Retire, got.Create)
	}
}

func whenEmptyInputs(views ...ServerView) ScalingInputs {
	in := floorInputs(0, views...)
	in.WhenEmpty = true
	return in
}

func TestWhenEmptyLeavesAnOccupiedServer(t *testing.T) {
	got := DecideSize(whenEmptyInputs(staleReady("old", 10, 100, "old"), ready("new", 0, 100)))
	if len(got.Retire) != 0 || got.FloorHeld || got.Create != 0 {
		t.Errorf("Retire = %v FloorHeld = %v Create = %d, want nothing: the round on old goes on",
			got.Retire, got.FloorHeld, got.Create)
	}
}

func TestWhenEmptyRetiresAnEmptyServer(t *testing.T) {
	got := DecideSize(whenEmptyInputs(staleReady("old", 0, 100, "old"), ready("new", 0, 100)))
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Errorf("Retire = %v, want [old]", got.Retire)
	}
}

func TestWhenEmptyLeavesAServerWithAnUntrustedCount(t *testing.T) {
	quiet := staleReady("old", 0, 100, "old")
	quiet.Stale = true
	got := DecideSize(whenEmptyInputs(quiet, ready("new", 0, 100)))
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none: a count nobody can trust reads as occupied", got.Retire)
	}
}

func TestWhenEmptyRetiresTheEmptyServerBesideAnOccupiedOne(t *testing.T) {
	got := DecideSize(whenEmptyInputs(
		staleReady("a", 5, 100, "old"),
		staleReady("b", 0, 100, "old"),
		ready("new", 0, 100),
	))
	if len(got.Retire) != 1 || got.Retire[0] != "b" {
		t.Errorf("Retire = %v, want [b]", got.Retire)
	}
}

func TestWhenEmptyKeepsTheFloor(t *testing.T) {
	in := whenEmptyInputs(staleReady("old", 0, 100, "old"), ready("new", 0, 100))
	in.MinAvailable = 2
	got := DecideSize(in)
	if len(got.Retire) != 0 || got.Create != 1 {
		t.Errorf("Retire = %v Create = %d, want an extra server before the empty old one goes", got.Retire, got.Create)
	}
}

func TestWhenEmptyStillShrinksTheNewGenerationBesideABusyStaleServer(t *testing.T) {
	views := []ServerView{staleReady("hub", 30, 100, "old")}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		v := ready(name, 0, 100)
		v.EmptyFor = time.Hour
		views = append(views, v)
	}
	got := DecideSize(whenEmptyInputs(views...))
	if len(got.Delete) != 1 {
		t.Errorf("Delete = %v, want one idle current server: the busy stale hub may never empty", got.Delete)
	}
}

func TestWhenEmptyKeepsTheLastCurrentServer(t *testing.T) {
	idle := ready("new", 0, 100)
	idle.EmptyFor = time.Hour
	got := DecideSize(whenEmptyInputs(staleReady("hub", 30, 100, "old"), idle))
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: without it the next pass would cold start it again", got.Delete)
	}
}

func held(v ServerView) ServerView { v.Hold = true; return v }

func TestAHeldServerIsNeverRetired(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:   []ServerView{held(staleReady("old", 10, 100, "old")), ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none: the server is held", got.Retire)
	}
}

func TestAHeldServerIsNeverDeletedForDemandOrTheCeiling(t *testing.T) {
	idle := held(ready("idle", 0, 100))
	idle.EmptyFor = time.Hour
	other := ready("other", 0, 100)
	other.EmptyFor = time.Hour
	for _, max := range []int32{10, 1} {
		got := DecideSize(ScalingInputs{
			Views:       []ServerView{idle, other},
			MinReplicas: 0, MaxReplicas: max, SpareSlots: 0, MaxPlayers: 100,
			Stabilization: time.Minute,
		})
		for _, name := range got.Delete {
			if name == "idle" {
				t.Errorf("maxReplicas %d: Delete = %v, want the held server kept", max, got.Delete)
			}
		}
	}
}

func TestHoldEndsTheChangeoverForItsServer(t *testing.T) {
	in := ScalingInputs{
		Views:   []ServerView{held(staleReady("old", 10, 100, "old"))},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	}
	if staleRemains(in) || coldStart(in) {
		t.Errorf("staleRemains = %v coldStart = %v, want both false for a held server", staleRemains(in), coldStart(in))
	}
}

func TestANodeDrainStillCondemnsAHeldServer(t *testing.T) {
	v := held(ready("held", 5, 100))
	v.Condemned = true
	got := DecideSize(ScalingInputs{Views: []ServerView{v}, MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100})
	if len(got.Condemn) != 1 {
		t.Errorf("Condemn = %v, want the held server: its node is leaving", got.Condemn)
	}
}

func TestAFullLobbyWithHeadroomOrdersTheNextServer(t *testing.T) {
	lobby := ready("duels-a", 12, 100)
	lobby.Playable = 12
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{lobby},
		MinReplicas: 1, MaxReplicas: 4,
		SpareSlots: 1, MaxPlayers: 100, PlayableSlots: 12,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want 1: a lobby full at its playable seats has no room", got.Create)
	}
}

func TestWithoutPlayableSlotsAFullLobbyStillReadsAsRoom(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{ready("duels-a", 12, 100)},
		MinReplicas: 1, MaxReplicas: 4,
		SpareSlots: 1, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0: without the field 88 seats are free, as before", got.Create)
	}
}

func TestTheCapacityUnitIsThePlayableFigure(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots: 24, MaxPlayers: 100, PlayableSlots: 12,
	})
	if got.Create != 2 {
		t.Errorf("Create = %d, want 2 servers of 12 playable seats for 24 spare", got.Create)
	}
}

func TestAPendingCreateCountsItsPlayableSeats(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots: 24, MaxPlayers: 100, PlayableSlots: 12,
		PendingCreates: 1,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want 1 more: the pending one brings 12, not 100", got.Create)
	}
}

func TestProvisionalCapacityCreditsAStartingServerItsPlayableSeats(t *testing.T) {
	if got := provisionalCapacity(starting("a"), 12); got != 12 {
		t.Errorf("provisionalCapacity = %d, want the group's 12", got)
	}
	full := ready("b", 14, 100)
	full.Playable = 12
	if got := provisionalCapacity(full, 12); got != 0 {
		t.Errorf("provisionalCapacity = %d, want 0 for a server past its playable seats", got)
	}
}

func TestReadyContributionCountsPlayableSeats(t *testing.T) {
	v := ready("a", 9, 100)
	v.Playable = 12
	if got := readyContribution(v); got != 3 {
		t.Errorf("readyContribution = %d, want 3", got)
	}
}
