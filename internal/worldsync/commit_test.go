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
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func firstGeneration(t *testing.T, st Store, data string, keepPrev bool) (Manifest, string) {
	t.Helper()
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	m, etag, err := CommitSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", keepPrev, NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 4, time.Unix(2, 0))
	return m, etag
}

func TestACommitPutsThePreviousManifestIntoHistory(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	data := t.TempDir()
	m1, e1 := firstGeneration(t, st, data, true)
	m2, _, err := CommitSnapshot(ctx, st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, true, NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Taken != snapTaken(1) || m2.Taken != snapTaken(2) {
		t.Fatalf("taken = %d, %d; want the snapshots' %d, %d", m1.Taken, m2.Taken, snapTaken(1), snapTaken(2))
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil || len(entries) != 1 || entries[0].Generation != 1 || !entries[0].Taken.Equal(time.UnixMilli(m1.Taken)) {
		t.Fatalf("history = %+v, %v; want generation 1 at its snapshot's time", entries, err)
	}
	got, err := readHistoryEntry(ctx, st, entries[0].Key)
	if err != nil || !reflect.DeepEqual(got, m1) {
		t.Fatalf("entry = %+v, %v; want %+v", got, err, m1)
	}
}

func TestAHistoryEntryAnEarlierAttemptWroteIsNoError(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	data := t.TempDir()
	m1, e1 := firstGeneration(t, st, data, true)
	key := prefix + HistoryKey(1, time.UnixMilli(m1.Taken))
	if _, err := st.Put(ctx, key, strings.NewReader(`{"earlier":"attempt"}`), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CommitSnapshot(ctx, st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, true, NewWorldID); err != nil {
		t.Fatalf("a commit after an earlier attempt's history write: %v", err)
	}
	b, _, err := readObject(ctx, st, key)
	if err != nil || string(b) != `{"earlier":"attempt"}` {
		t.Fatalf("entry = %q, %v; the write must be If-None-Match", b, err)
	}
}

func TestACommitWithoutHistoryWritesNone(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	m1, e1 := firstGeneration(t, st, data, false)
	if _, _, err := CommitSnapshot(context.Background(), st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, false, NewWorldID); err != nil {
		t.Fatal(err)
	}
	if got := historyGenerations(t, st); len(got) != 0 {
		t.Fatalf("history = %v, want none", got)
	}
}

func TestAManifestWithoutTakenGoesToHistoryAtItsLastModified(t *testing.T) {
	written := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	now := written
	st := NewMemStore(func() time.Time { return now })
	ctx := context.Background()
	data := t.TempDir()
	m1, e1 := firstGeneration(t, st, data, false)
	legacy := m1
	legacy.Taken = 0
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Put(ctx, prefix+ManifestName, bytes.NewReader(b), PutCondition{IfMatch: e1})
	if err != nil {
		t.Fatal(err)
	}
	now = written.Add(time.Hour)
	if _, _, err := CommitSnapshot(ctx, st, prefix, snapshotOf(t, data, legacy.Files, 2), &legacy, info.ETag, true, NewWorldID); err != nil {
		t.Fatal(err)
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil || len(entries) != 1 || !entries[0].Taken.Equal(written) {
		t.Fatalf("history = %+v, %v; want generation 1 at %v, when its manifest was written", entries, err, written)
	}
}

type failingPut struct {
	*MemStore
	key string
}

func (f *failingPut) Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	if key == f.key {
		return ObjectInfo{}, errors.New("store down")
	}
	return f.MemStore.Put(ctx, key, body, cond)
}

func TestACrashBeforeTheManifestLeavesAnEntryThePruneRemoves(t *testing.T) {
	mem := NewMemStore(time.Now)
	ctx := context.Background()
	data := t.TempDir()
	m1, e1 := firstGeneration(t, mem, data, true)
	st := &failingPut{MemStore: mem, key: prefix + ManifestName}
	if _, _, err := CommitSnapshot(ctx, st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, true, NewWorldID); err == nil {
		t.Fatal("a commit whose manifest put failed succeeded")
	}
	if got := historyGenerations(t, mem); !slices.Equal(got, []int64{1}) {
		t.Fatalf("history = %v, want the entry of the still current generation 1", got)
	}
	if _, err := Prune(ctx, mem, prefix, PruneRequest{Current: m1, Policy: Retention{Last: 10}}); err != nil {
		t.Fatal(err)
	}
	if got := historyGenerations(t, mem); len(got) != 0 {
		t.Fatalf("history = %v after the prune, want none", got)
	}
	if err := Download(ctx, mem, prefix, t.TempDir(), m1, 2, -1); err != nil {
		t.Fatalf("generation 1 lost an object to the prune: %v", err)
	}
}
