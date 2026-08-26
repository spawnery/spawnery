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
)

// PeerLimiter bounds how many connections one peer address may hold open at
// once. It is the answer to the availability half of milestone 2a's isolation
// promise, and it is worth being exact about why none of the bounds that came
// before it is: MaxConcurrentStreams bounds streams on ONE connection, so a
// pod that opens many is untouched; MaxConnectionIdle reaps a connection
// carrying no stream, and every connection in the attack carries a live one;
// and grpcauth's rate limit throttles TokenReview *misses*, which a pod
// replaying one valid token never produces because it hits the cache. Each of
// those bounds something real. None of them bounds the count this one does.
//
// # Why a listener wrapper rather than a grpc.StatsHandler
//
// The attack is resource consumption, so the refusal has to land before the
// resources are committed. A StatsHandler's TagConn runs after the TLS
// handshake, which is the expensive part — an asymmetric operation per
// connection, on the operator's CPU, at whatever rate the peer chooses. At
// Accept the connection has cost a file descriptor and nothing else.
//
// # Why the peer's IP is the key
//
// The authenticated identity is not known until the handshake and a
// TokenReview have both happened, which is exactly the work this exists to
// refuse. The IP is what is known at Accept, and on a Kubernetes pod network
// it is the pod: pod-to-pod traffic is not SNAT'd — a flat, un-NAT'd pod
// network is part of the network model every conforming CNI implements — so
// the address this sees is the peer pod's own.
//
// Read that as the assumption it is. Traffic that reaches this listener
// through something that rewrites the source address — a node-level proxy, a
// gateway, an operator reached from outside the cluster — collapses every
// client behind that address into one bucket, and the bucket is then far too
// small. Nothing here detects it. The agents dial the operator's Service
// directly, which is a DNAT on the destination and leaves the source alone.
//
// # What it does not bound
//
// Connections from *distinct* pods. An attacker holding several is bounded by
// the NetworkPolicy admitting only pods labelled spawnery.cloud/managed-by,
// and by nothing here: this bounds one peer, and the sum over peers is what
// docs/known-issues.md still carries as open. What it does close is the
// amplification available to a single compromised pod, which is the shape
// milestone 2a's promise takes.
type PeerLimiter struct {
	net.Listener

	// limit is the per-peer bound. Zero means count but never refuse, which is
	// what the stub operator uses to measure a legitimate agent's peak.
	limit int

	mu   sync.Mutex
	open map[string]int
	// peak is the high-water mark per peer, kept for the same measurement.
	// Bounded by the same set of keys as open: see release.
	peak map[string]int
	// refused counts the refusals a peer has collected while it has been at
	// its bound, and is what stops the log from becoming the attacker's second
	// amplifier -- see the Accept path. Cleared with the peer.
	refused map[string]int

	// observe is called for every change. Nil is allowed and means only the
	// metrics carry it.
	observe func(ConnEvent)
}

// ConnEvent is one change in a peer's connection count.
type ConnEvent struct {
	// Peer is the address without its port -- a pod, in a cluster.
	Peer string
	// Open is the peer's count after this event. On a refusal it is unchanged,
	// which is to say it equals the limit.
	Open int
	// Refused says the connection was turned away rather than served.
	Refused bool
	// Refusals is how many this peer has collected without ever dropping back
	// under its bound. It resets when the peer's last connection closes.
	Refusals int
	// Peak is the peer's high-water mark since its count was last zero.
	//
	// It is carried on the event rather than offered as a method because
	// release reports under the limiter's lock -- so that a close racing an
	// accept on one peer cannot report the two counts out of order -- and a
	// method would then be a caller re-entering a mutex it already holds.
	Peak int
}

// NewPeerLimiter wraps inner so that no peer address holds more than limit
// connections at once. A limit of zero counts without refusing, which is what
// cmd/spawnery-stubop uses to measure a real agent's peak against a real TCP
// socket -- the one place that peak is observable, since the unit tests'
// in-process transport opens no connection at all.
//
// It is exported for that caller and no other: the operator builds its own in
// Start.
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

