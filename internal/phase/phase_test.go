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

package phase

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func healthyReady() Inputs {
	return Inputs{
		PodExists:      true,
		PodRunning:     true,
		PodReady:       true,
		AgentReady:     true,
		AgentConnected: true,
		// Ready and unregistered is a distinct state Decide acts on.
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
			// This package must not depend on the caller never sending these together.
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
			name:    "a server that has ended its round leaves it",
			current: Ready,
			in:      Inputs{PodExists: true, PodRunning: true, PodReady: true, AgentReady: true, RoundEnded: true, Registered: true},
			want:    Decision{Next: Ready, Deregister: true, Reason: ReasonRoundFinished},
		},
		{
			name:    "a server whose round ended is not deregistered twice",
			current: Ready,
			in:      Inputs{PodExists: true, PodRunning: true, PodReady: true, AgentReady: true, RoundEnded: true, Registered: false},
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
			// The shape of every server right after an operator restart.
			name:    "ready tolerates an unheard agent until the reconnect grace",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentReady = false
				in.AgentConnected = false
				in.AgentUnheard = true
				in.AgentStreamDownFor = ReconnectGrace - time.Millisecond
				return in
			}(),
			want: Decision{Next: Ready, Reason: ReasonReadyGatePassed},
		},
		{
			name:    "ready falls back once even the reconnect grace has passed",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentReady = false
				in.AgentConnected = false
				in.AgentUnheard = true
				in.AgentStreamDownFor = ReconnectGrace
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
			// What the agent registry emits after Disconnect: inside the grace only
			// the timer may decide.
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
			// Losses are only counted on Ready -> Starting, which a permanently red
			// probe never repeats, so the flap counter alone cannot catch this.
			name:    "a server that cannot recover is failed and drained a deadline after falling back",
			current: Starting,
			in: Inputs{
				PodExists: true, PodRunning: true, StartupDeadlineReached: true,
				WasRegistered: true, PlayersOnline: 9, ReadinessLosses: 1,
			},
			want: Decision{Next: Failed, StartDrain: true, Reason: ReasonStartupTimeout},
		},
		{
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
			name:    "a terminal pod whose server said its round was over is Finished",
			current: Ready,
			in:      Inputs{PodExists: true, PodTerminal: true, RoundEnded: true},
			want:    Decision{Next: Finished, Deregister: true, Reason: ReasonRoundFinished},
		},
		{
			name:    "a terminal pod that said nothing still Fails",
			current: Ready,
			in:      Inputs{PodExists: true, PodTerminal: true},
			want:    Decision{Next: Failed, Deregister: true, Reason: ReasonPodTerminal},
		},
		{
			name:    "a finished server waits for its retention",
			current: Finished,
			in:      Inputs{PodExists: true, PodTerminal: true, RoundEnded: true},
			want:    Decision{Next: Finished, Reason: ReasonRoundFinished},
		},
		{
			name:    "a finished server is cleaned up once its retention elapses",
			current: Finished,
			in:      Inputs{PodExists: true, PodTerminal: true, RoundEnded: true, FinishedRetentionElapsed: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonFinishedRetentionElapsed},
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
			// A stale report must not make the drain wait for players who are gone.
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
			// internal/proxyreg sends DrainPlayers only for phase Draining.
			name:    "retiring never asks for a drain while it waits",
			current: Retiring,
			in:      Inputs{RetirementRequested: true, PodExists: true, PodRunning: true, PlayersOnline: 3},
			want:    Decision{Next: Retiring, Reason: ReasonRetiring},
		},
		{
			name:    "retiring terminates once the last player leaves",
			current: Retiring,
			in:      Inputs{RetirementRequested: true, PodExists: true, PodRunning: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDrained},
		},
		{
			name:    "an occupied retiring server is never terminated on a drain deadline",
			current: Retiring,
			in: Inputs{
				PodExists: true, PodRunning: true, PlayersOnline: 1, RetirementRequested: true,
				DrainDeadlineReached: true,
			},
			want: Decision{Next: Retiring, Reason: ReasonRetiring},
		},
		{
			name:    "the stale deadline escalates to a real drain",
			current: Retiring,
			in: Inputs{RetirementRequested: true,
				PodExists: true, PodRunning: true, PlayersOnline: 1,
				MaxStaleReached: true,
			},
			want: Decision{Next: Draining, StartDrain: true, Reason: ReasonMaxStaleElapsed},
		},
		{
			name:    "deleting a retiring server moves its players off",
			current: Retiring,
			in: Inputs{
				PodExists: true, PodRunning: true, PlayersOnline: 1, RetirementRequested: true,
				DeletionRequested: true,
			},
			want: Decision{Next: Draining, StartDrain: true, Reason: ReasonDeletionRequested},
		},
		{
			name:    "a lost pod ends a retirement without a drain",
			current: Retiring,
			in:      Inputs{RetirementRequested: true, PodLost: true, PlayersOnline: 1, PlayersStale: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonPodLost},
		},
		{
			name:    "a terminal pod ends a retirement without a drain",
			current: Retiring,
			in:      Inputs{RetirementRequested: true, PodExists: true, PodTerminal: true, PlayersOnline: 1},
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
			name:    "a failed server's pod that came up late is stopped once the group has a ready server",
			current: Failed,
			in:      Inputs{PodExists: true, PodRunning: true, PodReady: true, GroupHasReadyServer: true},
			want:    Decision{Next: Failed, DeletePod: true, Reason: ReasonStoppingFailedPod},
		},
		{
			name:    "a never-registered failed server's late pod is stopped without a player count",
			current: Failed,
			in:      Inputs{PodExists: true, PodRunning: true, GroupHasReadyServer: true, PlayersStale: true},
			want:    Decision{Next: Failed, DeletePod: true, Reason: ReasonStoppingFailedPod},
		},
		{
			name:    "a failed server's running pod stays while the group has no ready server",
			current: Failed,
			in:      Inputs{PodExists: true, PodRunning: true},
			want:    Decision{Next: Failed, Reason: ReasonPodTerminal},
		},
		{
			name:    "a failed server's running pod with players on it is not stopped early",
			current: Failed,
			in: Inputs{PodExists: true, PodRunning: true, GroupHasReadyServer: true,
				WasRegistered: true, PlayersOnline: 2},
			want: Decision{Next: Failed, Reason: ReasonPodTerminal},
		},
		{
			name:    "a failed server whose pod already ended keeps it for its logs",
			current: Failed,
			in:      Inputs{PodExists: true, PodTerminal: true, GroupHasReadyServer: true},
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

// Every other test is relative to the constant, so only this pins its value.
func TestStreamDownGraceIsFifteenSeconds(t *testing.T) {
	if StreamDownGrace != 15*time.Second {
		t.Errorf("StreamDownGrace = %v, want 15s (design spec 4.4)", StreamDownGrace)
	}
}

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

func TestAForceStopEndsEveryPhaseAtOnce(t *testing.T) {
	for _, current := range declaredPhases(t) {
		in := healthyReady()
		in.PlayersOnline = 12
		in.ForceStopRequested = true

		got := Decide(current, in)

		if got.Next != Terminating || !got.DeletePod || got.StartDrain || got.Reason != ReasonForceStopped {
			t.Errorf("%s: decision = %+v, want Terminating, DeletePod, no drain, ForceStopped", current, got)
		}
		if !got.Deregister {
			t.Errorf("%s: a registered server was not deregistered", current)
		}
	}

	in := healthyReady()
	in.Registered = false
	in.ForceStopRequested = true
	if Decide(Starting, in).Deregister {
		t.Error("an unregistered server was deregistered")
	}
}

func TestNoPathBackFromDraining(t *testing.T) {
	got := Decide(Draining, healthyReady())
	if got.Next == Ready || got.Register {
		t.Fatalf("draining went back to Ready: %+v", got)
	}
}

func TestNoPathBackFromRetiring(t *testing.T) {
	// While the retirement stands, a green probe must not re-register a server
	// the group has decided to remove.
	in := Inputs{
		PodExists: true, PodRunning: true, PodReady: true, AgentReady: true,
		PlayersOnline: 1, RetirementRequested: true,
	}
	if got := Decide(Retiring, in); got.Next == Ready || got.Register {
		t.Errorf("Decide(Retiring, healthy) = %+v, want no way back to Ready", got)
	}
}

func TestNoPathBackFromFailed(t *testing.T) {
	got := Decide(Failed, healthyReady())
	if got.Next == Ready || got.Register {
		t.Fatalf("failed went back to Ready: %+v", got)
	}
}

// Starting is checked only with WasRegistered: a never-registered server
// cannot hold players, and treating its unreported count as occupied would
// hang its deletion until the drain deadline.
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

func TestForceStopIsTheOnlyWayToDeleteAnOccupiedServerWithoutDeadline(t *testing.T) {
	for _, current := range []Phase{Ready, Draining, Starting} {
		in := healthyReady()
		in.PlayersOnline = 7
		in.WasRegistered = true

		if got := Decide(current, in); got.DeletePod {
			t.Errorf("Decide(%q, players=7) deleted the pod without a force-stop: %+v", current, got)
		}
		in.ForceStopRequested = true
		if got := Decide(current, in); !got.DeletePod {
			t.Errorf("Decide(%q, players=7, force-stop) kept the pod: %+v", current, got)
		}
	}
}

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

// The controller stops setting the deadline once a pod exists, but a stale
// input must not undo the transition either.
func TestAPodThatArrivedOutranksTheCreationDeadline(t *testing.T) {
	got := Decide(Pending, Inputs{
		PodExists: true, PodRunning: true, PodCreationDeadlineReached: true,
	})
	if got.Next != Starting {
		t.Errorf("next = %s, want %s: the pod is running, whatever the deadline says",
			got.Next, Starting)
	}
}

// A player still in the configuration phase is counted by neither the backend
// nor the proxy's own player list (Velocity calls addPlayer only on entering
// play), so only the attach report sees them.
func TestOccupiedCountsAPlayerOnlyAProxyCanSee(t *testing.T) {
	in := Inputs{PlayersOnline: 0, PlayersStale: false, ProxyAttached: 1}
	if !in.Occupied() {
		t.Error("a server with a player arriving reads as empty")
	}
}

func TestOccupiedTreatsASilentProxyAsOccupied(t *testing.T) {
	in := Inputs{PlayersOnline: 0, ProxyAttachStale: true}
	if !in.Occupied() {
		t.Error("a proxy that stopped reporting leaves the server readable as empty")
	}
}

// Every term of Occupied can only make it true, so an agent too old to
// report contributes zero and a fleet can upgrade in any order.
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

// The reason the operator sets no transport keepalive: anything that breaks
// a silent agent's stream turns it into an ordinary broken stream, which
// carries no StartDrain.
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

// A hard-powered-off node sends no FIN or RST, so the socket looks connected
// for minutes and AgentReady stays at the agent's last word.
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
	// Velocity kicks these players on its read timeout without a
	// KickedFromServerEvent, so the agent's own Rescue never sees them.
	if !d.StartDrain {
		t.Error("the players on a dead backend were left to be disconnected by the read timeout")
	}
}

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

