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
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

// committedButRefused commits the conditional puts match picks and answers
// 412, as the retry of a request whose first attempt went through does.
type committedButRefused struct {
	*MemStore
	armed   atomic.Bool
	match   func(key string) bool
	refused atomic.Int64
}

func (c *committedButRefused) Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	info, err := c.MemStore.Put(ctx, key, body, cond)
	if err == nil && c.armed.Load() && (cond.IfNoneMatch || cond.IfMatch != "") && c.match(key) {
		c.refused.Add(1)
		return ObjectInfo{}, ErrPrecondition
	}
	return info, err
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
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

func TestACacheRemovedBehindTheAgentsBackIsDownloadedAgain(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	if err := os.RemoveAll(h.node("a").worldDir(w)); err != nil {
		t.Fatal(err)
	}
	target2, err := h.publish("a", w, "p2")
	if err != nil {
		t.Fatal(err)
	}
	h.waitFile(target2, ReadyFile)
	if _, err := os.Stat(filepath.Join(target2, "worlds/world/level.dat")); err != nil {
		t.Fatalf("ready without the world: the state in memory vouched for a copy that is gone: %v", err)
	}
}

func TestACacheMissingAFileIsDownloadedAgain(t *testing.T) {
	h := newHarness(t)
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	writeFile(t, filepath.Join(target, "worlds/world/region/r.0.0.mca"), 7, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	if err := os.Remove(filepath.Join(h.node("a").dataDir(w), "worlds/world/region/r.0.0.mca")); err != nil {
		t.Fatal(err)
	}
	target2, err := h.publish("a", w, "p2")
	if err != nil {
		t.Fatal(err)
	}
	h.waitFile(target2, ReadyFile)
	if _, err := os.Stat(filepath.Join(target2, "worlds/world/region/r.0.0.mca")); err != nil {
		t.Fatalf("ready with a file of the world missing: %v", err)
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

func TestARetriedPutThatCommittedOrphansNothing(t *testing.T) {
	h := newHarness(t)
	st := &committedButRefused{MemStore: h.st, match: func(key string) bool {
		return strings.HasSuffix(key, LeaseName) || strings.HasSuffix(key, ManifestName)
	}}
	st.armed.Store(true)
	h.store = st
	a := h.node("a")
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatalf("publish when the lease put answered 412 after committing: %v", err)
	}
	for i, size := range []int{5, 6} {
		writeFile(t, filepath.Join(target, "worlds/world/level.dat"), size, h.now.Add(time.Duration(i)*time.Second))
		h.request(target, strconv.Itoa(i+1))
		a.Settle(context.Background())
		if got := h.orphans("a"); got != 0 {
			t.Fatalf("orphans = %d after puts that had committed", got)
		}
	}
	if st.refused.Load() == 0 {
		t.Fatal("the store refused nothing")
	}
	m, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", w))
	if err != nil || m.Generation != 2 {
		t.Fatalf("manifest = %+v, %v; want generation 2", m, err)
	}
	if _, err := h.publish("b", w, "p2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("b took a world a still holds: err = %v", err)
	}
}

func TestAnUploadCommittedBeforeACrashIsAdopted(t *testing.T) {
	for _, first := range []bool{true, false} {
		t.Run(map[bool]string{true: "first generation", false: "later generation"}[first], func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			a := h.node("a")
			target, _ := h.publish("a", w, "p1")
			writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
			if !first {
				h.request(target, "1")
				a.Settle(ctx)
			}
			writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 6, h.now.Add(time.Second))
			h.request(target, "2")
			a.pollRequests()
			crashed := t.TempDir()
			copyTree(t, a.worldDir(w), crashed)
			a.Settle(ctx)
			want, _, err := ReadManifest(ctx, h.st, WorldPrefix("", w))
			if err != nil {
				t.Fatal(err)
			}

			// The node died after the manifest put and before it saved the state.
			if err := os.RemoveAll(a.worldDir(w)); err != nil {
				t.Fatal(err)
			}
			copyTree(t, crashed, a.worldDir(w))
			restarted, _ := NewNode(a.cfg)
			if err := restarted.Resume(ctx); err != nil {
				t.Fatal(err)
			}
			restarted.Settle(ctx)
			if got := h.orphans("a"); got != 0 {
				t.Fatalf("orphans = %d; the node's own upload was taken for another writer's", got)
			}
			s := restarted.lookup(w)
			if s == nil || s.WorldID != want.WorldID || s.Generation != want.Generation || len(s.Pending) != 0 {
				t.Fatalf("state = %+v, want generation %d of %s adopted", s, want.Generation, want.WorldID)
			}

			writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 7, h.now.Add(2*time.Second))
			h.request(target, "3")
			restarted.Settle(ctx)
			m, _, err := ReadManifest(ctx, h.st, WorldPrefix("", w))
			if err != nil || m.Generation != want.Generation+1 {
				t.Fatalf("manifest = %+v, %v; the next upload failed", m, err)
			}
			if keys, _ := h.st.List(ctx, WorldPrefix("", w)+PacksDir); len(keys) != 1 {
				t.Fatalf("packs = %v, want only the current one", keys)
			}
		})
	}
}

