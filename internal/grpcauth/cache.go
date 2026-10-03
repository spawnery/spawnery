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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	// PositiveTTL sits well inside a projected token's 600 s life
	// (podspec.TokenExpirationSeconds). Only the TokenReview is cached, never
	// the pod lookup, so deleting a pod still revokes at once.
	PositiveTTL = 60 * time.Second

	// NegativeTTL is shorter so a wrong cached refusal heals quickly.
	NegativeTTL = 10 * time.Second

	// maxCacheEntries is a hard bound: store evicts expired entries first, then
	// the one closest to expiry.
	maxCacheEntries = 1024
)

// reviewResult leaves out the role check: it varies per call, so caching it
// would let one agent's rejection answer another agent's question.
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

type ReviewCache struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

func NewReviewCache(now func() time.Time) *ReviewCache {
	return &ReviewCache{now: now, entries: map[string]cacheEntry{}}
}

func cacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

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

// store never remembers an API server outage: it says nothing about the token.
// A refusal is stored as its message, losing its type, which is safe because
// unavailableErr is never stored.
func (c *ReviewCache) store(token string, result reviewResult, err error) {
	if c == nil || isUnavailable(err) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// Sweeping expired entries frees nothing when all are live; evicting the
	// soonest is what bounds the map under a flood of distinct tokens.
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

// evictSoonestLocked evicts closest-to-expiry rather than least recently used,
// which needs no bookkeeping. Evicting a live positive costs one extra
// TokenReview and admits nobody.
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
