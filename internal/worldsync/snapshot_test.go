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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/spawnery/spawnery/internal/prune"
)

func writeFile(t *testing.T, path string, size int, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func keepOf(t *testing.T, entries ...string) prune.Keep {
	t.Helper()
	k, err := prune.ParseKeep(entries)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestScanFindsOnlyKeptRegularFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(dir, "worlds/world/level.dat"), 10, now)
	writeFile(t, filepath.Join(dir, "worlds/world/region/r.0.0.mca"), 100_000, now)
	writeFile(t, filepath.Join(dir, "logs/latest.log"), 5, now)
	writeFile(t, filepath.Join(dir, prune.ControlDir, "ready"), 0, now)

	files, err := Scan(dir, keepOf(t, "worlds/world"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "worlds/world/level.dat" || files[1].Path != "worlds/world/region/r.0.0.mca" {
		t.Fatalf("Scan = %+v", files)
	}
	if files[1].MTime != now.UnixNano() {
		t.Fatalf("mtime = %d, want %d", files[1].MTime, now.UnixNano())
	}
}

func TestSnapshotCopiesChangedLargeFilesAndEverySmallOne(t *testing.T) {
	data, snap := t.TempDir(), t.TempDir()
	then := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 10, then)
	writeFile(t, filepath.Join(data, "worlds/world/region/same.mca"), PackBelow, then)
	writeFile(t, filepath.Join(data, "worlds/world/region/new.mca"), PackBelow+1, then)
	base := []FileEntry{
		{Path: "worlds/world/level.dat", Size: 10, MTime: then.UnixNano(), Object: "packs/1.tar.gz"},
		{Path: "worlds/world/region/same.mca", Size: PackBelow, MTime: then.UnixNano(), Object: "objects/aaa"},
		{Path: "worlds/world/region/gone.mca", Size: PackBelow, MTime: then.UnixNano(), Object: "objects/bbb"},
	}

	s, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), base, 7)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]SnapFile{}
	for _, f := range s.Files {
		byPath[f.Path] = f
	}
	if !byPath["worlds/world/level.dat"].Copied {
		t.Error("a small file was not copied; small files always travel in the pack")
	}
	if f := byPath["worlds/world/region/same.mca"]; f.Copied || f.Object != "objects/aaa" {
		t.Errorf("an unchanged large file: %+v, want it carried over by reference", f)
	}
	if !byPath["worlds/world/region/new.mca"].Copied {
		t.Error("a new large file was not copied")
	}
	if _, ok := byPath["worlds/world/region/gone.mca"]; ok {
		t.Error("a deleted file is still in the snapshot")
	}
	if _, err := os.Stat(filepath.Join(snap, "worlds/world/region/new.mca")); err != nil {
		t.Errorf("the copy is not in the snapshot directory: %v", err)
	}
	again, err := ReadSnap(snap)
	if err != nil || again.Seq != 7 || len(again.Files) != len(s.Files) {
		t.Fatalf("ReadSnap = %+v, %v", again, err)
	}
}

// A file that changes during every copy attempt fails the
// snapshot instead of uploading a torn file.
func TestSnapshotRefusesAFileThatNeverSettles(t *testing.T) {
	data, snap := t.TempDir(), t.TempDir()
	path := filepath.Join(data, "worlds/world/region/hot.mca")
	writeFile(t, path, PackBelow+1, time.Unix(1, 0))
	tick := int64(2)
	copyHook = func(p string) {
		if p == path {
			writeFile(t, path, PackBelow+1, time.Unix(tick, 0))
			tick++
		}
	}
	t.Cleanup(func() { copyHook = nil })

	if _, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), nil, 1); !errors.Is(err, ErrUnsettled) {
		t.Fatalf("err = %v, want ErrUnsettled", err)
	}
}

func TestAFileDeletedBeforeItsCopyIsLeftOut(t *testing.T) {
	data, snap := t.TempDir(), t.TempDir()
	first, second := filepath.Join(data, "worlds/world/a.dat"), filepath.Join(data, "worlds/world/b.dat")
	writeFile(t, first, 5, time.Unix(1, 0))
	writeFile(t, second, 5, time.Unix(1, 0))
	copyHook = func(p string) {
		if p == first {
			_ = os.Remove(second)
		}
	}
	t.Cleanup(func() { copyHook = nil })

	s, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), nil, 1)
	if err != nil {
		t.Fatalf("a deleted file failed the snapshot: %v", err)
	}
	if len(s.Files) != 1 || s.Files[0].Path != "worlds/world/a.dat" {
		t.Fatalf("files = %+v, want only a.dat", s.Files)
	}
}

func TestAFileSwappedForAFIFOStillFailsTheSnapshot(t *testing.T) {
	data, snap := t.TempDir(), t.TempDir()
	first, second := filepath.Join(data, "worlds/world/a.dat"), filepath.Join(data, "worlds/world/b.dat")
	writeFile(t, first, 5, time.Unix(1, 0))
	writeFile(t, second, 5, time.Unix(1, 0))
	copyHook = func(p string) {
		if p == first {
			_ = os.Remove(second)
			_ = unix.Mkfifo(second, 0o644)
		}
	}
	t.Cleanup(func() { copyHook = nil })

	if _, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), nil, 1); err == nil {
		t.Fatal("a FIFO in place of a file passed the snapshot")
	}
}