func TestAHungStoreCallHoldsUpNoOtherWorld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const other = "ns/g/other"
	g := h.gate(func(op, key string) bool { return op == "put" && key == WorldPrefix("", w)+LeaseName }, false)
	a := h.node("a")
	if _, err := h.publish("a", w, "p1"); err != nil {
		t.Fatal(err)
	}
	otherTarget := filepath.Join(t.TempDir(), "mount")
	if err := a.Publish(ctx, PublishRequest{World: other, Keep: []string{"worlds/world"}, Target: otherTarget, Pod: "p2"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(otherTarget, "worlds/world/level.dat"), 5, h.now)

	g.armed.Store(true)
	renewed := make(chan struct{})
	go func() { a.renewAll(ctx); close(renewed) }()
	<-g.held // w's renewal hangs while it holds w's lock
	defer func() { close(g.open); <-renewed; a.workers.Wait() }()

	h.request(otherTarget, "1")
	answered := make(chan struct{})
	go func() { a.pollRequests(); a.kickUploads(ctx); close(answered) }()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("another world's snapshot request waited for the hung call")
	}
	if b, _ := os.ReadFile(filepath.Join(otherTarget, DoneFile)); strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("done = %q, want 1", b)
	}

	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	published := make(chan error, 1)
	go func() {
		published <- a.Publish(short, PublishRequest{World: w, Keep: []string{"worlds/world"}, Target: filepath.Join(t.TempDir(), "mount"), Pod: "p3"})
	}()
	select {
	case err := <-published:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a publish waited for the world's lock past its context")
	}
}

func TestAHungStoreCallTimesOut(t *testing.T) {
	for name, match := range map[string]func(op, key string) bool{
		"lease":  func(op, key string) bool { return op == "put" && strings.HasSuffix(key, LeaseName) },
		"upload": func(op, key string) bool { return op == "put" && !strings.HasSuffix(key, LeaseName) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			g := h.gate(match, false)
			a := h.node("a")
			a.callTimeout, a.uploadTimeout = 20*time.Millisecond, 20*time.Millisecond
			target, _ := h.publish("a", w, "p1")
			writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
			h.request(target, "1")
			a.pollRequests()

			g.armed.Store(true)
			settled := make(chan struct{})
			go func() { a.Settle(context.Background()); close(settled) }()
			select {
			case <-settled:
			case <-time.After(5 * time.Second):
				close(g.open)
				<-settled
				t.Fatal("a hung store call never timed out")
			}
			unlock := a.lock(w)
			s := a.lookup(w)
			stuck, pending, retry := s.uploading != nil, len(s.Pending), s.retryAt
			unlock()
			if stuck || pending != 1 || retry.IsZero() {
				t.Fatalf("uploading = %v, pending = %d, retryAt = %v; want an idle world backing off", stuck, pending, retry)
			}
		})
	}
}

func TestPendingSnapshotsStayCountedWhileTheStoreHangs(t *testing.T) {
	h := newHarness(t)
	g := h.gate(func(op, key string) bool { return op == "put" && key == WorldPrefix("", w)+LeaseName }, false)
	a := h.node("a")
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	h.unpublish("a", target)

	ctx := context.Background()
	g.armed.Store(true)
	a.kickUploads(ctx)
	<-g.held // the lease check before the upload hangs while it holds the world's lock
	defer func() { close(g.open); a.workers.Wait() }()

	a.kickUploads(ctx)
	if got := testutil.ToFloat64(pendingUploads); got != 1 {
		t.Fatalf("pending snapshots = %v while the store hangs, want 1", got)
	}
}

func TestAPublishAfterARebootBindsTheSameTargetAgain(t *testing.T) {
	h := newHarness(t)
	a := h.node("a")
	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	if err := h.mnt["a"].Unmount(target); err != nil { // the reboot dropped the bind mount
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	restarted, _ := NewNode(a.cfg)
	if err := restarted.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Publish(context.Background(), PublishRequest{World: w, Keep: []string{"worlds/world"}, Target: target, Pod: "p1"}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{ReadyFile, "worlds/world/level.dat"} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Fatalf("%s is not under the target after the republish: %v", rel, err)
		}
	}
}

