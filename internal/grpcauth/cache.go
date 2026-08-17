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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	// PositiveTTL is how long an accepted token's review is reused. Projected
	// agent tokens live 600 seconds (podspec.TokenExpirationSeconds) and the
	// kubelet rotates them, so this sits well inside one token's life.
	//
	// What it can delay is narrow, and the narrowing is the design: only the
	// TokenReview is cached, never the pod lookup, so deleting a pod -- the
	// revocation an operator actually performs -- takes effect on the very
	// next connection attempt whatever this cache holds.
	PositiveTTL = 60 * time.Second

	// NegativeTTL is how long a refusal is reused, deliberately shorter. A
	// cached "no" that was wrong should heal quickly; a cached "yes" is what
	// removes the load.
	NegativeTTL = 10 * time.Second

	// maxCacheEntries is a hard bound on the map, not a target. store never
	// lets the map exceed it: expired entries go first, and if that frees
	// nothing the entry closest to expiry is dropped to make room.
	maxCacheEntries = 1024
)

// reviewResult is what a TokenReview establishes about a token on its own,
// independent of which role the caller asked for. The role check is
// deliberately not in here: it varies per call, so caching it would let one
// agent's rejection answer another agent's question.
type reviewResult struct {
	Namespace      string
	ServiceAccount string
	PodName        string
	PodUID         string
}

type cacheEntry struct {
	result  reviewResult
	reason  string // empty when the review succeeded
	expires time.Time
}

// ReviewCache remembers what the API server said about a token.
type ReviewCache struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

// NewReviewCache returns a cache reading time from now.
func NewReviewCache(now func() time.Time) *ReviewCache {
	return &ReviewCache{now: now, entries: map[string]cacheEntry{}}
}

func cacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// lookup returns a remembered answer, if one has not expired.
func (c *ReviewCache) lookup(token string) (reviewResult, error, bool) {
	if c == nil {
		return reviewResult{}, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[cacheKey(token)]
	if !ok || !c.now().Before(entry.expires) {
		return reviewResult{}, nil, false
	}
	if entry.reason != "" {
		return reviewResult{}, errors.New(entry.reason), true
	}
	return entry.result, nil, true
}

// store remembers an answer. An API server outage is never remembered: it says
// nothing about the token, and caching it would extend the outage past its end.
//
// A refusal is stored as its message rather than as the error value, which
// loses the error's type. That is safe precisely because the one type that
// matters -- unavailableErr -- is the one case this never stores.
func (c *ReviewCache) store(token string, result reviewResult, err error) {
	if c == nil || isUnavailable(err) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// Two steps, and the second is what makes the bound a bound. Sweeping
	// expired entries is free and usually enough, but with maxCacheEntries
	// live entries it deletes nothing, and a map that only ever sweeps the
	// dead grows without limit under a flood of distinct tokens -- exactly the
	// case a bound is for.
	if len(c.entries) >= maxCacheEntries {
		c.evictExpiredLocked()
	}
	if len(c.entries) >= maxCacheEntries {
		c.evictSoonestLocked()
	}

	entry := cacheEntry{result: result, expires: c.now().Add(PositiveTTL)}
	if err != nil {
		entry = cacheEntry{reason: err.Error(), expires: c.now().Add(NegativeTTL)}
	}
	c.entries[cacheKey(token)] = entry
}

func (c *ReviewCache) evictExpiredLocked() {
	now := c.now()
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, key)
		}
	}
}

// evictSoonestLocked drops the one entry closest to expiring, which is what
// makes room for the caller's. One is enough: store adds at most one entry per
// call, so evicting one before each store holds the map at maxCacheEntries for
// good.
//
// Closest to expiry rather than least recently used, because it needs no
// extra bookkeeping. Every entry has one of two lifetimes: the short
// NegativeTTL for a refusal, the longer PositiveTTL for an accepted review.
// Under a flood of distinct tokens, a freshly stored refusal expires before a
// freshly stored positive, so it is usually the refusal that goes first --
// but only for roughly the first fifty seconds of a positive entry's life.
// Past that point a positive has less time left than a freshly stored
// refusal and gets evicted first instead. Either order is fine: evicting a
// live positive costs one extra TokenReview and admits nobody who would not
// otherwise be admitted.
func (c *ReviewCache) evictSoonestLocked() {
	var soonestKey string
	var soonest time.Time
	for key, entry := range c.entries {
		if soonestKey == "" || entry.expires.Before(soonest) {
			soonestKey, soonest = key, entry.expires
		}
	}
	if soonestKey != "" {
		delete(c.entries, soonestKey)
	}
}
