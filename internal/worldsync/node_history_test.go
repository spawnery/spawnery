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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func setPolicy(t *testing.T, st Store, r Retention) {
	t.Helper()
	if err := WriteRetention(context.Background(), st, "", "ns", "g", r); err != nil {
		t.Fatal(err)
	}
}

// upload writes level.dat at size and has node id upload it as snapshot seq.
func (h *harness) upload(id, target string, size, seq int) {
	h.t.Helper()
	writeFile(h.t, filepath.Join(target, "worlds/world/level.dat"), size, h.now)
	h.request(target, strconv.Itoa(seq))
	h.node(id).Settle(context.Background())
}

func packCount(t *testing.T, st Store) int {
	t.Helper()
	keys, err := st.List(context.Background(), prefix+PacksDir)
	if err != nil {
		t.Fatal(err)
	}
	return len(keys)
}

func TestUploadsKeepTheGenerationsThePolicyKeeps(t *testing.T) {
	h := newHarness(t)
	setPolicy(t, h.st, Retention{Last: 3})
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		h.upload("a", target, 4+i, i)
	}
	m, _, err := ReadManifest(context.Background(), h.st, prefix)
	if err != nil || m.Generation != 4 {
		t.Fatalf("manifest = %+v, %v; want generation 4", m, err)
	}
	if got := historyGenerations(t, h.st); !slices.Equal(got, []int64{3, 2}) {
		t.Fatalf("history = %v, want [3 2]: last 3 is the current and two more", got)
	}
	if got := packCount(t, h.st); got != 3 {
		t.Fatalf("packs = %d, want the 3 of the kept generations", got)
	}
}

func TestWithoutAPolicyAWorldKeepsOnlyItsCurrentGeneration(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	if got := historyGenerations(t, h.st); len(got) != 0 {
		t.Fatalf("history = %v, want none", got)
	}
	if got := packCount(t, h.st); got != 1 {
		t.Fatalf("packs = %d, want 1", got)
	}
}

type failingKeys struct {
	*MemStore
	match func(key string) bool
}

func (f *failingKeys) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if f.match(key) {
		return nil, ObjectInfo{}, errors.New("store down")
	}
	return f.MemStore.Get(ctx, key)
}

func (f *failingKeys) Head(ctx context.Context, key string) (ObjectInfo, error) {
	if f.match(key) {
		return ObjectInfo{}, errors.New("store down")
	}
	return f.MemStore.Head(ctx, key)
}

func TestAPolicyThatCannotBeReadKeepsHistoryAndDeletesNothing(t *testing.T) {
	h := newHarness(t)
	h.store = &failingKeys{MemStore: h.st, match: func(key string) bool { return strings.HasPrefix(key, ".retention/") }}
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	if got := historyGenerations(t, h.st); !slices.Equal(got, []int64{1}) {
		t.Fatalf("history = %v, want [1]: an unread policy may not drop a generation", got)
	}
	if got := packCount(t, h.st); got != 2 {
		t.Fatalf("packs = %d, want 2: nothing is deleted on a policy never read", got)
	}
}

func TestAMalformedPolicyKeepsWhatWasReadBefore(t *testing.T) {
	h := newHarness(t)
	setPolicy(t, h.st, Retention{Last: 3})
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	if _, err := h.st.Put(context.Background(), RetentionKey("", "ns", "g"), strings.NewReader("last: 0"), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	h.upload("a", target, 7, 3)
	h.upload("a", target, 8, 4)
	if got := historyGenerations(t, h.st); !slices.Equal(got, []int64{3, 2}) {
		t.Fatalf("history = %v, want [3 2] under the last policy read", got)
	}
}

func TestAPolicyChangeReachesTheNextUpload(t *testing.T) {
	h := newHarness(t)
	setPolicy(t, h.st, Retention{Last: 5})
	target, _ := h.publish("a", w, "p1")
	for i := 1; i <= 3; i++ {
		h.upload("a", target, 4+i, i)
	}
	if got := historyGenerations(t, h.st); !slices.Equal(got, []int64{2, 1}) {
		t.Fatalf("history = %v, want [2 1]", got)
	}
	setPolicy(t, h.st, Retention{})
	h.upload("a", target, 8, 4)
	if got := historyGenerations(t, h.st); len(got) != 0 {
		t.Fatalf("history = %v after the policy was dropped, want none", got)
	}
	if got := packCount(t, h.st); got != 1 {
		t.Fatalf("packs = %d, want 1", got)
	}
}

func TestEveryTwelfthPruneSweepsTheWorld(t *testing.T) {
	h := newHarness(t)
	stray := prefix + ObjectsDir + strings.Repeat("f", 64)
	if _, err := h.st.Put(context.Background(), stray, strings.NewReader("left by a failed attempt"), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	target, _ := h.publish("a", w, "p1")
	for i := 1; i <= 11; i++ {
		h.upload("a", target, 4+i, i)
		if !stored(t, h.st, stray) {
			t.Fatalf("upload %d swept the world; the sweep is every twelfth", i)
		}
	}
	h.upload("a", target, 16, 12)
	if stored(t, h.st, stray) {
		t.Fatal("the twelfth upload did not sweep the stray object")
	}
}

func TestTheLeasesReleaseSweepsTheWorld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	stray := prefix + PacksDir + "9-0123456789abcdef.tar.gz"
	if _, err := h.st.Put(ctx, stray, strings.NewReader("x"), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(ctx)
	if stored(t, h.st, stray) {
		t.Fatal("the release did not sweep the stray pack")
	}
	if l, _, err := ReadLease(ctx, h.st, prefix); err != nil || l.Node != "" {
		t.Fatalf("lease = %+v, %v; want released after the sweep", l, err)
	}
}

func TestPrunedObjectsAreCounted(t *testing.T) {
	before := testutil.ToFloat64(prunedObjects)
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	h.upload("a", target, 5, 1)
	h.upload("a", target, 6, 2)
	if got := testutil.ToFloat64(prunedObjects); got != before+1 {
		t.Fatalf("pruned objects = %v, want %v: the pack of generation 1", got, before+1)
	}
}
