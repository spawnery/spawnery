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
	"strings"
	"sync/atomic"
	"testing"
)

const (
	slotOf  = "ns/g/g-abcd"
	kept    = "ns/g/round-7"
	keptKey = "round-7"
)

func (h *harness) publishStandby(id, standby, pod string) string {
	h.t.Helper()
	target := filepath.Join(h.t.TempDir(), "mount")
	if err := h.node(id).Publish(context.Background(), PublishRequest{
		Standby: standby, Keep: []string{"worlds/world"}, Target: target, Pod: pod,
	}); err != nil {
		h.t.Fatal(err)
	}
	return target
}

func (h *harness) bindWorld(world, server, pod string) {
	h.t.Helper()
	if err := WriteBinding(context.Background(), h.st, "", world, Binding{Server: server, Pod: pod}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) askKey(target, key string) string {
	h.t.Helper()
	_ = os.Remove(filepath.Join(target, KeyDoneFile))
	if err := os.WriteFile(filepath.Join(target, KeyRequestFile), []byte(key), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return strings.TrimSpace(h.waitFile(target, KeyDoneFile))
}

func TestAStandbySlotMakesNoStoreCalls(t *testing.T) {
	h := newHarness(t)
	counted := &countingStore{MemStore: h.st}
	h.store = counted
	target := h.publishStandby("a", slotOf, "ns/g-abcd")
	if _, err := os.Stat(filepath.Join(target, ReadyFile)); err != nil {
		t.Fatalf("a standby slot is not ready at once: %v", err)
	}
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.request(target, "1")
	if got := h.waitFile(target, DoneFile); !strings.HasPrefix(got, "failed 1") {
		t.Errorf("snapshot of a standby slot answered %q, want a failure", got)
	}
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())
	if n := counted.calls(); n != 0 {
		t.Errorf("a standby slot made %d store calls", n)
	}
	slot, _ := StandbySlot(slotOf)
	if _, err := os.Stat(h.node("a").worldDir(slot)); !os.IsNotExist(err) {
		t.Errorf("the slot's directory outlived its pod: %v", err)
	}
	if h.node("a").lookup(slot) != nil {
		t.Error("the node still knows the slot")
	}
}

func TestAStandbySlotTakesItsKeyAndSyncsFromThen(t *testing.T) {
	h := newHarness(t)
	h.bindWorld(kept, "g-abcd", "ns/g-abcd")
	target := h.publishStandby("a", slotOf, "ns/g-abcd")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)

	if got := h.askKey(target, keptKey); got != keptKey {
		t.Fatalf("key.done = %q, want %q", got, keptKey)
	}
	if _, _, err := ReadLease(context.Background(), h.st, WorldPrefix("", kept)); err != nil {
		t.Fatalf("no lease on the kept world: %v", err)
	}
	if got := h.askKey(target, keptKey); got != keptKey {
		t.Errorf("asked again: key.done = %q, want %q", got, keptKey)
	}
	if got := h.askKey(target, "other"); !strings.HasPrefix(got, "failed other") {
		t.Errorf("another key: key.done = %q, want a failure", got)
	}

	h.request(target, "1")
	if got := h.waitFile(target, DoneFile); strings.TrimSpace(got) != "1" {
		t.Fatalf("snapshot done = %q", got)
	}
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())
	m, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", kept))
	if err != nil {
		t.Fatalf("no manifest for the kept world: %v", err)
	}
	if len(m.Files) == 0 {
		t.Error("the kept world's manifest lists no files")
	}
	if held, err := LeaseIsHeld(context.Background(), h.st, WorldPrefix("", kept), StaleAfter); held || err != nil {
		t.Errorf("the lease outlived the final upload: held %v, %v", held, err)
	}
}

func TestAKeyNeedsTheOperatorsBinding(t *testing.T) {
	for name, bind := range map[string]func(h *harness){
		"no binding":     func(*harness) {},
		"another server": func(h *harness) { h.bindWorld(kept, "g-wxyz", "ns/g-abcd") },
		"another pod":    func(h *harness) { h.bindWorld(kept, "g-abcd", "ns/g-abcd-old") },
		"a deletion pending": func(h *harness) {
			h.bindWorld(kept, "g-abcd", "ns/g-abcd")
			_ = MarkDeleted(context.Background(), h.st, "", kept)
		},
		"a key that is a path": func(*harness) {},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			bind(h)
			target := h.publishStandby("a", slotOf, "ns/g-abcd")
			key := keptKey
			if name == "a key that is a path" {
				key = "../g-wxyz"
			}
			if got := h.askKey(target, key); !strings.HasPrefix(got, "failed ") {
				t.Fatalf("key.done = %q, want a failure", got)
			}
			if _, _, err := ReadLease(context.Background(), h.st, WorldPrefix("", kept)); !errors.Is(err, ErrNotFound) {
				t.Errorf("a refused key left a lease: %v", err)
			}
			slot, _ := StandbySlot(slotOf)
			if s := h.node("a").lookup(slot); s == nil || !s.Standby {
				t.Error("a refused key moved the slot")
			}
		})
	}
}

