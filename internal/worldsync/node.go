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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"github.com/spawnery/spawnery/internal/prune"
)

const (
	ReadyFile   = prune.ControlDir + "/ready"
	FailedFile  = prune.ControlDir + "/failed"
	RequestFile = prune.ControlDir + "/snapshot.request"
	DoneFile    = prune.ControlDir + "/snapshot.done"
)

var ErrUnavailable = errors.New("worldsync: the world cannot be published on this node now")

type Mounter interface {
	Bind(source, target string) error
	Unmount(target string) error
}

type Config struct {
	Root, NodeID, Base string
	Store              Store
	Mounter            Mounter
	StaleAfter         time.Duration
	RenewEvery         time.Duration
	PollEvery          time.Duration
	EvictAfter         time.Duration
	Parallel           int
	MinFree            float64
	Clock              func() time.Time
	Log                logr.Logger
}

type PublishRequest struct {
	World  string
	Keep   []string
	Target string
	Pod    string
}

type download struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type uploadJob struct {
	seq      int64
	dir      string
	prev     *Manifest
	prevETag string
}

// Node keeps the worlds on one node. A world's state is touched only under
// that world's lock; n.mu guards the maps and is never held while waiting
// for a world's lock.
type Node struct {
	cfg       Config
	mu        sync.Mutex
	worlds    map[string]*worldState
	targets   map[string]string
	locks     map[string]*sync.Mutex
	workers   sync.WaitGroup
	downloads atomic.Int64
}

func strconvI(v int64) string { return strconv.FormatInt(v, 10) }

func NewNode(cfg Config) (*Node, error) {
	if cfg.Root == "" || cfg.NodeID == "" || cfg.Store == nil || cfg.Mounter == nil {
		return nil, errors.New("worldsync: Root, NodeID, Store and Mounter are required")
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = StaleAfter
	}
	if cfg.RenewEvery <= 0 {
		cfg.RenewEvery = 30 * time.Second
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 500 * time.Millisecond
	}
	if cfg.EvictAfter <= 0 {
		cfg.EvictAfter = 24 * time.Hour
	}
	if cfg.Parallel <= 0 {
		cfg.Parallel = 32
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root, "worlds"), 0o755); err != nil {
		return nil, err
	}
	return &Node{cfg: cfg, worlds: map[string]*worldState{}, targets: map[string]string{}, locks: map[string]*sync.Mutex{}}, nil
}

func (n *Node) lock(world string) func() {
	n.mu.Lock()
	l, ok := n.locks[world]
	if !ok {
		l = &sync.Mutex{}
		n.locks[world] = l
	}
	n.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (n *Node) lookup(world string) *worldState {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.worlds[world]
}

func (n *Node) state(world string) *worldState {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, ok := n.worlds[world]
	if !ok {
		s = &worldState{World: world}
		n.worlds[world] = s
	}
	return s
}

func (n *Node) setTarget(s *worldState, target string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if s.Target != "" && n.targets[s.Target] == s.World {
		delete(n.targets, s.Target)
	}
	s.Target = target
	if target != "" {
		n.targets[target] = s.World
	}
}

func (n *Node) forget(s *worldState) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.worlds[s.World] == s {
		delete(n.worlds, s.World)
	}
	if s.Target != "" && n.targets[s.Target] == s.World {
		delete(n.targets, s.Target)
	}
}

func (n *Node) save(s *worldState) {
	if err := n.saveState(s); err != nil {
		n.cfg.Log.Error(err, "could not save the world state", "world", s.World)
	}
}

func (n *Node) prefix(world string) string { return WorldPrefix(n.cfg.Base, world) }

func (n *Node) lease(s *worldState) Lease {
	return Lease{Node: n.cfg.NodeID, Pod: s.Pod, RenewedAt: n.cfg.Clock()}
}

func checkWorld(world string) error {
	parts := strings.Split(world, "/")
	ok := len(parts) == 3
	for _, p := range parts {
		ok = ok && p != "" && p != "." && p != ".."
	}
	if !ok {
		return fmt.Errorf("worldsync: world %q is not <namespace>/<group>/<key>", world)
	}
	return nil
}

func notMounted(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, fs.ErrNotExist)
}

func (n *Node) Resume(ctx context.Context) error {
	states, err := n.loadStates()
	if err != nil {
		return err
	}
	for _, s := range states {
		if s.Target != "" {
			n.resumeControl(s)
		}
		n.mu.Lock()
		n.worlds[s.World] = s
		if s.Target != "" {
			n.targets[s.Target] = s.World
		}
		n.mu.Unlock()
	}
	return nil
}

