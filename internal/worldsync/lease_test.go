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

package worldsync

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestTheFirstTakerWinsAndAnotherNodeIsTurnedAway(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: now}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	_, err := TakeLease(ctx, st, prefix, Lease{Node: "b", RenewedAt: now}, StaleAfter)
	var held *HeldError
	if !errors.As(err, &held) || held.Node != "a" {
		t.Fatalf("err = %v, want HeldError{a}", err)
	}
}

func TestTheSameNodeTakesItsOwnLeaseAgain(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "a", Pod: "p1", RenewedAt: now}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "a", Pod: "p2", RenewedAt: now}, StaleAfter); err != nil {
		t.Fatalf("the holder could not take its own lease again: %v", err)
	}
	got, _, err := ReadLease(ctx, st, prefix)
	if err != nil || got.Pod != "p2" {
		t.Fatalf("lease = %+v, %v; want pod p2", got, err)
	}
}

func TestAStaleLeaseIsTakenOverOnTheStoresClock(t *testing.T) {
	storeNow := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return storeNow })
	ctx := context.Background()
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: storeNow}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	storeNow = storeNow.Add(StaleAfter + time.Second)
	etag, err := TakeLease(ctx, st, prefix, Lease{Node: "b", RenewedAt: storeNow}, StaleAfter)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if _, err := RenewLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: storeNow}, "old-etag-of-a"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the old holder's renewal: err = %v, want ErrLeaseLost", err)
	}
	if _, err := RenewLease(ctx, st, prefix, Lease{Node: "b", RenewedAt: storeNow}, etag); err != nil {
		t.Fatalf("the new holder's renewal: %v", err)
	}
}

func TestAFreshLeaseIsNotTakenOverJustBeforeItGoesStale(t *testing.T) {
	storeNow := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return storeNow })
	ctx := context.Background()
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: storeNow}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	storeNow = storeNow.Add(StaleAfter)
	_, err := TakeLease(ctx, st, prefix, Lease{Node: "b", RenewedAt: storeNow}, StaleAfter)
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("err = %v, want HeldError", err)
	}
}

func TestReleaseTombstonesOnlyTheHoldersLeaseAndAnotherNodeTakesItAtOnce(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	etag, err := TakeLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: now}, StaleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseLease(ctx, st, prefix, "not-mine"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("release with a foreign ETag: err = %v", err)
	}
	if err := ReleaseLease(ctx, st, prefix, etag); err != nil {
		t.Fatal(err)
	}
	if l, _, err := ReadLease(ctx, st, prefix); err != nil || l.Node != "" {
		t.Fatalf("lease after release = %+v, %v; want a tombstone", l, err)
	}
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "b", RenewedAt: now}, StaleAfter); err != nil {
		t.Fatalf("take after release: %v", err)
	}
}

func TestReleaseOfAMissingLeaseIsLost(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	if err := ReleaseLease(context.Background(), st, prefix, "x"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v", err)
	}
}

func TestRenewAfterTheLeaseObjectWasDeletedIsLost(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	etag, err := TakeLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: now}, StaleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, prefix+LeaseName); err != nil {
		t.Fatal(err)
	}
	if _, err := RenewLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: now}, etag); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
}

func TestStalenessIgnoresTheHoldersOwnClock(t *testing.T) {
	storeNow := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return storeNow })
	ctx := context.Background()
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "a", RenewedAt: storeNow.Add(24 * time.Hour)}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	storeNow = storeNow.Add(StaleAfter + time.Second)
	if _, err := TakeLease(ctx, st, prefix, Lease{Node: "b", RenewedAt: storeNow}, StaleAfter); err != nil {
		t.Fatalf("takeover of a lease stale by the store's clock: %v", err)
	}
}

type zeroDates struct{ Store }

func (z zeroDates) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	rc, info, err := z.Store.Get(ctx, key)
	info.Date, info.LastModified = time.Time{}, time.Time{}
	return rc, info, err
}

func TestTakeLeaseFailsWithoutStoreTimes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	mem := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	if _, err := TakeLease(ctx, mem, prefix, Lease{Node: "a", RenewedAt: now}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	_, err := TakeLease(ctx, zeroDates{mem}, prefix, Lease{Node: "b", RenewedAt: now}, StaleAfter)
	var held *HeldError
	if err == nil || errors.As(err, &held) {
		t.Fatalf("err = %v, want a plain error", err)
	}
}