func TestAKeyThatHoldsAWorldIsRefused(t *testing.T) {
	h := newHarness(t)
	other, _ := h.publish("b", kept, "ns/other")
	writeFile(t, filepath.Join(other, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("b", other)
	h.node("b").Settle(context.Background())

	h.bindWorld(kept, "g-abcd", "ns/g-abcd")
	target := h.publishStandby("a", slotOf, "ns/g-abcd")
	if got := h.askKey(target, keptKey); !strings.Contains(got, "already holds a world") {
		t.Fatalf("key.done = %q, want a refusal", got)
	}
	if held, err := LeaseIsHeld(context.Background(), h.st, WorldPrefix("", kept), StaleAfter); held || err != nil {
		t.Errorf("the refused key's lease was not released: held %v, %v", held, err)
	}
}

func restartNode(h *harness, id string) *Node {
	h.t.Helper()
	old := h.node(id)
	n, err := NewNode(old.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := n.Resume(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.nodes[id] = n
	return n
}

func TestARestartMidKeyingFinishesTheKey(t *testing.T) {
	h := newHarness(t)
	h.bindWorld(kept, "g-abcd", "ns/g-abcd")
	target := h.publishStandby("a", slotOf, "ns/g-abcd")
	// The lease was taken, then the node agent died before it saved the state.
	if _, err := TakeLease(context.Background(), h.st, WorldPrefix("", kept),
		Lease{Node: "a", Pod: "ns/g-abcd", RenewedAt: h.now}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, KeyRequestFile), []byte(keptKey), 0o644); err != nil {
		t.Fatal(err)
	}
	restartNode(h, "a")
	if got := strings.TrimSpace(h.waitFile(target, KeyDoneFile)); got != keptKey {
		t.Fatalf("key.done = %q, want %q", got, keptKey)
	}

	// Saved, then died before the answer.
	if err := os.Remove(filepath.Join(target, KeyDoneFile)); err != nil {
		t.Fatal(err)
	}
	n := restartNode(h, "a")
	if got := strings.TrimSpace(h.waitFile(target, KeyDoneFile)); got != keptKey {
		t.Fatalf("after the second restart key.done = %q, want %q", got, keptKey)
	}
	if s := n.lookup(kept); s == nil || s.Standby || s.Target != target {
		t.Fatalf("the resumed state = %+v", s)
	}
}

func TestAResumedMemberOnTheSameNodeReusesTheSlot(t *testing.T) {
	h := newHarness(t)
	h.bindWorld(kept, "g-abcd", "ns/g-abcd")
	target := h.publishStandby("a", slotOf, "ns/g-abcd")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	if got := h.askKey(target, keptKey); got != keptKey {
		t.Fatalf("key.done = %q", got)
	}
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	before := downloadsCounted(h.node("a"))
	target2, err := h.publish("a", kept, "ns/g-round-7")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target2, ReadyFile)); err != nil {
		t.Fatalf("not ready at once on the node that keyed the world: %v", err)
	}
	if downloadsCounted(h.node("a")) != before {
		t.Error("a download ran for the slot's own copy")
	}
	slot, _ := StandbySlot(slotOf)
	if got, want := h.mnt["a"].binds[target2], h.node("a").dataDir(slot); got != want {
		t.Errorf("bound %q, want the slot's directory %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(target2, "worlds/world/level.dat")); err != nil {
		t.Errorf("the world is not there: %v", err)
	}
}

func TestAStandbySlotWaitsForTheCacheOfAWorldKeptUnderItsName(t *testing.T) {
	h := newHarness(t)
	h.bindWorld(kept, "g-abcd", "ns/g-abcd")
	first := h.publishStandby("a", slotOf, "ns/g-abcd")
	writeFile(t, filepath.Join(first, "worlds/world/level.dat"), 5, h.now)
	if got := h.askKey(first, keptKey); got != keptKey {
		t.Fatalf("key.done = %q", got)
	}

	// A new member of the same name while the kept world is still in use.
	again := filepath.Join(t.TempDir(), "mount")
	req := PublishRequest{Standby: slotOf, Keep: []string{"worlds/world"}, Target: again, Pod: "ns/g-abcd"}
	if err := h.node("a").Publish(context.Background(), req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("publish over a kept world's cache: err = %v, want ErrUnavailable", err)
	}
	if _, err := os.Stat(filepath.Join(first, "worlds/world/level.dat")); err != nil {
		t.Fatalf("the kept world's files went with the refused publish: %v", err)
	}

	// Once the world is uploaded and idle, its cache gives way.
	h.unpublish("a", first)
	h.node("a").Settle(context.Background())
	if err := h.node("a").Publish(context.Background(), req); err != nil {
		t.Fatalf("publish once the cache is idle: %v", err)
	}
	if h.node("a").lookup(kept) != nil {
		t.Error("the node still knows a cache whose directory went to the new slot")
	}
	if _, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", kept)); err != nil {
		t.Errorf("the kept world is not in the store: %v", err)
	}
}

func TestAKeyWhoseLeaseAnotherNodeHoldsFails(t *testing.T) {
	h := newHarness(t)
	h.bindWorld(kept, "g-abcd", "ns/g-abcd")
	if _, err := TakeLease(context.Background(), h.st, WorldPrefix("", kept),
		Lease{Node: "b", Pod: "ns/other", RenewedAt: h.now}, StaleAfter); err != nil {
		t.Fatal(err)
	}
	target := h.publishStandby("a", slotOf, "ns/g-abcd")
	if got := h.askKey(target, keptKey); !strings.HasPrefix(got, "failed "+keptKey) {
		t.Fatalf("key.done = %q, want a failure", got)
	}
	if l, _, err := ReadLease(context.Background(), h.st, WorldPrefix("", kept)); err != nil || l.Node != "b" {
		t.Errorf("lease = %+v, %v; want node b's untouched", l, err)
	}
	slot, _ := StandbySlot(slotOf)
	if s := h.node("a").lookup(slot); s == nil || !s.Standby {
		t.Error("a refused key moved the slot")
	}
}

func TestAKeyWhoseManifestCannotBeReadFailsAndReleasesTheLease(t *testing.T) {
	h := newHarness(t)
	h.bindWorld(kept, "g-abcd", "ns/g-abcd")
	h.store = &failingKeys{MemStore: h.st, match: func(key string) bool { return key == WorldPrefix("", kept)+ManifestName }}
	target := h.publishStandby("a", slotOf, "ns/g-abcd")
	if got := h.askKey(target, keptKey); !strings.HasPrefix(got, "failed "+keptKey) || !strings.Contains(got, "manifest") {
		t.Fatalf("key.done = %q, want a failure naming the manifest", got)
	}
	l, _, err := ReadLease(context.Background(), h.st, WorldPrefix("", kept))
	if err != nil {
		t.Fatalf("no lease left behind to read: %v", err)
	}
	if l.Node != "" {
		t.Errorf("lease = %+v, want it released", l)
	}
	slot, _ := StandbySlot(slotOf)
	if s := h.node("a").lookup(slot); s == nil || !s.Standby {
		t.Error("a refused key moved the slot")
	}
}

func TestAFailedKeyLeavesTheKeyFreeForTheNextAttempt(t *testing.T) {
	h := newHarness(t)
	h.bindWorld(kept, "g-abcd", "ns/g-abcd")
	var down atomic.Bool
	down.Store(true)
	manifest := WorldPrefix("", kept) + ManifestName
	h.store = &failingKeys{MemStore: h.st, match: func(key string) bool { return down.Load() && key == manifest }}
	target := h.publishStandby("a", slotOf, "ns/g-abcd")
	if got := h.askKey(target, keptKey); !strings.HasPrefix(got, "failed "+keptKey) {
		t.Fatalf("key.done = %q, want a failure", got)
	}
	worlds := BucketWorlds{Store: h.st}
	if exists, err := worlds.Exists(context.Background(), kept); err != nil || exists {
		t.Fatalf("Exists = %v, %v; a released lease with no manifest reads as a world", exists, err)
	}
	if err := ReplaceBinding(context.Background(), h.st, "", kept,
		Binding{Server: "g-abcd", Pod: "ns/g-abcd"}, Binding{Server: "g-abcd", Pod: "ns/g-abcd"}); err != nil {
		t.Fatalf("a released lease blocks replacing the binding: %v", err)
	}

	down.Store(false)
	if got := h.askKey(target, keptKey); got != keptKey {
		t.Fatalf("second key.done = %q, want %q", got, keptKey)
	}
	if exists, err := worlds.Exists(context.Background(), kept); err != nil || !exists {
		t.Fatalf("Exists = %v, %v after the world was keyed", exists, err)
	}
}
