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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

type fakeMounter struct{ binds map[string]string }

func (f *fakeMounter) Bind(source, target string) error {
	f.binds[target] = source
	_ = os.Remove(target)
	return os.Symlink(source, target)
}

func (f *fakeMounter) Unmount(target string) error {
	delete(f.binds, target)
	return os.Remove(target)
}

// gate holds the store calls match picks, before the call or, with after,
// after it succeeded, until open is closed or the caller gives up.
type gate struct {
	*MemStore
	armed atomic.Bool
	match func(op, key string) bool
	after bool
	open  chan struct{}
	held  chan string
}

func (g *gate) hold(ctx context.Context, op, key string) error {
	if !g.armed.Load() || !g.match(op, key) {
		return nil
	}
	select {
	case g.held <- key:
	default:
	}
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if err := g.hold(ctx, "get", key); err != nil {
		return nil, ObjectInfo{}, err
	}
	return g.MemStore.Get(ctx, key)
}

func (g *gate) Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	if !g.after {
		if err := g.hold(ctx, "put", key); err != nil {
			return ObjectInfo{}, err
		}
	}
	info, err := g.MemStore.Put(ctx, key, body, cond)
	if err == nil && g.after {
		_ = g.hold(ctx, "put", key)
	}
	return info, err
}

type harness struct {
	t     *testing.T
	st    *MemStore
	store Store
	now   time.Time
	nodes map[string]*Node
	mnt   map[string]*fakeMounter
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, now: time.Unix(1_700_000_000, 0), nodes: map[string]*Node{}, mnt: map[string]*fakeMounter{}}
	h.st = NewMemStore(func() time.Time { return h.now })
	return h
}

// gate puts a gate in front of the store for every node created after it;
// it holds nothing until armed.
func (h *harness) gate(match func(op, key string) bool, after bool) *gate {
	g := &gate{MemStore: h.st, match: match, after: after, open: make(chan struct{}), held: make(chan string, 16)}
	h.store = g
	return g
}

