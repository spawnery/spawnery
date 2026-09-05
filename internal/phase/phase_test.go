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
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		// A healthy Ready server is one the proxies have. Saying so here is
		// not bookkeeping: Ready and unregistered is a real state -- a server
		// whose door is shut, or one whose registration failed -- and Decide
		// now acts on it, so a fixture that left this false would be
		// describing a different server than it means to.
		Registered: true,
		Slots:      100,
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
			// A server that fell out of Ready and cannot come back must still be
			// failed, and must take its players with it. The flap counter can
			// never catch this on its own: losses are only counted on a
			// Ready -> Starting transition, which a permanently red probe never
			// produces again. The controller re-arms status.startedAt on entry
			// into Starting, so the deadline here is one full recovery window
			// after the fall-back, not the age of the pod.
			name:    "a server that cannot recover is failed and drained a deadline after falling back",
			current: Starting,
			in: Inputs{
				PodExists: true, PodRunning: true, StartupDeadlineReached: true,
				WasRegistered: true, PlayersOnline: 9, ReadinessLosses: 1,
			},
			want: Decision{Next: Failed, StartDrain: true, Reason: ReasonStartupTimeout},
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
			name:    "ready retires when the group asks",
			current: Ready,
			in: Inputs{
				PodExists: true, PodRunning: true, PodReady: true, AgentReady: true,
				RetirementRequested: true, WasRegistered: true,
			},
			want: Decision{Next: Retiring, Deregister: true, Reason: ReasonRetiring},
		},
		{
			// Soft drain is deregistration without a move. The proxies learn the
			// server is gone from status.registered; nothing tells them to take
			// anyone off it, and internal/proxyreg only sends DrainPlayers for
			// phase Draining.
			name:    "retiring never asks for a drain while it waits",
			current: Retiring,
			in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 3},
			want:    Decision{Next: Retiring, Reason: ReasonRetiring},
		},
		{
			name:    "retiring terminates once the last player leaves",
			current: Retiring,
			in:      Inputs{PodExists: true, PodRunning: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDrained},
		},
		{
			// The whole difference from Draining: an occupied retiring server has
			// no deadline over it at all until maxStaleSeconds says so.
			name:    "an occupied retiring server is never terminated on a drain deadline",
			current: Retiring,
			in: Inputs{
				PodExists: true, PodRunning: true, PlayersOnline: 1,
				DrainDeadlineReached: true,
			},
			want: Decision{Next: Retiring, Reason: ReasonRetiring},
		},
		{
			name:    "the stale deadline escalates to a real drain",
			current: Retiring,
			in: Inputs{
				PodExists: true, PodRunning: true, PlayersOnline: 1,
				MaxStaleReached: true,
			},
			want: Decision{Next: Draining, StartDrain: true, Reason: ReasonMaxStaleElapsed},
		},
		{
			// Whoever deletes a retiring server gets the proper move, not a drop:
			// it still has players on it.
			name:    "deleting a retiring server moves its players off",
			current: Retiring,
			in: Inputs{
				PodExists: true, PodRunning: true, PlayersOnline: 1,
				DeletionRequested: true,
			},
			want: Decision{Next: Draining, StartDrain: true, Reason: ReasonDeletionRequested},
		},
		{
			name:    "a lost pod ends a retirement without a drain",
			current: Retiring,
			in:      Inputs{PodLost: true, PlayersOnline: 1, PlayersStale: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonPodLost},
		},
		{
			name:    "a terminal pod ends a retirement without a drain",
			current: Retiring,
			in:      Inputs{PodExists: true, PodTerminal: true, PlayersOnline: 1},
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

// TestNoPathBackFromDraining guards the rule that a draining server never
// serves players again, no matter how healthy it looks.
func TestNoPathBackFromDraining(t *testing.T) {
	got := Decide(Draining, healthyReady())
	if got.Next == Ready || got.Register {
		t.Fatalf("draining went back to Ready: %+v", got)
	}
}

func TestNoPathBackFromRetiring(t *testing.T) {
	// Retiring is one-way, like Draining. A server that is being replaced
	// must not re-register itself because its probe happens to be green:
	// the proxies would start sending joins to a server the group has
	// already decided to remove.
	in := Inputs{
		PodExists: true, PodRunning: true, PodReady: true, AgentReady: true,
		PlayersOnline: 1,
	}
	if got := Decide(Retiring, in); got.Next == Ready || got.Register {
		t.Errorf("Decide(Retiring, healthy) = %+v, want no way back to Ready", got)
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

// A server whose pod was never created has no other way out.
//
// status.podName stays empty so PodLost never applies, and the startup
// deadline asks whether a pod that exists became playable — which is a
// different question with a different remedy. Without this transition such a
// server stayed Pending for as long as whatever refused the create stood,
// counting against its group's replicas the whole time.
func TestAPodThatWasNeverCreatedFailsAtItsDeadline(t *testing.T) {
	got := Decide(Pending, Inputs{PodCreationDeadlineReached: true})
	if got.Next != Failed {
		t.Errorf("next = %s, want %s", got.Next, Failed)
	}
	if got.Reason != ReasonPodNeverCreated {
		t.Errorf("reason = %s, want %s — a server that never had a pod did not fail to "+
			"become ready, it failed to be created, and the remedy is somewhere else",
			got.Reason, ReasonPodNeverCreated)
	}
	// Nothing was ever registered and nobody is on it, so there is nobody to
	// move and nothing to withdraw.
	if got.StartDrain {
		t.Error("a drain was started for a server that never had a pod")
	}
	if got.Deregister {
		t.Error("a deregistration was sent for a server that was never registered")
	}
	if got.DeletePod {
		t.Error("a pod delete was ordered for a server that never had a pod")
	}
}

// The deadline must not outrank a pod that has since turned up. It is
// computed from "no pod, and none named in the status", so the controller
// stops setting it the moment one exists — but a stale input reaching Decide
// must not undo the transition either.
func TestAPodThatArrivedOutranksTheCreationDeadline(t *testing.T) {
	got := Decide(Pending, Inputs{
		PodExists: true, PodRunning: true, PodCreationDeadlineReached: true,
	})
	if got.Next != Starting {
		t.Errorf("next = %s, want %s: the pod is running, whatever the deadline says",
			got.Next, Starting)
	}
}

// TestOccupiedCountsAPlayerOnlyAProxyCanSee is the operator's half of the
// drain gap. A player still completing the configuration phase is counted by
// neither the backend nor the proxy's own player list -- disassembling
// velocity 3.5.1 build 615, VelocityRegisteredServer.addPlayer is called only
// from BackendPlaySessionHandler.activated(), the play phase -- so this server
// read empty while a connection to it was in flight, and the pod went.
func TestOccupiedCountsAPlayerOnlyAProxyCanSee(t *testing.T) {
	// Exactly the state that used to delete a pod under somebody: the backend
	// has reported zero, freshly, and a proxy says one player is on their way.
	in := Inputs{PlayersOnline: 0, PlayersStale: false, ProxyAttached: 1}
	if !in.Occupied() {
		t.Error("a server with a player arriving reads as empty")
	}
}

// TestOccupiedTreatsASilentProxyAsOccupied applies the same rule the backend's
// own count already follows: a report we cannot trust counts as occupied,
// because one server too many beats one kick.
func TestOccupiedTreatsASilentProxyAsOccupied(t *testing.T) {
	in := Inputs{PlayersOnline: 0, ProxyAttachStale: true}
	if !in.Occupied() {
		t.Error("a proxy that stopped reporting leaves the server readable as empty")
	}
}

// TestOccupiedIsUnchangedWithoutProxyReports is the property that lets a fleet
// upgrade in any order, and the one worth a test of its own: every term of
// Occupied can only make it true, so an agent too old to report backends
// contributes zero and the rule is exactly what it was before this existed.
func TestOccupiedIsUnchangedWithoutProxyReports(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Inputs
		want bool
	}{
		{"nobody anywhere", Inputs{}, false},
		{"the backend has players", Inputs{PlayersOnline: 3}, true},
		{"the backend's count is stale", Inputs{PlayersStale: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Occupied(); got != tc.want {
				t.Errorf("Occupied() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBreakingASilentAgentsStreamCostsTheDrain is the reason the operator sets
// no transport keepalive, made executable.
//
// AgentSilent is defined as "the stream is up and has gone quiet". Anything
// that breaks that stream -- a keepalive on internal/agentserver, a shorter
// MaxConnectionIdle, an operator that decides to hang up on a peer it has
// written off -- converts this state into the one below: an ordinary broken
// stream, which is tolerated for StreamDownGrace and carries no StartDrain. So
// the twenty-second window for moving players off a backend that will never
// answer is not merely delayed by such a change, it is gone.
//
// Whoever comes to add a keepalive should meet this test rather than a fleet
// that quietly stopped rescuing anybody. The agent's own keepalive is a
// different thing and displaces nothing: see OperatorChannel in agent/common.
func TestBreakingASilentAgentsStreamCostsTheDrain(t *testing.T) {
	silent := Inputs{
		PodExists: true, PodRunning: true, PodReady: true,
		AgentConnected: true, AgentReady: true,
		AgentSilent:   true,
		WasRegistered: true,
	}
	if d := Decide(Ready, silent); !d.StartDrain {
		t.Fatal("a silent agent on a live stream does not start a drain; the premise of this test is gone")
	}

	// The same moment, with the stream broken instead of quiet. AgentSilent
	// cannot be true without AgentConnected, so this is what a keepalive would
	// leave behind.
	broken := silent
	broken.AgentConnected = false
	broken.AgentSilent = false
	broken.AgentStreamDownFor = StreamDownGrace

	d := Decide(Ready, broken)
	if !d.Deregister {
		t.Error("a stream down past the grace does not deregister")
	}
	if d.StartDrain {
		t.Error("a broken stream now starts a drain; if that is deliberate, this test is the " +
			"place to say so -- it exists to record that it did not, which is why breaking a " +
			"silent agent's stream costs the rescue")
	}
}

// TestASilentAgentOnALiveStreamLosesReadinessAndDrains is the case a dead node
// actually produces, and the one nothing used to notice.
//
// A node that is hard-powered off sends no FIN and no RST, so the operator's
// socket goes on looking connected for minutes -- measured through a
// freezable relay at over 200 seconds. AgentConnected therefore stays true and
// AgentReady stays at the last thing the agent said, so the server stayed
// Ready, stayed registered, and went on being sent new players, while the ones
// already on it waited for Velocity's read timeout to disconnect them.
func TestASilentAgentOnALiveStreamLosesReadinessAndDrains(t *testing.T) {
	d := Decide(Ready, Inputs{
		PodExists: true, PodRunning: true, PodReady: true,
		AgentConnected: true, AgentReady: true,
		AgentSilent:   true,
		WasRegistered: true,
	})

	if d.Next != Starting {
		t.Errorf("Next = %s, want Starting", d.Next)
	}
	if !d.Deregister {
		t.Error("a server whose agent has gone silent stays registered, so new players keep arriving on it")
	}
	// The half that matters to the people already there. Velocity kicks them
	// outright when its read timeout fires -- no KickedFromServerEvent, so the
	// agent's own Rescue never sees them -- and this is the only thing that
	// moves them first.
	if !d.StartDrain {
		t.Error("the players on a dead backend were left to be disconnected by the read timeout")
	}
}

// TestAnOrdinaryReadinessLossDoesNotDrain keeps the drain to the case that
// needs it. A server that reports not-ready may come back, and moving its
// players costs them a loading screen for nothing.
func TestAnOrdinaryReadinessLossDoesNotDrain(t *testing.T) {
	d := Decide(Ready, Inputs{
		PodExists: true, PodRunning: true, PodReady: true,
		AgentConnected: true, AgentReady: false,
		WasRegistered: true,
	})

	if d.Next != Starting || !d.Deregister {
		t.Fatalf("Next = %s deregister = %v, want Starting and true", d.Next, d.Deregister)
	}
	if d.StartDrain {
		t.Error("an unhealthy server that may recover had its players moved off anyway")
	}
}

// TestABrokenStreamIsStillJustABrokenStream is the discriminator, and the
// reason this is safe to run in an operator restart -- which breaks every
// agent's stream at once. A broken stream leaves AgentConnected false and is
// tolerated for StreamDownGrace exactly as before; only a stream that is up
// and quiet is read as a peer that has gone without TCP noticing.
func TestABrokenStreamIsStillJustABrokenStream(t *testing.T) {
	d := Decide(Ready, Inputs{
		PodExists: true, PodRunning: true, PodReady: true,
		AgentConnected: false, AgentReady: true,
		AgentStreamDownFor: StreamDownGrace / 2,
		WasRegistered:      true,
	})

	if d.Next != Ready {
		t.Errorf("Next = %s, want Ready: a briefly broken stream is not a dead backend", d.Next)
	}
	if d.StartDrain {
		t.Error("a reconnecting agent had its server's players moved off")
	}
}

// TestTheShippedVelocityDefaultMatchesTheConstant is what keeps
// VelocityReadTimeout from being a number somebody wrote down once.
//
// The operator does not render velocity.toml -- spawnery-config does, inside
// the pod, from the defaults this repository ships plus the user's overlay --
// so the constant here and the file there are two statements of one fact with
// nothing between them. This is the something.
func TestTheShippedVelocityDefaultMatchesTheConstant(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "render", "defaults", "velocity.default.toml"))
	if err != nil {
		t.Fatalf("read the shipped velocity defaults: %v", err)
	}

	found := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "read-timeout"); ok {
			found = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(after), "="))
			break
		}
	}
	if found == "" {
		t.Fatal("the shipped velocity.default.toml sets no read-timeout, so nothing here " +
			"is pinned to anything. Either the key was renamed or the file was rewritten")
	}
	millis, err := strconv.Atoi(found)
	if err != nil {
		t.Fatalf("read-timeout = %q is not a number of milliseconds: %v", found, err)
	}
	if got := time.Duration(millis) * time.Millisecond; got != VelocityReadTimeout {
		t.Errorf("the shipped velocity.default.toml says read-timeout = %s and "+
			"VelocityReadTimeout says %s. Everything that decides whether a player on a "+
			"dead node is moved or kicked is arithmetic on these two agreeing",
			got, VelocityReadTimeout)
	}
}

// TestTheRescueWindowIsTheReadTimeoutLessWhatStalenessSpends pins the
// arithmetic at the operator's own default and at the boundary the entry that
// produced this named: above fifteen seconds the operator is later than the
// kick it is racing.
func TestTheRescueWindowIsTheReadTimeoutLessWhatStalenessSpends(t *testing.T) {
	for _, tc := range []struct {
		reportInterval time.Duration
		want           time.Duration
	}{
		{5 * time.Second, 20 * time.Second},
		{10 * time.Second, 10 * time.Second},
		{15 * time.Second, 0},
		{20 * time.Second, -10 * time.Second},
	} {
		if got := RescueWindow(tc.reportInterval, 0); got != tc.want {
			t.Errorf("RescueWindow(%s, shipped default) = %s, want %s",
				tc.reportInterval, got, tc.want)
		}
	}
}

// TestRescueWindowUsesWhatTheProxyReported is the point of the field the proxy
// now sends: a velocity.toml overlay lowering read-timeout used to close this
// window with nothing noticing, because the operator could only assume the
// value this repository ships.
func TestRescueWindowUsesWhatTheProxyReported(t *testing.T) {
	// Half the shipped default, at the operator's own report interval: the
	// window halves with it rather than staying at the shipped twenty.
	if got, want := RescueWindow(5*time.Second, 15*time.Second), 5*time.Second; got != want {
		t.Errorf("RescueWindow(5s, 15s) = %s, want %s", got, want)
	}
	// A proxy more patient than the default is believed too. The operator is
	// not entitled to assume the worse of the two.
	if got, want := RescueWindow(5*time.Second, 60*time.Second), 50*time.Second; got != want {
		t.Errorf("RescueWindow(5s, 60s) = %s, want %s", got, want)
	}
	// Nothing reported falls back to the shipped default, which is the reading
	// the operator took before any proxy sent this.
	if got, want := RescueWindow(5*time.Second, 0), RescueWindow(5*time.Second, VelocityReadTimeout); got != want {
		t.Errorf("an unreported timeout gave %s, want the shipped default's %s", got, want)
	}
}

// The door a server shuts on itself, which is the one thing here that changes
// what the proxies have without changing what the server is.

func TestAClosedDoorDeregistersWithoutMovingThePhase(t *testing.T) {
	in := healthyReady()
	in.JoinsClosed = true

	got := Decide(Ready, in)

	if got.Next != Ready {
		t.Errorf("Next = %v, want Ready: a shut door is not a lifecycle event, and a phase "+
			"change would tell every reader that this server is on its way out", got.Next)
	}
	if !got.Deregister {
		t.Error("a server that asked for no new players stayed in the routing tables")
	}
	if got.StartDrain {
		t.Error("closing a door moved players: nobody is drained, they finish on their own")
	}
	if got.Reason != ReasonJoinsClosed {
		t.Errorf("Reason = %q, want %q so that a reader can tell this from a drain",
			got.Reason, ReasonJoinsClosed)
	}
}

func TestAClosedDoorIsDeregisteredOnceAndNotOnEveryPass(t *testing.T) {
	// Each Deregister is a broadcast to every proxy in the namespace, so the
	// second pass over an already-closed server has to be silent.
	in := healthyReady()
	in.JoinsClosed = true
	in.Registered = false

	got := Decide(Ready, in)

	if got.Deregister {
		t.Error("an already-deregistered server was deregistered again")
	}
	if got.Register {
		t.Error("a closed server was registered again, which is the door reopening itself")
	}
}

func TestAnOpenedDoorRegistersAgain(t *testing.T) {
	// The way back, which is the whole difference between this and retiring.
	in := healthyReady()
	in.JoinsClosed = false
	in.Registered = false

	got := Decide(Ready, in)

	if !got.Register {
		t.Error("a server that opened its door again was not put back in the routing tables")
	}
	if got.Next != Ready {
		t.Errorf("Next = %v, want Ready", got.Next)
	}
}

func TestRetiringWinsOverAClosedDoor(t *testing.T) {
	// A retiring server is going away. If the door branch could speak first,
	// a server that had closed its door and was then retired would report the
	// smaller of the two facts -- and one that had *opened* its door would be
	// registered again on its way out.
	in := healthyReady()
	in.JoinsClosed = false
	in.Registered = false
	in.RetirementRequested = true

	got := Decide(Ready, in)

	if got.Next != Retiring || !got.Deregister {
		t.Errorf("got %+v, want a retiring server to be deregistered and stay retiring", got)
	}
}

func TestTheGateDoesNotRegisterAServerWhoseDoorIsShut(t *testing.T) {
	// A plugin that closes the door while starting must not see the server
	// registered for one pass and deregistered on the next: those seconds are
	// the window this exists to close.
	in := Inputs{
		PodExists: true, PodRunning: true, PodReady: true, AgentReady: true,
		JoinsClosed: true,
	}

	got := Decide(Starting, in)

	if got.Next != Ready {
		t.Errorf("Next = %q, want %q: the server is ready, it is only closed",
			got.Next, Ready)
	}
	if got.Register {
		t.Error("Register = true, want false while the door is shut")
	}
	if got.Reason != ReasonJoinsClosed {
		t.Errorf("Reason = %q, want %q so a reader can tell why nobody arrives",
			got.Reason, ReasonJoinsClosed)
	}
}

func TestTheGateRegistersWhenNobodyClosedTheDoor(t *testing.T) {
	// The default matters more than the new branch: a network that never calls
	// acceptJoins must decide exactly what it decided before.
	in := Inputs{PodExists: true, PodRunning: true, PodReady: true, AgentReady: true}

	got := Decide(Starting, in)

	want := Decision{Next: Ready, Register: true, Reason: ReasonReadyGatePassed}
	if got.Next != want.Next || !got.Register || got.Reason != want.Reason {
		t.Errorf("Decide(Starting, open door) = %+v, want %+v", got, want)
	}
}