// An operator restart breaks every stream at once; that must stay tolerated.
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

// The operator does not render velocity.toml, spawnery-config does inside the
// pod, so nothing else ties the constant to the shipped file.
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

func TestRescueWindowUsesWhatTheProxyReported(t *testing.T) {
	if got, want := RescueWindow(5*time.Second, 15*time.Second), 5*time.Second; got != want {
		t.Errorf("RescueWindow(5s, 15s) = %s, want %s", got, want)
	}
	if got, want := RescueWindow(5*time.Second, 60*time.Second), 50*time.Second; got != want {
		t.Errorf("RescueWindow(5s, 60s) = %s, want %s", got, want)
	}
	if got, want := RescueWindow(5*time.Second, 0), RescueWindow(5*time.Second, VelocityReadTimeout); got != want {
		t.Errorf("an unreported timeout gave %s, want the shipped default's %s", got, want)
	}
}

func TestAnUnregisteredServerRegistersAgain(t *testing.T) {
	in := healthyReady()
	in.Registered = false

	got := Decide(Ready, in)

	if !got.Register {
		t.Error("a server that was not registered, and whose round has not ended, " +
			"was not put back in the routing tables")
	}
	if got.Next != Ready {
		t.Errorf("Next = %v, want Ready", got.Next)
	}
}

