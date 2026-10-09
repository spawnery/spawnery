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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTheRestorePointsAreTheCurrentAndItsOlderEntriesNewestFirst(t *testing.T) {
	st := NewMemStore(time.Now)
	cur := fiveGenerations(t, st)
	seedEntry(t, st, cur) // left by a crash between the history write and the manifest put
	points, err := ListRestorePoints(context.Background(), st, prefix)
	if err != nil {
		t.Fatal(err)
	}
	var gens []int64
	for i, p := range points {
		gens = append(gens, p.Generation)
		if p.Current != (i == 0) {
			t.Errorf("point %d current = %v", p.Generation, p.Current)
		}
	}
	if !slices.Equal(gens, []int64{5, 4, 3, 2, 1}) {
		t.Fatalf("generations = %v, want [5 4 3 2 1] with the current listed once", gens)
	}
	if !points[0].Taken.Equal(time.UnixMilli(cur.Taken)) {
		t.Fatalf("current taken = %v, want %v", points[0].Taken, time.UnixMilli(cur.Taken))
	}
}

func TestARestoreRefusesTheCurrentAnUnknownGenerationAndAMissingWorld(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	fiveGenerations(t, st)
	if _, err := RestoreWorld(ctx, st, prefix, 5, Retention{Last: 10}, time.Now()); !errors.Is(err, ErrCurrentGeneration) {
		t.Fatalf("restore of the current: err = %v, want ErrCurrentGeneration", err)
	}
	if _, err := RestoreWorld(ctx, st, prefix, 99, Retention{Last: 10}, time.Now()); !errors.Is(err, ErrNoGeneration) {
		t.Fatalf("restore of generation 99: err = %v, want ErrNoGeneration", err)
	}
	const none = "ns/g/none/"
	if _, err := RestoreWorld(ctx, st, none, 1, Retention{Last: 10}, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore of a missing world: err = %v, want ErrNotFound", err)
	}
	if stored(t, st, none+LeaseName) {
		t.Fatal("a restore of a missing world left a lease in the bucket")
	}
	m, _, err := ReadManifest(ctx, st, prefix)
	if err != nil || m.Generation != 5 {
		t.Fatalf("manifest = %+v, %v; a refused restore changed it", m, err)
	}
	if l, _, err := ReadLease(ctx, st, prefix); err != nil || l.Node != "" {
		t.Fatalf("lease = %+v, %v; want released", l, err)
	}
}

func TestARestoreWhoseCommitWasAnswered412IsARestore(t *testing.T) {
	mem := NewMemStore(time.Now)
	fiveGenerations(t, mem)
	st := &committedButRefused{MemStore: mem, match: func(key string) bool { return strings.HasSuffix(key, ManifestName) }}
	st.armed.Store(true)
	got, err := RestoreWorld(context.Background(), st, prefix, 2, Retention{Last: 10}, time.Now())
	if err != nil || got.Generation != 6 || got.RestoredFrom != 2 {
		t.Fatalf("restore = %+v, %v; want generation 6 from 2", got, err)
	}
	if st.refused.Load() == 0 {
		t.Fatal("the store refused nothing")
	}
}

func TestARestoreMakesAnOldGenerationCurrentUnderANewNumber(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setPolicy(t, h.st, Retention{Last: 10})
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	h.unpublish("a", target)
	h.node("a").Settle(ctx) // generation 3, the final snapshot, and the release
	before, _, err := ReadManifest(ctx, h.st, prefix)
	if err != nil || before.Generation != 3 {
		t.Fatalf("manifest = %+v, %v; want generation 3", before, err)
	}

	got, err := BucketWorlds{Store: h.st}.Restore(ctx, w, 1, Retention{Last: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 4 || got.RestoredFrom != 1 || got.PruneErr != nil {
		t.Fatalf("restore = %+v, want generation 4 from 1", got)
	}
	m, _, err := ReadManifest(ctx, h.st, prefix)
	if err != nil || m.Generation != 4 || m.RestoredFrom != 1 || m.WorldID != before.WorldID || len(m.Files) != 1 || m.Files[0].Size != 5 {
		t.Fatalf("manifest = %+v, %v; want generation 4 holding generation 1's level.dat of 5 bytes", m, err)
	}
	points, err := BucketWorlds{Store: h.st}.RestorePoints(ctx, w)
	if err != nil || len(points) != 4 || points[0].Generation != 4 || points[1].Generation != 3 {
		t.Fatalf("restore points = %+v, %v; want 4 (current), 3, 2, 1", points, err)
	}

	downloads := downloadsCounted(h.node("a"))
	target2, err := h.publish("a", w, "p2")
	if err != nil {
		t.Fatal(err)
	}
	h.waitFile(target2, ReadyFile)
	if got := downloadsCounted(h.node("a")); got != downloads+1 {
		t.Fatalf("downloads = %d, want %d: the node's copy of generation 3 was taken for current", got, downloads+1)
	}
	if got := h.orphans("a"); got != 0 {
		t.Fatalf("orphans = %d after a restore", got)
	}
	st, err := os.Stat(filepath.Join(target2, "worlds/world/level.dat"))
	if err != nil || st.Size() != 5 {
		t.Fatalf("level.dat = %v, %v; want the restored 5 bytes", st, err)
	}
}

func TestARestoreIsRefusedWhileANodeHoldsTheLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	setPolicy(t, h.st, Retention{Last: 10})
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	_, err := RestoreWorld(ctx, h.st, prefix, 1, Retention{Last: 10}, h.now)
	var held *HeldError
	if !errors.As(err, &held) || held.Node != "a" {
		t.Fatalf("err = %v, want HeldError{a}", err)
	}
	if m, _, err := ReadManifest(ctx, h.st, prefix); err != nil || m.Generation != 2 {
		t.Fatalf("manifest = %+v, %v; a refused restore changed it", m, err)
	}
}

func TestAPublishWaitsForARestore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	etag, err := TakeLease(ctx, h.st, prefix, Lease{Node: OperatorLeaseNode, RenewedAt: h.now}, StaleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.publish("b", w, "p1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("publish during a restore: err = %v, want ErrUnavailable", err)
	}
	if err := ReleaseLease(ctx, h.st, prefix, etag); err != nil {
		t.Fatal(err)
	}
	if _, err := h.publish("b", w, "p1"); err != nil {
		t.Fatalf("publish after the restore: %v", err)
	}
}
