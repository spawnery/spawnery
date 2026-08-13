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
			// 4a does not roll updates, so it does not read the generation at
			// all. A rule that did would order a full replacement set on every
			// spec edit, because every edit raises the group's generation.
			name: "a server of another generation still credits its capacity",
			in: ScalingInputs{
				Views: []ServerView{{
					Name: "a", Phase: phase.Ready, Slots: 100,
					WasRegistered: true, Generation: 7,
				}},
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

// TestDecideSizeShortOfCapacityNeverAlsoShrinks pins the early return: three
// servers against a ceiling of one is a surplus, and the group is also 700
// slots short. Only one of those two answers may come out of a single pass,
// and it is the one that does not disturb anybody.
func TestDecideSizeShortOfCapacityNeverAlsoShrinks(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			ready("a", 0, 100), ready("b", 0, 100), ready("c", 0, 100),
		},
		MinReplicas: 1, MaxReplicas: 1,
		SpareSlots: 1000, MaxPlayers: 100,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v while the group is short of capacity, want none", got.Delete)
	}
	if got.Create != 0 || got.Wanted != 7 || !got.Limited {
		t.Errorf("Create = %d, Wanted = %d, Limited = %v; want 0, 7 and true",
			got.Create, got.Wanted, got.Limited)
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