func TestRetiringWinsOverRegistering(t *testing.T) {
	// If the register branch spoke first, a retiring server that is not in the
	// table would be put back on its way out.
	in := healthyReady()
	in.Registered = false
	in.RetirementRequested = true

	got := Decide(Ready, in)

	if got.Next != Retiring || !got.Deregister {
		t.Errorf("got %+v, want a retiring server to be deregistered and stay retiring", got)
	}
}

// The table is checked against the phases the package declares, so a new
// Phase fails this test until someone decides whether it is terminal.
func TestTerminalIsFailedAndFinishedAndNothingElse(t *testing.T) {
	terminal := map[Phase]bool{
		Pending:     false,
		Starting:    false,
		Ready:       false,
		Retiring:    false,
		Draining:    false,
		Terminating: false,
		Failed:      true,
		Finished:    true,
	}
	for p, want := range terminal {
		if got := Terminal(p); got != want {
			t.Errorf("Terminal(%s) = %v, want %v", p, got, want)
		}
	}
	if Terminal("") {
		t.Error("a server with no phase yet counts as one whose run is over")
	}

	for _, p := range declaredPhases(t) {
		if _, listed := terminal[p]; !listed {
			t.Errorf("phase %q is declared and this test says nothing about it: "+
				"decide whether it ends a server's run and add it above", p)
		}
	}
}

