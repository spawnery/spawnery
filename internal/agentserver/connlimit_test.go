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

package agentserver

import (
	"net"
	"sync"
	"testing"
	"time"
)

// serveLimited runs a limiter over a real loopback listener and hands every
// accepted connection to the caller's channel. A real socket rather than a
// pipe, because the thing under test reads RemoteAddr and splits a port off
// it: net.Pipe has no addresses at all and would let peerKey's fallback pass
// for the behaviour that matters.
func serveLimited(t *testing.T, limit int) (*PeerLimiter, <-chan net.Conn, func() []ConnEvent) {
	t.Helper()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	// A copy under the lock rather than the slice itself: the observer keeps
	// appending while a test reads, and handing out the header would be a race
	// on the header -- which is what the first draft of this helper did, and
	// -race caught.
	var mu sync.Mutex
	events := make([]ConnEvent, 0, 16)
	recorded := func() []ConnEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]ConnEvent(nil), events...)
	}
	limiter := NewPeerLimiter(inner, limit, func(ev ConnEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	})
	t.Cleanup(func() { _ = limiter.Close() })

	accepted := make(chan net.Conn, 64)
	go func() {
		for {
			conn, err := limiter.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- conn
		}
	}()
	return limiter, accepted, recorded
}

func dial(t *testing.T, limiter *PeerLimiter) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", limiter.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestALimitedPeerIsRefusedAtItsBound is the whole point of the type: the
// connection over the limit is closed rather than served, and the ones under
// it are untouched.
func TestALimitedPeerIsRefusedAtItsBound(t *testing.T) {
	const limit = 3
	limiter, accepted, _ := serveLimited(t, limit)

	for i := 0; i < limit; i++ {
		dial(t, limiter)
		select {
		case <-accepted:
		case <-time.After(5 * time.Second):
			t.Fatalf("connection %d of the permitted %d was not served", i+1, limit)
		}
	}

	// The one over the bound. It is refused at the listener, so nothing is
	// handed to the server -- and the client learns only from the close, since
	// the kernel completed the handshake before Accept ever saw it.
	over := dial(t, limiter)
	select {
	case conn := <-accepted:
		t.Fatalf("connection %d was served from %s; the bound is not in force",
			limit+1, conn.RemoteAddr())
	case <-time.After(500 * time.Millisecond):
	}

	_ = over.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil {
		t.Error("the refused connection is still readable")
	}
}

// TestAClosedConnectionGivesItsSlotBack is the other half. Without it the
// bound would be a lifetime quota rather than a concurrency limit, and a pod
// that renewed MaxConnectionsPerPeer times would lock itself out permanently.
func TestAClosedConnectionGivesItsSlotBack(t *testing.T) {
	const limit = 2
	limiter, accepted, _ := serveLimited(t, limit)

	first := dial(t, limiter)
	served := <-accepted
	dial(t, limiter)
	<-accepted

	// At the bound now. Give one back from the server side, which is the side
	// that counts.
	_ = served.Close()
	_ = first.Close()

	dial(t, limiter)
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the slot a closed connection freed was never reusable")
	}
}

// TestClosingTwiceReleasesOneSlot guards the sync.Once. net.Conn permits a
// second Close and grpc-go's transport makes one, so a naive decrement would
// hand the peer a slot it never gave back -- and the bound would then leak
// upward, silently, one double-close at a time.
func TestClosingTwiceReleasesOneSlot(t *testing.T) {
	limiter, accepted, _ := serveLimited(t, 2)

	dial(t, limiter)
	served := <-accepted
	_ = served.Close()
	_ = served.Close()

	limiter.mu.Lock()
	open := limiter.open[peerKey(served.RemoteAddr())]
	limiter.mu.Unlock()
	if open != 0 {
		t.Errorf("open = %d after a double close, want 0", open)
	}
}

// TestAZeroLimitCountsWithoutRefusing is the mode cmd/spawnery-stubop measures
// in. The peak it reports is the number MaxConnectionsPerPeer is derived from,
// so a zero limit quietly refusing anything would corrupt that measurement.
func TestAZeroLimitCountsWithoutRefusing(t *testing.T) {
	limiter, accepted, recorded := serveLimited(t, 0)

	const many = 12
	for i := 0; i < many; i++ {
		dial(t, limiter)
		select {
		case <-accepted:
		case <-time.After(5 * time.Second):
			t.Fatalf("connection %d was refused under a zero limit", i+1)
		}
	}

	peak := 0
	for _, ev := range recorded() {
		if ev.Refused {
			t.Fatal("a zero limit refused a connection")
		}
		if ev.Peak > peak {
			peak = ev.Peak
		}
	}
	if peak != many {
		t.Errorf("peak = %d, want %d", peak, many)
	}
}

