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

package phase

import (
	"testing"
	"time"
)

// healthyReady is the input set of a server that is fine in phase Ready.
func healthyReady() Inputs {
	return Inputs{
		PodExists:      true,
		PodRunning:     true,
		PodReady:       true,
		AgentReady:     true,
		AgentConnected: true,
		Slots:          100,
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name    string
		current Phase
		in      Inputs
		want    Decision
	}{
		{
			name:    "pending stays pending without a pod",
			current: Pending,
			in:      Inputs{},
			want:    Decision{Next: Pending, Reason: ReasonPodPending},
		},
		{
			name:    "pending advances once the pod runs",
			current: Pending,
			in:      Inputs{PodExists: true, PodRunning: true},
			want:    Decision{Next: Starting, Reason: ReasonPodRunning},
		},
		{
			name:    "starting waits for the agent when only the probe is green",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, PodReady: true},
			want:    Decision{Next: Starting, Reason: ReasonPodPending},
		},
		{
			name:    "starting waits for the probe when only the agent is ready",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, AgentReady: true},
			want:    Decision{Next: Starting, Reason: ReasonPodPending},
		},
		{
			name:    "starting becomes ready on both signals and registers",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, PodReady: true, AgentReady: true},
			want:    Decision{Next: Ready, Register: true, Reason: ReasonReadyGatePassed},
		},
		{
			// Regression for Minor 2: the ready gate must not trust a green
			// probe and agent alone. Task 8 should never send PodReady/AgentReady
			// without PodExists/PodRunning, but this package must not depend on
			// caller discipline.
			name:    "starting does not skip to ready on contradictory inputs without the pod existing and running",
			current: Starting,
			in:      Inputs{PodExists: false, PodRunning: false, PodReady: true, AgentReady: true},
			want:    Decision{Next: Starting, Reason: ReasonPodPending},
		},
		{
			name:    "ready stays ready while healthy",
			current: Ready,
			in:      healthyReady(),
			want:    Decision{Next: Ready, Reason: ReasonReadyGatePassed},
		},
		{
			name:    "ready falls back to starting when the probe turns red",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.PodReady = false
				return in
			}(),
			want: Decision{Next: Starting, Deregister: true, CountReadinessLoss: true, Reason: ReasonReadinessLost},
		},
		{
			// A live stream that reports not-ready is the agent telling us
			// something, not us failing to hear it: no grace period applies.
			name:    "ready falls back to starting at once when a live agent reports not ready",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentReady = false
				return in
			}(),
			want: Decision{Next: Starting, Deregister: true, CountReadinessLoss: true, Reason: ReasonReadinessLost},
		},
		{
			name:    "ready falls back to starting when the agent stream is down too long",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentConnected = false
				in.AgentStreamDownFor = StreamDownGrace
				return in
			}(),
			want: Decision{Next: Starting, Deregister: true, CountReadinessLoss: true, Reason: ReasonReadinessLost},
		},
		{
			name:    "ready tolerates a short stream gap",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentConnected = false
				in.AgentStreamDownFor = StreamDownGrace - time.Millisecond
				return in
			}(),
			want: Decision{Next: Ready, Reason: ReasonReadyGatePassed},
		},
		{
			// The exact shape the agent registry emits after Disconnect: it
			// clears ready and starts the clock, so a Ready server inside the
			// grace window arrives here as neither ready nor connected. This is
			// the composition that made the StreamDownGrace clause unreachable
			// before — inside the grace only the timer may decide.
			name:    "ready tolerates a dropped stream whose agent has not reported ready since",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentReady = false
				in.AgentConnected = false
				in.AgentStreamDownFor = StreamDownGrace - time.Millisecond
				return in
			}(),
			want: Decision{Next: Ready, Reason: ReasonReadyGatePassed},
		},
		{
			name:    "ready resets the flap counter after a long healthy stretch",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.ReadinessLosses = 2
				in.ReadyFor = FlapResetWindow
				return in
			}(),
			want: Decision{Next: Ready, ResetReadinessLosses: true, Reason: ReasonReadyGatePassed},
		},
		{
			name:    "flapping past the threshold fails and deregisters",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.ReadinessLosses = MaxReadinessLosses
				return in
			}(),
			want: Decision{Next: Failed, Deregister: true, Reason: ReasonFlapping},
		},
		{
			name:    "a terminal pod fails the server",
			current: Starting,
			in:      Inputs{PodExists: true, PodTerminal: true},
			want:    Decision{Next: Failed, Reason: ReasonPodTerminal},
		},
		{
			name:    "a server that never becomes ready fails on the startup deadline",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, StartupDeadlineReached: true},
			want:    Decision{Next: Failed, Reason: ReasonStartupTimeout},
		},
		{
			// status.startedAt is written once at pod creation and never
			// refreshed, so StartupDeadlineReached stays true for the whole life
			// of a long-lived server. Without the WasRegistered guard, one probe
			// blip on a server that has been serving for hours would fail it —
			// and the Failed retention would then delete its pod undrained.
			name:    "the startup deadline does not fail a server that was already registered",
			current: Starting,
			in: Inputs{
				PodExists: true, PodRunning: true, StartupDeadlineReached: true,
				WasRegistered: true, PlayersOnline: 40, ReadinessLosses: 1,
			},
			want: Decision{Next: Starting, Reason: ReasonPodPending},
		},
		{
			// Flapping is the bound for a server that was once playable, and it
			// must take its players with it: the readiness-loss fallback only
			// deregistered, it never moved anyone off.
			name:    "failing on flapping drains a server that still has players",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.ReadinessLosses = MaxReadinessLosses
				in.WasRegistered = true
				in.PlayersOnline = 12
				return in
			}(),
			want: Decision{Next: Failed, Deregister: true, StartDrain: true, Reason: ReasonFlapping},
		},
		{
			// A terminal pod means the process is already down: there is nobody
			// left to move, so draining would be pointless.
			name:    "failing on a terminal pod does not try to drain",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.PodTerminal = true
				in.WasRegistered = true
				in.PlayersOnline = 12
				return in
			}(),
			want: Decision{Next: Failed, Deregister: true, Reason: ReasonPodTerminal},
		},
		{
			name:    "a failed server with players is drained instead of cleaned up at the retention",
			current: Failed,
			in: Inputs{
				FailedRetentionElapsed: true, WasRegistered: true, PlayersOnline: 5,
			},
			want: Decision{Next: Failed, StartDrain: true, Reason: ReasonDrainingBeforeCleanup},
		},
		{
			name:    "a failed server with a stale count is drained, not cleaned up",
			current: Failed,
			in: Inputs{
				FailedRetentionElapsed: true, WasRegistered: true, PlayersStale: true,
			},
			want: Decision{Next: Failed, StartDrain: true, Reason: ReasonDrainingBeforeCleanup},
		},
		{
			// The escape hatch: one stuck player must not pin a failed server
			// forever.
			name:    "a failed server is cleaned up once its drain deadline passes",
			current: Failed,
			in: Inputs{
				FailedRetentionElapsed: true, WasRegistered: true, PlayersOnline: 5,
				DrainDeadlineReached: true,
			},
			want: Decision{Next: Terminating, DeletePod: true, Reason: ReasonRetentionElapsed},
		},
		{
			name:    "deleting an occupied failed server drains it",
			current: Failed,
			in: Inputs{
				DeletionRequested: true, WasRegistered: true, PlayersOnline: 5,
			},
			want: Decision{Next: Failed, StartDrain: true, Reason: ReasonDrainingBeforeCleanup},
		},
		{
			name:    "deleting an empty failed server terminates it",
			current: Failed,
			in:      Inputs{DeletionRequested: true, WasRegistered: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDeletionRequested},
		},
		{
			// Never registered means no session was ever routed here, so there
			// is nothing to move off.
			name:    "a failed server that was never registered is cleaned up directly",
			current: Failed,
			in:      Inputs{FailedRetentionElapsed: true, PlayersStale: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonRetentionElapsed},
		},
		{
			name:    "deleting a ready server drains it",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.DeletionRequested = true
				in.PlayersOnline = 4
				return in
			}(),
			want: Decision{Next: Draining, Deregister: true, StartDrain: true, Reason: ReasonDeletionRequested},
		},
		{
			name:    "deleting a starting server terminates it right away",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, DeletionRequested: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDeletionRequested},
		},
		{
			// Regression for the Critical finding: a Starting server that fell
			// out of Ready (WasRegistered) still has its players connected — the
			// readiness-loss fallback only deregistered to stop new joins, it did
			// not move anyone off. Deleting such a server must drain it, not
			// terminate it out from under 20 connected players. Reproduction as
			// confirmed by the reviewer, one tick after the Ready server lost its
			// probe and fell back to Starting.
			name:    "deleting a starting server that was registered before drains it instead of dropping its players",
			current: Starting,
			in: Inputs{
				PodExists: true, PodRunning: true, PodReady: false, AgentReady: true,
				PlayersOnline: 20, DeletionRequested: true, ReadinessLosses: 1,
				WasRegistered: true,
			},
			want: Decision{Next: Draining, StartDrain: true, Reason: ReasonDeletionRequested},
		},
		{
			name:    "deleting a pending server terminates it right away",
			current: Pending,
			in:      Inputs{DeletionRequested: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDeletionRequested},
		},
		{
			name:    "draining terminates once the server is empty",
			current: Draining,
			in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 0},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDrained},
		},
		{
			name:    "draining keeps waiting while players are online",
			current: Draining,
			in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 1},
			want:    Decision{Next: Draining, Reason: ReasonDeletionRequested},
		},
		{
			name:    "draining keeps waiting when the count is stale even at zero",
			current: Draining,
			in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 0, PlayersStale: true},
			want:    Decision{Next: Draining, Reason: ReasonDeletionRequested},
		},
		{
			name:    "draining gives up at the deadline",
			current: Draining,
			in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 3, DrainDeadlineReached: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDrainTimeout},
		},
		{
			// Regression for Minor 1: a crashed pod's players are already gone
			// no matter what a stale report still claims, so draining must not
			// burn the full drain timeout waiting for players who cannot leave
			// a pod that no longer runs.
			name:    "draining terminates right away when the pod goes terminal, even with players reported online",
			current: Draining,
			in:      Inputs{PodTerminal: true, PlayersOnline: 3, PlayersStale: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonPodTerminal},
		},
		{
			name:    "a lost pod terminates a ready server and deregisters it",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.PodExists = false
				in.PodRunning = false
				in.PodReady = false
				in.PodLost = true
				return in
			}(),
			want: Decision{Next: Terminating, Deregister: true, DeletePod: true, Reason: ReasonPodLost},
		},
		{
			name:    "a lost pod ends a drain",
			current: Draining,
			in:      Inputs{PodLost: true, PlayersOnline: 3, PlayersStale: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonPodLost},
		},
		{
			name:    "failed is kept for diagnosis",
			current: Failed,
			in:      Inputs{},
			want:    Decision{Next: Failed, Reason: ReasonPodTerminal},
		},
		{
			name:    "failed is cleaned up after the retention",
			current: Failed,
			in:      Inputs{FailedRetentionElapsed: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonRetentionElapsed},
		},
		{
			name:    "terminating is absorbing",
			current: Terminating,
			in:      healthyReady(),
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonTerminating},
		},
		{
			name:    "an unknown phase restarts at pending",
			current: Phase("Bogus"),
			in:      Inputs{},
			want:    Decision{Next: Pending, Reason: ReasonUnknownPhase},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.current, tc.in)
			got.Message = ""
			if got != tc.want {
				t.Errorf("Decide(%q, %+v)\n got  %+v\n want %+v", tc.current, tc.in, got, tc.want)
			}
		})
	}
}