// declaredPhases reads the source because constants leave nothing to
// enumerate at run time. It only sees the shape `Name Phase = "Name"`; a
// phase declared any other way passes unnoticed.
func declaredPhases(t *testing.T) []Phase {
	t.Helper()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list this package: %v", err)
	}
	fset := token.NewFileSet()
	var declared []Phase
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, source, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", source, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if name, ok := value.Type.(*ast.Ident); !ok || name.Name != "Phase" {
					continue
				}
				for _, v := range value.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok {
						continue
					}
					declared = append(declared, Phase(strings.Trim(lit.Value, `"`)))
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no Phase constants found, so this test would pass whatever Terminal did")
	}
	return declared
}

// After endRound the probe going red is the process stopping, not a fault.
func TestAServerWhoseRoundEndedShutsDownAsFinished(t *testing.T) {
	ended := func(mutate func(*Inputs)) Inputs {
		in := healthyReady()
		in.RoundEnded = true
		in.Registered = false
		mutate(&in)
		return in
	}
	cases := []struct {
		name string
		in   Inputs
		want Decision
	}{
		{
			name: "the probe turns red",
			in:   ended(func(in *Inputs) { in.PodReady = false }),
			want: Decision{Next: Finished, Reason: ReasonRoundFinished},
		},
		{
			name: "the stream stays broken past its grace",
			in: ended(func(in *Inputs) {
				in.AgentConnected = false
				in.AgentStreamDownFor = StreamDownGrace
			}),
			want: Decision{Next: Finished, Reason: ReasonRoundFinished},
		},
		{
			name: "the proxies still have it",
			in: ended(func(in *Inputs) {
				in.Registered = true
				in.PodReady = false
			}),
			want: Decision{Next: Finished, Deregister: true, Reason: ReasonRoundFinished},
		},
		{
			name: "a silent agent still rescues whoever is left",
			in: ended(func(in *Inputs) {
				in.AgentConnected = true
				in.AgentSilent = true
			}),
			want: Decision{Next: Starting, Deregister: true, CountReadinessLoss: true, StartDrain: true, Reason: ReasonReadinessLost},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(Ready, tc.in)
			got.Message = ""
			if got != tc.want {
				t.Errorf("Decide(Ready, %+v)\n got  %+v\n want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestAWithdrawnRetirementGoesBackToReady(t *testing.T) {
	healthy := Inputs{
		PodExists: true, PodRunning: true, PodReady: true, AgentReady: true, AgentConnected: true,
		PlayersOnline: 2,
	}
	for _, tc := range []struct {
		name string
		in   Inputs
		want Decision
	}{
		{
			name: "withdrawn and healthy",
			in:   healthy,
			want: Decision{Next: Ready, Register: true, Reason: ReasonRetirementWithdrawn},
		},
		{
			name: "withdrawn but the probe is red",
			in:   func() Inputs { in := healthy; in.PodReady = false; return in }(),
			want: Decision{Next: Retiring, Reason: ReasonRetiring},
		},
		{
			name: "running empty wins over a withdrawal",
			in:   func() Inputs { in := healthy; in.PlayersOnline = 0; return in }(),
			want: Decision{Next: Terminating, DeletePod: true, Reason: ReasonDrained},
		},
		{
			name: "still requested stays retiring",
			in:   func() Inputs { in := healthy; in.RetirementRequested = true; return in }(),
			want: Decision{Next: Retiring, Reason: ReasonRetiring},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(Retiring, tc.in)
			got.Message = ""
			if got != tc.want {
				t.Errorf("Decide(Retiring, %+v)\n got  %+v\n want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// The retention runs from the round's end, so it can expire with players
// still on a pod that kept running.
func TestAFinishedServerStillRunningMovesItsPlayersBeforeItGoes(t *testing.T) {
	running := Inputs{
		PodExists: true, PodRunning: true, RoundEnded: true, WasRegistered: true,
		PlayersOnline: 2, FinishedRetentionElapsed: true,
	}
	if got := Decide(Finished, running); got.Next != Finished || !got.StartDrain || got.DeletePod {
		t.Errorf("Decide(Finished, running with players) = %+v, want a drain before the pod goes", got)
	}
	running.DrainDeadlineReached = true
	if got := Decide(Finished, running); got.Next != Terminating || !got.DeletePod {
		t.Errorf("Decide(Finished, drain deadline reached) = %+v, want Terminating", got)
	}
	empty := running
	empty.DrainDeadlineReached, empty.PlayersOnline = false, 0
	if got := Decide(Finished, empty); got.Next != Terminating || !got.DeletePod {
		t.Errorf("Decide(Finished, empty) = %+v, want Terminating", got)
	}
}
