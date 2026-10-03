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

package agent

import (
	"math"
	"testing"
	"time"
)

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

// The next stream is registered before its Hello arrives; without carried-over
// readiness every renewal would look like a readiness loss.
func TestSupersedeKeepsReadiness(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")

	r.Supersede("pod-uid-1", RoleServer)

	got := r.Lookup("pod-uid-1")
	if !got.Connected {
		t.Fatalf("after Supersede: %+v", got)
	}
	if !got.Ready {
		t.Error("Supersede dropped the readiness of the stream it replaced")
	}
}

// A stream that supersedes nothing readable must not invent readiness either.
func TestSupersedeDoesNotInventReadiness(t *testing.T) {
	r, _ := newTestRegistry()
	r.Supersede("pod-uid-1", RoleServer)

	if got := r.Lookup("pod-uid-1"); got.Ready {
		t.Errorf("snapshot = %+v, want a pod that has never reported readiness to stay unready", got)
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

func TestKeysListsEveryKnownPod(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("a", RoleServer)
	r.Connect("b", RoleProxy)
	r.Disconnect("b")

	keys := r.Keys()
	if len(keys) != 2 {
		t.Fatalf("Keys() = %v, want both a and b — a disconnected agent is still known", keys)
	}
}

func TestEmptyForStartsWhenTheCountReachesZero(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	if err := r.ReportPlayers("pod-uid-1", 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	if got := r.Lookup("pod-uid-1").EmptyFor; got != 0 {
		t.Errorf("EmptyFor = %v on an occupied server, want 0", got)
	}

	if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	clock.Advance(90 * time.Second)
	if got := r.Lookup("pod-uid-1").EmptyFor; got != 90*time.Second {
		t.Errorf("EmptyFor = %v, want 90s since the count reached zero", got)
	}

	if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	if got := r.Lookup("pod-uid-1").EmptyFor; got != 90*time.Second {
		t.Errorf("EmptyFor = %v after a repeated zero report, want 90s", got)
	}
}

func TestEmptyForClearsWhenPlayersReturn(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	clock.Advance(time.Minute)
	if err := r.ReportPlayers("pod-uid-1", 1, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	if got := r.Lookup("pod-uid-1").EmptyFor; got != 0 {
		t.Errorf("EmptyFor = %v after a player joined, want 0", got)
	}
}

func TestEmptyForIsZeroBeforeTheFirstReport(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	clock.Advance(time.Minute)

	if got := r.Lookup("pod-uid-1").EmptyFor; got != 0 {
		t.Errorf("EmptyFor = %v before the first report, want 0: a server that "+
			"has never reported is not known to be empty", got)
	}
}

// Connect forgets emptiness because the process may have restarted; Supersede
// keeps it because the displaced stream was live; Disconnect keeps it, which is
// inert because the count goes stale.
func TestEmptyForAcrossStreamChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event func(r *Registry)
		want  time.Duration
	}{
		{"connect clears it", func(r *Registry) { r.Connect("pod-uid-1", RoleServer) }, 0},
		{"supersede keeps it", func(r *Registry) { r.Supersede("pod-uid-1", RoleServer) }, time.Minute},
		{"disconnect keeps it", func(r *Registry) { r.Disconnect("pod-uid-1") }, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, clock := newTestRegistry()
			r.Connect("pod-uid-1", RoleServer)
			if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
				t.Fatalf("ReportPlayers: %v", err)
			}
			clock.Advance(time.Minute)

			tc.event(r)

			if got := r.Lookup("pod-uid-1").EmptyFor; got != tc.want {
				t.Errorf("EmptyFor = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAttachedToSumsTheProxiesThatAreCurrent(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)

	for _, uid := range []string{"proxy-a", "proxy-b"} {
		r.Connect(uid, RoleProxy)
	}
	if err := r.ReportBackends("proxy-a", "minecraft", map[string]int32{"lobby-0": 2, "lobby-1": 1}); err != nil {
		t.Fatalf("report from proxy-a: %v", err)
	}
	if err := r.ReportBackends("proxy-b", "minecraft", map[string]int32{"lobby-0": 3}); err != nil {
		t.Fatalf("report from proxy-b: %v", err)
	}

	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); n != 5 || stale {
		t.Errorf("lobby-0 = %d stale=%v, want 5 and fresh", n, stale)
	}
	if n, stale := r.AttachedTo("minecraft", "lobby-1", time.Time{}); n != 1 || stale {
		t.Errorf("lobby-1 = %d stale=%v, want 1 and fresh", n, stale)
	}
	// Absence from the map is an answer.
	if n, stale := r.AttachedTo("minecraft", "lobby-2", time.Time{}); n != 0 || stale {
		t.Errorf("lobby-2 = %d stale=%v, want 0 and fresh", n, stale)
	}
}

// Server names are unique per namespace, not across a cluster.
func TestAttachedToIsScopedToItsNamespace(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-other", RoleProxy)
	if err := r.ReportBackends("proxy-other", "other", map[string]int32{"lobby-0": 4}); err != nil {
		t.Fatalf("report: %v", err)
	}

	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); n != 0 || stale {
		t.Errorf("lobby-0 in minecraft = %d stale=%v, want 0 and fresh", n, stale)
	}
	if n, _ := r.AttachedTo("other", "lobby-0", time.Time{}); n != 4 {
		t.Errorf("lobby-0 in other = %d, want 4", n)
	}
}

func TestAProxyThatNeverReportedBackendsIsNotStale(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("old-proxy", RoleProxy)
	if err := r.ReportPlayers("old-proxy", 7, 100); err != nil {
		t.Fatalf("report players: %v", err)
	}
	clock.now = clock.now.Add(time.Hour)

	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); n != 0 || stale {
		t.Errorf("= %d stale=%v, want 0 and fresh: an old agent is silent, not stale", n, stale)
	}
}

func TestAProxyThatStoppedReportingBackendsIsStale(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-a", RoleProxy)
	if err := r.ReportBackends("proxy-a", "minecraft", map[string]int32{"lobby-0": 1}); err != nil {
		t.Fatalf("report: %v", err)
	}

	clock.now = clock.now.Add(9 * time.Second)
	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); n != 1 || stale {
		t.Errorf("at 9s: = %d stale=%v, want 1 and fresh", n, stale)
	}
	clock.now = clock.now.Add(3 * time.Second)
	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); !stale {
		t.Errorf("at 12s: = %d stale=%v, want stale", n, stale)
	}
}