// TestNoPathBackFromDraining guards the rule that a draining server never
// serves players again, no matter how healthy it looks.
// TestStreamDownGraceIsFifteenSeconds pins the value, not just the symbol.
// Every other test is written relative to the constant, so shrinking it to a
// second would break nothing and silently drop the tolerance design spec 4.4
// requires.
func TestStreamDownGraceIsFifteenSeconds(t *testing.T) {
	if StreamDownGrace != 15*time.Second {
		t.Errorf("StreamDownGrace = %v, want 15s (design spec 4.4)", StreamDownGrace)
	}
}

// TestOccupiedFailedServerIsNeverDeletedBeforeItsDrainDeadline is the Failed
// counterpart of TestOccupiedServerIsNeverDeletedWithoutDeadline: a failed
// server can still hold live sessions, so neither the retention nor a deletion
// request may remove its pod while players are on it.
func TestOccupiedFailedServerIsNeverDeletedBeforeItsDrainDeadline(t *testing.T) {
	for _, stale := range []bool{false, true} {
		for _, deleting := range []bool{false, true} {
			for _, retention := range []bool{false, true} {
				in := Inputs{
					WasRegistered:          true,
					PlayersOnline:          7,
					PlayersStale:           stale,
					DeletionRequested:      deleting,
					FailedRetentionElapsed: retention,
				}
				if got := Decide(Failed, in); got.DeletePod {
					t.Errorf("Decide(Failed, stale=%v deleting=%v retention=%v) deleted an occupied pod: %+v",
						stale, deleting, retention, got)
				}
			}
		}
	}
}

