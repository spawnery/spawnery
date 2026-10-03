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

package agentserver

import (
	"net"
	"sync"
	"testing"
	"time"
)

// serveLimited uses a real loopback socket rather than net.Pipe, which has no
// addresses and would only exercise peerKey's fallback.
func serveLimited(t *testing.T, limit int) (*PeerLimiter, <-chan net.Conn, func() []ConnEvent) {
	t.Helper()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

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

	// The kernel completed the handshake, so the client learns only from the close.
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

func TestAClosedConnectionGivesItsSlotBack(t *testing.T) {
	const limit = 2
	limiter, accepted, _ := serveLimited(t, limit)

	first := dial(t, limiter)
	served := <-accepted
	dial(t, limiter)
	<-accepted

	// The server side is the side that counts.
	_ = served.Close()
	_ = first.Close()

	dial(t, limiter)
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the slot a closed connection freed was never reusable")
	}
}

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

// TestAZeroLimitCountsWithoutRefusing: cmd/spawnery-stubop measures the peak
// MaxConnectionsPerPeer is derived from in this mode.
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

func TestTheRefusalCountResetsWithThePeer(t *testing.T) {
	limiter, accepted, _ := serveLimited(t, 1)

	dial(t, limiter)
	served := <-accepted
	peer := peerKey(served.RemoteAddr())

	for i := 0; i < 3; i++ {
		dial(t, limiter)
	}
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

// dialFrom relies on every address in 127.0.0.0/8 being local on Linux, which
// gives a loopback listener more than one peer. It skips where that fails.
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

func served(t *testing.T, accepted <-chan net.Conn) bool {
	t.Helper()
	select {
	case <-accepted:
		return true
	case <-time.After(time.Second):
		return false
	}
}

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

func TestTheFleetBoundTightensEveryPeerAtItsCeiling(t *testing.T) {
	limiter, accepted, recorded := serveLimited(t, 8)
	limiter.Expect(func() (int, bool) { return 1, true })

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

func TestTheFleetCeilingRefusesAPeerHoldingNothing(t *testing.T) {
	limiter, accepted, recorded := serveLimited(t, MaxConnectionsPerPeer)
	limiter.Expect(func() (int, bool) { return 1, true })

	// No single peer reaches the MaxConnectionsPerPeer ceiling once tightened
	// to FleetConnectionsPerAgent; two together do.
	for _, local := range []string{"127.0.0.2", "127.0.0.3"} {
		for i := 0; i < FleetConnectionsPerAgent; i++ {
			dialFrom(t, limiter, local)
			if !served(t, accepted) {
				t.Fatalf("%s connection %d was refused below the ceiling", local, i+1)
			}
		}
	}

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