// Storing one would let a compromised server pin any other server in its
// namespace as occupied.
func TestABackendReportFromAServerAgentIsRefused(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("a-server", RoleServer)

	if err := r.ReportBackends("a-server", "minecraft", map[string]int32{"lobby-0": 9}); err == nil {
		t.Fatal("a server agent's backend report was accepted")
	}
	if n, _ := r.AttachedTo("minecraft", "lobby-0", time.Time{}); n != 0 {
		t.Errorf("= %d, want 0: the refused report was stored anyway", n)
	}
}

func TestAReportFromBeforeTheDrainCannotAnswerAboutIt(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-a", RoleProxy)
	if err := r.ReportBackends("proxy-a", "minecraft", map[string]int32{}); err != nil {
		t.Fatalf("report: %v", err)
	}

	clock.now = clock.now.Add(time.Second)
	drainStart := clock.now
	clock.now = clock.now.Add(time.Second)

	if n, stale := r.AttachedTo("minecraft", "lobby-0", drainStart); !stale {
		t.Errorf("= %d stale=%v, want stale: this report predates the drain", n, stale)
	}
	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); n != 0 || stale {
		t.Errorf("with no threshold: = %d stale=%v, want 0 and fresh", n, stale)
	}
}

func TestTheNextReportAfterTheDrainAnswersIt(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-a", RoleProxy)
	if err := r.ReportBackends("proxy-a", "minecraft", map[string]int32{}); err != nil {
		t.Fatalf("report: %v", err)
	}
	drainStart := clock.now.Add(time.Second)

	clock.now = drainStart.Add(2 * time.Second)
	if _, stale := r.AttachedTo("minecraft", "lobby-0", drainStart); !stale {
		t.Fatal("the pre-drain report was believed, so this test would prove nothing")
	}

	if err := r.ReportBackends("proxy-a", "minecraft", map[string]int32{}); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if n, stale := r.AttachedTo("minecraft", "lobby-0", drainStart); n != 0 || stale {
		t.Errorf("= %d stale=%v, want 0 and fresh: the proxy has spoken since the drain", n, stale)
	}
}

