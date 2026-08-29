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

// Make-before-break: the agent opens its next stream while the current one is
// still up, and that stream is registered before its Hello arrives. Readiness
// has to survive the handover, or every renewal would look like a readiness
// loss to the reconciler.
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

	// A second zero report does not restart the clock: the server has been
	// empty since the first one, and the stabilization window measures that.
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

// TestEmptyForAcrossStreamChanges pins the three edges the design fixes on
// purpose. Connect may have a restarted process behind it and must forget what
// the previous one reported; Supersede cannot, because the displaced stream was
// still live; Disconnect keeps it, which is inert because the count goes stale
// and stale counts as occupied.
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

// TestAttachedToSumsTheProxiesThatAreCurrent is the query the drain's exit
// condition rests on: how many players are on, or heading to, one backend,
// across every proxy that has said so recently.
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
	// A backend nobody named has nobody on it. That is what makes the map a
	// state rather than a stream of changes: absence is an answer.
	if n, stale := r.AttachedTo("minecraft", "lobby-2", time.Time{}); n != 0 || stale {
		t.Errorf("lobby-2 = %d stale=%v, want 0 and fresh", n, stale)
	}
}

// TestAttachedToIsScopedToItsNamespace keeps one network's proxies from
// answering about another's. Server names are unique per namespace and not
// across a cluster, so a sum that ignored the namespace would hold a server
// occupied because a same-named server elsewhere had players.
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

// TestAProxyThatNeverReportedBackendsIsNotStale is the property that lets a
// fleet upgrade in any order, and it is the one most easily got wrong.
//
// An agent too old to send the report says nothing, and reading that as "this
// proxy may be hiding players" would hold every server in the installation
// occupied for as long as one un-upgraded proxy ran -- no drain would ever
// finish. Silent and old are different states and only the first is stale.
func TestAProxyThatNeverReportedBackendsIsNotStale(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("old-proxy", RoleProxy)
	// It reports players like any agent, and nothing about backends.
	if err := r.ReportPlayers("old-proxy", 7, 100); err != nil {
		t.Fatalf("report players: %v", err)
	}
	clock.now = clock.now.Add(time.Hour)

	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); n != 0 || stale {
		t.Errorf("= %d stale=%v, want 0 and fresh: an old agent is silent, not stale", n, stale)
	}
}

// TestAProxyThatStoppedReportingBackendsIsStale is the other half. A proxy
// that used to report and no longer does may be holding players on this
// backend without saying so, and the caller has to read that as occupied for
// the same reason it reads a stale player count that way.
func TestAProxyThatStoppedReportingBackendsIsStale(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-a", RoleProxy)
	if err := r.ReportBackends("proxy-a", "minecraft", map[string]int32{"lobby-0": 1}); err != nil {
		t.Fatalf("report: %v", err)
	}

	// Inside twice the report interval it still counts.
	clock.now = clock.now.Add(9 * time.Second)
	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); n != 1 || stale {
		t.Errorf("at 9s: = %d stale=%v, want 1 and fresh", n, stale)
	}
	// Past it, the count is not believed and the caller is told so.
	clock.now = clock.now.Add(3 * time.Second)
	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); !stale {
		t.Errorf("at 12s: = %d stale=%v, want stale", n, stale)
	}
}

// TestABackendReportFromAServerAgentIsRefused keeps the direction of the
// channel straight. A server agent knows about itself and about nobody else,
// so a backend map from one is an agent bug rather than a state to store --
// and storing it would let one compromised server pin any other server in its
// namespace as occupied for ever.
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

