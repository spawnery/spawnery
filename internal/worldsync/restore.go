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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const OperatorLeaseNode = "spawnery-operator"

var (
	ErrNoGeneration      = errors.New("worldsync: the world keeps no such generation")
	ErrCurrentGeneration = errors.New("worldsync: that generation is the current one")
)

type RestorePoint struct {
	Generation int64
	Taken      time.Time
	Current    bool
}

type Restored struct {
	Generation    int64
	RestoredFrom  int64
	RestoredTaken time.Time
	// PruneErr: the restore committed and the prune after it failed; the
	// world's next prune catches up.
	PruneErr error
}

// ListRestorePoints leaves out an entry at or above the current generation:
// it is a crash's copy of the current one.
func ListRestorePoints(ctx context.Context, st Store, prefix string) ([]RestorePoint, error) {
	_, cur, info, err := readCurrent(ctx, st, prefix)
	if err != nil {
		return nil, err
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil {
		return nil, err
	}
	points := []RestorePoint{{Generation: cur.Generation, Taken: cur.TakenAt(info.LastModified), Current: true}}
	for _, e := range entries {
		if e.Generation < cur.Generation {
			points = append(points, RestorePoint{Generation: e.Generation, Taken: e.Taken})
		}
	}
	return points, nil
}

// RestoreWorld writes the old files as generation current+1: reusing the
// old number would let a node holding another copy under it count that copy
// as current.
func RestoreWorld(ctx context.Context, st Store, prefix string, generation int64, policy Retention, now time.Time) (Restored, error) {
	if _, err := st.Head(ctx, prefix+ManifestName); err != nil {
		return Restored{}, err
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return Restored{}, err
	}
	// TakeLease re-takes a lease held under its own node name, so a name
	// shared by all restores would let a second one steal and then release
	// the first one's lease.
	me := Lease{Node: OperatorLeaseNode + "/" + hex.EncodeToString(id), Pod: "restore", RenewedAt: now}
	leaseETag, err := TakeLease(ctx, st, prefix, me, StaleAfter)
	if err != nil {
		return Restored{}, err
	}
	defer func() { _ = ReleaseLease(context.WithoutCancel(ctx), st, prefix, leaseETag) }()

	raw, cur, info, err := readCurrent(ctx, st, prefix)
	if err != nil {
		return Restored{}, err
	}
	if generation == cur.Generation {
		return Restored{}, ErrCurrentGeneration
	}
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil {
		return Restored{}, err
	}
	var from *HistoryEntry
	for i := range entries {
		if entries[i].Generation == generation && generation < cur.Generation {
			from = &entries[i]
			break
		}
	}
	if from == nil {
		return Restored{}, ErrNoGeneration
	}
	old, err := readHistoryEntry(ctx, st, from.Key)
	if errors.Is(err, ErrNotFound) {
		return Restored{}, ErrNoGeneration
	}
	if err != nil {
		return Restored{}, err
	}
	if old.WorldID != cur.WorldID {
		return Restored{}, ErrNoGeneration
	}

	_, err = st.Put(ctx, prefix+HistoryKey(cur.Generation, cur.TakenAt(info.LastModified)), bytes.NewReader(raw), PutCondition{IfNoneMatch: true})
	if err != nil && !errors.Is(err, ErrPrecondition) {
		return Restored{}, err
	}
	next := Manifest{WorldID: cur.WorldID, Generation: cur.Generation + 1, Taken: now.UnixMilli(), RestoredFrom: generation, Files: old.Files}
	body, err := json.Marshal(next)
	if err != nil {
		return Restored{}, err
	}
	if _, err := putManifest(ctx, st, prefix, body, PutCondition{IfMatch: info.ETag}); err != nil {
		return Restored{}, err
	}
	// Pinned so that the restore can be undone before the next start.
	_, perr := Prune(ctx, st, prefix, PruneRequest{Current: next, Policy: policy, Pin: cur.Generation})
	return Restored{Generation: next.Generation, RestoredFrom: generation, RestoredTaken: from.Taken, PruneErr: perr}, nil
}

func (b BucketWorlds) RestorePoints(ctx context.Context, world string) ([]RestorePoint, error) {
	return ListRestorePoints(ctx, b.Store, WorldPrefix(b.Base, world))
}

func (b BucketWorlds) Restore(ctx context.Context, world string, generation int64, policy Retention) (Restored, error) {
	return RestoreWorld(ctx, b.Store, WorldPrefix(b.Base, world), generation, policy, time.Now())
}
