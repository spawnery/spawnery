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

package agent

import (
	"testing"
	"time"
)

// fakeClock is a hand-cranked clock so the time rules are testable.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestRegistry() (*Registry, *fakeClock) {
	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	return New(clock.Now, 5*time.Second, start), clock
}

func TestLookupUnknownPod(t *testing.T) {
	r, clock := newTestRegistry()
	clock.Advance(3 * time.Second)

	got := r.Lookup("pod-uid-1")
	if got.Known {
		t.Error("unknown pod reported as known")
	}
	if got.Ready {
		t.Error("unknown pod reported as ready")
	}
	if !got.PlayersStale {
		t.Error("unknown pod must count as stale, i.e. occupied")
	}
	if got.StreamDownFor != 3*time.Second {
		t.Errorf("StreamDownFor = %v, want 3s since operator start", got.StreamDownFor)
	}
}

func TestConnectDoesNotImplyReady(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)

	got := r.Lookup("pod-uid-1")
	if !got.Known || !got.Connected {
		t.Fatalf("after Connect: %+v", got)
	}
	if got.Ready {
		t.Error("Connect must not mark the agent ready")
	}
	if got.StreamDownFor != 0 {
		t.Errorf("StreamDownFor = %v on a live stream, want 0", got.StreamDownFor)
	}
}

func TestMarkReadyAndReport(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	if err := r.ReportPlayers("pod-uid-1", 12, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	got := r.Lookup("pod-uid-1")
	if !got.Ready || got.Players != 12 || got.Slots != 100 || got.PlayersStale {
		t.Errorf("snapshot = %+v, want ready with 12/100 and fresh", got)
	}
}

func TestPlayerCountGoesStaleAtTwiceTheInterval(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	clock.Advance(9 * time.Second)
	if r.Lookup("pod-uid-1").PlayersStale {
		t.Error("count went stale before twice the report interval")
	}

	clock.Advance(1 * time.Second)
	if !r.Lookup("pod-uid-1").PlayersStale {
		t.Error("count did not go stale at twice the report interval")
	}
}

func TestReportPlayersRejectsMoreThanSlots(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	if err := r.ReportPlayers("pod-uid-1", 5, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	if err := r.ReportPlayers("pod-uid-1", 101, 100); err == nil {
		t.Fatal("player count above slots accepted, want rejection")
	}
	if got := r.Lookup("pod-uid-1"); got.Players != 5 {
		t.Errorf("bogus report changed the count to %d, want the previous 5", got.Players)
	}
}

func TestReportPlayersRejectsUnknownPod(t *testing.T) {
	r, _ := newTestRegistry()
	if err := r.ReportPlayers("pod-uid-1", 1, 100); err == nil {
		t.Fatal("report for an unconnected pod accepted, want rejection")
	}
}

func TestDisconnectKeepsTheLastCountAndStartsTheClock(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	if err := r.ReportPlayers("pod-uid-1", 7, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	r.Disconnect("pod-uid-1")
	clock.Advance(4 * time.Second)

	got := r.Lookup("pod-uid-1")
	if got.Connected {
		t.Error("still connected after Disconnect")
	}
	if got.Ready {
		t.Error("a disconnected agent must not count as ready")
	}
	if got.Players != 7 {
		t.Errorf("Players = %d, want the last known 7", got.Players)
	}
	if got.StreamDownFor != 4*time.Second {
		t.Errorf("StreamDownFor = %v, want 4s", got.StreamDownFor)
	}
}

func TestReconnectRestoresReadiness(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	r.Disconnect("pod-uid-1")
	clock.Advance(30 * time.Second)

	r.Connect("pod-uid-1", RoleServer)
	if got := r.Lookup("pod-uid-1"); got.Ready {
		t.Error("reconnect alone must not restore readiness")
	}
	r.MarkReady("pod-uid-1")

	got := r.Lookup("pod-uid-1")
	if !got.Ready || got.StreamDownFor != 0 {
		t.Errorf("snapshot after reconnect = %+v, want ready with a live stream", got)
	}
}

func TestForgetRemovesThePod(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.Forget("pod-uid-1")
	if r.Lookup("pod-uid-1").Known {
		t.Error("pod still known after Forget")
	}
}
