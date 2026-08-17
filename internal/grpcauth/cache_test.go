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
	"errors"
	"fmt"
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
func TestReviewCacheDoesNotKeepTheTokenItself(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	c.store("super-secret-token", reviewResult{Namespace: "minecraft"}, nil)

	for key := range c.entries {
		if key == "super-secret-token" {
			t.Fatal("the cache is keyed on the raw token")
		}
	}
}

// Without eviction the map grows for as long as distinct tokens arrive. The
// rate limit in the interceptor bounds how fast that can happen, and this
// bounds how large it gets in between.
func TestReviewCacheEvictsExpiredEntriesAsItGrows(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	for i := 0; i < maxCacheEntries+1; i++ {
		c.store(fmt.Sprintf("token-%d", i), reviewResult{}, nil)
	}
	now = now.Add(PositiveTTL + time.Second)
	c.store("one-more", reviewResult{}, nil)

	if len(c.entries) > maxCacheEntries {
		t.Errorf("the cache holds %d entries past its bound of %d",
			len(c.entries), maxCacheEntries)
	}
}