// TestARefusedPeerIsLoggedOnPowersOfTen pins the throttle rather than the
// wording. A line per refusal would make the operator's log the amplifier the
// connections themselves no longer are, so what the rule admits is the part
// worth a test.
func TestARefusedPeerIsLoggedOnPowersOfTen(t *testing.T) {
	for n, want := range map[int]bool{
		0: false, 1: true, 2: false, 9: false, 10: true,
		11: false, 99: false, 100: true, 1000: true, 1001: false,
	} {
		if got := isPowerOfTen(n); got != want {
			t.Errorf("isPowerOfTen(%d) = %v, want %v", n, got, want)
		}
	}
}

// TestTheRefusalCountResetsWithThePeer keeps the refusal map bounded by the
// live fleet. It is the one map an attacker drives the size of, so a key that
// outlived its peer would be a leak they choose the rate of.
func TestTheRefusalCountResetsWithThePeer(t *testing.T) {
	limiter, accepted, _ := serveLimited(t, 1)

	dial(t, limiter)
	served := <-accepted
	peer := peerKey(served.RemoteAddr())

	for i := 0; i < 3; i++ {
		dial(t, limiter)
	}
	// The refusals are asynchronous; wait for the count rather than assume it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		limiter.mu.Lock()
		refused := limiter.refused[peer]
		limiter.mu.Unlock()
		if refused >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refusals = %d, want 3", refused)
		}
		time.Sleep(10 * time.Millisecond)
	}

	_ = served.Close()
	deadline = time.Now().Add(5 * time.Second)
	for {
		limiter.mu.Lock()
		_, stillOpen := limiter.open[peer]
		_, stillRefused := limiter.refused[peer]
		limiter.mu.Unlock()
		if !stillOpen && !stillRefused {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer's entries outlived its last connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPeerKeyDropsThePort is what makes the bound per pod rather than per
// connection: every connection from one pod has a different source port, so
// keeping it would give each its own bucket and bound nothing at all.
func TestPeerKeyDropsThePort(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr net.Addr
		want string
	}{
		{"ipv4", &net.TCPAddr{IP: net.IPv4(10, 1, 90, 12), Port: 41234}, "10.1.90.12"},
		{"ipv6", &net.TCPAddr{IP: net.ParseIP("fd00::1"), Port: 41234}, "fd00::1"},
		{"portless", &net.UnixAddr{Name: "/tmp/x", Net: "unix"}, "/tmp/x"},
		{"nil", nil, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := peerKey(tc.addr); got != tc.want {
				t.Errorf("peerKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// dialFrom dials the limiter from a chosen local address, which is how a test
// gets more than one peer out of a loopback listener: every address in
// 127.0.0.0/8 is local on Linux, so 127.0.0.2 reaches a listener on 127.0.0.1
// and arrives with a different RemoteAddr. Skips rather than fails where that
// is not true -- the fleet bound is about the sum across peers, and a platform
// with one usable loopback address cannot show that either way.
func dialFrom(t *testing.T, limiter *PeerLimiter, local string) net.Conn {
	t.Helper()
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(local)}}
	conn, err := dialer.Dial("tcp", limiter.Addr().String())
	if err != nil {
		t.Skipf("dialling from %s: %v", local, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// served waits for one connection to reach the server side, and says whether
// it did. The negative case is a wait for nothing to happen, so it is
// deliberately not a Fatal: both readings are results here.
func served(t *testing.T, accepted <-chan net.Conn) bool {
	t.Helper()
	select {
	case <-accepted:
		return true
	case <-time.After(time.Second):
		return false
	}
}

// lastRefusal is the most recent refusal event, which is the one carrying
// which bound did the refusing.
func lastRefusal(t *testing.T, recorded func() []ConnEvent) ConnEvent {
	t.Helper()
	events := recorded()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Refused {
			return events[i]
		}
	}
	t.Fatal("no connection was refused")
	return ConnEvent{}
}

// TestTheFleetBoundTightensEveryPeerAtItsCeiling is the fleet half of the
// type. The per-peer bound here is 8 and nothing about this peer changes; what
// changes is that the operator is holding more connections than one pod can
// account for, and every peer's slack goes away at once.
func TestTheFleetBoundTightensEveryPeerAtItsCeiling(t *testing.T) {
	limiter, accepted, recorded := serveLimited(t, 8)
	limiter.Expect(func() (int, bool) { return 1, true })

	// One pod expected, so the ceiling is FleetConnectionsPerAgent. Everything
	// up to it is served: the tightened bound is still above any legitimate
	// agent's peak, which is the property it is chosen for.
	for i := 0; i < FleetConnectionsPerAgent; i++ {
		dial(t, limiter)
		if !served(t, accepted) {
			t.Fatalf("connection %d was refused below the ceiling", i+1)
		}
	}

	dial(t, limiter)
	if served(t, accepted) {
		t.Fatal("the connection over the fleet ceiling was served")
	}
	ev := lastRefusal(t, recorded)
	if ev.Bound != BoundFleet {
		t.Errorf("the refusal was reported as %q, want %q", ev.Bound, BoundFleet)
	}
	if ev.Limit != FleetConnectionsPerAgent {
		t.Errorf("refused at limit %d, want the fleet bound %d", ev.Limit, FleetConnectionsPerAgent)
	}
}

// TestAnUncountedFleetBoundsNobody is the fail-open direction, and it is the
// one that matters most in a cluster: the count comes from a cache, and a
// cache is empty for a moment after every operator restart. Reading that as
// "this fleet should hold nothing" would refuse every agent in the cluster at
// the worst possible moment.
func TestAnUncountedFleetBoundsNobody(t *testing.T) {
	limiter, accepted, _ := serveLimited(t, 8)
	limiter.Expect(func() (int, bool) { return 0, false })

	for i := 0; i < 8; i++ {
		dial(t, limiter)
		if !served(t, accepted) {
			t.Fatalf("connection %d was refused, but no fleet size is known", i+1)
		}
	}
}

// TestACountedEmptyFleetStillServesTheLegitimateShape is the other half of the
// same distinction. Zero pods is a real answer and does tighten the bound --
// but tightening is not refusing, and a peer within the shape a working agent
// has is served even by a fleet that ought to have no agents at all.
func TestACountedEmptyFleetStillServesTheLegitimateShape(t *testing.T) {
	limiter, accepted, recorded := serveLimited(t, 8)
	limiter.Expect(func() (int, bool) { return 0, true })

	for i := 0; i < FleetConnectionsPerAgent; i++ {
		dial(t, limiter)
		if !served(t, accepted) {
			t.Fatalf("connection %d was refused below the legitimate shape", i+1)
		}
	}
	dial(t, limiter)
	if served(t, accepted) {
		t.Fatal("a fleet counted as empty served past the tightened bound")
	}
	if ev := lastRefusal(t, recorded); ev.Bound != BoundFleet {
		t.Errorf("the refusal was reported as %q, want %q", ev.Bound, BoundFleet)
	}
}

// TestTheFleetCeilingRefusesAPeerHoldingNothing is the one thing a per-peer
// bound could never say. Every peer here is within the bound the fleet
// tightened them to; what is over its limit is the fleet, and the peer refused
// is holding no connections at all. Nothing compared those two numbers before,
// and that comparison is the whole of what this closes.
func TestTheFleetCeilingRefusesAPeerHoldingNothing(t *testing.T) {
	limiter, accepted, recorded := serveLimited(t, MaxConnectionsPerPeer)
	limiter.Expect(func() (int, bool) { return 1, true })

	// One pod expected, so the ceiling on the sum is MaxConnectionsPerPeer and
	// no single peer can reach it -- the tightening below cuts each of them to
	// FleetConnectionsPerAgent first. Two peers together do.
	for _, local := range []string{"127.0.0.2", "127.0.0.3"} {
		for i := 0; i < FleetConnectionsPerAgent; i++ {
			dialFrom(t, limiter, local)
			if !served(t, accepted) {
				t.Fatalf("%s connection %d was refused below the ceiling", local, i+1)
			}
		}
	}

	// A third address, holding nothing, asking for its first.
	dialFrom(t, limiter, "127.0.0.4")
	if served(t, accepted) {
		t.Fatal("a connection was served past the fleet ceiling")
	}
	ev := lastRefusal(t, recorded)
	if ev.Bound != BoundFleet {
		t.Errorf("the refusal was reported as %q, want %q", ev.Bound, BoundFleet)
	}
	if ev.Open != 0 {
		t.Errorf("the refused peer held %d connections, want none", ev.Open)
	}
	if ev.Total != ev.Limit {
		t.Errorf("refused with %d open against a ceiling of %d", ev.Total, ev.Limit)
	}
}

// TestFleetSlackComesBackWhenConnectionsDo guards the release path's half of
// the total. Without it the tightening would be permanent after one episode,
// which is a lifetime quota on the fleet rather than a bound on what it holds
// at once -- the same mistake TestAClosedConnectionGivesItsSlotBack guards for
// one peer.
func TestFleetSlackComesBackWhenConnectionsDo(t *testing.T) {
	limiter, accepted, _ := serveLimited(t, 8)
	limiter.Expect(func() (int, bool) { return 1, true })

	var open []net.Conn
	for i := 0; i < FleetConnectionsPerAgent; i++ {
		dial(t, limiter)
		select {
		case conn := <-accepted:
			open = append(open, conn)
		case <-time.After(time.Second):
			t.Fatalf("connection %d was refused below the ceiling", i+1)
		}
	}
	dial(t, limiter)
	if served(t, accepted) {
		t.Fatal("the connection over the fleet ceiling was served")
	}

	// Give one back from the server side, which is the side that counts.
	_ = open[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		dial(t, limiter)
		if served(t, accepted) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fleet stayed tightened after a connection closed")
		}
	}
}
