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

package grpcauth

import (
	"sync"
	"time"
)

const (
	// PeerBurst is how many token checks one peer may cause back to back.
	// A legitimate agent misses the cache at most once per token rotation --
	// projected tokens live 600 seconds -- and once per reconnect, so this is
	// generous by an order of magnitude.
	PeerBurst = 5

	// PeerRefill is how long one token takes to come back.
	PeerRefill = 10 * time.Second

	// maxBuckets bounds the map. Pod IPs are recycled and a peer that has
	// refilled to full is indistinguishable from one that never appeared, so
	// full buckets are dropped rather than kept.
	maxBuckets = 4096
)

type bucket struct {
	tokens float64
	last   time.Time
}

// PeerLimiter is a token bucket per peer address.
//
// It is consulted only when the review cache misses, and that is what makes it
// harmless to legitimate traffic and effective against the documented attack.
// A pod in a connection loop replays one token, hits the cache, and never
// reaches this. Feeling it at all requires presenting tokens the cache has not
// seen -- and those cannot be manufactured, because TokenReview is
// audience-bound and the token is signed by the cluster.
type PeerLimiter struct {
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]bucket
}

// NewPeerLimiter returns a limiter reading time from now.
func NewPeerLimiter(now func() time.Time) *PeerLimiter {
	return &PeerLimiter{now: now, buckets: map[string]bucket{}}
}

// allow spends one token for peer, reporting whether there was one.
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

func (l *PeerLimiter) evictFullLocked() {
	for key, b := range l.buckets {
		if b.tokens >= PeerBurst {
			delete(l.buckets, key)
		}
	}
}
