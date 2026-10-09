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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"

	"github.com/spawnery/spawnery/internal/prune"
)

var ErrUnsettled = errors.New("worldsync: a file kept changing while it was copied")

const snapFile = "snapshot.json"

type LocalFile struct {
	Path  string
	Size  int64
	Mode  uint32
	MTime int64
}

type SnapFile struct {
	FileEntry
	// Copied: the content is in the snapshot directory. Otherwise Object names
	// where it already is.
	Copied bool `json:"copied"`
}

type Snap struct {
	Seq int64 `json:"seq"`
	// Taken is unix milliseconds on the node's clock.
	Taken int64      `json:"taken,omitempty"`
	Files []SnapFile `json:"files"`
}

// copyHook runs after each copy, before the recheck; tests use it to change a file.
var copyHook func(path string)

func Scan(dir string, keep prune.Keep) ([]LocalFile, error) {
	var out []LocalFile
	err := walkKept(dir, keep, func(_ int, _, rel string, st *unix.Stat_t) error {
		if st.Mode&unix.S_IFMT == unix.S_IFREG && keep.Holds(rel) {
			out = append(out, LocalFile{Path: rel, Size: st.Size, Mode: uint32(st.Mode & 0o777), MTime: st.Mtim.Nano()})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// copyStable copies root/rel to dst and checks that size and mtime did not
// move during the copy, up to three attempts. It returns the stat it copied at.
func copyStable(root, rel, dst string) (int64, int64, error) {
	for attempt := 0; attempt < 3; attempt++ {
		size, mtime, settled, err := copyOnce(root, rel, dst)
		if err != nil || settled {
			return size, mtime, err
		}
	}
	return 0, 0, fmt.Errorf("%w: %s", ErrUnsettled, filepath.Join(root, rel))
}

func copyOnce(root, rel, dst string) (int64, int64, bool, error) {
	in, err := openRegular(root, rel)
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { _ = in.Close() }()
	before, err := in.Stat()
	if err != nil {
		return 0, 0, false, err
	}
	if err := copyFile(in, dst); err != nil {
		return 0, 0, false, err
	}
	if copyHook != nil {
		copyHook(in.Name())
	}
	after, err := in.Stat()
	if err != nil {
		return 0, 0, false, err
	}
	settled := before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
	return before.Size(), before.ModTime().UnixNano(), settled, nil
}

func copyFile(in io.Reader, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// TakeSnapshot copies what an upload needs out of dataDir into snapDir: every
// file below PackBelow, and every larger file whose size or mtime differs
// from base. Unchanged large files are carried over by reference.
func TakeSnapshot(dataDir, snapDir string, keep prune.Keep, base []FileEntry, seq int64, taken time.Time) (Snap, error) {
	files, err := Scan(dataDir, keep)
	if err != nil {
		return Snap{}, err
	}
	prev := make(map[string]FileEntry, len(base))
	for _, b := range base {
		prev[b.Path] = b
	}
	s := Snap{Seq: seq, Taken: taken.UnixMilli()}
	for _, f := range files {
		b, ok := prev[f.Path]
		if ok && f.Size >= PackBelow && b.Size == f.Size && b.MTime == f.MTime {
			s.Files = append(s.Files, SnapFile{FileEntry: b})
			continue
		}
		size, mtime, err := copyStable(dataDir, f.Path, filepath.Join(snapDir, filepath.FromSlash(f.Path)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Snap{}, err
		}
		s.Files = append(s.Files, SnapFile{FileEntry: FileEntry{Path: f.Path, Size: size, Mode: f.Mode, MTime: mtime}, Copied: true})
	}
	if err := writeJSON(filepath.Join(snapDir, snapFile), s); err != nil {
		return Snap{}, err
	}
	return s, nil
}

func ReadSnap(snapDir string) (Snap, error) {
	var s Snap
	return s, readJSON(filepath.Join(snapDir, snapFile), &s)
}

// writeJSON replaces path atomically.
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
