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
	"encoding/json"
	"fmt"
	"time"
)

type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	MTime  int64  `json:"mtime"`
	Object string `json:"object"`
}

type Manifest struct {
	WorldID    string `json:"worldId"`
	Generation int64  `json:"generation"`
	// Taken is the node's clock in unix milliseconds when it took the
	// snapshot; manifests written before it existed have none.
	Taken        int64 `json:"taken,omitempty"`
	RestoredFrom int64 `json:"restoredFrom,omitempty"`
	// RestoredTaken is the Taken of RestoredFrom; restores before it existed
	// wrote none.
	RestoredTaken int64       `json:"restoredTaken,omitempty"`
	Files         []FileEntry `json:"files"`
}

func (m Manifest) TakenAt(lastModified time.Time) time.Time {
	if m.Taken > 0 {
		return time.UnixMilli(m.Taken).UTC()
	}
	return lastModified.UTC()
}

// readCurrent returns the manifest's bytes too, for a copy into history/.
func readCurrent(ctx context.Context, st Store, prefix string) ([]byte, Manifest, ObjectInfo, error) {
	b, info, err := readObject(ctx, st, prefix+ManifestName)
	if err != nil {
		return nil, Manifest{}, ObjectInfo{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, Manifest{}, ObjectInfo{}, fmt.Errorf("decode %s%s: %w", prefix, ManifestName, err)
	}
	return b, m, info, nil
}

func ReadManifest(ctx context.Context, st Store, prefix string) (Manifest, string, error) {
	rc, info, err := st.Get(ctx, prefix+ManifestName)
	if err != nil {
		return Manifest{}, "", err
	}
	defer func() { _ = rc.Close() }()
	var m Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		return Manifest{}, "", fmt.Errorf("decode %s%s: %w", prefix, ManifestName, err)
	}
	return m, info.ETag, nil
}
