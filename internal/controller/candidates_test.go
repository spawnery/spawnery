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

func view(name string, p phase.Phase, players, slots int32, stale bool, gen int64, ageSeconds int) ServerView {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	return ServerView{
		Name:    name,
		Phase:   p,
		Players: players,
		Slots:   slots,
		Stale:   stale,
		// A server that walked the normal path was registered once it reached
		// Ready and never before. The cases where the two come apart — a server
		// that fell back out of Ready with its players still on it — are built
		// with registered() below.
		WasRegistered: p == phase.Ready,
		Generation:    gen,
		CreatedAt:     base.Add(time.Duration(ageSeconds) * time.Second),
	}
}

// registered marks a view as one the proxies routed to at some point in the
// life of its pod, whatever phase it is in now.
func registered(v ServerView) ServerView {
	v.WasRegistered = true
	return v
}

// sessionsGone marks a view whose pod is terminal or has disappeared, so
// whatever it was carrying is already gone.
func sessionsGone(v ServerView) ServerView {
	v.SessionsGone = true
	return v
}

func TestSelectDeletionCandidates(t *testing.T) {
	cases := []struct {
		name  string
		views []ServerView
		count int
		want  []string
	}{
		{
			name: "picks the youngest empty server",
			views: []ServerView{
				view("old", phase.Ready, 0, 100, false, 1, 0),
				view("young", phase.Ready, 0, 100, false, 1, 60),
			},
			count: 1,
			want:  []string{"young"},
		},
		{
			name: "never picks an occupied server",
			views: []ServerView{
				view("busy", phase.Ready, 3, 100, false, 1, 60),
				view("empty", phase.Ready, 0, 100, false, 1, 0),
			},
			count: 1,
			want:  []string{"empty"},
		},
		{
			name: "treats a stale count as occupied",
			views: []ServerView{
				view("stale", phase.Ready, 0, 100, true, 1, 60),
				view("fresh", phase.Ready, 0, 100, false, 1, 0),
			},
			count: 1,
			want:  []string{"fresh"},
		},
		{
			name: "returns fewer than asked when nothing else is free",
			views: []ServerView{
				view("busy-a", phase.Ready, 1, 100, false, 1, 0),
				view("busy-b", phase.Ready, 2, 100, false, 1, 60),
			},
			count: 2,
			want:  nil,
		},
		{
			name: "prefers servers that never took players over ready ones",
			views: []ServerView{
				view("ready", phase.Ready, 0, 100, false, 1, 60),
				view("pending", phase.Pending, 0, 0, true, 1, 0),
			},
			count: 1,
			want:  []string{"pending"},
		},
		{
			name: "ignores servers that are already going away",
			views: []ServerView{
				view("draining", phase.Draining, 0, 100, false, 1, 0),
				view("terminating", phase.Terminating, 0, 100, false, 1, 0),
				view("ready", phase.Ready, 0, 100, false, 1, 60),
			},
			count: 3,
			want:  []string{"ready"},
		},
		{
			// The phase says Starting, but the server was Ready until its probe
			// went red and its players are still connected — deregistering only
			// stopped new joins. Its count is unreadable, so it must be left
			// alone even though the empty peer is older.
			name: "never picks a server that kept its players after a readiness loss",
			views: []ServerView{
				registered(view("recovering", phase.Starting, 0, 100, true, 1, 60)),
				view("empty", phase.Ready, 0, 100, false, 1, 0),
			},
			count: 1,
			want:  []string{"empty"},
		},
		{
			// A Failed server is not part of the group's size, so removing it
			// does nothing for the size — and it is being kept on purpose so
			// somebody can look at it. Its cleanup is the Server controller's
			// job once the group's failed retention has elapsed.
			name: "leaves a failed server to its retention",
			views: []ServerView{
				view("failed", phase.Failed, 0, 100, false, 1, 60),
				view("ready", phase.Ready, 0, 100, false, 1, 0),
			},
			count: 2,
			want:  []string{"ready"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectDeletionCandidates(tc.views, tc.count)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestSelectNeverReturnsAnOccupiedServer is the core invariant at the
// selection layer: no combination of inputs may nominate a server carrying
// players, or one whose count we cannot trust.
func TestSelectNeverReturnsAnOccupiedServer(t *testing.T) {
	views := []ServerView{
		view("a", phase.Ready, 1, 100, false, 1, 0),
		view("b", phase.Ready, 0, 100, true, 1, 10),
		view("c", phase.Starting, 0, 100, true, 1, 20),
		view("d", phase.Ready, 99, 100, false, 1, 30),
		registered(view("e", phase.Starting, 0, 100, true, 1, 40)),
	}
	occupied := map[string]bool{"a": true, "b": true, "d": true, "e": true}

	for count := 0; count <= len(views)+2; count++ {
		for _, name := range SelectDeletionCandidates(views, count) {
			if occupied[name] {
				t.Errorf("count=%d nominated the occupied server %q", count, name)
			}
		}
	}
}

func TestAggregateGroup(t *testing.T) {
	views := []ServerView{
		view("current-a", phase.Ready, 20, 100, false, 7, 0),
		view("current-b", phase.Ready, 10, 100, false, 7, 10),
		view("stale-gen", phase.Ready, 40, 100, false, 6, 20),
		view("starting", phase.Starting, 0, 100, true, 7, 30),
		view("draining", phase.Draining, 5, 100, false, 7, 40),
	}

	got := AggregateGroup(views, 7)

	if got.Replicas != 5 {
		t.Errorf("Replicas = %d, want 5", got.Replicas)
	}
	if got.ReadyReplicas != 3 {
		t.Errorf("ReadyReplicas = %d, want 3 (two current plus the stale generation)", got.ReadyReplicas)
	}
	if got.OnlinePlayers != 75 {
		t.Errorf("OnlinePlayers = %d, want 75 across every server that has players", got.OnlinePlayers)
	}
	// 80 free on current-a plus 90 on current-b. The old generation does not
	// count, otherwise a rolling update would never create replacements, and
	// neither do the starting and draining servers.
	if got.FreeSlots != 170 {
		t.Errorf("FreeSlots = %d, want 170 from the current generation only", got.FreeSlots)
	}
}

func TestAggregateIgnoresStaleCountsForFreeSlots(t *testing.T) {
	views := []ServerView{
		view("fresh", phase.Ready, 0, 100, false, 7, 0),
		view("stale", phase.Ready, 0, 100, true, 7, 10),
	}
	if got := AggregateGroup(views, 7).FreeSlots; got != 100 {
		t.Errorf("FreeSlots = %d, want 100 — a server we cannot measure offers no free slots", got)
	}
}

// TestCountsTowardSize pins which servers hold the group at its floor. A server
// on its way out no longer does, and neither does a Failed one: it is kept for
// diagnosis and can take no player, so counting it would leave the group below
// its floor for the whole retention window.
func TestCountsTowardSize(t *testing.T) {
	cases := []struct {
		p    phase.Phase
		want bool
	}{
		{phase.Pending, true},
		{phase.Starting, true},
		{phase.Ready, true},
		{phase.Draining, false},
		{phase.Terminating, false},
		{phase.Failed, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.p), func(t *testing.T) {
			if got := view("s", tc.p, 0, 100, false, 1, 0).countsTowardSize(); got != tc.want {
				t.Errorf("countsTowardSize() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOccupiedPodsCountsEveryProtectedPod pins the number the group's
// PodDisruptionBudget is built from. It has to match the rule the Server
// controller labels pods with, pod for pod. Counting fewer leaves the eviction
// API with a disruption to spend on a pod that still carries players; counting
// more pins minAvailable above a budget that can never be met.
func TestOccupiedPodsCountsEveryProtectedPod(t *testing.T) {
	views := []ServerView{
		view("ready-busy", phase.Ready, 4, 100, false, 1, 0),
		view("ready-empty", phase.Ready, 0, 100, false, 1, 10),
		view("ready-stale", phase.Ready, 0, 100, true, 1, 20),
		view("draining-busy", phase.Draining, 2, 100, false, 1, 30),
		// Registered, so its players outlived the readiness loss that put it
		// back in Starting.
		registered(view("starting-stale", phase.Starting, 0, 100, true, 1, 40)),
		view("terminating-empty", phase.Terminating, 0, 100, false, 1, 50),
	}
	if got := occupiedPods(views); got != 4 {
		t.Errorf("occupiedPods = %d, want 4 — the draining and starting pods stay labelled and must stay protected", got)
	}
}

// TestOccupiedPodsReleasesPodsThatCarryNobody is the other direction of the
// same rule. A pod the label has released must not be counted, or minAvailable
// sits above a budget that can never be met and the eviction API refuses to
// release the pod for a kubectl drain that then never finishes.
func TestOccupiedPodsReleasesPodsThatCarryNobody(t *testing.T) {
	cases := []struct {
		name string
		v    ServerView
	}{
		{
			// Failed with a dead pod: the state machine refuses to drain it
			// because its sessions went down with the process.
			name: "failed with a terminal pod",
			v:    sessionsGone(registered(view("dead", phase.Failed, 0, 100, true, 1, 0))),
		},
		{
			// The same dead pod, but the registry still holds the count it
			// reported just before it died. Nothing ever tells the registry to
			// forget a pod, so that seven survives for the whole retention
			// window. Sessions that went down with the process are gone whatever
			// the last count said — otherwise this one pod pins minAvailable at
			// 1 against a currentHealthy of 0 and wedges an operator's drain.
			name: "failed with a terminal pod and seven players last seen on it",
			v:    sessionsGone(registered(view("dead-busy", phase.Failed, 7, 100, false, 1, 0))),
		},
		{
			// Never registered, so the proxies never routed anybody to it, so
			// an unreadable count hides nothing.
			name: "starting and never registered",
			v:    view("cold", phase.Starting, 0, 100, true, 1, 0),
		},
		{
			name: "pending without a pod",
			v:    view("waiting", phase.Pending, 0, 0, true, 1, 0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := occupiedPods([]ServerView{tc.v}); got != 0 {
				t.Errorf("occupiedPods = %d, want 0 — this pod carries nobody and must not hold the budget", got)
			}
		})
	}
}

// TestSelectFailedForPruning pins the retention cap and, above all, which
// failure survives it: the oldest, because the first failure after a change is
// the one that says what broke.
func TestSelectFailedForPruning(t *testing.T) {
	cases := []struct {
		name  string
		views []ServerView
		keep  int
		want  []string
	}{
		{
			name: "keeps a lone failure",
			views: []ServerView{
				view("failed", phase.Failed, 0, 100, false, 1, 0),
				view("ready", phase.Ready, 0, 100, false, 1, 10),
			},
			keep: 1,
			want: nil,
		},
		{
			name: "keeps the oldest failure and prunes the rest",
			views: []ServerView{
				view("second", phase.Failed, 0, 100, false, 1, 60),
				view("first", phase.Failed, 0, 100, false, 1, 0),
				view("third", phase.Failed, 0, 100, false, 1, 120),
			},
			keep: 1,
			want: []string{"second", "third"},
		},
		{
			name: "never prunes a server that is not failed",
			views: []ServerView{
				view("failed", phase.Failed, 0, 100, false, 1, 0),
				view("ready-a", phase.Ready, 0, 100, false, 1, 10),
				view("ready-b", phase.Ready, 0, 100, false, 1, 20),
				view("draining", phase.Draining, 0, 100, false, 1, 30),
			},
			keep: 1,
			want: nil,
		},
		{
			name: "falls back to the name when two failed in the same second",
			views: []ServerView{
				view("b", phase.Failed, 0, 100, false, 1, 0),
				view("a", phase.Failed, 0, 100, false, 1, 0),
			},
			keep: 1,
			want: []string{"b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectFailedForPruning(tc.views, tc.keep)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestClampReport(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		players, slots, maxPlay int32
		wantPlayers, wantSlots  int32
	}{
		{"an honest report passes through", 7, 100, 100, 7, 100},
		{"a smaller capacity than the group's is the agent's business", 7, 40, 100, 7, 40},
		{"slots above the group's capacity are cut to it", 0, 1000000, 100, 0, 100},
		{"players follow the cut capacity down", 900, 1000, 100, 100, 100},
		{"a zero capacity leaves nothing", 5, 50, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotPlayers, gotSlots := clampReport(tc.players, tc.slots, tc.maxPlay)
			if gotPlayers != tc.wantPlayers || gotSlots != tc.wantSlots {
				t.Errorf("clampReport(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tc.players, tc.slots, tc.maxPlay,
					gotPlayers, gotSlots, tc.wantPlayers, tc.wantSlots)
			}
		})
	}
}
