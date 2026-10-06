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
	m := Manifest{Generation: 1}
	if prev != nil {
		m.WorldID, m.Generation = prev.WorldID, prev.Generation+1
	} else {
		m.WorldID = newWorldID()
	}
	pack := PacksDir + strconv.FormatInt(m.Generation, 10) + ".tar.gz"

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
	if errors.Is(err, ErrPrecondition) {
		return Manifest{}, "", fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if err != nil {
		return Manifest{}, "", err
	}

	if prev != nil {
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
	return m, info.ETag, nil
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
	t := time.Unix(0, mtime)
	return os.Chtimes(path, t, t)
}

func Download(ctx context.Context, st Store, prefix, dataDir string, m Manifest, parallel int) error {
	byPath := map[string]FileEntry{}
	packs := map[string]bool{}
	for _, e := range m.Files {
		if _, err := localPath(dataDir, e.Path); err != nil {
			return err
		}
		byPath[e.Path] = e
		if e.Size < PackBelow {
			packs[e.Object] = true
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for pack := range packs {
		g.Go(func() error { return extractPack(gctx, st, prefix+pack, dataDir, byPath) })
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

func extractPack(ctx context.Context, st Store, key, dataDir string, byPath map[string]FileEntry) error {
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
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		e, ok := byPath[h.Name]
		if !ok {
			continue // a pack may hold files an older manifest named
		}
		dst, err := localPath(dataDir, h.Name)
		if err != nil {
			return err
		}
		if err := place(dst, tr, e.Mode, e.MTime); err != nil {
			return err
		}
	}
}
