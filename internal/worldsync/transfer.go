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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"
)

var ErrConflict = errors.New("worldsync: the world's manifest changed under this writer")

const uploadParallel = 16

func NewWorldID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newPackKey(generation int64) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return PacksDir + strconv.FormatInt(generation, 10) + "-" + hex.EncodeToString(b[:]) + ".tar.gz"
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func UploadSnapshot(ctx context.Context, st Store, prefix, snapDir string, prev *Manifest, prevETag string, newWorldID func() string) (Manifest, string, error) {
	snap, err := ReadSnap(snapDir)
	if err != nil {
		return Manifest{}, "", err
	}
	if prev != nil && prevETag == "" {
		return Manifest{}, "", errors.New("worldsync: a previous manifest needs its ETag")
	}
	prevObjects := map[string]bool{}
	if prev != nil {
		for _, e := range prev.Files {
			prevObjects[e.Object] = true
		}
	}
	for _, f := range snap.Files {
		if !f.Copied && (f.Object == "" || !prevObjects[f.Object]) {
			return Manifest{}, "", fmt.Errorf("worldsync: unchanged file %s names object %q, which the previous manifest does not", f.Path, f.Object)
		}
	}
	m := Manifest{Generation: 1}
	if prev != nil {
		m.WorldID, m.Generation = prev.WorldID, prev.Generation+1
	} else {
		m.WorldID = newWorldID()
	}
	pack := newPackKey(m.Generation)

	var packBuf bytes.Buffer
	gz := gzip.NewWriter(&packBuf)
	tw := tar.NewWriter(gz)
	packed := 0
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadParallel)
	m.Files = make([]FileEntry, len(snap.Files))
	for i, f := range snap.Files {
		e := f.FileEntry
		switch {
		case !f.Copied:
		case e.Size < PackBelow:
			b, err := os.ReadFile(filepath.Join(snapDir, e.Path))
			if err != nil {
				return Manifest{}, "", err
			}
			if err := tw.WriteHeader(&tar.Header{Name: e.Path, Mode: int64(e.Mode), Size: int64(len(b)), ModTime: time.Unix(0, e.MTime)}); err != nil {
				return Manifest{}, "", err
			}
			if _, err := tw.Write(b); err != nil {
				return Manifest{}, "", err
			}
			e.Object = pack
			packed++
		default:
			sum, err := hashFile(filepath.Join(snapDir, e.Path))
			if err != nil {
				return Manifest{}, "", err
			}
			e.Object = ObjectsDir + sum
			key, src := prefix+e.Object, filepath.Join(snapDir, e.Path)
			g.Go(func() error {
				if _, err := st.Head(gctx, key); err == nil {
					return nil
				} else if !errors.Is(err, ErrNotFound) {
					return err
				}
				f, err := os.Open(src)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = st.Put(gctx, key, f, PutCondition{})
				return err
			})
		}
		m.Files[i] = e
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, "", err
	}
	if err := gz.Close(); err != nil {
		return Manifest{}, "", err
	}
	if packed > 0 {
		g.Go(func() error {
			_, err := st.Put(gctx, prefix+pack, bytes.NewReader(packBuf.Bytes()), PutCondition{})
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return Manifest{}, "", err
	}

	body, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, "", err
	}
	cond := PutCondition{IfNoneMatch: prev == nil, IfMatch: prevETag}
	if prev == nil {
		cond.IfMatch = ""
	}
	info, err := st.Put(ctx, prefix+ManifestName, bytes.NewReader(body), cond)
	if errors.Is(err, ErrPrecondition) || errors.Is(err, ErrNotFound) {
		// The SDK's retryer re-sends a conditional put after a 5xx or a reset
		// connection; when the first attempt had committed, the retry fails
		// its own condition.
		stored, storedInfo, rerr := readObject(ctx, st, prefix+ManifestName)
		if rerr != nil || !bytes.Equal(stored, body) {
			return Manifest{}, "", fmt.Errorf("%w: %v", ErrConflict, err)
		}
		info, err = storedInfo, nil
	}
	if err != nil {
		return Manifest{}, "", err
	}
	dropUnnamed(ctx, st, prefix, prev, m)
	return m, info.ETag, nil
}

func readObject(ctx context.Context, st Store, key string) ([]byte, ObjectInfo, error) {
	rc, info, err := st.Get(ctx, key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	return b, info, err
}

func dropUnnamed(ctx context.Context, st Store, prefix string, prev *Manifest, m Manifest) {
	if prev == nil {
		return
	}
	handled := map[string]bool{}
	for _, e := range m.Files {
		handled[e.Object] = true
	}
	for _, e := range prev.Files {
		if !handled[e.Object] {
			handled[e.Object] = true
			_ = st.Delete(ctx, prefix+e.Object)
		}
	}
}

func localPath(dir, rel string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("worldsync: manifest path %q leaves the world", rel)
	}
	return filepath.Join(dir, filepath.FromSlash(rel)), nil
}

// place writes r to path through a temporary file, then sets mode and mtime.
func place(path string, r io.Reader, mode uint32, mtime int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".worldsync-part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	placed := false
	defer func() {
		if !placed {
			os.Remove(tmp)
		}
	}()
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if mode != 0 {
		if err := os.Chmod(tmp, os.FileMode(mode)); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	placed = true
	t := time.Unix(0, mtime)
	return os.Chtimes(path, t, t)
}

func Download(ctx context.Context, st Store, prefix, dataDir string, m Manifest, parallel int) error {
	packs := map[string]map[string]FileEntry{}
	for _, e := range m.Files {
		if _, err := localPath(dataDir, e.Path); err != nil {
			return err
		}
		if e.Size < PackBelow {
			if packs[e.Object] == nil {
				packs[e.Object] = map[string]FileEntry{}
			}
			packs[e.Object][e.Path] = e
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for pack, want := range packs {
		g.Go(func() error { return extractPack(gctx, st, prefix+pack, dataDir, want) })
	}
	for _, e := range m.Files {
		if e.Size < PackBelow {
			continue
		}
		g.Go(func() error {
			rc, _, err := st.Get(gctx, prefix+e.Object)
			if err != nil {
				return fmt.Errorf("get %s for %s: %w", e.Object, e.Path, err)
			}
			defer rc.Close()
			dst, _ := localPath(dataDir, e.Path)
			return place(dst, rc, e.Mode, e.MTime)
		})
	}
	return g.Wait()
}

func extractPack(ctx context.Context, st Store, key, dataDir string, want map[string]FileEntry) error {
	rc, _, err := st.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get %s: %w", key, err)
	}
	defer rc.Close()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	placed := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			if placed != len(want) {
				return fmt.Errorf("worldsync: %s holds %d of the %d files the manifest names", key, placed, len(want))
			}
			return nil
		}
		if err != nil {
			return err
		}
		e, ok := want[h.Name]
		if !ok {
			continue
		}
		dst, err := localPath(dataDir, h.Name)
		if err != nil {
			return err
		}
		if err := place(dst, tr, e.Mode, e.MTime); err != nil {
			return err
		}
		placed++
	}
}