// TestAReportFromBeforeTheDrainCannotAnswerAboutIt is the deeper half of the
// drain gap, and the one that is easiest to mistake for freshness.
//
// A count taken four seconds ago is perfectly fresh and says nothing about a
// player who joined three seconds ago. Every source is asked the same way: a
// report that predates the question cannot answer it, however recent it is.
func TestAReportFromBeforeTheDrainCannotAnswerAboutIt(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-a", RoleProxy)
	if err := r.ReportBackends("proxy-a", "minecraft", map[string]int32{}); err != nil {
		t.Fatalf("report: %v", err)
	}

	// The drain begins a second after that report. The report is well inside
	// its freshness window and still cannot say whether the drain is done.
	clock.now = clock.now.Add(time.Second)
	drainStart := clock.now
	clock.now = clock.now.Add(time.Second)

	if n, stale := r.AttachedTo("minecraft", "lobby-0", drainStart); !stale {
		t.Errorf("= %d stale=%v, want stale: this report predates the drain", n, stale)
	}
	// Without a drain to be about, the same report answers perfectly well.
	if n, stale := r.AttachedTo("minecraft", "lobby-0", time.Time{}); n != 0 || stale {
		t.Errorf("with no threshold: = %d stale=%v, want 0 and fresh", n, stale)
	}
}

// TestTheNextReportAfterTheDrainAnswersIt is what keeps the rule above from
// being a permanent hold. It clears on the proxy's very next report, at most
// one interval away -- which is the whole cost of the rule.
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

	// One report interval later the proxy speaks again, and now it is
	// answering the question that was actually asked.
	if err := r.ReportBackends("proxy-a", "minecraft", map[string]int32{}); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if n, stale := r.AttachedTo("minecraft", "lobby-0", drainStart); n != 0 || stale {
		t.Errorf("= %d stale=%v, want 0 and fresh: the proxy has spoken since the drain", n, stale)
	}
}

// TestAnOldProxyIsStillNotHeldAgainstADrain keeps the upgrade-in-any-order
// property intact under the new rule. An agent that cannot report backends has
// no report to predate anything, and treating it as one would hold every
// draining server in the installation for as long as one old proxy ran.
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

// TestLookupCarriesWhenTheCountArrived is the backend half of the same rule.
// The Server controller compares this against the drain's start, so a
// snapshot that did not carry it would leave that half unanswerable.
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

// TestTheShortestReadTimeoutWins is the rule the whole field exists for: a
// fleet is only as patient as its least patient proxy, because whichever gives
// up first is the one that disconnects the players the operator was about to
// move.
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

// TestAnUnreportedReadTimeoutIsUnknownRatherThanZero keeps the fallback honest.
// Zero and "the shipped default" are different answers, and the caller has to
// be able to tell them apart: an agent too old to send the field must leave the
// operator reading what this repository ships, not reading no patience at all.
func TestAnUnreportedReadTimeoutIsUnknownRatherThanZero(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)

	r.Connect("proxy-a", RoleProxy)
	if got, known := r.ShortestReadTimeout("minecraft"); known {
		t.Errorf("a proxy that said nothing answered %s as known", got)
	}

	// A zero is what an older agent's Hello carries, and it must not become an
	// answer either -- one silent agent would otherwise speak for every
	// talkative one, since the smallest wins.
	r.ReportReadTimeout("proxy-a", "minecraft", 0)
	if got, known := r.ShortestReadTimeout("minecraft"); known {
		t.Errorf("a reported zero answered %s as known", got)
	}
}

// TestTheReadTimeoutIsScopedAndProxyOnly covers the three ways a value must not
// count: another namespace's proxy, a disconnected one, and a server agent,
// which reports about itself and has no such timeout at all.
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
	// A player appears on two proxies while a rollout hands them over. They
	// are one person and must be counted once -- a plugin iterating this to
	// message everybody would otherwise message them twice.
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
	// The rule the counts already use: older than twice the report interval.
	// A proxy that stopped reporting must stop asserting who is online rather
	// than freezing a roster.
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
	// A backend has no view of anybody but its own players and no UUIDs at
	// all, so a roster from one is a bug in an agent rather than a state to
	// store. Same rule, same reason, as ReportBackends.
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("pod-uid-1", RoleServer)

	if err := r.ReportRoster("pod-uid-1", "minecraft", nil); err == nil {
		t.Error("a server agent's roster was accepted")
	}
}

func TestRosterIsScopedToItsNamespace(t *testing.T) {
	// namespace comes from the authenticated identity at the call site, never
	// from the message. This asserts the reader's half.
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