func (h *harness) node(id string) *Node {
	if n, ok := h.nodes[id]; ok {
		return n
	}
	var st Store = h.st
	if h.store != nil {
		st = h.store
	}
	m := &fakeMounter{binds: map[string]string{}}
	n, err := NewNode(Config{
		Root: filepath.Join(h.t.TempDir(), id), NodeID: id, Store: st, Mounter: m,
		StaleAfter: StaleAfter, RenewEvery: 30 * time.Second, PollEvery: time.Millisecond,
		EvictAfter: 24 * time.Hour, Parallel: 4, MinFree: 0,
		Clock: func() time.Time { return h.now }, Log: logr.Discard(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.nodes[id], h.mnt[id] = n, m
	return n
}

func (h *harness) publish(id, world, pod string) (string, error) {
	target := filepath.Join(h.t.TempDir(), "mount")
	return target, h.node(id).Publish(context.Background(), PublishRequest{World: world, Keep: []string{"worlds/world"}, Target: target, Pod: pod})
}

func (h *harness) waitFile(target, rel string) string {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range h.nodes {
			n.Settle(context.Background())
		}
		if b, err := os.ReadFile(filepath.Join(target, rel)); err == nil {
			return string(b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("%s never appeared under %s", rel, target)
	return ""
}

func (h *harness) unpublish(id, target string) {
	h.t.Helper()
	if err := h.node(id).Unpublish(context.Background(), target); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) request(target, seq string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(target, RequestFile), []byte(seq), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) orphans(id string) int {
	entries, _ := os.ReadDir(filepath.Join(h.node(id).cfg.Root, "orphans"))
	return len(entries)
}

func downloadsCounted(n *Node) int { return int(n.downloads.Load()) }

const w = "ns/g/k"

func TestANewWorldIsReadyAtOnce(t *testing.T) {
	h := newHarness(t)
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ReadyFile)); err != nil {
		t.Fatalf("no ready file for a world without manifest: %v", err)
	}
}

func TestAStoppedWorldResumesOnAnotherNodeWithItsContent(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	if err := h.node("a").Unpublish(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	h.node("a").Settle(context.Background()) // final upload and release

	target2, err := h.publish("b", w, "p2")
	if err != nil {
		t.Fatalf("publish on b after a released: %v", err)
	}
	h.waitFile(target2, ReadyFile)
	if _, err := os.Stat(filepath.Join(target2, "worlds/world/level.dat")); err != nil {
		t.Fatalf("the world did not travel: %v", err)
	}
}

func TestAWorldHeldByAnotherNodeIsUnavailable(t *testing.T) {
	h := newHarness(t)
	if _, err := h.publish("a", w, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.publish("b", w, "p2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestACurrentCacheNeedsNoDownload(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	gets := downloadsCounted(h.node("a"))
	target2, _ := h.publish("a", w, "p2")
	if _, err := os.Stat(filepath.Join(target2, ReadyFile)); err != nil {
		t.Fatalf("not ready at once on the node that holds the current copy: %v", err)
	}
	if downloadsCounted(h.node("a")) != gets {
		t.Fatal("a download ran for a current cache")
	}
}

func TestASnapshotRequestIsAnsweredAndUploaded(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.request(target, "3")
	if got := h.waitFile(target, DoneFile); strings.TrimSpace(got) != "3" {
		t.Fatalf("done = %q, want 3", got)
	}
	h.node("a").Settle(context.Background())
	m, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", w))
	if err != nil || len(m.Files) != 1 {
		t.Fatalf("manifest after the snapshot: %+v, %v", m, err)
	}
}

func TestAStopDuringAnOutageUploadsLater(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.st.SetOutage(errors.New("down"))
	if err := h.node("a").Unpublish(context.Background(), target); err != nil {
		t.Fatalf("unpublish must not wait for the store: %v", err)
	}
	h.node("a").Settle(context.Background())
	h.st.SetOutage(nil)
	h.now = h.now.Add(2 * time.Minute) // past the upload back-off
	h.node("a").Settle(context.Background())
	if exists, _ := WorldExists(context.Background(), h.st, "", w); !exists {
		t.Fatal("the world was not uploaded after the outage ended")
	}
}

func TestALostLeaseOrphansTheLocalCopy(t *testing.T) {
	h := newHarness(t)
	if _, err := h.publish("a", w, "p1"); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(StaleAfter + time.Minute) // a stopped renewing
	if _, err := h.publish("b", w, "p2"); err != nil {
		t.Fatalf("takeover by b: %v", err)
	}
	h.node("a").Settle(context.Background()) // a's renewal hits 412
	if got := h.orphans("a"); got != 1 {
		t.Fatalf("orphans = %d, want 1", got)
	}
}

// Review Focus 2.
func TestAPublishUnderANewTargetUnpublishesTheOldOneFirst(t *testing.T) {
	h := newHarness(t)
	old, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(old, "worlds/world/level.dat"), 5, h.now)
	fresh, err := h.publish("a", w, "p2")
	if err != nil {
		t.Fatal(err)
	}
	if _, bound := h.mnt["a"].binds[old]; bound {
		t.Fatal("the old target is still bound")
	}
	if _, err := os.Stat(filepath.Join(fresh, "worlds/world/level.dat")); err != nil {
		t.Fatalf("the world is not under the new target: %v", err)
	}
}

// Review Focus 4.
func TestAReusedKeyDoesNotInheritADeletedWorld(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())
	if err := MarkDeleted(context.Background(), h.st, "", w); err != nil {
		t.Fatal(err)
	}
	if err := (&Sweeper{Store: h.st, StaleAfter: StaleAfter, Log: logr.Discard()}).SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	again, _ := h.publish("a", w, "p2")
	if _, err := os.Stat(filepath.Join(again, "worlds/world/level.dat")); !os.IsNotExist(err) {
		t.Fatal("a deleted world's cache came back under its old key")
	}
}

func TestAPendingDeletionIsUnavailable(t *testing.T) {
	h := newHarness(t)
	if err := MarkDeleted(context.Background(), h.st, "", w); err != nil {
		t.Fatal(err)
	}
	if _, err := h.publish("a", w, "p1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestResumeContinuesAPendingUpload(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.st.SetOutage(errors.New("down"))
	h.unpublish("a", target)
	h.st.SetOutage(nil)

	restarted, err := NewNode(h.node("a").cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.Settle(context.Background())
	if exists, _ := WorldExists(context.Background(), h.st, "", w); !exists {
		t.Fatal("the restarted node agent did not finish the upload")
	}
}

func TestTwoQueuedSnapshotsUploadInOrderWithReferencesFilled(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/region/a.mca"), PackBelow+1, h.now)
	h.st.SetOutage(errors.New("down"))
	h.request(target, "1")
	h.waitFile(target, DoneFile)
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.request(target, "2")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.node("a").Settle(context.Background())
		if b, _ := os.ReadFile(filepath.Join(target, DoneFile)); strings.TrimSpace(string(b)) == "2" {
			break
		}
	}
	h.st.SetOutage(nil)
	h.now = h.now.Add(2 * time.Minute) // past the upload back-off
	h.node("a").Settle(context.Background())
	m, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", w))
	if err != nil || m.Generation != 2 || len(m.Files) != 2 {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	for _, f := range m.Files {
		if f.Object == "" {
			t.Fatalf("%s has no object", f.Path)
		}
	}
}

func TestAnUploadAfterALostLeaseIsNotWrittenAndTheWorldIsForgotten(t *testing.T) {
	h := newHarness(t)
	a := h.node("a")
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.st.SetOutage(errors.New("down"))
	h.unpublish("a", target)
	h.st.SetOutage(nil)
	h.now = h.now.Add(StaleAfter + time.Minute)
	if _, err := h.publish("b", w, "p2"); err != nil {
		t.Fatalf("takeover by b: %v", err)
	}

	a.kickUploads(context.Background()) // the upload runs before any renewal
	a.workers.Wait()
	if exists, _ := WorldExists(context.Background(), h.st, "", w); exists {
		t.Fatal("a wrote the manifest of a world it no longer held")
	}
	a.Settle(context.Background())
	if got := h.orphans("a"); got != 1 {
		t.Fatalf("orphans = %d, want 1", got)
	}
	if a.lookup(w) != nil {
		t.Fatal("the orphaned stopped world is still tracked")
	}
}

func TestAFailedFinalSnapshotKeepsTheLeaseUntilARetrySucceeds(t *testing.T) {
	h := newHarness(t)
	a := h.node("a")
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	bump := h.now
	copyHook = func(p string) {
		bump = bump.Add(time.Second)
		_ = os.Chtimes(p, bump, bump)
	}
	t.Cleanup(func() { copyHook = nil })
	if err := a.Unpublish(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	a.Settle(context.Background())
	if _, err := h.publish("b", w, "p2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("b took the world before its final snapshot: err = %v", err)
	}

	copyHook = nil
	h.now = h.now.Add(2 * time.Minute)
	a.Settle(context.Background())
	target2, err := h.publish("b", w, "p2")
	if err != nil {
		t.Fatalf("publish on b after the retry: %v", err)
	}
	h.waitFile(target2, ReadyFile)
	if _, err := os.Stat(filepath.Join(target2, "worlds/world/level.dat")); err != nil {
		t.Fatalf("the world did not travel: %v", err)
	}
}

func TestAStopDuringADownloadUploadsNothingAndReleases(t *testing.T) {
	h := newHarness(t)
	g := h.gate(func(op, key string) bool { return op == "get" && strings.Contains(key, PacksDir) }, false)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	g.armed.Store(true)
	target2, err := h.publish("b", w, "p2")
	if err != nil {
		t.Fatal(err)
	}
	<-g.held
	if err := h.node("b").Unpublish(context.Background(), target2); err != nil {
		t.Fatal(err)
	}
	h.node("b").Settle(context.Background())

	m, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", w))
	if err != nil || m.Generation != 1 {
		t.Fatalf("manifest = %+v, %v; a half-downloaded world was uploaded", m, err)
	}
	if l, _, err := ReadLease(context.Background(), h.st, WorldPrefix("", w)); err != nil || l.Node != "" {
		t.Fatalf("lease = %+v, %v; want released", l, err)
	}
	if _, err := os.Stat(filepath.Join(h.node("b").dataDir(w), ReadyFile)); !os.IsNotExist(err) {
		t.Fatal("a cancelled download marked the world ready")
	}
}

func TestASnapshotIsAnsweredWhileTheWorldsUploadHangs(t *testing.T) {
	h := newHarness(t)
	g := h.gate(func(op, key string) bool { return op == "put" && !strings.HasSuffix(key, LeaseName) }, false)
	a := h.node("a")
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.request(target, "1")
	a.pollRequests()

	g.armed.Store(true)
	settled := make(chan struct{})
	go func() { a.Settle(context.Background()); close(settled) }()
	<-g.held
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 6, h.now.Add(time.Second))
	h.request(target, "2")
	answered := make(chan struct{})
	go func() { a.pollRequests(); close(answered) }()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		close(g.open)
		t.Fatal("a snapshot request waited for the upload")
	}
	if b, _ := os.ReadFile(filepath.Join(target, DoneFile)); strings.TrimSpace(string(b)) != "2" {
		t.Fatalf("done = %q, want 2", b)
	}
	close(g.open)
	<-settled
	a.Settle(context.Background())
	m, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", w))
	if err != nil || m.Generation != 2 || len(m.Files) != 1 || m.Files[0].Size != 6 {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
}

func TestARepublishWaitsForTheRunningUploadAndKeepsItsCache(t *testing.T) {
	h := newHarness(t)
	g := h.gate(func(op, key string) bool { return op == "put" && strings.HasSuffix(key, ManifestName) }, true)
	ctx := context.Background()
	a := h.node("a")
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("a", target)
	a.Settle(ctx)
	target2, _ := h.publish("a", w, "p2")
	writeFile(t, filepath.Join(target2, "worlds/world/level.dat"), 6, h.now.Add(time.Second))
	h.unpublish("a", target2)

	g.armed.Store(true)
	settled := make(chan struct{})
	go func() { a.Settle(ctx); close(settled) }()
	<-g.held // generation 2 is in the store; the node has not seen the answer
	target3 := filepath.Join(t.TempDir(), "mount")
	published := make(chan error, 1)
	go func() {
		published <- a.Publish(ctx, PublishRequest{World: w, Keep: []string{"worlds/world"}, Target: target3, Pod: "p3"})
	}()
	var err error
	returned := false
	select {
	case err = <-published:
		returned = true
	case <-time.After(300 * time.Millisecond):
	}
	close(g.open)
	if !returned {
		err = <-published
	}
	<-settled
	if err != nil {
		t.Fatal(err)
	}
	if got := h.orphans("a"); got != 0 {
		t.Fatalf("orphans = %d; the node took its own upload for another writer's", got)
	}
	if _, err := os.Stat(filepath.Join(target3, ReadyFile)); err != nil {
		t.Fatalf("not ready at once: %v", err)
	}
	if downloadsCounted(a) != 0 {
		t.Fatal("a download ran for the node's own generation")
	}
}

func TestAnIdleCacheIsEvictedAndComesBackByDownload(t *testing.T) {
	h := newHarness(t)
	a := h.node("a")
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("a", target)
	a.Settle(context.Background())

	h.now = h.now.Add(25 * time.Hour)
	a.Settle(context.Background())
	if _, err := os.Stat(a.worldDir(w)); !os.IsNotExist(err) {
		t.Fatalf("the idle cache is still on disk: %v", err)
	}
	if a.lookup(w) != nil {
		t.Fatal("the evicted world is still tracked")
	}

	target2, err := h.publish("a", w, "p2")
	if err != nil {
		t.Fatal(err)
	}
	h.waitFile(target2, ReadyFile)
	if _, err := os.Stat(filepath.Join(target2, "worlds/world/level.dat")); err != nil {
		t.Fatalf("the world did not come back: %v", err)
	}
	if downloadsCounted(a) != 1 {
		t.Fatalf("downloads = %d, want 1", downloadsCounted(a))
	}
}

func TestEachSkipsAWorldRemovedDuringThePass(t *testing.T) {
	h := newHarness(t)
	a := h.node("a")
	a.state("ns/g/one")
	a.state("ns/g/two")
	visits := 0
	a.each(func(s *worldState) {
		visits++
		for _, other := range []string{"ns/g/one", "ns/g/two"} {
			if other != s.World {
				if o := a.lookup(other); o != nil {
					a.forget(o)
				}
			}
		}
	})
	if visits != 1 || len(a.worlds) != 1 {
		t.Fatalf("visits = %d, worlds = %v; a removed world came back", visits, a.worlds)
	}
}

func TestResumeReadsOnlyTheNodesOwnStateFiles(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	decoy := filepath.Join(target, "worlds/world/state.json")
	writeFile(t, decoy, 0, h.now)
	if err := os.WriteFile(decoy, []byte(`{"world":"other/g/k"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	restarted, _ := NewNode(h.node("a").cfg)
	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(restarted.worlds) != 1 || restarted.worlds[w] == nil {
		t.Fatalf("worlds after resume: %v", restarted.worlds)
	}
}

func TestResumeFailsADownloadTheRestartCutShort(t *testing.T) {
	h := newHarness(t)
	a := h.node("a")
	target := filepath.Join(t.TempDir(), "mount")
	if err := a.saveState(&worldState{World: w, Keep: []string{"worlds/world"}, Target: target, Incomplete: true}); err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewNode(a.cfg)
	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(restarted.dataDir(w), FailedFile)); err != nil {
		t.Fatalf("no failed file for an interrupted download: %v", err)
	}
}
