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
	"sync/atomic"
)

// PeerLimiter bounds how many connections one peer address may hold open at
// once. MaxConcurrentStreams bounds streams per connection, MaxConnectionIdle
// spares connections with a live stream, and grpcauth's rate limit only sees
// TokenReview cache misses, so none of them bounds this.
//
// It wraps the listener rather than using a grpc.StatsHandler because TagConn
// runs after the TLS handshake, which is the cost being refused.
//
// The key is the peer IP, the only identity known at Accept. That assumes a
// pod network without SNAT; anything that rewrites the source address
// collapses its clients into one bucket, and nothing here detects it.
//
// The fleet bounds scale with Expect's pod count instead of being a fixed
// ceiling, which legitimate growth would eventually hit:
//
//	total >= expected * FleetConnectionsPerAgent
//	    Every peer's bound drops to FleetConnectionsPerAgent.
//
//	total >= expected * MaxConnectionsPerPeer
//	    The connection is refused whatever peer it came from.
//
// Both are floored at one peer's worth. When the second binds, the refused
// connection is whichever arrived next, possibly a well-behaved agent's.
type PeerLimiter struct {
	net.Listener

	// limit is the per-peer bound. Zero means count but never refuse.
	limit int

	// fleet is nil, or reports unknown, when there is no fleet bound.
	fleet atomic.Pointer[func() (int, bool)]

	mu sync.Mutex
	// total is kept rather than summed from open: the fleet bound reads it on
	// every Accept.
	total int
	open  map[string]int
	peak  map[string]int
	// refused throttles the refusal log so it is not a second amplifier.
	refused map[string]int

	observe func(ConnEvent)
}

// ConnEvent is one change in a peer's connection count.
type ConnEvent struct {
	// Peer is the address without its port.
	Peer string
	// Open is the peer's count after this event; unchanged on a refusal.
	Open    int
	Refused bool
	// Total is how many connections the whole listener holds after this event.
	Total int
	// Limit is the number this event was decided against: the peer's bound
	// (FleetConnectionsPerAgent where the fleet tightened it) or the fleet
	// ceiling. Zero on a release.
	Limit int
	// Bound is BoundPeer or BoundFleet on a refusal, empty otherwise. A peer
	// refused under BoundFleet may be behaving perfectly.
	Bound string
	// Refusals resets when the peer's last connection closes.
	Refusals int
	// Peak is the peer's high-water mark since its count was last zero. It is
	// carried here rather than offered as a method because release reports
	// under the limiter's lock.
	Peak int
}

// NewPeerLimiter wraps inner so that no peer address holds more than limit
// connections at once. A limit of zero counts without refusing, which is what
// cmd/spawnery-stubop uses to measure a real agent's peak; the operator builds
// its own in Start.
func NewPeerLimiter(inner net.Listener, limit int, observe func(ConnEvent)) *PeerLimiter {
	return &PeerLimiter{
		Listener: inner,
		limit:    limit,
		open:     make(map[string]int),
		peak:     make(map[string]int),
		refused:  make(map[string]int),
		observe:  observe,
	}
}

// Expect tells the limiter how many agents the operator ought to be serving;
// size is called once per Accept.
//
// Unknown, or no Expect at all, means no fleet bound. That fails open on
// purpose: the count comes from a cache that is empty before it syncs, and
// reading that as zero would refuse every agent right after an operator
// restart.
func (l *PeerLimiter) Expect(size func() (int, bool)) {
	if size == nil {
		l.fleet.Store(nil)
		return
	}
	l.fleet.Store(&size)
}

func (l *PeerLimiter) expected() (int, bool) {
	size := l.fleet.Load()
	if size == nil {
		return 0, false
	}
	return (*size)()
}

// Accept returns the next connection whose peer is under the bound. A refusal
// closes the connection and takes the next one rather than returning an error,
// because grpc.Serve gives up on errors it does not read as temporary and one
// peer over its limit would end the whole listener.
func (l *PeerLimiter) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer := peerKey(conn.RemoteAddr())

		// Outside the lock: expected calls into a cache the limiter does not own.
		expected, known := 0, false
		if l.limit > 0 {
			expected, known = l.expected()
		}

		l.mu.Lock()
		count := l.open[peer]
		limit, bound := l.limit, BoundPeer
		refuse := limit > 0 && count >= limit
		if known {
			if ceiling := atLeastOnePeer(expected*MaxConnectionsPerPeer, MaxConnectionsPerPeer); l.total >= ceiling {
				limit, bound, refuse = ceiling, BoundFleet, true
			} else if tight := atLeastOnePeer(expected*FleetConnectionsPerAgent, FleetConnectionsPerAgent); l.total >= tight &&
				limit > FleetConnectionsPerAgent {
				limit, bound = FleetConnectionsPerAgent, BoundFleet
				refuse = count >= limit
			}
		}
		if refuse {
			l.refused[peer]++
			refusals := l.refused[peer]
			peak := l.peak[peer]
			total := l.total
			l.mu.Unlock()
			ConnectionsRefused.WithLabelValues(bound).Inc()
			if l.observe != nil {
				l.observe(ConnEvent{
					Peer: peer, Open: count, Total: total, Refused: true,
					Refusals: refusals, Peak: peak, Limit: limit, Bound: bound,
				})
			}
			_ = conn.Close()
			continue
		}
		count++
		l.open[peer] = count
		l.total++
		if count > l.peak[peer] {
			l.peak[peer] = count
		}
		peak, total := l.peak[peer], l.total
		l.mu.Unlock()

		OpenConnections.Inc()
		if l.observe != nil {
			l.observe(ConnEvent{Peer: peer, Open: count, Total: total, Peak: peak, Limit: limit})
		}
		return &countedConn{Conn: conn, limiter: l, peer: peer}, nil
	}
}

// release drops the peer's entries once its count reaches zero, so the maps
// are bounded by the live fleet rather than every pod IP ever seen.
func (l *PeerLimiter) release(peer string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	peak := l.peak[peer]
	count := l.open[peer] - 1
	if l.total > 0 {
		l.total--
	}
	if count <= 0 {
		delete(l.open, peer)
		delete(l.peak, peer)
		delete(l.refused, peer)
		count = 0
	} else {
		l.open[peer] = count
	}
	OpenConnections.Dec()
	if l.observe != nil {
		// Under the lock, unlike Accept, so a racing close and accept report in order.
		l.observe(ConnEvent{Peer: peer, Open: count, Peak: peak})
	}
}

func peerKey(addr net.Addr) string {
	if addr == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		// In-process and pipe listeners in the unit tests.
		return addr.String()
	}
	return host
}

// countedConn releases the peer's slot exactly once: grpc-go's transport
// closes a connection on both the read and the write path.
type countedConn struct {
	net.Conn
	limiter *PeerLimiter
	peer    string
	once    sync.Once
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.limiter.release(c.peer) })
	return err
}

func isPowerOfTen(n int) bool {
	if n < 1 {
		return false
	}
	for n%10 == 0 {
		n /= 10
	}
	return n == 1
}

// atLeastOnePeer floors a fleet threshold at one peer's worth, because zero is
// also what a broken count reports, and unfloored it would refuse every
// connection in the cluster.
func atLeastOnePeer(threshold, floor int) int {
	if threshold < floor {
		return floor
	}
	return threshold
}
