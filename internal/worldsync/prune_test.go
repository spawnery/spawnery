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
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// manifestOf is generation n, taken n hours after a fixed start, with one
// large file per object.
func manifestOf(n int64, objects ...string) Manifest {
	m := Manifest{WorldID: "w", Generation: n, Taken: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour).UnixMilli()}
	for i, o := range objects {
		m.Files = append(m.Files, FileEntry{Path: fmt.Sprintf("f%d", i), Size: PackBelow, Object: o})
	}
	return m
}

func putJSON(t *testing.T, st Store, key string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), key, bytes.NewReader(b), PutCondition{}); err != nil {
		t.Fatal(err)
	}
}

func seedObjects(t *testing.T, st Store, m Manifest) {
	t.Helper()
	for _, f := range m.Files {
		if _, err := st.Put(context.Background(), prefix+f.Object, strings.NewReader(f.Object), PutCondition{}); err != nil {
			t.Fatal(err)
		}
	}
}

func entryKey(m Manifest) string { return prefix + HistoryKey(m.Generation, time.UnixMilli(m.Taken)) }

func seedEntry(t *testing.T, st Store, m Manifest) {
	t.Helper()
	seedObjects(t, st, m)
	putJSON(t, st, entryKey(m), m)
}

func seedCurrent(t *testing.T, st Store, m Manifest) {
	t.Helper()
	seedObjects(t, st, m)
	putJSON(t, st, prefix+ManifestName, m)
}

func stored(t *testing.T, st Store, key string) bool {
	t.Helper()
	_, err := st.Head(context.Background(), key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func historyGenerations(t *testing.T, st Store) []int64 {
	t.Helper()
	entries, err := ListHistory(context.Background(), st, prefix)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, e := range entries {
		out = append(out, e.Generation)
	}
	return out
}

// fiveGenerations seeds generations 1 to 4 as history and 5 as current,
// each naming objects/shared and an object of its own.
func fiveGenerations(t *testing.T, st Store) Manifest {
	t.Helper()
	for n := int64(1); n <= 4; n++ {
		seedEntry(t, st, manifestOf(n, ObjectsDir+"shared", fmt.Sprintf("%sg%d", ObjectsDir, n)))
	}
	cur := manifestOf(5, ObjectsDir+"shared", ObjectsDir+"g5")
	seedCurrent(t, st, cur)
	return cur
}

func TestPruneWithoutAPolicyDropsWhatOnlyTheReplacedManifestNamed(t *testing.T) {
	st := NewMemStore(time.Now)
	m1 := manifestOf(1, ObjectsDir+"a", ObjectsDir+"b")
	m2 := manifestOf(2, ObjectsDir+"a", ObjectsDir+"c")
	seedObjects(t, st, m1)
	seedCurrent(t, st, m2)
	deleted, err := Prune(context.Background(), st, prefix, PruneRequest{Current: m2, Replaced: &m1})
	if err != nil || deleted != 1 {
		t.Fatalf("deleted = %d, %v; want 1", deleted, err)
	}
	if stored(t, st, prefix+ObjectsDir+"b") || !stored(t, st, prefix+ObjectsDir+"a") || !stored(t, st, prefix+ObjectsDir+"c") {
		t.Fatalf("store = %v; want b gone, a and c kept", st.Keys())
	}
}

func TestPruneKeepsWhatThePolicyKeepsAndWhatItNames(t *testing.T) {
	st := NewMemStore(time.Now)
	cur := fiveGenerations(t, st)
	deleted, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 3}})
	if err != nil || deleted != 2 {
		t.Fatalf("deleted = %d, %v; want 2", deleted, err)
	}
	if got := historyGenerations(t, st); !slices.Equal(got, []int64{4, 3}) {
		t.Fatalf("history = %v, want [4 3]", got)
	}
	for _, o := range []string{"shared", "g3", "g4", "g5"} {
		if !stored(t, st, prefix+ObjectsDir+o) {
			t.Errorf("%s is gone, but a kept manifest names it", o)
		}
	}
	for _, o := range []string{"g1", "g2"} {
		if stored(t, st, prefix+ObjectsDir+o) {
			t.Errorf("%s survived, but only dropped generations named it", o)
		}
	}
}

func TestAnEntryOfTheCurrentGenerationIsCovered(t *testing.T) {
	st := NewMemStore(time.Now)
	cur := fiveGenerations(t, st)
	seedEntry(t, st, cur)
	if _, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 10}}); err != nil {
		t.Fatal(err)
	}
	if got := historyGenerations(t, st); !slices.Equal(got, []int64{4, 3, 2, 1}) {
		t.Fatalf("history = %v, want [4 3 2 1]: the entry of the current generation goes, the rest stays", got)
	}
	if !stored(t, st, prefix+ObjectsDir+"g5") {
		t.Fatal("the current generation lost an object with its leftover entry")
	}
}

type brokenEntry struct {
	*MemStore
	key string
}

func (f *brokenEntry) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if key == f.key {
		return nil, ObjectInfo{}, errors.New("store down")
	}
	return f.MemStore.Get(ctx, key)
}

