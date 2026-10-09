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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/spawnery/spawnery/internal/prune"
)

func secretOf(n int) []byte {
	return bytes.Repeat([]byte("s3cr3t-"), n/7+1)[:n]
}

// outside makes a directory beside the world holding name with content.
func outside(t *testing.T, name string, content []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// assertOnly checks that dir holds exactly the regular files in want.
func assertOnly(t *testing.T, dir string, want map[string][]byte) {
	t.Helper()
	got := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if !d.Type().IsRegular() {
			got[rel] = []byte("<" + d.Type().String() + ">")
			return nil
		}
		b, err := os.ReadFile(p)
		got[rel] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("%s holds %d entries, want %d: %v", dir, len(got), len(want), keysOf(got))
	}
	for name, b := range want {
		if !bytes.Equal(got[name], b) {
			t.Errorf("%s/%s changed", dir, name)
		}
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func treeHolds(t *testing.T, dir string, needle []byte) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			found = found || bytes.Contains(b, needle)
		}
		return nil
	})
	return found
}

// storeHolds looks for needle in every object, inside packs too.
func storeHolds(t *testing.T, st *MemStore, needle []byte) bool {
	t.Helper()
	for _, key := range st.Keys() {
		b, _, err := readObject(context.Background(), st, key)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, needle) {
			return true
		}
		if !strings.HasSuffix(key, ".tar.gz") {
			continue
		}
		gz, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		for {
			if _, err := tr.Next(); err != nil {
				break
			}
			entry, _ := io.ReadAll(tr)
			if bytes.Contains(entry, needle) {
				return true
			}
		}
	}
	return false
}

// swapDuring runs swap once, after the copy of the first file the snapshot takes.
func swapDuring(t *testing.T, first string, swap func()) {
	var once sync.Once
	copyHook = func(p string) {
		if p == first {
			once.Do(swap)
		}
	}
	t.Cleanup(func() { copyHook = nil })
}

func withTimeout(t *testing.T, what string, fn func(), unblock func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		unblock()
		t.Fatalf("%s hung", what)
	}
}

func TestASnapshotDoesNotFollowAParentDirectorySwappedForASymlink(t *testing.T) {
	data, snap := t.TempDir(), t.TempDir()
	secret := secretOf(100)
	away, _ := outside(t, "r.mca", secret)
	now := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 10, now)
	writeFile(t, filepath.Join(data, "worlds/world/region/r.mca"), 100, now)
	swapDuring(t, filepath.Join(data, "worlds/world/level.dat"), func() {
		symlink(t, away, filepath.Join(data, "worlds/world/region"))
	})

	if _, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), nil, 1, time.Unix(1_700_000_000, 0)); err == nil {
		t.Error("the snapshot went through a directory symlinked out of the world")
	}
	if treeHolds(t, snap, secret) {
		t.Error("a file outside the world was copied into the snapshot")
	}
	assertOnly(t, away, map[string][]byte{"r.mca": secret})
}

func TestASnapshotDoesNotFollowAFileSwappedForASymlink(t *testing.T) {
	h := newHarness(t)
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	secret := secretOf(PackBelow + 1)
	away, _ := outside(t, "environ", secret)
	data := h.node("a").dataDir(w)
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 10, h.now)
	writeFile(t, filepath.Join(target, "worlds/world/region/r.mca"), PackBelow+1, h.now)
	swapDuring(t, filepath.Join(data, "worlds/world/level.dat"), func() {
		symlink(t, filepath.Join(away, "environ"), filepath.Join(data, "worlds/world/region/r.mca"))
	})

	h.request(target, "1")
	if got := h.waitFile(target, DoneFile); !strings.HasPrefix(got, "failed") {
		t.Errorf("done = %q, want a failed snapshot", got)
	}
	h.node("a").Settle(context.Background())
	if storeHolds(t, h.st, secret) {
		t.Error("a file outside the world was uploaded")
	}
	assertOnly(t, away, map[string][]byte{"environ": secret})
}

func TestASnapshotDoesNotOpenAFIFOSwappedIn(t *testing.T) {
	data, snap := t.TempDir(), t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 10, now)
	writeFile(t, filepath.Join(data, "worlds/world/region/r.mca"), 100, now)
	fifo := filepath.Join(data, "worlds/world/region/r.mca")
	swapDuring(t, filepath.Join(data, "worlds/world/level.dat"), func() {
		_ = os.Remove(fifo)
		if err := unix.Mkfifo(fifo, 0o644); err != nil {
			t.Error(err)
		}
	})

	var err error
	withTimeout(t, "a snapshot over a FIFO", func() {
		_, err = TakeSnapshot(data, snap, keepOf(t, "worlds/world"), nil, 1, time.Unix(1_700_000_000, 0))
	}, func() { unblockFIFO(fifo) })
	if err == nil {
		t.Error("the snapshot read a FIFO")
	}
}

func unblockFIFO(path string) {
	if f, err := os.OpenFile(path, os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
		_ = f.Close()
	}
}

