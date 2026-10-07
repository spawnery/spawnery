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
	"errors"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

func MarkDeleted(ctx context.Context, st Store, base, world string) error {
	_, err := st.Put(ctx, DeletionKey(base, world), bytes.NewReader(nil), PutCondition{})
	return err
}

func exists(ctx context.Context, st Store, key string) (bool, error) {
	_, err := st.Head(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func DeletionPending(ctx context.Context, st Store, base, world string) (bool, error) {
	return exists(ctx, st, DeletionKey(base, world))
}

func WorldExists(ctx context.Context, st Store, base, world string) (bool, error) {
	return exists(ctx, st, WorldPrefix(base, world)+ManifestName)
}

// BucketWorlds is agentserver.WorldDeleter over a Store.
type BucketWorlds struct {
	Store Store
	Base  string
}

func (b BucketWorlds) Exists(ctx context.Context, world string) (bool, error) {
	return WorldExists(ctx, b.Store, b.Base, world)
}

func (b BucketWorlds) MarkDeleted(ctx context.Context, world string) error {
	return MarkDeleted(ctx, b.Store, b.Base, world)
}

func (b BucketWorlds) DeletionPending(ctx context.Context, world string) (bool, error) {
	return DeletionPending(ctx, b.Store, b.Base, world)
}

// Sweeper deletes worlds marked by MarkDeleted once no node holds them.
type Sweeper struct {
	Store      Store
	Base       string
	Interval   time.Duration
	StaleAfter time.Duration
	Log        logr.Logger
}

func (s *Sweeper) NeedLeaderElection() bool { return true }

func (s *Sweeper) Start(ctx context.Context) error {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if err := s.SweepOnce(ctx); err != nil {
			s.Log.Error(err, "world deletion sweep failed; retrying next interval")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (s *Sweeper) SweepOnce(ctx context.Context) error {
	markers, err := s.Store.List(ctx, DeletionPrefix(s.Base))
	if err != nil {
		return err
	}
	var errs []error
	for _, marker := range markers {
		world := strings.TrimPrefix(marker, DeletionPrefix(s.Base))
		if err := checkWorld(world); err != nil {
			s.Log.Info("skipping a deletion marker that names no world", "marker", marker)
			continue
		}
		if err := s.sweep(ctx, world, marker); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Sweeper) sweep(ctx context.Context, world, marker string) error {
	prefix := WorldPrefix(s.Base, world)
	l, info, err := ReadLease(ctx, s.Store, prefix)
	switch {
	case err == nil:
		held, err := leaseHeld(l, info, s.StaleAfter)
		if err != nil || held {
			return err
		}
	case !errors.Is(err, ErrNotFound):
		return err
	}
	keys, err := s.Store.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k == prefix+ManifestName {
			continue // last but one: a world without manifest is gone for the node agent
		}
		if err := s.Store.Delete(ctx, k); err != nil {
			return err
		}
	}
	if err := s.Store.Delete(ctx, prefix+ManifestName); err != nil {
		return err
	}
	s.Log.Info("deleted world", "world", world, "objects", len(keys))
	return s.Store.Delete(ctx, marker)
}
