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
