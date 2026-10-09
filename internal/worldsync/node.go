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
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"

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
	World   string
	Keep    []string
	Replace []string
	Target  string
	Pod     string
	// Group is the pod's fsGroup; nil leaves what the node agent creates root's.
	Group *int
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
	locks     map[string]chan struct{}
	workers   sync.WaitGroup
	downloads atomic.Int64

	callTimeout, uploadTimeout time.Duration

	// counted is each world's figures from the last pass that could lock it:
	// a hung store call holds the lock just when snapshots pile up.
	counted map[string]worldCount
}

type worldCount struct {
	pending int
	mounted bool
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
	// The worlds hold what pods wrote, setgid directories among them; no
	// other user on the node needs a way in.
	for _, d := range []string{cfg.Root, filepath.Join(cfg.Root, "worlds"), filepath.Join(cfg.Root, "orphans")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &Node{cfg: cfg, worlds: map[string]*worldState{}, targets: map[string]string{}, locks: map[string]chan struct{}{},
		// An upload must end before the lease it confirmed can go stale, or
		// its manifest could land after another node took the world over.
		callTimeout: 30 * time.Second, uploadTimeout: cfg.StaleAfter}, nil
}

// worldLock is a world's mutex as a channel of one, so that waiting for it
// can end with a context.
func (n *Node) worldLock(world string) chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	l, ok := n.locks[world]
	if !ok {
		l = make(chan struct{}, 1)
		n.locks[world] = l
	}
	return l
}

func (n *Node) lock(world string) func() {
	l := n.worldLock(world)
	l <- struct{}{}
	return func() { <-l }
}

func (n *Node) lockCtx(ctx context.Context, world string) (func(), error) {
	l := n.worldLock(world)
	select {
	case l <- struct{}{}:
		return func() { <-l }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (n *Node) tryLock(world string) (func(), bool) {
	l := n.worldLock(world)
	select {
	case l <- struct{}{}:
		return func() { <-l }, true
	default:
		return nil, false
	}
}

// call bounds one short store call; a hung one would hold its world's lock.
func (n *Node) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, n.callTimeout)
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
	return errors.Is(err, unix.EINVAL) || errors.Is(err, fs.ErrNotExist)
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
		if err := lstatAt(n.dataDir(s.World), f); err == nil {
			return
		}
	}
	if err := n.control(s, ReadyFile, ""); err != nil {
		n.cfg.Log.Error(err, "could not mark a resumed world ready", "world", s.World)
	}
}

func (n *Node) control(s *worldState, rel, content string) error {
	data := n.dataOf(s)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	return place(data, rel, strings.NewReader(content), 0, time.Now().UnixNano(), s.gid())
}