func TestAnOldProxyIsStillNotHeldAgainstADrain(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("old-proxy", RoleProxy)
	if err := r.ReportPlayers("old-proxy", 5, 500); err != nil {
		t.Fatalf("report players: %v", err)
	}

	drainStart := clock.now.Add(time.Second)
	clock.now = drainStart.Add(time.Second)

	if n, stale := r.AttachedTo("minecraft", "lobby-0", drainStart); n != 0 || stale {
		t.Errorf("= %d stale=%v, want 0 and fresh", n, stale)
	}
}

func TestLookupCarriesWhenTheCountArrived(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("a-server", RoleServer)

	if got := r.Lookup("a-server").PlayersReportedAt; !got.IsZero() {
		t.Errorf("PlayersReportedAt = %v before any report, want zero", got)
	}
	clock.now = clock.now.Add(3 * time.Second)
	if err := r.ReportPlayers("a-server", 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	if got := r.Lookup("a-server").PlayersReportedAt; !got.Equal(clock.now) {
		t.Errorf("PlayersReportedAt = %v, want %v", got, clock.now)
	}
}

func TestTheShortestReadTimeoutWins(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)

	for _, uid := range []string{"proxy-a", "proxy-b"} {
		r.Connect(uid, RoleProxy)
	}
	r.ReportReadTimeout("proxy-a", "minecraft", 30*time.Second)
	r.ReportReadTimeout("proxy-b", "minecraft", 12*time.Second)

	if got, known := r.ShortestReadTimeout("minecraft"); !known || got != 12*time.Second {
		t.Errorf("ShortestReadTimeout = %s known=%v, want 12s", got, known)
	}
}

func TestAnUnreportedReadTimeoutIsUnknownRatherThanZero(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)

	r.Connect("proxy-a", RoleProxy)
	if got, known := r.ShortestReadTimeout("minecraft"); known {
		t.Errorf("a proxy that said nothing answered %s as known", got)
	}

	// An older agent's Hello carries zero, and since the smallest wins it must
	// not count.
	r.ReportReadTimeout("proxy-a", "minecraft", 0)
	if got, known := r.ShortestReadTimeout("minecraft"); known {
		t.Errorf("a reported zero answered %s as known", got)
	}
}

func TestTheReadTimeoutIsScopedAndProxyOnly(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)

	r.Connect("elsewhere", RoleProxy)
	r.ReportReadTimeout("elsewhere", "other", 3*time.Second)
	r.Connect("a-server", RoleServer)
	r.ReportReadTimeout("a-server", "minecraft", 4*time.Second)
	r.Connect("gone", RoleProxy)
	r.ReportReadTimeout("gone", "minecraft", 5*time.Second)
	r.Disconnect("gone")
	r.Connect("here", RoleProxy)
	r.ReportReadTimeout("here", "minecraft", 20*time.Second)

	if got, known := r.ShortestReadTimeout("minecraft"); !known || got != 20*time.Second {
		t.Errorf("ShortestReadTimeout = %s known=%v, want the 20s of the one connected proxy "+
			"in this namespace", got, known)
	}
}

