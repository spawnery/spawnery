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

package grpcauth

import (
	"sync"
	"time"
)

const (
	// PeerBurst is generous by an order of magnitude: a legitimate agent misses
	// the cache once per token rotation (600 s) and once per reconnect.
	PeerBurst = 5

	PeerRefill = 10 * time.Second

	// maxBuckets: a full bucket is indistinguishable from an absent one, so full
	// buckets are dropped.
	maxBuckets = 4096
)

type bucket struct {
	tokens float64
	last   time.Time
}

// PeerLimiter is a token bucket per peer address, consulted only when the
// review cache misses. A reconnect loop replays one token and hits the cache;
// fresh tokens cannot be manufactured, as TokenReview is audience-bound.
type PeerLimiter struct {
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]bucket
}

func NewPeerLimiter(now func() time.Time) *PeerLimiter {
	return &PeerLimiter{now: now, buckets: map[string]bucket{}}
}

func (l *PeerLimiter) allow(peer string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, seen := l.buckets[peer]
	if !seen {
		b = bucket{tokens: PeerBurst, last: now}
	} else {
		refilled := now.Sub(b.last).Seconds() / PeerRefill.Seconds()
		b.tokens += refilled
		if b.tokens > PeerBurst {
			b.tokens = PeerBurst
		}
		b.last = now
	}

	if b.tokens < 1 {
		l.buckets[peer] = b
		return false
	}
	b.tokens--

	if len(l.buckets) >= maxBuckets {
		l.evictFullLocked()
	}
	l.buckets[peer] = b
	return true
}

// evictFullLocked is deliberately no hard cap: with maxBuckets peers active the
// map grows past it, because capping would refuse a legitimate agent to make
// room for an attacker.
func (l *PeerLimiter) evictFullLocked() {
	for key, b := range l.buckets {
		if b.tokens >= PeerBurst {
			delete(l.buckets, key)
		}
	}
}
