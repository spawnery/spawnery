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
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReviewCacheServesAPositiveAnswerUntilItExpires(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	want := reviewResult{Namespace: "minecraft", PodName: "lobby-abc"}
	c.store("token", want, nil)

	got, err, ok := c.lookup("token")
	if !ok {
		t.Fatal("a freshly stored entry was not served")
	}
	if err != nil || got.PodName != want.PodName {
		t.Fatalf("lookup = (%+v, %v), want (%+v, nil)", got, err, want)
	}

	now = now.Add(PositiveTTL - time.Second)
	if _, _, ok := c.lookup("token"); !ok {
		t.Error("the entry expired before its TTL")
	}

	now = now.Add(2 * time.Second)
	if _, _, ok := c.lookup("token"); ok {
		t.Error("the entry outlived its TTL")
	}
}

// A cached "no" heals faster than a cached "yes" on purpose: a rejection that
// was wrong -- clock skew, a token checked before its ServiceAccount existed --
// should not stick, while a cached "yes" is what removes the API server load.
func TestReviewCacheForgetsANegativeAnswerSooner(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	c.store("bad", reviewResult{}, errors.New("token not authenticated"))

	if _, err, ok := c.lookup("bad"); !ok || err == nil {
		t.Fatal("a stored rejection was not served back as a rejection")
	}

	now = now.Add(NegativeTTL + time.Second)
	if _, _, ok := c.lookup("bad"); ok {
		t.Error("the rejection outlived NegativeTTL")
	}
	if NegativeTTL >= PositiveTTL {
		t.Errorf("NegativeTTL (%s) must be shorter than PositiveTTL (%s)",
			NegativeTTL, PositiveTTL)
	}
}

// The whole point of the split in Authenticate: an outage must not be cached,
// or it outlives itself. internal/grpcauth already distinguishes the two, and
// the interceptor already maps this one to codes.Unavailable so an agent backs
// off rather than concluding its credentials are wrong.
func TestReviewCacheRefusesToStoreAnOutage(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	c.store("token", reviewResult{}, wrapUnavailable(errors.New("apiserver down")))

	if _, _, ok := c.lookup("token"); ok {
		t.Error("an API server outage was cached; it would outlive the outage")
	}
}

// The operator must not hold bearer tokens in a map.
//
// "Not the raw token" is not enough to assert: a cacheKey returning
// token + "!" satisfies it while still holding every byte of the credential.
// So this pins the key to the SHA-256 digest, recomputed here from the
// standard library rather than by calling cacheKey, which would move with the
// code under test and prove nothing.
func TestReviewCacheDoesNotKeepTheTokenItself(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	const token = "super-secret-token"
	c.store(token, reviewResult{Namespace: "minecraft"}, nil)

	sum := sha256.Sum256([]byte(token))
	want := hex.EncodeToString(sum[:])

	if len(c.entries) != 1 {
		t.Fatalf("one store left %d entries, want 1", len(c.entries))
	}
	for key := range c.entries {
		if strings.Contains(key, token) {
			t.Fatalf("the cache key %q contains the raw token", key)
		}
		if key != want {
			t.Fatalf("cache key = %q, want the SHA-256 digest %q", key, want)
		}
	}
}

// maxCacheEntries is a hard bound, and this is the assertion that can tell
// that apart from the thing it used to be.
//
// The earlier version of this test stored maxCacheEntries+1 entries at one
// frozen instant, then advanced the clock past PositiveTTL and stored one
// more -- so its length check ran against a map that the expiry sweep had
// just emptied. It was green for a cache that bounded nothing. Here the clock
// never moves, so nothing ever expires, and the only way to stay under the
// bound is to drop a live entry to make room.
func TestReviewCacheHoldsItsBoundWithNothingExpired(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	for i := 0; i < maxCacheEntries*2; i++ {
		c.store(fmt.Sprintf("token-%d", i), reviewResult{}, nil)
		if len(c.entries) > maxCacheEntries {
			t.Fatalf("after %d stores at one instant the cache holds %d entries, "+
				"past its bound of %d: nothing had expired, so sweeping expired "+
				"entries freed nothing and the map grew unbounded",
				i+1, len(c.entries), maxCacheEntries)
		}
	}
}

// The cheap sweep still has to happen, and to come first: dropping a live
// entry when a dead one would do costs a TokenReview for nothing.
func TestReviewCacheSweepsExpiredEntriesFirst(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	for i := 0; i < maxCacheEntries; i++ {
		c.store(fmt.Sprintf("old-%d", i), reviewResult{}, nil)
	}
	now = now.Add(PositiveTTL + time.Second)
	c.store("fresh", reviewResult{}, nil)

	if len(c.entries) != 1 {
		t.Errorf("the cache holds %d entries, want 1: every one of the %d earlier "+
			"entries had expired and all of them should have gone at once",
			len(c.entries), maxCacheEntries)
	}
	if _, _, ok := c.lookup("fresh"); !ok {
		t.Error("the entry the sweep made room for is not in the cache")
	}
}
