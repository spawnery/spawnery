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
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

type writeLog struct {
	*MemStore
	mu     sync.Mutex
	writes []string
}

// Put logs only what it wrote: TakeLease tries a create before it replaces.
func (w *writeLog) Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	info, err := w.MemStore.Put(ctx, key, body, cond)
	if err == nil {
		w.mu.Lock()
		w.writes = append(w.writes, "put "+key)
		w.mu.Unlock()
	}
	return info, err
}

func (w *writeLog) Delete(ctx context.Context, key string) error {
	w.mu.Lock()
	w.writes = append(w.writes, "delete "+key)
	w.mu.Unlock()
	return w.MemStore.Delete(ctx, key)
}

func (w *writeLog) reset() {
	w.mu.Lock()
	w.writes = nil
	w.mu.Unlock()
}

// agedWorld seeds, under world, a large world reset in game in August: three
// history entries and a small current generation.
func agedWorld(t *testing.T, st Store, world string) {
	t.Helper()
	p := WorldPrefix("", world)
	for n := int64(1); n <= 3; n++ {
		m := manifestAt(n, augustDay(int(n)), ObjectsDir+"large", fmt.Sprintf("%sbuilt%d", ObjectsDir, n))
		for _, f := range m.Files {
			mustPut(t, st, p+f.Object)
		}
		putJSON(t, st, p+HistoryKey(m.Generation, time.UnixMilli(m.Taken)), m)
	}
	cur := manifestAt(4, augustDay(4), ObjectsDir+"fresh")
	mustPut(t, st, p+ObjectsDir+"fresh")
	putJSON(t, st, p+ManifestName, cur)
}