func TestADownloadDoesNotWriteThroughPlantedSymlinks(t *testing.T) {
	st := NewMemStore(time.Now)
	src := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(src, "worlds/world/level.dat"), 10, now)
	writeFile(t, filepath.Join(src, "worlds/world/data/small.dat"), 5, now)
	writeFile(t, filepath.Join(src, "worlds/world/region/r.mca"), PackBelow+1, now)
	m, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, src, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}

	untouched := []byte("untouched")
	away, file := outside(t, "victim", untouched)
	out := t.TempDir()
	symlink(t, away, filepath.Join(out, "worlds/world/data"))
	symlink(t, away, filepath.Join(out, "worlds/world/region"))
	symlink(t, file, filepath.Join(out, "worlds/world/level.dat.worldsync-part"))

	_ = Download(context.Background(), st, prefix, out, m, 4, -1)
	assertOnly(t, away, map[string][]byte{"victim": untouched})
}

func TestADownloadReplacesASymlinkWhereAFileGoes(t *testing.T) {
	st := NewMemStore(time.Now)
	src := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(src, "worlds/world/level.dat"), 10, now)
	writeFile(t, filepath.Join(src, "worlds/world/region/r.mca"), PackBelow+1, now)
	m, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, src, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}

	untouched := []byte("untouched")
	away, file := outside(t, "victim", untouched)
	out := t.TempDir()
	symlink(t, file, filepath.Join(out, "worlds/world/level.dat"))
	symlink(t, file, filepath.Join(out, "worlds/world/region/r.mca"))

	if err := Download(context.Background(), st, prefix, out, m, 4, -1); err != nil {
		t.Fatal(err)
	}
	assertOnly(t, away, map[string][]byte{"victim": untouched})
	files, err := Scan(out, keepOf(t, "worlds/world"))
	if err != nil || len(files) != 2 {
		t.Fatalf("restored = %+v, %v; want both files as regular files", files, err)
	}
}

func TestAPlantedDirectorySymlinkIsGoneBeforeTheNextDownload(t *testing.T) {
	h := newHarness(t)
	untouched := []byte("untouched")
	away, _ := outside(t, "victim", untouched)

	target, _ := h.publish("a", w, "p1")
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	symlink(t, away, filepath.Join(target, "worlds/world/region"))
	h.unpublish("a", target)
	h.node("a").Settle(context.Background())

	target, err := h.publish("b", w, "p2")
	if err != nil {
		t.Fatal(err)
	}
	h.waitFile(target, ReadyFile)
	writeFile(t, filepath.Join(target, "worlds/world/region/r.mca"), PackBelow+1, h.now)
	writeFile(t, filepath.Join(target, "worlds/world/region/small.dat"), 5, h.now)
	h.unpublish("b", target)
	h.node("b").Settle(context.Background())

	target, err = h.publish("a", w, "p3")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.waitFile(target, ReadyFile); got != "" {
		t.Fatalf("ready = %q", got)
	}
	assertOnly(t, away, map[string][]byte{"victim": untouched})
	if info, err := os.Lstat(filepath.Join(h.node("a").dataDir(w), "worlds/world/region")); err != nil || !info.IsDir() {
		t.Fatalf("region after the download: %v, %v; want a directory", info, err)
	}
}

func TestControlFilesDoNotFollowASymlinkedControlDirectory(t *testing.T) {
	h := newHarness(t)
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(target, "worlds/world/level.dat"), 5, h.now)
	request := []byte("1")
	away, _ := outside(t, "snapshot.request", request)
	symlink(t, away, filepath.Join(target, prune.ControlDir))

	for range 3 {
		h.node("a").Settle(context.Background())
	}
	assertOnly(t, away, map[string][]byte{"snapshot.request": request})
}

func TestAFIFOAsTheSnapshotRequestDoesNotHangThePoll(t *testing.T) {
	h := newHarness(t)
	target, err := h.publish("a", w, "p1")
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(target, RequestFile)
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	withTimeout(t, "the request poll", func() { h.node("a").Settle(context.Background()) }, func() { unblockFIFO(fifo) })
}

func TestWipeKeptRemovesPlantedSymlinksAndNotTheirTargets(t *testing.T) {
	h := newHarness(t)
	n := h.node("a")
	untouched := []byte("untouched")
	away, file := outside(t, "victim", untouched)
	data := n.dataDir(w)
	symlink(t, file, filepath.Join(data, "worlds/world/level.dat"))
	symlink(t, away, filepath.Join(data, "worlds/world/region"))
	symlink(t, away, filepath.Join(data, "plugins"))
	symlink(t, away, filepath.Join(data, "worlds/other"))

	if err := n.wipeKept(&worldState{World: w, Keep: []string{"worlds/world", "plugins/Example/data"}}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"worlds/world/level.dat", "worlds/world/region", "plugins"} {
		if _, err := os.Lstat(filepath.Join(data, rel)); !os.IsNotExist(err) {
			t.Errorf("%s survived the wipe: %v", rel, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(data, "worlds/other")); err != nil {
		t.Errorf("a symlink keep does not name was wiped: %v", err)
	}
	assertOnly(t, away, map[string][]byte{"victim": untouched})
}

func TestDropScratchRemovesPlantedSymlinksAndNotTheirTargets(t *testing.T) {
	h := newHarness(t)
	n := h.node("a")
	untouched := []byte("untouched")
	away, file := outside(t, "victim", untouched)
	data := n.dataDir(w)
	symlink(t, away, filepath.Join(data, "logs"))
	symlink(t, file, filepath.Join(data, "cache.txt"))

	n.dropScratch(&worldState{World: w, Keep: []string{"worlds/world"}})
	for _, rel := range []string{"logs", "cache.txt"} {
		if _, err := os.Lstat(filepath.Join(data, rel)); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", rel, err)
		}
	}
	assertOnly(t, away, map[string][]byte{"victim": untouched})
}