func TestRosterMergesEveryProxyInTheNamespace(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	for _, uid := range []string{"proxy-a", "proxy-b"} {
		r.Connect(uid, RoleProxy)
	}
	if err := r.ReportRoster("proxy-a", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-0"},
	}); err != nil {
		t.Fatalf("report from proxy-a: %v", err)
	}
	if err := r.ReportRoster("proxy-b", "minecraft", []RosterEntry{
		{UUID: "u-bob", Name: "bob", Server: "lobby-1"},
	}); err != nil {
		t.Fatalf("report from proxy-b: %v", err)
	}

	got, stale := r.Roster("minecraft")
	if stale {
		t.Error("stale = true with two fresh reports")
	}
	if len(got) != 2 || got[0].UUID != "u-alice" || got[1].UUID != "u-bob" {
		t.Fatalf("roster = %+v, want both players sorted by UUID", got)
	}
}

func TestRosterKeepsOneEntryPerPlayerAcrossProxies(t *testing.T) {
	// A rollout hands the player over; they must be counted once.
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-a", RoleProxy)
	r.Connect("proxy-b", RoleProxy)

	if err := r.ReportRoster("proxy-a", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-0"},
	}); err != nil {
		t.Fatalf("report from proxy-a: %v", err)
	}
	clock.now = clock.now.Add(time.Second)
	if err := r.ReportRoster("proxy-b", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-1"},
	}); err != nil {
		t.Fatalf("report from proxy-b: %v", err)
	}

	got, _ := r.Roster("minecraft")
	if len(got) != 1 {
		t.Fatalf("roster = %+v, want one entry for one player", got)
	}
	if got[0].Server != "lobby-1" {
		t.Errorf("server = %q, want the most recently reported one", got[0].Server)
	}
}

func TestRosterSkipsAProxyWhoseReportWentStale(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-a", RoleProxy)
	if err := r.ReportRoster("proxy-a", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-0"},
	}); err != nil {
		t.Fatalf("report: %v", err)
	}

	clock.now = clock.now.Add(11 * time.Second) // > 2 * 5s

	got, stale := r.Roster("minecraft")
	if !stale {
		t.Error("stale = false for a report older than twice the interval")
	}
	if len(got) != 0 {
		t.Errorf("roster = %+v, want nothing from a stale proxy", got)
	}
}

func TestARosterFromAServerAgentIsRefused(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("pod-uid-1", RoleServer)

	if err := r.ReportRoster("pod-uid-1", "minecraft", nil); err == nil {
		t.Error("a server agent's roster was accepted")
	}
}

func TestRosterIsScopedToItsNamespace(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-other", RoleProxy)
	if err := r.ReportRoster("proxy-other", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-0"},
	}); err != nil {
		t.Fatalf("report: %v", err)
	}

	if got, _ := r.Roster("other"); len(got) != 0 {
		t.Errorf("roster = %+v, want nothing from another namespace", got)
	}
}

func TestAnAnnouncementIsKeptUnderTheNameTheIdentityGave(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)

	if err := r.ReportAnnouncement("pod-a", "ns", "lobby-a", Announcement{
		State:      "running",
		Attributes: map[string]string{"map": "arena"},
	}); err != nil {
		t.Fatalf("ReportAnnouncement: %v", err)
	}

	got := r.Announcements("ns")
	if got["lobby-a"].State != "running" || got["lobby-a"].Attributes["map"] != "arena" {
		t.Errorf("announcements = %+v, want what lobby-a said", got)
	}
	if len(r.Announcements("other")) != 0 {
		t.Errorf("another namespace sees %+v", r.Announcements("other"))
	}
}

func TestAnAnnouncementReplacesItsPredecessorWhole(t *testing.T) {
	// Not merged: otherwise an attribute could never be taken back.
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)

	_ = r.ReportAnnouncement("pod-a", "ns", "lobby-a", Announcement{
		State:      "waiting",
		Attributes: map[string]string{"map": "arena", "teams": "2"},
	})
	_ = r.ReportAnnouncement("pod-a", "ns", "lobby-a", Announcement{
		State:      "running",
		Attributes: map[string]string{"map": "arena"},
	})

	got := r.Announcements("ns")["lobby-a"]
	if got.State != "running" {
		t.Errorf("state = %q, want the newer one", got.State)
	}
	if _, ok := got.Attributes["teams"]; ok {
		t.Errorf("attributes = %v, want the older ones gone", got.Attributes)
	}
}