func releasedBy(t *testing.T, st Store, world, node string, now time.Time) {
	t.Helper()
	p := WorldPrefix("", world)
	etag, err := TakeLease(context.Background(), st, p, Lease{Node: node, RenewedAt: now}, StaleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseLease(context.Background(), st, p, etag); err != nil {
		t.Fatal(err)
	}
}

func generationsOf(t *testing.T, st Store, world string) []int64 {
	t.Helper()
	entries, err := ListHistory(context.Background(), st, WorldPrefix("", world))
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, e := range entries {
		out = append(out, e.Generation)
	}
	return out
}

func TestTheAgingPassPrunesAnIdleWorldUnderItsLeaseAndTouchesNothingElse(t *testing.T) {
	now := augustDay(4).Add(31 * 24 * time.Hour)
	mem := NewMemStore(func() time.Time { return now })
	st := &writeLog{MemStore: mem}
	ctx := context.Background()
	policies := &Policies{Store: st}
	if err := policies.Sync(ctx, "ns", "aged", Retention{Last: 10, MaxAgeDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err := policies.Sync(ctx, "ns", "counted", Retention{Last: 10}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"ns/aged/idle", "ns/aged/playing", "ns/counted/idle"} {
		agedWorld(t, st, w)
	}
	releasedBy(t, st, "ns/aged/idle", "node-a", now)
	if _, err := TakeLease(ctx, st, WorldPrefix("", "ns/aged/playing"), Lease{Node: "node-b", RenewedAt: now}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	st.reset()

	a := &Ager{Store: st, Policies: policies, StaleAfter: StaleAfter, Clock: func() time.Time { return now }, Log: logr.Discard()}
	if err := a.AgeOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if got := generationsOf(t, st, "ns/aged/idle"); len(got) != 0 {
		t.Fatalf("idle world's history = %v, want none", got)
	}
	idle := WorldPrefix("", "ns/aged/idle")
	if !stored(t, st, idle+ObjectsDir+"fresh") || !stored(t, st, idle+ManifestName) {
		t.Fatalf("store = %v; the idle world's current generation must stay", st.Keys())
	}
	l, _, err := ReadLease(ctx, st, idle)
	if err != nil || l.Node != "" {
		t.Fatalf("idle world's lease = %+v, %v; want released", l, err)
	}
	var leaseWrites, other []string
	for _, w := range st.writes {
		switch {
		case w == "put "+idle+LeaseName:
			leaseWrites = append(leaseWrites, w)
		case slices.Contains([]string{
			"delete " + idle + HistoryKey(1, augustDay(1)),
			"delete " + idle + HistoryKey(2, augustDay(2)),
			"delete " + idle + HistoryKey(3, augustDay(3)),
			"delete " + idle + ObjectsDir + "large",
			"delete " + idle + ObjectsDir + "built1",
			"delete " + idle + ObjectsDir + "built2",
			"delete " + idle + ObjectsDir + "built3",
		}, w):
		default:
			other = append(other, w)
		}
	}
	if len(leaseWrites) != 2 {
		t.Errorf("lease writes = %v, want one take and one release", leaseWrites)
	}
	if len(other) != 0 {
		t.Errorf("the pass also wrote %v", other)
	}
	for _, w := range []string{"ns/aged/playing", "ns/counted/idle"} {
		if got := generationsOf(t, st, w); !slices.Equal(got, []int64{3, 2, 1}) {
			t.Errorf("%s history = %v, want it untouched", w, got)
		}
	}
	if l, _, err := ReadLease(ctx, st, WorldPrefix("", "ns/aged/playing")); err != nil || l.Node != "node-b" {
		t.Errorf("held lease = %+v, %v; want node-b's, untouched", l, err)
	}
}

func TestTheAgingPassLeavesAWorldWithNothingExpiredWithoutTakingItsLease(t *testing.T) {
	now := augustDay(4).Add(20 * 24 * time.Hour)
	mem := NewMemStore(func() time.Time { return now })
	st := &writeLog{MemStore: mem}
	ctx := context.Background()
	policies := &Policies{Store: st}
	if err := policies.Sync(ctx, "ns", "aged", Retention{Last: 10, MaxAgeDays: 30}); err != nil {
		t.Fatal(err)
	}
	agedWorld(t, st, "ns/aged/recent")
	st.reset()

	a := &Ager{Store: st, Policies: policies, StaleAfter: StaleAfter, Clock: func() time.Time { return now }, Log: logr.Discard()}
	if err := a.AgeOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(st.writes) != 0 {
		t.Fatalf("the pass wrote %v to a world with nothing expired", st.writes)
	}
}

func TestTheAgingPassChecksTheCurrentGenerationBeforeTakingTheLease(t *testing.T) {
	now := augustDay(4).Add(31 * 24 * time.Hour)
	mem := NewMemStore(func() time.Time { return now })
	st := &writeLog{MemStore: mem}
	ctx := context.Background()
	policies := &Policies{Store: st}
	if err := policies.Sync(ctx, "ns", "aged", Retention{Last: 10, MaxAgeDays: 30}); err != nil {
		t.Fatal(err)
	}
	p := WorldPrefix("", "ns/aged/one")
	old := manifestAt(1, augustDay(1), ObjectsDir+"large")
	mustPut(t, st, p+ObjectsDir+"large")
	putJSON(t, st, p+HistoryKey(1, augustDay(1)), old)
	putJSON(t, st, p+ManifestName, manifestAt(2, now.Add(-24*time.Hour), ObjectsDir+"large"))
	st.reset()

	a := &Ager{Store: st, Policies: policies, StaleAfter: StaleAfter, Clock: func() time.Time { return now }, Log: logr.Discard()}
	if err := a.AgeOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(st.writes) != 0 {
		t.Fatalf("the pass wrote %v, but the entry was replaced a day ago", st.writes)
	}
	if got := generationsOf(t, st, "ns/aged/one"); !slices.Equal(got, []int64{1}) {
		t.Fatalf("history = %v, want [1]", got)
	}
}

func TestTheAgingPassKeepsWhatARestoreReplacedUntilTheNextUpload(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := now
	mem := NewMemStore(func() time.Time { return clock })
	ctx := context.Background()
	policies := &Policies{Store: mem}
	policy := Retention{Last: 1, MaxAgeDays: 7}
	if err := policies.Sync(ctx, "ns", "aged", policy); err != nil {
		t.Fatal(err)
	}
	agedWorld(t, mem, "ns/aged/restored")
	p := WorldPrefix("", "ns/aged/restored")
	if _, err := RestoreWorld(ctx, mem, p, 2, Retention{Last: 10, MaxAgeDays: 30}, now); err != nil {
		t.Fatal(err)
	}
	if got := generationsOf(t, mem, "ns/aged/restored"); !slices.Equal(got, []int64{4}) {
		t.Fatalf("history after the restore = %v, want [4]", got)
	}

	clock = now.Add(30 * 24 * time.Hour)
	a := &Ager{Store: mem, Policies: policies, StaleAfter: StaleAfter, Clock: func() time.Time { return clock }, Log: logr.Discard()}
	if err := a.AgeOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := generationsOf(t, mem, "ns/aged/restored"); !slices.Equal(got, []int64{4}) {
		t.Fatalf("history = %v, want [4]: the restore's pin holds until the member's next upload", got)
	}
}

func TestTheAgingPassSkipsAWorldMarkedForDeletion(t *testing.T) {
	now := augustDay(4).Add(31 * 24 * time.Hour)
	mem := NewMemStore(func() time.Time { return now })
	st := &writeLog{MemStore: mem}
	ctx := context.Background()
	policies := &Policies{Store: st}
	if err := policies.Sync(ctx, "ns", "aged", Retention{Last: 10, MaxAgeDays: 30}); err != nil {
		t.Fatal(err)
	}
	agedWorld(t, st, "ns/aged/doomed")
	if err := MarkDeleted(ctx, st, "", "ns/aged/doomed"); err != nil {
		t.Fatal(err)
	}
	st.reset()

	a := &Ager{Store: st, Policies: policies, StaleAfter: StaleAfter, Clock: func() time.Time { return now }, Log: logr.Discard()}
	if err := a.AgeOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(st.writes) != 0 {
		t.Fatalf("the pass wrote %v to a world the deletion sweep owns", st.writes)
	}
}
