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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// Ager applies MaxAgeDays to worlds nobody plays, which no upload prunes.
type Ager struct {
	Store      Store
	Base       string
	Policies   *Policies
	Interval   time.Duration
	StaleAfter time.Duration
	Clock      func() time.Time
	Log        logr.Logger
}

func (a *Ager) NeedLeaderElection() bool { return true }

// Start waits one interval before the first pass: Policies learns the groups
// from the ServerGroup reconciler, which has passed over each by then.
func (a *Ager) Start(ctx context.Context) error {
	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := a.AgeOnce(ctx); err != nil {
			a.Log.Error(err, "world history aging failed; retrying next interval")
		}
	}
}

func (a *Ager) AgeOnce(ctx context.Context) error {
	now := a.Clock()
	var errs []error
	for group, policy := range a.Policies.aging() {
		if err := a.ageGroup(ctx, group, policy, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *Ager) ageGroup(ctx context.Context, group string, policy Retention, now time.Time) error {
	groupPrefix := WorldPrefix(a.Base, group)
	keys, err := a.Store.List(ctx, groupPrefix)
	if err != nil {
		return err
	}
	history := map[string][]Point{}
	for _, k := range keys {
		key, rest, ok := strings.Cut(strings.TrimPrefix(k, groupPrefix), "/")
		if !ok {
			continue
		}
		if p, ok := ParseHistoryKey(rest); ok {
			history[key] = append(history[key], p)
		}
	}
	var errs []error
	for key, points := range history {
		sort.SliceStable(points, func(i, j int) bool { return points[i].Generation > points[j].Generation })
		// The current generation is not in the listing, and was taken no
		// earlier than the newest entry.
		bound := append([]Point{{Taken: points[0].Taken}}, points...)
		if !slices.Contains(policy.Expired(bound, now), true) {
			continue
		}
		world := group + "/" + key
		if checkWorld(world) != nil {
			continue
		}
		deleted, err := a.ageWorld(ctx, world, points, policy, now)
		agedObjects.Add(float64(deleted))
		if err != nil {
			errs = append(errs, fmt.Errorf("world %s: %w", world, err))
		} else if deleted > 0 {
			a.Log.Info("aged out world history", "world", world, "objects", deleted)
		}
	}
	return errors.Join(errs...)
}

func (a *Ager) ageWorld(ctx context.Context, world string, points []Point, policy Retention, now time.Time) (int, error) {
	prefix := WorldPrefix(a.Base, world)
	if pending, err := DeletionPending(ctx, a.Store, a.Base, world); err != nil || pending {
		return 0, err
	}
	if held, err := LeaseIsHeld(ctx, a.Store, prefix, a.StaleAfter); err != nil || held {
		return 0, err
	}
	cur, _, err := ReadManifest(ctx, a.Store, prefix)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !expiresAny(policy, cur, points, now) {
		return 0, nil
	}

	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return 0, err
	}
	me := Lease{Node: OperatorLeaseNode + "/" + hex.EncodeToString(id), Pod: "aging", RenewedAt: now}
	leaseETag, err := TakeLease(ctx, a.Store, prefix, me, a.StaleAfter)
	var held *HeldError
	if errors.As(err, &held) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = ReleaseLease(context.WithoutCancel(ctx), a.Store, prefix, leaseETag) }()

	cur, _, err = ReadManifest(ctx, a.Store, prefix)
	if err != nil {
		return 0, err
	}
	return Prune(ctx, a.Store, prefix, PruneRequest{Current: cur, Policy: policy, Pin: restorePin(cur), Now: now})
}

// restorePin is the generation RestoreWorld pinned: the one it replaced. The
// member's next upload drops RestoredFrom and with it the pin.
func restorePin(cur Manifest) int64 {
	if cur.RestoredFrom == 0 {
		return 0
	}
	return cur.Generation - 1
}

func expiresAny(policy Retention, cur Manifest, points []Point, now time.Time) bool {
	all := []Point{{Generation: cur.Generation, Taken: cur.TakenAt(time.Time{})}}
	for _, p := range points {
		if p.Generation < cur.Generation {
			all = append(all, p)
		}
	}
	pin := restorePin(cur)
	for i, e := range policy.Expired(all, now) {
		if e && all[i].Generation != pin {
			return true
		}
	}
	return false
}