func TestAnAnnouncementIsNotAliasedToTheCallersMap(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)

	attributes := map[string]string{"map": "arena"}
	_ = r.ReportAnnouncement("pod-a", "ns", "lobby-a", Announcement{Attributes: attributes})
	attributes["map"] = "somewhere else"

	if got := r.Announcements("ns")["lobby-a"].Attributes["map"]; got != "arena" {
		t.Errorf("map = %q, want the value as it was announced", got)
	}
	handed := r.Announcements("ns")["lobby-a"].Attributes
	handed["map"] = "mutated"
	if got := r.Announcements("ns")["lobby-a"].Attributes["map"]; got != "arena" {
		t.Errorf("map = %q after a reader wrote to what it was handed", got)
	}
}

func TestAnAnnouncementOutlivesADisconnect(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)
	_ = r.ReportAnnouncement("pod-a", "ns", "lobby-a", Announcement{State: "running"})

	r.Disconnect("pod-a")

	if got := r.Announcements("ns")["lobby-a"].State; got != "running" {
		t.Errorf("state = %q after a disconnect, want it kept", got)
	}
}

func TestAProxyCannotAnnounce(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("proxy-a", RoleProxy)

	if err := r.ReportAnnouncement("proxy-a", "ns", "gateway-0",
		Announcement{State: "running"}); err == nil {
		t.Error("a proxy's announcement was accepted")
	}
	if len(r.Announcements("ns")) != 0 {
		t.Errorf("announcements = %+v, want none", r.Announcements("ns"))
	}
}

func TestAnAnnouncementNeedsALiveStream(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())

	if err := r.ReportAnnouncement("pod-a", "ns", "lobby-a",
		Announcement{State: "running"}); err == nil {
		t.Error("an announcement from a pod with no stream was accepted")
	}
}

func TestAServerTakesPlayersUntilItSaysOtherwise(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())

	if !r.Lookup("nobody-has-heard-of-this-pod").AcceptingJoins {
		t.Error("a pod the registry has never seen was read as refusing players")
	}

	r.Connect("pod-a", RoleServer)
	if !r.Lookup("pod-a").AcceptingJoins {
		t.Error("a server that has said nothing was read as refusing players")
	}
}

func TestAServerCanCloseItsDoorAndOpenItAgain(t *testing.T) {
	// Both directions: unlike a retire, this verb has a way back.
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)

	if _, err := r.ReportAcceptJoins("pod-a", "ns", false, false); err != nil {
		t.Fatalf("ReportAcceptJoins(false): %v", err)
	}
	if r.Lookup("pod-a").AcceptingJoins {
		t.Error("a server that closed its door was still read as taking players")
	}

	if _, err := r.ReportAcceptJoins("pod-a", "ns", true, false); err != nil {
		t.Fatalf("ReportAcceptJoins(true): %v", err)
	}
	if !r.Lookup("pod-a").AcceptingJoins {
		t.Error("a server that opened its door again was still read as refusing")
	}
}

func TestReportAcceptJoinsSaysWhenTheDoorMoved(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)

	for _, step := range []struct {
		accept, want bool
	}{{true, false}, {false, true}, {false, false}, {true, true}} {
		changed, err := r.ReportAcceptJoins("pod-a", "ns", step.accept, false)
		if err != nil {
			t.Fatalf("ReportAcceptJoins(%v): %v", step.accept, err)
		}
		if changed != step.want {
			t.Errorf("ReportAcceptJoins(%v) changed = %v, want %v", step.accept, changed, step.want)
		}
	}
}

func TestAClosedDoorOutlivesADisconnect(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)
	_, _ = r.ReportAcceptJoins("pod-a", "ns", false, false)

	r.Disconnect("pod-a")

	if r.Lookup("pod-a").AcceptingJoins {
		t.Error("a disconnect opened a door the server had shut")
	}
}