func (n *Node) resetControl(s *worldState) error {
	data := n.dataDir(s.World)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	if err := removeAllAt(data, prune.ControlDir); err != nil {
		return err
	}
	s.LastRequest = 0
	fd, err := openDir(data, []string{prune.ControlDir}, true, s.gid())
	if err != nil {
		return err
	}
	return unix.Close(fd)
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
	data := n.dataDir(s.World)
	if _, err := os.Lstat(data); os.IsNotExist(err) {
		return nil
	}
	return walkKept(data, keep, func(dir int, name, rel string, st *unix.Stat_t) error {
		if !keep.Holds(rel) && st.Mode&unix.S_IFMT != unix.S_IFLNK {
			return nil
		}
		if err := unix.Unlinkat(dir, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return &fs.PathError{Op: "remove", Path: filepath.Join(data, rel), Err: err}
		}
		return nil
	})
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
		unlock, err := n.lockCtx(ctx, world)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: the world is busy: %v", ErrUnavailable, err)
		}
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
	if _, err := prune.ParseReplace(req.Replace); err != nil {
		return err
	}
	if req.Group == nil {
		n.cfg.Log.Info("the pod names no fsGroup: what the node agent writes into its world stays root's, and the pod may not be able to write it", "world", req.World, "pod", req.Pod)
	}
	s, unlock, err := n.lockIdle(ctx, req.World)
	if err != nil {
		return err
	}
	defer unlock()

	if s.Target == req.Target {
		s.Group = req.Group
		if !s.lost {
			if err := n.shareWorld(s); err != nil {
				return err
			}
			n.save(s)
		}
		return n.rebind(s)
	}
	if s.Target != "" {
		// The old pod's teardown has not reached us yet.
		if err := n.unpublishLocked(s); err != nil {
			return err
		}
		s = n.state(req.World)
	}

	if pending, err := n.deletionPending(ctx, req.World); err != nil {
		return fmt.Errorf("%w: check for a pending deletion: %v", ErrUnavailable, err)
	} else if pending {
		return fmt.Errorf("%w: the world is being deleted", ErrUnavailable)
	}

	s.Keep, s.Replace, s.Pod, s.Group = req.Keep, req.Replace, req.Pod, req.Group
	etag, err := n.takeLease(ctx, s)
	if errors.Is(err, ErrLeaseLost) {
		leaseConflicts.Inc()
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err != nil {
		return fmt.Errorf("%w: take the lease: %v", ErrUnavailable, err)
	}
	s.LeaseETag = etag

	fetch, metag, err := n.settleContent(ctx, s)
	if err == nil {
		err = n.shareWorld(s)
	}
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

// shareWorld brings a cache from an earlier run, root's or another pod's
// group, to the publishing pod's group.
func (n *Node) shareWorld(s *worldState) error {
	if s.Group == nil {
		return nil
	}
	keep, err := prune.ParseKeep(s.Keep)
	if err != nil {
		return err
	}
	data := n.dataDir(s.World)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	if err := regroupKept(data, keep, *s.Group); err != nil {
		return fmt.Errorf("worldsync: give the world to the pod's group %d: %w", *s.Group, err)
	}
	return nil
}

// rebind binds the target again when it no longer shows the world's data,
// as after a reboot of the node: Resume restores the target from the saved
// state, but not the mount.
func (n *Node) rebind(s *worldState) error {
	data := n.dataOf(s)
	want, err := os.Stat(data)
	if err != nil {
		return err
	}
	if got, err := os.Stat(s.Target); err == nil && os.SameFile(got, want) {
		return nil
	}
	if err := os.MkdirAll(s.Target, 0o755); err != nil {
		return err
	}
	return n.cfg.Mounter.Bind(data, s.Target)
}

func (n *Node) deletionPending(ctx context.Context, world string) (bool, error) {
	ctx, cancel := n.call(ctx)
	defer cancel()
	return DeletionPending(ctx, n.cfg.Store, n.cfg.Base, world)
}

func (n *Node) readManifest(ctx context.Context, world string) (Manifest, string, error) {
	ctx, cancel := n.call(ctx)
	defer cancel()
	return ReadManifest(ctx, n.cfg.Store, n.prefix(world))
}

// takeLease returns ErrLeaseLost when another node holds the lease.
func (n *Node) takeLease(ctx context.Context, s *worldState) (string, error) {
	c, cancel := n.call(ctx)
	defer cancel()
	etag, err := TakeLease(c, n.cfg.Store, n.prefix(s.World), n.lease(s), n.cfg.StaleAfter)
	var held *HeldError
	if errors.As(err, &held) {
		if held.Node != n.cfg.NodeID {
			return "", fmt.Errorf("%w: %v", ErrLeaseLost, err)
		}
		return n.ownLease(ctx, s.World)
	}
	return etag, err
}

func (n *Node) renewLease(ctx context.Context, s *worldState) error {
	c, cancel := n.call(ctx)
	defer cancel()
	etag, err := RenewLease(c, n.cfg.Store, n.prefix(s.World), n.lease(s), s.LeaseETag)
	if errors.Is(err, ErrLeaseLost) {
		etag, err = n.ownLease(ctx, s.World)
	}
	if err != nil {
		return err
	}
	s.LeaseETag = etag
	n.save(s)
	return nil
}

// ownLease reads the lease back after a 412. Only this node writes its own
// name, so a lease that still names it is a write that committed and whose
// retry failed the condition (see UploadSnapshot).
func (n *Node) ownLease(ctx context.Context, world string) (string, error) {
	ctx, cancel := n.call(ctx)
	defer cancel()
	l, info, err := ReadLease(ctx, n.cfg.Store, n.prefix(world))
	if errors.Is(err, ErrNotFound) {
		return "", ErrLeaseLost
	}
	if err != nil {
		return "", err
	}
	if l.Node != n.cfg.NodeID {
		return "", fmt.Errorf("%w: held by node %q", ErrLeaseLost, l.Node)
	}
	return info.ETag, nil
}

// copyIntact reports whether the files the state lists are still on disk.
// Snapshots not yet uploaded have changed the copy since that list, so only
// the data directory counts then.
func (n *Node) copyIntact(s *worldState) bool {
	data := n.dataDir(s.World)
	if len(s.Pending) > 0 || s.FinalPending {
		_, err := os.Lstat(data)
		return err == nil
	}
	for _, f := range s.Files {
		st, err := os.Lstat(filepath.Join(data, filepath.FromSlash(f.Path)))
		if err != nil || !st.Mode().IsRegular() || st.Size() != f.Size {
			return false
		}
	}
	return true
}

// settleContent brings the local copy in line with the manifest. It returns
// the manifest to download when the copy is not current.
func (n *Node) settleContent(ctx context.Context, s *worldState) (*Manifest, string, error) {
	m, etag, err := n.readManifest(ctx, s.World)
	switch {
	case errors.Is(err, ErrNotFound):
		// A copy of a world that no longer exists.
		if s.WorldID != "" || s.Incomplete {
			if err := n.discardLocal(s); err != nil {
				return nil, "", err
			}
		}
		return nil, "", n.markReady(s)
	case err != nil:
		return nil, "", fmt.Errorf("%w: read the manifest: %v", ErrUnavailable, err)
	case !s.Incomplete && m.WorldID == s.WorldID && m.Generation == s.Generation:
		if n.copyIntact(s) {
			s.ManifestETag = etag
			return nil, "", n.markReady(s)
		}
		// Ready on a copy that is gone would start the server on a fresh world,
		// and its first snapshot would replace this one in the bucket.
		n.cfg.Log.Info("downloading the world again", "world", s.World, "reason", "files the state lists are missing from the local copy")
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
	world, prefix, dir, gid := s.World, n.prefix(s.World), n.dataDir(s.World), s.gid()
	go func() {
		started := time.Now()
		err := Download(ctx, n.cfg.Store, prefix, dir, m, n.cfg.Parallel, gid)
		close(d.done)
		unlock := n.lock(world)
		defer unlock()
		if n.lookup(world) != s || s.download != d {
			return
		}
		s.download = nil
		cancel()
		if err != nil {
			downloadFailures.Inc()
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
		unlock, err := n.lockCtx(ctx, world)
		if err != nil {
			return fmt.Errorf("worldsync: the world is busy: %w", err)
		}
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
		if err := n.checkWorldsKept(s); err != nil {
			n.cfg.Log.Error(err, "the final snapshot saves only what keep holds; the rest stays on this node", "world", s.World)
		}
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
	if _, err := TakeSnapshot(n.dataDir(s.World), dir, keep, base, seq, n.cfg.Clock()); err != nil {
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

func (n *Node) each(fn func(*worldState)) { n.walk(fn, false) }

// eachFree skips a world whose lock is taken, so that one world's hung
// store call holds up no other world's snapshot or upload.
func (n *Node) eachFree(fn func(*worldState)) { n.walk(fn, true) }

func (n *Node) walk(fn func(*worldState), skipBusy bool) {
	n.mu.Lock()
	worlds := make([]string, 0, len(n.worlds))
	for w := range n.worlds {
		worlds = append(worlds, w)
	}
	n.mu.Unlock()
	for _, w := range worlds {
		var unlock func()
		if skipBusy {
			var ok bool
			if unlock, ok = n.tryLock(w); !ok {
				continue
			}
		} else {
			unlock = n.lock(w)
		}
		if s := n.lookup(w); s != nil {
			fn(s)
		}
		unlock()
	}
}

func (n *Node) pollRequests() {
	n.eachFree(func(s *worldState) {
		if s.Target == "" {
			return
		}
		b, err := readSmall(n.dataOf(s), RequestFile, 64)
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
			if err = n.checkWorldsKept(s); err == nil {
				err = n.snapshot(s)
			}
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
	counted := map[string]worldCount{}
	n.eachFree(func(s *worldState) {
		counted[s.World] = worldCount{pending: len(s.Pending), mounted: s.Target != ""}
		if s.working || !n.hasWork(s) || n.cfg.Clock().Before(s.retryAt) {
			return
		}
		s.working = true
		n.workers.Add(1)
		go n.work(ctx, s)
	})
	n.mu.Lock()
	pending, mounted := 0, 0
	for w := range n.worlds {
		c, ok := counted[w]
		if !ok {
			c = n.counted[w]
			counted[w] = c
		}
		pending += c.pending
		if c.mounted {
			mounted++
		}
	}
	for w := range counted {
		if _, ok := n.worlds[w]; !ok {
			delete(counted, w)
		}
	}
	n.counted = counted
	n.mu.Unlock()
	pendingUploads.Set(float64(pending))
	worlds.WithLabelValues("mounted").Set(float64(mounted))
	worlds.WithLabelValues("cached").Set(float64(len(counted) - mounted))
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

		uctx, cancel := context.WithTimeout(ctx, n.uploadTimeout)
		started := time.Now()
		m, etag, err := UploadSnapshot(uctx, n.cfg.Store, n.prefix(s.World), job.dir, job.prev, job.prevETag, NewWorldID)
		cancel()
		if err == nil {
			uploadSeconds.Observe(time.Since(started).Seconds())
		}

		unlock = n.lock(s.World)
		s.uploading = nil
		close(running)
		more := n.finishUpload(ctx, s, job, m, etag, err)
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
	if s.LeaseETag != "" {
		return n.renewLease(ctx, s)
	}
	etag, err := n.takeLease(ctx, s)
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
func (n *Node) finishUpload(ctx context.Context, s *worldState, job *uploadJob, m Manifest, etag string, err error) bool {
	if n.lookup(s.World) != s || s.lost || len(s.Pending) == 0 || s.Pending[0] != job.seq {
		return false
	}
	if errors.Is(err, ErrConflict) {
		m, etag, err = n.adoptCommitted(ctx, s, job)
		if errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrConflict) {
			n.orphan(s, err.Error())
			return false
		}
	}
	if err != nil {
		n.retryLater(s, "upload failed", err)
		return false
	}
	s.WorldID, s.Generation, s.ManifestETag, s.Files = m.WorldID, m.Generation, etag, m.Files
	s.Pending = s.Pending[1:]
	s.retryAt, s.retryDelay = time.Time{}, 0
	n.save(s)
	_ = os.RemoveAll(job.dir)
	return true
}

// adoptCommitted decides a manifest conflict, as after a crash between the
// manifest put and the state save. With the lease chain unbroken no other
// node wrote since, so a manifest of exactly the snapshot's files is ours.
func (n *Node) adoptCommitted(ctx context.Context, s *worldState, job *uploadJob) (Manifest, string, error) {
	if err := n.renewLease(ctx, s); err != nil {
		return Manifest{}, "", err
	}
	m, etag, err := n.readManifest(ctx, s.World)
	if errors.Is(err, ErrNotFound) {
		return Manifest{}, "", fmt.Errorf("%w: the manifest is gone", ErrConflict)
	}
	if err != nil {
		return Manifest{}, "", err
	}
	snap, err := ReadSnap(job.dir)
	if err != nil {
		return Manifest{}, "", err
	}
	if !sameUpload(s, snap, m) {
		return Manifest{}, "", fmt.Errorf("%w: generation %d of world %s is not this node's", ErrConflict, m.Generation, m.WorldID)
	}
	c, cancel := n.call(ctx)
	defer cancel()
	_, _ = Prune(c, n.cfg.Store, n.prefix(s.World), PruneRequest{Current: m, Replaced: job.prev})
	n.dropStrayPacks(c, s.World, m)
	n.cfg.Log.Info("adopted an upload that committed before its answer arrived", "world", s.World, "generation", m.Generation)
	return m, etag, nil
}

// sameUpload: a state without a world ID had not seen its first upload
// recorded, and the first upload names the world ID.
func sameUpload(s *worldState, snap Snap, m Manifest) bool {
	if s.WorldID != "" && m.WorldID != s.WorldID || m.Generation != s.Generation+1 || len(m.Files) != len(snap.Files) {
		return false
	}
	for i, f := range snap.Files {
		e := m.Files[i]
		if e.Path != f.Path || e.Size != f.Size || e.MTime != f.MTime {
			return false
		}
	}
	return true
}

// dropStrayPacks deletes the packs m does not name: the conflicting retry
// wrote one under a fresh name. The lease keeps other writers out.
func (n *Node) dropStrayPacks(ctx context.Context, world string, m Manifest) {
	named := map[string]bool{}
	for _, e := range m.Files {
		named[e.Object] = true
	}
	keys, err := n.cfg.Store.List(ctx, n.prefix(world)+PacksDir)
	if err != nil {
		return
	}
	for _, k := range keys {
		if !named[strings.TrimPrefix(k, n.prefix(world))] {
			_ = n.cfg.Store.Delete(ctx, k)
		}
	}
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
	retries.Inc()
	n.cfg.Log.Error(err, msg+"; retrying", "world", s.World, "in", s.retryDelay)
}

func (n *Node) release(ctx context.Context, s *worldState) {
	n.dropScratch(s)
	c, cancel := n.call(ctx)
	defer cancel()
	err := ReleaseLease(c, n.cfg.Store, n.prefix(s.World), s.LeaseETag)
	if err != nil && !errors.Is(err, ErrLeaseLost) {
		n.retryLater(s, "lease release failed", err)
		return
	}
	s.LeaseETag = ""
	s.retryAt, s.retryDelay = time.Time{}, 0
	n.save(s)
}

// checkWorldsKept refuses a world the prune would refuse to delete: it is
// not synced, so it would not travel to another node.
func (n *Node) checkWorldsKept(s *worldState) error {
	keep, err := prune.ParseKeep(s.Keep)
	if err != nil {
		return err
	}
	replace, err := prune.ParseReplace(s.Replace)
	if err != nil {
		return err
	}
	rel, err := worldOutside(n.dataOf(s), keep, replace)
	if err != nil {
		return err
	}
	if rel != "" {
		return fmt.Errorf("spec.storage.keep does not keep %s, which holds a world", rel)
	}
	return nil
}

// dropScratch deletes the top-level entries keep does not hold, except one
// that holds a world outside replace, which the prune would refuse to delete.
func (n *Node) dropScratch(s *worldState) {
	keep, err := prune.ParseKeep(s.Keep)
	if err != nil {
		return
	}
	replace, err := prune.ParseReplace(s.Replace)
	if err != nil {
		return
	}
	data := n.dataDir(s.World)
	entries, _ := os.ReadDir(data)
	for _, e := range entries {
		name := e.Name()
		if name == prune.ControlDir || keep.Holds(name) || keep.Toward(name) {
			continue
		}
		if !replace.Holds(name) {
			world, err := entryHoldsWorld(data, name)
			if err != nil {
				n.cfg.Log.Error(err, "keeping a path keep does not hold: cannot tell whether it holds a world", "world", s.World, "path", name)
				continue
			}
			if world {
				n.cfg.Log.Info("keeping a path keep does not hold on this node: it holds a world, which is not synced", "world", s.World, "path", name)
				continue
			}
		}
		_ = removeAllAt(data, name)
	}
}

func (n *Node) renewAll(ctx context.Context) {
	n.each(func(s *worldState) {
		if s.LeaseETag == "" || s.lost {
			return
		}
		switch err := n.renewLease(ctx, s); {
		case errors.Is(err, ErrLeaseLost):
			n.orphan(s, err.Error())
		case err != nil:
			n.cfg.Log.Error(err, "lease renewal failed; retrying", "world", s.World)
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
	n.worlds[s.World] = &worldState{World: s.World, Target: s.Target, Group: s.Group, LastRequest: s.LastRequest, lost: true, lostDir: dst}
	n.targets[s.Target] = s.World
	n.mu.Unlock()
}

func (n *Node) moveAside(world string) (string, error) {
	src := n.worldDir(world)
	if _, err := os.Lstat(src); os.IsNotExist(err) {
		return "", nil
	}
	base := filepath.Join(n.cfg.Root, "orphans", strings.ReplaceAll(world, "/", "_")+"_"+strconvI(n.cfg.Clock().Unix()))
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
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
	var st unix.Statfs_t
	if err := unix.Statfs(n.cfg.Root, &st); err != nil || st.Blocks == 0 {
		return 1
	}
	return float64(st.Bavail) / float64(st.Blocks)
}
