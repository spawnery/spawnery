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
		PodExists:  true,
		PodRunning: true,
		PodReady:   true,
		AgentReady: true,
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
			name:    "ready falls back to starting when the agent stream is down too long",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
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
// Only Ready and Draining are checked. A server in Pending or Starting was
// never registered with the proxies, so it cannot hold players — and treating
// its stale, never-reported count as "occupied" would make deletion of a
// server that never started hang until the drain deadline.
func TestOccupiedServerIsNeverDeletedWithoutDeadline(t *testing.T) {
	phases := []Phase{Ready, Draining}
	for _, p := range phases {
		for _, stale := range []bool{false, true} {
			for _, deleting := range []bool{false, true} {
				in := healthyReady()
				in.PlayersOnline = 7
				in.PlayersStale = stale
				in.DeletionRequested = deleting
				got := Decide(p, in)
				if got.DeletePod {
					t.Errorf("Decide(%q, players=7 stale=%v deleting=%v) deleted the pod: %+v",
						p, stale, deleting, got)
				}
			}
		}
	}
}