func TestAProxyHasNoDoorToClose(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("proxy-a", RoleProxy)

	if _, err := r.ReportAcceptJoins("proxy-a", "ns", false, false); err == nil {
		t.Error("a proxy was allowed to close a door it does not have")
	}
}

func TestARoundEndIsRememberedAfterTheStreamDrops(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)

	if got := r.Lookup("pod-a").RoundEnded; got {
		t.Error("a server that has said nothing has not ended a round")
	}
	if _, err := r.ReportAcceptJoins("pod-a", "ns", false, true); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}
	if got := r.Lookup("pod-a"); !got.RoundEnded || got.AcceptingJoins {
		t.Errorf("after the round ended: %+v", got)
	}

	// The phase that reads it only runs once the pod is terminal.
	r.Disconnect("pod-a")
	if got := r.Lookup("pod-a").RoundEnded; !got {
		t.Error("the round end was forgotten when the stream dropped")
	}
}

func TestClosedDoorsListsAPodThatClosedItsDoor(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)
	if _, err := r.ReportAcceptJoins("pod-a", "ns", false, false); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}

	if closed := r.ClosedDoors("ns"); !closed["pod-a"] {
		t.Errorf("ClosedDoors(ns) = %v, want pod-a", closed)
	}
}

func TestClosedDoorsDropsAPodThatReopened(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)
	_, _ = r.ReportAcceptJoins("pod-a", "ns", false, false)
	if _, err := r.ReportAcceptJoins("pod-a", "ns", true, false); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}

	if closed := r.ClosedDoors("ns"); closed["pod-a"] {
		t.Errorf("ClosedDoors(ns) = %v, want pod-a open again", closed)
	}
}

func TestClosedDoorsOmitsAServerNothingIsKnownAbout(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())

	if closed := r.ClosedDoors("ns"); len(closed) != 0 {
		t.Errorf("ClosedDoors(ns) = %v, want none", closed)
	}
}

func TestClosedDoorsSurvivesASameUIDDisconnect(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("pod-a", RoleServer)
	_, _ = r.ReportAcceptJoins("pod-a", "ns", false, false)

	r.Disconnect("pod-a")

	if closed := r.ClosedDoors("ns"); !closed["pod-a"] {
		t.Errorf("ClosedDoors(ns) = %v, want pod-a still closed across a disconnect", closed)
	}
}

func TestClosedDoorsIsKeyedByPodNotByServerName(t *testing.T) {
	r := New(time.Now, time.Second, time.Now())
	r.Connect("old-pod", RoleServer)
	_, _ = r.ReportAcceptJoins("old-pod", "ns", false, false)

	r.Connect("new-pod", RoleServer)
	_, _ = r.ReportAcceptJoins("new-pod", "ns", true, false)

	closed := r.ClosedDoors("ns")
	if !closed["old-pod"] {
		t.Errorf("ClosedDoors(ns) = %v, want old-pod still closed", closed)
	}
	if closed["new-pod"] {
		t.Errorf("ClosedDoors(ns) = %v, want new-pod open", closed)
	}
}

func TestAnUnknownPodIsMeasuredFromWhenAgentsCouldReachTheOperator(t *testing.T) {
	r, clock := newTestRegistry()
	clock.Advance(20 * time.Second) // leader election
	r.MarkServing()
	clock.Advance(3 * time.Second)

	if got := r.Lookup("never-seen").StreamDownFor; got != 3*time.Second {
		t.Errorf("StreamDownFor = %v, want 3s since serving began, not 23s since the process started", got)
	}
	r.MarkServing()
	clock.Advance(time.Second)
	if got := r.Lookup("never-seen").StreamDownFor; got != 4*time.Second {
		t.Errorf("StreamDownFor = %v after a second MarkServing, want 4s", got)
	}
}

