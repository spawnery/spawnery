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
	"strings"
	"testing"
	"time"
)

const prefix = "ns/g/k/"

func snapshotOf(t *testing.T, data string, base []FileEntry, seq int64) string {
	t.Helper()
	snap := t.TempDir()
	if _, err := TakeSnapshot(data, snap, keepOf(t, "worlds/world"), base, seq); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestARoundTripRestoresContentModeAndMTime(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	then := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 10, then)
	writeFile(t, filepath.Join(data, "worlds/world/region/r.0.0.mca"), PackBelow+5, then)

	m, etag, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", func() string { return "w1" })
	if err != nil {
		t.Fatal(err)
	}
	if m.Generation != 1 || m.WorldID != "w1" || etag == "" {
		t.Fatalf("manifest = %+v etag=%q", m, etag)
	}

	out := t.TempDir()
	if err := Download(context.Background(), st, prefix, out, m, 4); err != nil {
		t.Fatal(err)
	}
	files, err := Scan(out, keepOf(t, "worlds/world"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[1].Size != PackBelow+5 || files[1].MTime != then.UnixNano() {
		t.Fatalf("restored = %+v", files)
	}
}

func TestASecondGenerationUploadsOnlyWhatChangedAndCollectsTheRest(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	then := time.Unix(1_700_000_000, 0)
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow, then)
	writeFile(t, filepath.Join(data, "worlds/world/region/b.mca"), PackBelow+1, then)
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 3, then)
	m1, e1, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}

	os.Remove(filepath.Join(data, "worlds/world/region/b.mca"))
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow+2, then.Add(time.Minute))
	m2, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, e1, NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Generation != 2 || m2.WorldID != m1.WorldID {
		t.Fatalf("m2 = %+v", m2)
	}
	var objects, packs int
	for _, k := range st.Keys() {
		switch {
		case strings.HasPrefix(k, prefix+ObjectsDir):
			objects++
		case strings.HasPrefix(k, prefix+PacksDir):
			packs++
		}
	}
	if objects != 1 || packs != 1 {
		t.Fatalf("after gen 2: %d objects, %d packs (%v); the old ones should be collected", objects, packs, st.Keys())
	}
}

func TestAStaleManifestETagIsAConflict(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	m1, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, "stale", NewWorldID); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a first upload over an existing world: err = %v, want ErrConflict", err)
	}
}

func TestADeletedPreviousManifestIsAConflict(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	m1, etag, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(context.Background(), prefix+ManifestName); err != nil {
		t.Fatal(err)
	}
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, etag, NewWorldID); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

// Review Focus 5: an outage during the objects must leave the old manifest in
// place, and the same snapshot must upload cleanly afterwards.
func TestAnOutageMidUploadLeavesTheOldManifestAndRetriesClean(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow+1, time.Unix(1, 0))
	m1, e1, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow+9, time.Unix(2, 0))
	snap := snapshotOf(t, data, m1.Files, 2)

	st.SetOutage(errors.New("down"))
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snap, &m1, e1, NewWorldID); err == nil {
		t.Fatal("an upload during an outage succeeded")
	}
	st.SetOutage(nil)
	got, _, err := ReadManifest(context.Background(), st, prefix)
	if err != nil || got.Generation != 1 {
		t.Fatalf("manifest after a failed upload = %+v, %v; want generation 1 untouched", got, err)
	}
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snap, &m1, e1, NewWorldID); err != nil {
		t.Fatalf("the retry of the same snapshot: %v", err)
	}
}

func TestDownloadRefusesAPathOutsideTheDirectory(t *testing.T) {
	st := NewMemStore(time.Now)
	m := Manifest{WorldID: "w", Generation: 1, Files: []FileEntry{{Path: "../escape", Size: 1, Object: "objects/x"}}}
	if err := Download(context.Background(), st, prefix, t.TempDir(), m, 1); err == nil {
		t.Fatal("a manifest path with .. was accepted")
	}
}

func TestALosingWriterDoesNotOverwriteTheWinnersPack(t *testing.T) {
	st := NewMemStore(time.Now)
	then := time.Unix(1_700_000_000, 0)
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "worlds/world/level.dat"), 3, then)
	m1, e1, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, base, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	winner, loser := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(winner, "worlds/world/level.dat"), 5, then)
	writeFile(t, filepath.Join(loser, "worlds/world/level.dat"), 7, then)
	mw, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, winner, m1.Files, 2), &m1, e1, NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, loser, m1.Files, 2), &m1, e1, NewWorldID); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	out := t.TempDir()
	if err := Download(context.Background(), st, prefix, out, mw, 2); err != nil {
		t.Fatal(err)
	}
	files, err := Scan(out, keepOf(t, "worlds/world"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Size != 5 {
		t.Fatalf("restored = %+v, want the winner's 5-byte file", files)
	}
}

func TestDownloadFailsWhenAPackLacksAnEntry(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	m, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	m.Files = append(m.Files, FileEntry{Path: "worlds/world/extra.dat", Size: 2, Object: m.Files[0].Object})
	if err := Download(context.Background(), st, prefix, t.TempDir(), m, 2); err == nil {
		t.Fatal("a small entry missing from its pack went unnoticed")
	}
}

func TestUploadRefusesAPreviousManifestWithoutETag(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "worlds/world/level.dat"), 3, time.Unix(1, 0))
	m1, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, m1.Files, 2), &m1, "", NewWorldID); err == nil {
		t.Fatal("an upload over a previous manifest without its ETag succeeded")
	}
}

func TestUploadRefusesUnchangedEntriesThePreviousManifestDoesNotName(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	then := time.Unix(1, 0)
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow+1, then)
	m1, e1, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	base := append([]FileEntry(nil), m1.Files...)
	base[0].Object = "objects/unknown"
	before := len(st.Keys())
	if _, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, base, 2), &m1, e1, NewWorldID); err == nil {
		t.Fatal("an unchanged entry the previous manifest does not name was accepted")
	}
	if len(st.Keys()) != before {
		t.Fatalf("keys changed: %v", st.Keys())
	}
}

type failingGet struct {
	Store
}

type failingBody struct{ n int }

func (f *failingBody) Read(p []byte) (int, error) {
	if f.n > 0 {
		return 0, errors.New("connection reset")
	}
	f.n = 1
	copy(p, "xx")
	return 2, nil
}
func (f *failingBody) Close() error { return nil }

func (failingGet) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	return &failingBody{}, ObjectInfo{}, nil
}

func TestAFailedDownloadLeavesNoPartFiles(t *testing.T) {
	st := NewMemStore(time.Now)
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "worlds/world/region/a.mca"), PackBelow+1, time.Unix(1, 0))
	m, _, err := UploadSnapshot(context.Background(), st, prefix, snapshotOf(t, data, nil, 1), nil, "", NewWorldID)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Download(context.Background(), failingGet{st}, prefix, out, m, 1); err == nil {
		t.Fatal("a download with a failing body succeeded")
	}
	filepath.WalkDir(out, func(p string, d fs.DirEntry, _ error) error {
		if strings.HasSuffix(p, ".worldsync-part") {
			t.Errorf("left behind: %s", p)
		}
		return nil
	})
}