func TestAKeptEntryThatCannotBeReadDeletesNothing(t *testing.T) {
	mem := NewMemStore(time.Now)
	cur := fiveGenerations(t, mem)
	before := mem.Keys()
	st := &brokenEntry{MemStore: mem, key: entryKey(manifestOf(3))}
	if _, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 3}}); err == nil {
		t.Fatal("a prune that could not read a kept entry reported success")
	}
	if after := mem.Keys(); !slices.Equal(after, before) {
		t.Fatalf("keys after = %v, before = %v; nothing may go while a kept entry's names are unknown", after, before)
	}
}

func TestTheSweepDeletesStraysAndNothingAKeptManifestNames(t *testing.T) {
	st := NewMemStore(time.Now)
	cur := fiveGenerations(t, st)
	strays := []string{ObjectsDir + "stray", PacksDir + "9-0123456789abcdef.tar.gz"}
	for _, k := range strays {
		if _, err := st.Put(context.Background(), prefix+k, strings.NewReader("x"), PutCondition{}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 5}, Sweep: true})
	if err != nil || deleted != 2 {
		t.Fatalf("deleted = %d, %v; want the 2 strays", deleted, err)
	}
	for _, k := range strays {
		if stored(t, st, prefix+k) {
			t.Errorf("%s survived the sweep", k)
		}
	}
	for _, o := range []string{"shared", "g1", "g2", "g3", "g4", "g5"} {
		if !stored(t, st, prefix+ObjectsDir+o) {
			t.Errorf("the sweep deleted %s, which a kept manifest names", o)
		}
	}
}

func TestTheCacheSparesTheReadsOfKeptEntries(t *testing.T) {
	mem := NewMemStore(time.Now)
	cur := fiveGenerations(t, mem)
	st := &countingStore{MemStore: mem}
	cache := ObjectCache{}
	req := PruneRequest{Current: cur, Policy: Retention{Last: 4}, Cache: cache}
	if _, err := Prune(context.Background(), st, prefix, req); err != nil {
		t.Fatal(err)
	}
	if got := st.gets.Load(); got != 4 {
		t.Fatalf("the first prune read %d entries, want 4: three kept, one dropped", got)
	}
	st.gets.Store(0)
	if _, err := Prune(context.Background(), st, prefix, req); err != nil {
		t.Fatal(err)
	}
	if got := st.gets.Load(); got != 0 {
		t.Fatalf("the second prune read %d entries, want 0", got)
	}
	if len(cache) != 3 {
		t.Fatalf("cache holds %d entries, want the 3 kept", len(cache))
	}
}

type deleteLog struct {
	*MemStore
	mu   sync.Mutex
	keys []string
}

func (d *deleteLog) Delete(ctx context.Context, key string) error {
	d.mu.Lock()
	d.keys = append(d.keys, strings.TrimPrefix(key, prefix))
	d.mu.Unlock()
	return d.MemStore.Delete(ctx, key)
}

func TestHistoryEntriesGoBeforeTheObjectsOnlyTheyNamed(t *testing.T) {
	mem := NewMemStore(time.Now)
	cur := fiveGenerations(t, mem)
	st := &deleteLog{MemStore: mem}
	if _, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 2}}); err != nil {
		t.Fatal(err)
	}
	lastEntry, firstObject := -1, len(st.keys)
	for i, k := range st.keys {
		if strings.HasPrefix(k, HistoryDir) {
			lastEntry = i
		}
		if strings.HasPrefix(k, ObjectsDir) && i < firstObject {
			firstObject = i
		}
	}
	if lastEntry == -1 || firstObject == len(st.keys) || lastEntry > firstObject {
		t.Fatalf("deletes in order %v; every entry must go before the first object, or a restore could pick an entry whose objects are gone", st.keys)
	}
}

type failingEntryDelete struct {
	*MemStore
	key string
}

func (f *failingEntryDelete) Delete(ctx context.Context, key string) error {
	if key == f.key {
		return errors.New("store down")
	}
	return f.MemStore.Delete(ctx, key)
}

func TestObjectsStayWhenADroppedEntryCannotBeDeleted(t *testing.T) {
	mem := NewMemStore(time.Now)
	cur := fiveGenerations(t, mem)
	blobs := func() []string {
		var out []string
		for _, k := range mem.Keys() {
			if strings.HasPrefix(k, prefix+ObjectsDir) || strings.HasPrefix(k, prefix+PacksDir) {
				out = append(out, k)
			}
		}
		return out
	}
	before := blobs()
	st := &failingEntryDelete{MemStore: mem, key: entryKey(manifestOf(2))}
	if _, err := Prune(context.Background(), st, prefix, PruneRequest{Current: cur, Policy: Retention{Last: 2}}); err == nil {
		t.Fatal("a prune that could not delete a dropped entry reported success")
	}
	if after := blobs(); !slices.Equal(after, before) {
		t.Fatalf("objects after = %v, before = %v; none may go while a dropped entry is still listed", after, before)
	}
}