// Accept returns the next connection whose peer is under the bound, refusing
// the ones that are not.
//
// A refusal closes the connection and takes the next one rather than returning
// an error. That is deliberate on both halves. Returning an error would hand
// grpc.Serve a decision it has no context for — grpc-go retries what it reads
// as temporary and gives up on the rest, and "this one peer is over its bound"
// is neither — so a peer over its limit could end the whole listener.
//
// What the refusal cannot do is refuse the SYN: by the time Accept sees a
// connection the kernel has completed the handshake for it. So an attacker
// hammering connect() still costs an accept and a close per attempt. That is
// two syscalls against their three, with no TLS and no goroutine behind it,
// and the kernel's own backlog is what bounds the rate. There is no way to do
// better from here that does not mean writing the bound into the CNI.
func (l *PeerLimiter) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer := peerKey(conn.RemoteAddr())

		l.mu.Lock()
		count := l.open[peer]
		if l.limit > 0 && count >= l.limit {
			l.refused[peer]++
			refusals := l.refused[peer]
			peak := l.peak[peer]
			l.mu.Unlock()
			ConnectionsRefused.Inc()
			if l.observe != nil {
				l.observe(ConnEvent{
					Peer: peer, Open: count, Refused: true, Refusals: refusals, Peak: peak,
				})
			}
			_ = conn.Close()
			continue
		}
		count++
		l.open[peer] = count
		if count > l.peak[peer] {
			l.peak[peer] = count
		}
		peak := l.peak[peer]
		l.mu.Unlock()

		OpenConnections.Inc()
		if l.observe != nil {
			l.observe(ConnEvent{Peer: peer, Open: count, Peak: peak})
		}
		return &countedConn{Conn: conn, limiter: l, peer: peer}, nil
	}
}

// release decrements the peer's count, and drops the peer's three entries once
// it reaches zero.
//
// Dropping them is what keeps the maps bounded by the live fleet rather than
// by every pod IP the cluster has ever assigned -- which matters most for the
// refusal count, since that is the one an attacker can drive. It also costs
// the peak: a pod that closes every connection and opens another starts its
// high-water mark again. The peak is a measurement aid, not an operational
// signal, and a map that grows for the life of the process to preserve one
// would be a leak dressed up as telemetry.
func (l *PeerLimiter) release(peer string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	peak := l.peak[peer]
	count := l.open[peer] - 1
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
		// Under the lock, unlike the Accept path: a close racing an accept on
		// the same peer would otherwise report the two counts out of order,
		// and the measurement this feeds reads the sequence.
		l.observe(ConnEvent{Peer: peer, Open: count, Peak: peak})
	}
}

// peerKey is the address without its port. Every connection from one pod
// carries a different source port, so the port is exactly what has to go.
func peerKey(addr net.Addr) string {
	if addr == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		// Not a host:port address at all. The in-process and pipe listeners
		// the unit tests use land here, and so would any future transport
		// that is not TCP; a single shared key is the honest answer for them
		// rather than a parse that pretends.
		return addr.String()
	}
	return host
}

// countedConn releases the peer's slot when it is closed, exactly once.
//
// The once is not defensive: net.Conn's contract permits Close to be called
// more than once, grpc-go's transport does call it on both the read and the
// write path, and a second decrement would give the peer a slot it never gave
// back.
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

// isPowerOfTen is the log throttle's whole rule: 1, 10, 100, and so on. It is
// here rather than inlined at the call site because what it decides -- how
// loud a peer under refusal is allowed to be -- belongs with the refusing.
func isPowerOfTen(n int) bool {
	if n < 1 {
		return false
	}
	for n%10 == 0 {
		n /= 10
	}
	return n == 1
}
