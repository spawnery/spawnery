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
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func mustPut(t *testing.T, st Store, key string) {
	t.Helper()
	if _, err := st.Put(context.Background(), key, bytes.NewReader(nil), PutCondition{}); err != nil {
		t.Fatal(err)
	}
}

func TestTheSweeperDeletesAMarkedWorldWithoutALease(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	p := WorldPrefix("", "ns/g/k")
	for _, k := range []string{ManifestName, "objects/a", "packs/1.tar.gz"} {
		mustPut(t, st, p+k)
	}
	mustPut(t, st, "ns/g/other/manifest.json")
	if err := MarkDeleted(ctx, st, "", "ns/g/k"); err != nil {
		t.Fatal(err)
	}
	if pending, _ := DeletionPending(ctx, st, "", "ns/g/k"); !pending {
		t.Fatal("no deletion pending after MarkDeleted")
	}

	s := &Sweeper{Store: st, StaleAfter: StaleAfter, Log: logr.Discard()}
	if err := s.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	keys := st.Keys()
	if len(keys) != 1 || keys[0] != "ns/g/other/manifest.json" {
		t.Fatalf("after the sweep: %v", keys)
	}
}

func TestTheSweeperWaitsForAFreshLease(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	p := WorldPrefix("", "ns/g/k")
	mustPut(t, st, p+ManifestName)
	if _, err := TakeLease(ctx, st, p, Lease{Node: "a", RenewedAt: now}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	if err := MarkDeleted(ctx, st, "", "ns/g/k"); err != nil {
		t.Fatal(err)
	}

	s := &Sweeper{Store: st, StaleAfter: StaleAfter, Log: logr.Discard()}
	if err := s.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if exists, _ := WorldExists(ctx, st, "", "ns/g/k"); !exists {
		t.Fatal("the sweep deleted a world whose lease is fresh: a node is still uploading it")
	}
	now = now.Add(StaleAfter + time.Second)
	if err := s.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if exists, _ := WorldExists(ctx, st, "", "ns/g/k"); exists {
		t.Fatal("the sweep left a world whose lease went stale")
	}
	if pending, _ := DeletionPending(ctx, st, "", "ns/g/k"); pending {
		t.Fatal("the marker outlived the world")
	}
}

func TestTheSweeperDeletesAtOnceWhenTheLeaseWasReleased(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	p := WorldPrefix("", "ns/g/k")
	mustPut(t, st, p+ManifestName)
	etag, err := TakeLease(ctx, st, p, Lease{Node: "a", RenewedAt: now}, StaleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseLease(ctx, st, p, etag); err != nil {
		t.Fatal(err)
	}
	if err := MarkDeleted(ctx, st, "", "ns/g/k"); err != nil {
		t.Fatal(err)
	}

	s := &Sweeper{Store: st, StaleAfter: StaleAfter, Log: logr.Discard()}
	if err := s.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if exists, _ := WorldExists(ctx, st, "", "ns/g/k"); exists {
		t.Fatal("the sweep kept a world whose lease was released")
	}
}