func TestNoPathBackFromDraining(t *testing.T) {
	got := Decide(Draining, healthyReady())
	if got.Next == Ready || got.Register {
		t.Fatalf("draining went back to Ready: %+v", got)
	}
}

// TestNoPathBackFromFailed guards the same rule for Failed.
func TestNoPathBackFromFailed(t *testing.T) {
	got := Decide(Failed, healthyReady())
	if got.Next == Ready || got.Register {
		t.Fatalf("failed went back to Ready: %+v", got)
	}
}

// TestOccupiedServerIsNeverDeletedWithoutDeadline is the core invariant: as
// long as players are online and no deadline has passed, no decision may
// delete the pod.
//
// Ready and Draining are always checked: a Ready server is registered with
// the proxies, and a Draining server was registered until it started
// draining. Starting is checked only with WasRegistered: true — a Starting
// server that fell out of Ready still has players connected from before it
// lost readiness, even though it deregistered to stop new joins. A
// never-registered Pending or Starting server is excluded: it was never
// registered with the proxies, so it cannot hold players — and treating its
// stale, never-reported count as "occupied" would make deletion of a server
// that never started hang until the drain deadline.
func TestOccupiedServerIsNeverDeletedWithoutDeadline(t *testing.T) {
	cases := []struct {
		phase         Phase
		wasRegistered bool
	}{
		{Ready, false},
		{Draining, false},
		{Starting, true},
	}
	for _, tc := range cases {
		for _, stale := range []bool{false, true} {
			for _, deleting := range []bool{false, true} {
				in := healthyReady()
				in.PlayersOnline = 7
				in.PlayersStale = stale
				in.DeletionRequested = deleting
				in.WasRegistered = tc.wasRegistered
				got := Decide(tc.phase, in)
				if got.DeletePod {
					t.Errorf("Decide(%q, players=7 stale=%v deleting=%v wasRegistered=%v) deleted the pod: %+v",
						tc.phase, stale, deleting, tc.wasRegistered, got)
				}
			}
		}
	}
}