func TestReportTicksShowsInSnapshot(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New(func() time.Time { return now }, 5*time.Second, now)
	r.Connect("pod-a", RoleServer)
	if err := r.ReportPlayers("pod-a", 2, 20); err != nil {
		t.Fatal(err)
	}
	if err := r.ReportTicks("pod-a", 19.5, 12.25); err != nil {
		t.Fatal(err)
	}
	snap := r.Lookup("pod-a")
	if snap.TPS != 19.5 || snap.MSPT != 12.25 {
		t.Fatalf("snapshot TPS/MSPT = %v/%v, want 19.5/12.25", snap.TPS, snap.MSPT)
	}
}

func TestReportTicksRefusesImpossibleValues(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New(func() time.Time { return now }, 5*time.Second, now)
	r.Connect("pod-a", RoleServer)
	for _, c := range []struct{ tps, mspt float64 }{
		{-1, 10}, {101, 10}, {20, -1}, {20, 600001}, {math.NaN(), 10}, {20, math.Inf(1)},
	} {
		if err := r.ReportTicks("pod-a", c.tps, c.mspt); err == nil {
			t.Errorf("ReportTicks(%v, %v) accepted", c.tps, c.mspt)
		}
	}
	if snap := r.Lookup("pod-a"); snap.TPS != 0 || snap.MSPT != 0 {
		t.Fatalf("a refused report was kept: %v/%v", snap.TPS, snap.MSPT)
	}
}

func TestReportHeapShowsInSnapshot(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New(func() time.Time { return now }, 5*time.Second, now)
	r.Connect("pod-a", RoleServer)
	if snap := r.Lookup("pod-a"); snap.HeapUsed != 0 || snap.HeapMax != 0 {
		t.Fatalf("heap before any report = %v/%v, want 0/0", snap.HeapUsed, snap.HeapMax)
	}
	if err := r.ReportHeap("pod-a", 1<<30, 4<<30); err != nil {
		t.Fatal(err)
	}
	if snap := r.Lookup("pod-a"); snap.HeapUsed != 1<<30 || snap.HeapMax != 4<<30 {
		t.Fatalf("heap = %v/%v, want 1GiB/4GiB", snap.HeapUsed, snap.HeapMax)
	}
}

func TestReportHeapRefusesNegatives(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New(func() time.Time { return now }, 5*time.Second, now)
	r.Connect("pod-a", RoleServer)
	for _, c := range []struct{ used, max int64 }{{-1, 10}, {10, -1}} {
		if err := r.ReportHeap("pod-a", c.used, c.max); err == nil {
			t.Errorf("ReportHeap(%v, %v) accepted", c.used, c.max)
		}
	}
}

func TestReportTicksNeedsALiveStream(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New(func() time.Time { return now }, 5*time.Second, now)
	if err := r.ReportTicks("nobody", 20, 10); err == nil {
		t.Fatal("ReportTicks accepted a pod with no stream")
	}
}

func TestThePlayableFigureReachesTheSnapshot(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	if err := r.ReportPlayers("pod-uid-1", 14, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	if err := r.ReportPlayableSlots("pod-uid-1", 12); err != nil {
		t.Fatalf("ReportPlayableSlots: %v", err)
	}
	if got := r.Lookup("pod-uid-1"); got.PlayableSlots != 12 || got.Players != 14 {
		t.Errorf("snapshot = %+v, want 14 players against 12 playable seats kept", got)
	}
}

func TestANegativePlayableFigureIsRefusedAndTheLastOneStands(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	if err := r.ReportPlayableSlots("pod-uid-1", 12); err != nil {
		t.Fatalf("ReportPlayableSlots: %v", err)
	}
	if err := r.ReportPlayableSlots("pod-uid-1", -1); err == nil {
		t.Fatal("a negative playable figure was accepted")
	}
	if got := r.Lookup("pod-uid-1").PlayableSlots; got != 12 {
		t.Errorf("PlayableSlots = %d, want the previous 12", got)
	}
}

func TestPlayableSlotsNeedsALiveStream(t *testing.T) {
	r, _ := newTestRegistry()
	if err := r.ReportPlayableSlots("nobody", 12); err == nil {
		t.Fatal("a report for an unknown pod was accepted")
	}
}
