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
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

type ObjectCache map[string][]string

type PruneRequest struct {
	Current  Manifest
	Replaced *Manifest
	Policy   Retention
	Cache    ObjectCache
	Sweep    bool
}

// Prune runs under the world's lease, right after a manifest commit. The
// sweep is safe only there: no other writer can have uploaded an object
// whose manifest is still to come.
func Prune(ctx context.Context, st Store, prefix string, req PruneRequest) (int, error) {
	entries, err := ListHistory(ctx, st, prefix)
	if err != nil {
		return 0, err
	}
	points := []Point{{Generation: req.Current.Generation, Taken: req.Current.TakenAt(time.Time{})}}
	var older, kept, dropped []HistoryEntry
	for _, e := range entries {
		if e.Generation >= req.Current.Generation {
			dropped = append(dropped, e)
			continue
		}
		older = append(older, e)
		points = append(points, e.Point)
	}
	keep := req.Policy.Select(points)
	for i, e := range older {
		if keep[i+1] {
			kept = append(kept, e)
		} else {
			dropped = append(dropped, e)
		}
	}

	cache := req.Cache
	if cache == nil {
		cache = ObjectCache{}
	}
	keptNames, err := entryObjects(ctx, st, kept, cache)
	if err != nil {
		return 0, err
	}
	droppedNames, err := entryObjects(ctx, st, dropped, cache)
	if err != nil {
		return 0, err
	}
	named := map[string]bool{}
	for _, f := range req.Current.Files {
		named[f.Object] = true
	}
	for _, objs := range keptNames {
		for _, o := range objs {
			named[o] = true
		}
	}
	candidates := map[string]bool{}
	if req.Replaced != nil {
		for _, f := range req.Replaced.Files {
			candidates[f.Object] = true
		}
	}
	for _, objs := range droppedNames {
		for _, o := range objs {
			candidates[o] = true
		}
	}

	// Entries first: an entry whose objects are gone would restore a broken world.
	var errs []error
	for _, e := range dropped {
		if err := st.Delete(ctx, e.Key); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(cache, e.Key)
	}
	if len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	deleted := 0
	for o := range candidates {
		if named[o] {
			continue
		}
		if err := st.Delete(ctx, prefix+o); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted++
	}
	if req.Sweep {
		for _, dir := range []string{ObjectsDir, PacksDir} {
			keys, err := st.List(ctx, prefix+dir)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, k := range keys {
				if named[strings.TrimPrefix(k, prefix)] {
					continue
				}
				if err := st.Delete(ctx, k); err != nil {
					errs = append(errs, err)
					continue
				}
				deleted++
			}
		}
	}
	keptKeys := map[string]bool{}
	for _, e := range kept {
		keptKeys[e.Key] = true
	}
	for k := range cache {
		if !keptKeys[k] {
			delete(cache, k)
		}
	}
	return deleted, errors.Join(errs...)
}

// entryObjects skips an entry that is gone: nothing can restore it, and
// nothing it named is kept by it.
func entryObjects(ctx context.Context, st Store, entries []HistoryEntry, cache ObjectCache) (map[string][]string, error) {
	out := make(map[string][]string, len(entries))
	var missing []HistoryEntry
	for _, e := range entries {
		if objs, ok := cache[e.Key]; ok {
			out[e.Key] = objs
		} else {
			missing = append(missing, e)
		}
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadParallel)
	for _, e := range missing {
		g.Go(func() error {
			m, err := readHistoryEntry(gctx, st, e.Key)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			var objs []string
			for _, f := range m.Files {
				if !seen[f.Object] {
					seen[f.Object] = true
					objs = append(objs, f.Object)
				}
			}
			mu.Lock()
			out[e.Key], cache[e.Key] = objs, objs
			mu.Unlock()
			return nil
		})
	}
	return out, g.Wait()
}