func (h *harness) publishKeep(id, pod string, keep, replace []string) string {
	h.t.Helper()
	target := filepath.Join(h.t.TempDir(), "mount")
	if err := h.node(id).Publish(context.Background(), PublishRequest{World: w, Keep: keep, Replace: replace, Target: target, Pod: pod}); err != nil {
		h.t.Fatal(err)
	}
	return target
}

func TestTheReleaseKeepsATopLevelWorldThatKeepMisses(t *testing.T) {
	h := newHarness(t)
	target := h.publishKeep("a", "p1", []string{"world"}, nil)
	writeFile(t, filepath.Join(target, "world/level.dat"), 5, h.now)
	writeFile(t, filepath.Join(target, "world_nether/DIM-1/region/r.0.0.mca"), 5, h.now)
	writeFile(t, filepath.Join(target, "logs/latest.log"), 5, h.now)
	away, _ := outside(t, "level.dat", []byte("not the pod's"))
	symlink(t, away, filepath.Join(target, "elsewhere"))
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	data := h.node("a").dataDir(w)
	if _, err := os.Stat(filepath.Join(data, "world_nether/DIM-1/region/r.0.0.mca")); err != nil {
		t.Fatalf("the release deleted a world keep does not list: %v", err)
	}
	for _, gone := range []string{"logs", "elsewhere"} {
		if _, err := os.Lstat(filepath.Join(data, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the release: %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(away, "level.dat")); err != nil {
		t.Fatalf("the release reached through a symlink: %v", err)
	}
	if l, _, err := ReadLease(context.Background(), h.st, WorldPrefix("", w)); err != nil || l.Node != "" {
		t.Fatalf("lease = %+v, %v; want it released", l, err)
	}
}

func TestTheReleaseDropsAWorldThatReplaceOwns(t *testing.T) {
	h := newHarness(t)
	target := h.publishKeep("a", "p1", []string{"plugins/Example/data"}, []string{"lobby"})
	writeFile(t, filepath.Join(target, "lobby/level.dat"), 5, h.now)
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())
	if _, err := os.Lstat(filepath.Join(h.node("a").dataDir(w), "lobby")); !os.IsNotExist(err) {
		t.Fatalf("a world replace owns survived the release: %v", err)
	}
}

func TestASnapshotRefusesAWorldOutsideKeep(t *testing.T) {
	h := newHarness(t)
	target := h.publishKeep("a", "p1", []string{"worlds/world"}, nil)
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	writeFile(t, filepath.Join(target, "worlds/world_nether/region/r.0.0.mca"), 5, h.now)
	h.request(target, "1")
	want := "failed 1 spec.storage.keep does not keep worlds/world_nether, which holds a world"
	if got := strings.TrimSpace(h.waitFile(target, DoneFile)); got != want {
		t.Fatalf("done = %q, want %q", got, want)
	}
	if exists, _ := WorldExists(context.Background(), h.st, "", w); exists {
		t.Fatal("a refused snapshot was uploaded")
	}

	h.unpublish("a", target)
	h.node("a").Settle(context.Background())
	m, _, err := ReadManifest(context.Background(), h.st, WorldPrefix("", w))
	if err != nil || len(m.Files) != 1 || m.Files[0].Path != "worlds/world/level.dat" {
		t.Fatalf("the final snapshot did not save what keep holds: %+v, %v", m, err)
	}
	if _, err := os.Stat(filepath.Join(h.node("a").dataDir(w), "worlds/world_nether/region/r.0.0.mca")); err != nil {
		t.Fatalf("the world outside keep is gone from the node: %v", err)
	}
}

func TestASnapshotTakesAWorldThatReplaceOwns(t *testing.T) {
	h := newHarness(t)
	target := h.publishKeep("a", "p1", []string{"worlds/world"}, []string{"worlds/lobby"})
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	writeFile(t, filepath.Join(target, "worlds/lobby/level.dat"), 5, h.now)
	h.request(target, "1")
	if got := strings.TrimSpace(h.waitFile(target, DoneFile)); got != "1" {
		t.Fatalf("done = %q, want 1", got)
	}
}

func TestTheWorldCheckDoesNotFollowSymlinks(t *testing.T) {
	h := newHarness(t)
	target := h.publishKeep("a", "p1", []string{"worlds/world"}, nil)
	away, _ := outside(t, "level.dat", []byte("x"))
	symlink(t, away, filepath.Join(target, "worlds/link"))
	h.request(target, "1")
	if got := strings.TrimSpace(h.waitFile(target, DoneFile)); got != "1" {
		t.Fatalf("done = %q, want 1", got)
	}
}