// resumeControl answers a pod that waits for a download this process no
// longer runs.
func (n *Node) resumeControl(s *worldState) {
	if s.Incomplete {
		if err := n.control(s, FailedFile, "the node agent restarted during the download"); err != nil {
			n.cfg.Log.Error(err, "could not fail an interrupted download", "world", s.World)
		}
		return
	}
	for _, f := range []string{ReadyFile, FailedFile} {
		if _, err := os.Stat(filepath.Join(n.dataDir(s.World), f)); err == nil {
			return
		}
	}
	if err := n.control(s, ReadyFile, ""); err != nil {
		n.cfg.Log.Error(err, "could not mark a resumed world ready", "world", s.World)
	}
}

func (n *Node) control(s *worldState, rel, content string) error {
	return writeFileAtomic(filepath.Join(n.dataOf(s), rel), content)
}

func writeFileAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (n *Node) resetControl(s *worldState) error {
	dir := filepath.Join(n.dataDir(s.World), prune.ControlDir)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	s.LastRequest = 0
	return os.MkdirAll(dir, 0o755)
}

func (n *Node) markReady(s *worldState) error {
	if err := n.resetControl(s); err != nil {
		return err
	}
	return n.control(s, ReadyFile, "")
}

func (n *Node) wipeKept(s *worldState) error {
	keep, err := prune.ParseKeep(s.Keep)
	if err != nil {
		return err
	}
	files, err := Scan(n.dataDir(s.World), keep)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, f := range files {
		if err := os.Remove(filepath.Join(n.dataDir(s.World), f.Path)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (n *Node) discardLocal(s *worldState) error {
	if err := n.wipeKept(s); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(n.worldDir(s.World), "snapshots")); err != nil {
		return err
	}
	s.forgetContent()
	return nil
}

// lockIdle locks the world once no upload of it is in flight: the manifest
// may already hold that upload's generation while the state does not yet.
func (n *Node) lockIdle(ctx context.Context, world string) (*worldState, func(), error) {
	for {
		unlock := n.lock(world)
		s := n.state(world)
		if s.uploading == nil {
			return s, unlock, nil
		}
		running := s.uploading
		unlock()
		select {
		case <-running:
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("%w: an upload of the world is still running: %v", ErrUnavailable, ctx.Err())
		}
	}
}

func (n *Node) Publish(ctx context.Context, req PublishRequest) error {
	if err := checkWorld(req.World); err != nil {
		return err
	}
	if _, err := prune.ParseKeep(req.Keep); err != nil {
		return err
	}
	s, unlock, err := n.lockIdle(ctx, req.World)
	if err != nil {
		return err
	}
	defer unlock()

	if s.Target == req.Target {
		return nil
	}
	if s.Target != "" {
		// Review Focus 2: the old pod's teardown has not reached us yet.
		if err := n.unpublishLocked(s); err != nil {
			return err
		}
		s = n.state(req.World)
	}

	if pending, err := DeletionPending(ctx, n.cfg.Store, n.cfg.Base, req.World); err != nil {
		return fmt.Errorf("%w: check for a pending deletion: %v", ErrUnavailable, err)
	} else if pending {
		return fmt.Errorf("%w: the world is being deleted", ErrUnavailable)
	}

	s.Keep, s.Pod = req.Keep, req.Pod
	etag, err := TakeLease(ctx, n.cfg.Store, n.prefix(req.World), n.lease(s), n.cfg.StaleAfter)
	var held *HeldError
	if errors.As(err, &held) {
		leaseConflicts.Inc()
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err != nil {
		return fmt.Errorf("%w: take the lease: %v", ErrUnavailable, err)
	}
	s.LeaseETag = etag

	fetch, metag, err := n.settleContent(ctx, s)
	if err == nil {
		err = n.bind(s, req.Target)
	}
	if err != nil {
		n.save(s)
		return err
	}
	s.FinalPending = false
	s.LastUsed = n.cfg.Clock()
	if fetch != nil {
		n.startDownload(s, *fetch, metag)
	}
	return n.saveState(s)
}

// settleContent brings the local copy in line with the manifest. It returns
// the manifest to download when the copy is not current.
func (n *Node) settleContent(ctx context.Context, s *worldState) (*Manifest, string, error) {
	m, etag, err := ReadManifest(ctx, n.cfg.Store, n.prefix(s.World))
	switch {
	case errors.Is(err, ErrNotFound):
		// Review Focus 4: a copy of a world that no longer exists.
		if s.WorldID != "" || s.Incomplete {
			if err := n.discardLocal(s); err != nil {
				return nil, "", err
			}
		}
		return nil, "", n.markReady(s)
	case err != nil:
		return nil, "", fmt.Errorf("%w: read the manifest: %v", ErrUnavailable, err)
	case !s.Incomplete && m.WorldID == s.WorldID && m.Generation == s.Generation:
		s.ManifestETag = etag
		return nil, "", n.markReady(s)
	}
	if len(s.Pending) > 0 || s.FinalPending {
		dst, err := n.moveAside(s.World)
		if err != nil {
			return nil, "", err
		}
		orphans.Inc()
		n.cfg.Log.Info("orphaned the local copy", "world", s.World, "reason", "the manifest moved past snapshots this node had not uploaded", "path", dst)
		s.forgetContent()
	} else if err := n.discardLocal(s); err != nil {
		return nil, "", err
	}
	return &m, etag, n.resetControl(s)
}

func (n *Node) bind(s *worldState, target string) error {
	if err := os.MkdirAll(n.dataDir(s.World), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := n.cfg.Mounter.Bind(n.dataDir(s.World), target); err != nil {
		return err
	}
	n.setTarget(s, target)
	return nil
}

// startDownload runs in the background; the container starts meanwhile and
// the agent waits for ready or failed.
func (n *Node) startDownload(s *worldState, m Manifest, etag string) {
	s.forgetContent()
	s.Incomplete = true
	ctx, cancel := context.WithCancel(context.Background())
	d := &download{cancel: cancel, done: make(chan struct{})}
	s.download = d
	n.downloads.Add(1)
	world, prefix, dir := s.World, n.prefix(s.World), n.dataDir(s.World)
	go func() {
		started := time.Now()
		err := Download(ctx, n.cfg.Store, prefix, dir, m, n.cfg.Parallel)
		close(d.done)
		unlock := n.lock(world)
		defer unlock()
		if n.lookup(world) != s || s.download != d {
			return
		}
		s.download = nil
		cancel()
		if err != nil {
			n.cfg.Log.Error(err, "download failed", "world", world)
			if err := n.control(s, FailedFile, err.Error()); err != nil {
				n.cfg.Log.Error(err, "could not report the failed download", "world", world)
			}
			return
		}
		downloadSeconds.Observe(time.Since(started).Seconds())
		s.WorldID, s.Generation, s.ManifestETag, s.Files, s.Incomplete = m.WorldID, m.Generation, etag, m.Files, false
		if err := n.saveState(s); err != nil {
			_ = n.control(s, FailedFile, err.Error())
			return
		}
		if err := n.control(s, ReadyFile, ""); err != nil {
			n.cfg.Log.Error(err, "could not mark the world ready", "world", world)
		}
	}()
}

// stopDownload cancels a running download and waits until it no longer
// writes; the download only takes the world's lock after it stopped writing.
func (n *Node) stopDownload(s *worldState) {
	if d := s.download; d != nil {
		d.cancel()
		<-d.done
		s.download = nil
	}
}

func (n *Node) Unpublish(ctx context.Context, target string) error {
	n.mu.Lock()
	world, ok := n.targets[target]
	n.mu.Unlock()
	if ok {
		unlock := n.lock(world)
		defer unlock()
		if s := n.lookup(world); s != nil && s.Target == target {
			return n.unpublishLocked(s)
		}
	}
	if err := n.cfg.Mounter.Unmount(target); err != nil && !notMounted(err) {
		return err
	}
	return nil
}

// unpublishLocked unbinds and takes the final snapshot; the upload follows
// in the background.
func (n *Node) unpublishLocked(s *worldState) error {
	if err := n.cfg.Mounter.Unmount(s.Target); err != nil && !notMounted(err) {
		return err
	}
	if s.lost {
		n.forget(s)
		return nil
	}
	n.stopDownload(s)
	n.setTarget(s, "")
	if s.LeaseETag != "" && !s.Incomplete {
		if err := n.snapshot(s); err != nil {
			n.cfg.Log.Error(err, "final snapshot failed; retrying in the background", "world", s.World)
			s.FinalPending = true
		}
	}
	s.LastUsed = n.cfg.Clock()
	return n.saveState(s)
}

func (n *Node) snapshot(s *worldState) error {
	keep, err := prune.ParseKeep(s.Keep)
	if err != nil {
		return err
	}
	base := s.Files
	if len(s.Pending) > 0 {
		// Compare against the newest queued snapshot, not the uploaded one,
		// so a file changed twice is copied for each change.
		last, err := ReadSnap(n.snapDir(s.World, s.Pending[len(s.Pending)-1]))
		if err != nil {
			return err
		}
		base = make([]FileEntry, 0, len(last.Files))
		for _, f := range last.Files {
			base = append(base, f.FileEntry)
		}
	}
	s.NextSeq++
	seq := s.NextSeq
	dir := n.snapDir(s.World, seq)
	_ = os.RemoveAll(dir)
	if _, err := TakeSnapshot(n.dataDir(s.World), dir, keep, base, seq); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	s.Pending = append(s.Pending, seq)
	return nil
}

func (n *Node) Run(ctx context.Context) {
	var loops sync.WaitGroup
	every := func(d time.Duration, fn func()) {
		loops.Add(1)
		go func() {
			defer loops.Done()
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fn()
				}
			}
		}()
	}
	every(n.cfg.PollEvery, func() {
		n.pollRequests()
		n.kickUploads(ctx)
	})
	every(n.cfg.RenewEvery, func() { n.renewAll(ctx) })
	every(time.Minute, n.evict)
	loops.Wait()
	n.workers.Wait()
}

// Settle runs one pass of every loop and waits for the uploads it started,
// for tests.
func (n *Node) Settle(ctx context.Context) {
	n.pollRequests()
	n.renewAll(ctx)
	n.kickUploads(ctx)
	n.workers.Wait()
	n.evict()
}

func (n *Node) each(fn func(*worldState)) {
	n.mu.Lock()
	worlds := make([]string, 0, len(n.worlds))
	for w := range n.worlds {
		worlds = append(worlds, w)
	}
	n.mu.Unlock()
	for _, w := range worlds {
		unlock := n.lock(w)
		if s := n.lookup(w); s != nil {
			fn(s)
		}
		unlock()
	}
}

func (n *Node) pollRequests() {
	n.each(func(s *worldState) {
		if s.Target == "" {
			return
		}
		b, err := os.ReadFile(filepath.Join(n.dataOf(s), RequestFile))
		if err != nil {
			return
		}
		seq, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || seq <= s.LastRequest {
			return
		}
		s.LastRequest = seq
		switch {
		case s.lost:
			err = errors.New("this node lost the world's lease and no longer saves it")
		case s.Incomplete:
			err = errors.New("the world is still downloading")
		default:
			err = n.snapshot(s)
		}
		answer := strconvI(seq)
		if err != nil {
			n.cfg.Log.Error(err, "snapshot failed", "world", s.World, "seq", seq)
			answer = fmt.Sprintf("failed %d %v", seq, err)
		}
		n.save(s)
		if err := n.control(s, DoneFile, answer); err != nil {
			n.cfg.Log.Error(err, "could not answer a snapshot request", "world", s.World)
		}
	})
}

func (n *Node) hasWork(s *worldState) bool {
	if s.lost {
		return false
	}
	return len(s.Pending) > 0 || s.Target == "" && (s.FinalPending || s.LeaseETag != "")
}

// kickUploads starts a worker for every world with something to upload or
// release; a world's upload runs outside its lock so that it holds up
// neither snapshot requests nor lease renewals.
func (n *Node) kickUploads(ctx context.Context) {
	pending := 0
	n.each(func(s *worldState) {
		pending += len(s.Pending)
		if s.working || !n.hasWork(s) || n.cfg.Clock().Before(s.retryAt) {
			return
		}
		s.working = true
		n.workers.Add(1)
		go n.work(ctx, s)
	})
	pendingUploads.Set(float64(pending))
}

func (n *Node) work(ctx context.Context, s *worldState) {
	defer n.workers.Done()
	for {
		unlock := n.lock(s.World)
		job := n.nextUpload(ctx, s)
		if job == nil {
			s.working = false
			unlock()
			return
		}
		running := make(chan struct{})
		s.uploading = running
		unlock()

		m, etag, err := UploadSnapshot(ctx, n.cfg.Store, n.prefix(s.World), job.dir, job.prev, job.prevETag, NewWorldID)

		unlock = n.lock(s.World)
		s.uploading = nil
		close(running)
		more := n.finishUpload(s, job, m, etag, err)
		if !more {
			s.working = false
		}
		unlock()
		if !more {
			return
		}
	}
}

// nextUpload does the steps that need no upload (a final snapshot owed, the
// release once nothing is left) and returns the next snapshot to upload.
func (n *Node) nextUpload(ctx context.Context, s *worldState) *uploadJob {
	if n.lookup(s.World) != s || s.lost {
		return nil
	}
	if s.Target == "" && s.FinalPending {
		if err := n.snapshot(s); err != nil {
			n.retryLater(s, "final snapshot failed", err)
			return nil
		}
		s.FinalPending = false
		n.save(s)
	}
	if len(s.Pending) == 0 {
		if s.Target == "" && s.LeaseETag != "" {
			n.release(ctx, s)
		}
		return nil
	}
	if err := n.confirmLease(ctx, s); errors.Is(err, ErrLeaseLost) {
		n.orphan(s, "another node took the lease")
		return nil
	} else if err != nil {
		n.retryLater(s, "lease check before the upload failed", err)
		return nil
	}
	job, err := n.prepareUpload(s)
	if err != nil {
		n.retryLater(s, "could not prepare the upload", err)
		return nil
	}
	return job
}

// confirmLease renews the lease right before an upload, so a node that was
// cut off for longer than StaleAfter learns it lost the world before it
// writes a manifest, not after.
func (n *Node) confirmLease(ctx context.Context, s *worldState) error {
	var etag string
	var err error
	if s.LeaseETag == "" {
		etag, err = TakeLease(ctx, n.cfg.Store, n.prefix(s.World), n.lease(s), n.cfg.StaleAfter)
		var held *HeldError
		if errors.As(err, &held) {
			return ErrLeaseLost
		}
	} else {
		etag, err = RenewLease(ctx, n.cfg.Store, n.prefix(s.World), n.lease(s), s.LeaseETag)
	}
	if err != nil {
		return err
	}
	s.LeaseETag = etag
	n.save(s)
	return nil
}

func (n *Node) prepareUpload(s *worldState) (*uploadJob, error) {
	seq := s.Pending[0]
	dir := n.snapDir(s.World, seq)
	snap, err := ReadSnap(dir)
	if err != nil {
		return nil, err
	}
	fillReferences(&snap, s.Files)
	if err := writeJSON(filepath.Join(dir, snapFile), snap); err != nil {
		return nil, err
	}
	job := &uploadJob{seq: seq, dir: dir}
	if s.WorldID != "" {
		job.prev = &Manifest{WorldID: s.WorldID, Generation: s.Generation, Files: s.Files}
		job.prevETag = s.ManifestETag
	}
	return job, nil
}

// finishUpload records the upload's outcome and reports whether the worker
// should go on.
func (n *Node) finishUpload(s *worldState, job *uploadJob, m Manifest, etag string, err error) bool {
	if n.lookup(s.World) != s || s.lost || len(s.Pending) == 0 || s.Pending[0] != job.seq {
		return false
	}
	if errors.Is(err, ErrConflict) {
		n.orphan(s, err.Error())
		return false
	}
	if err != nil {
		n.retryLater(s, "upload failed", err)
		return false
	}
	s.WorldID, s.Generation, s.ManifestETag, s.Files = m.WorldID, m.Generation, etag, m.Files
	s.Pending = s.Pending[1:]
	s.retryAt, s.retryDelay = time.Time{}, 0
	_ = os.RemoveAll(job.dir)
	n.save(s)
	return true
}

// fillReferences names the object of every entry a queued snapshot carried
// over from an earlier queued one, which had no object yet when it was taken.
func fillReferences(snap *Snap, uploaded []FileEntry) {
	byPath := make(map[string]FileEntry, len(uploaded))
	for _, f := range uploaded {
		byPath[f.Path] = f
	}
	for i, f := range snap.Files {
		if f.Copied || f.Object != "" {
			continue
		}
		if u, ok := byPath[f.Path]; ok && u.Size == f.Size && u.MTime == f.MTime {
			snap.Files[i].Object = u.Object
		}
	}
}

// retryLater backs off from 1 s, doubling, to at most 1 min.
func (n *Node) retryLater(s *worldState, msg string, err error) {
	s.retryDelay = min(max(2*s.retryDelay, time.Second), time.Minute)
	s.retryAt = n.cfg.Clock().Add(s.retryDelay)
	n.cfg.Log.Error(err, msg+"; retrying", "world", s.World, "in", s.retryDelay)
}

func (n *Node) release(ctx context.Context, s *worldState) {
	n.dropScratch(s)
	err := ReleaseLease(ctx, n.cfg.Store, n.prefix(s.World), s.LeaseETag)
	if err != nil && !errors.Is(err, ErrLeaseLost) {
		n.retryLater(s, "lease release failed", err)
		return
	}
	s.LeaseETag = ""
	s.retryAt, s.retryDelay = time.Time{}, 0
	n.save(s)
}

// dropScratch deletes what keep does not hold; the next start's prune would.
func (n *Node) dropScratch(s *worldState) {
	keep, err := prune.ParseKeep(s.Keep)
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(n.dataDir(s.World))
	for _, e := range entries {
		if e.Name() == prune.ControlDir || keep.Holds(e.Name()) || keep.Toward(e.Name()) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(n.dataDir(s.World), e.Name()))
	}
}

func (n *Node) renewAll(ctx context.Context) {
	n.each(func(s *worldState) {
		if s.LeaseETag == "" || s.lost {
			return
		}
		etag, err := RenewLease(ctx, n.cfg.Store, n.prefix(s.World), n.lease(s), s.LeaseETag)
		switch {
		case errors.Is(err, ErrLeaseLost):
			n.orphan(s, "another node took the lease")
		case err != nil:
			n.cfg.Log.Error(err, "lease renewal failed; retrying", "world", s.World)
		default:
			s.LeaseETag = etag
			n.save(s)
		}
	})
}

// orphan moves the world's directory aside for manual recovery. A stopped
// world is forgotten; a running one stays known by its target, so that its
// unpublish finds it, but is never saved again.
func (n *Node) orphan(s *worldState, why string) {
	n.stopDownload(s)
	dst, err := n.moveAside(s.World)
	if err != nil {
		n.cfg.Log.Error(err, "could not move an orphaned world aside", "world", s.World)
	}
	orphans.Inc()
	n.cfg.Log.Info("orphaned the local copy", "world", s.World, "reason", why, "path", dst)
	n.forget(s)
	if s.Target == "" {
		return
	}
	n.mu.Lock()
	n.worlds[s.World] = &worldState{World: s.World, Target: s.Target, LastRequest: s.LastRequest, lost: true, lostDir: dst}
	n.targets[s.Target] = s.World
	n.mu.Unlock()
}

func (n *Node) moveAside(world string) (string, error) {
	src := n.worldDir(world)
	if _, err := os.Lstat(src); os.IsNotExist(err) {
		return "", nil
	}
	base := filepath.Join(n.cfg.Root, "orphans", strings.ReplaceAll(world, "/", "_")+"_"+strconvI(n.cfg.Clock().Unix()))
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return "", err
	}
	dst := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(dst); os.IsNotExist(err) {
			break
		}
		dst = base + "-" + strconv.Itoa(i)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func idle(s *worldState) bool {
	return s.Target == "" && len(s.Pending) == 0 && !s.FinalPending && s.LeaseETag == "" &&
		!s.working && s.download == nil && !s.lost
}

// evict drops caches idle for EvictAfter, then, oldest first, as many more
// as it takes to get MinFree back.
func (n *Node) evict() {
	now := n.cfg.Clock()
	type spare struct {
		world string
		used  time.Time
	}
	var spares []spare
	n.each(func(s *worldState) {
		if !idle(s) {
			return
		}
		if now.Sub(s.LastUsed) >= n.cfg.EvictAfter {
			n.drop(s)
			return
		}
		spares = append(spares, spare{s.World, s.LastUsed})
	})
	sort.Slice(spares, func(i, j int) bool { return spares[i].used.Before(spares[j].used) })
	for _, c := range spares {
		if n.freeFraction() >= n.cfg.MinFree {
			return
		}
		unlock := n.lock(c.world)
		if s := n.lookup(c.world); s != nil && idle(s) {
			n.drop(s)
		}
		unlock()
	}
}

func (n *Node) drop(s *worldState) {
	if err := os.RemoveAll(n.worldDir(s.World)); err != nil {
		n.cfg.Log.Error(err, "could not evict a cached world", "world", s.World)
		return
	}
	n.forget(s)
}

func (n *Node) freeFraction() float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(n.cfg.Root, &st); err != nil || st.Blocks == 0 {
		return 1
	}
	return float64(st.Bavail) / float64(st.Blocks)
}
