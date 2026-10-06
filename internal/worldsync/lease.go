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
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const StaleAfter = 10 * time.Minute

const takeAttempts = 3

var ErrLeaseLost = errors.New("worldsync: the lease is no longer this node's")

type Lease struct {
	Node      string    `json:"node"`
	Pod       string    `json:"pod"`
	RenewedAt time.Time `json:"renewedAt"`
}

type HeldError struct{ Node string }

func (e *HeldError) Error() string { return "worldsync: the world is held by node " + e.Node }

func ReadLease(ctx context.Context, st Store, prefix string) (Lease, ObjectInfo, error) {
	rc, info, err := st.Get(ctx, prefix+LeaseName)
	if err != nil {
		return Lease{}, ObjectInfo{}, err
	}
	defer rc.Close()
	var l Lease
	if err := json.NewDecoder(rc).Decode(&l); err != nil {
		return Lease{}, ObjectInfo{}, fmt.Errorf("decode lease: %w", err)
	}
	return l, info, nil
}

func putLease(ctx context.Context, st Store, prefix string, l Lease, cond PutCondition) (string, error) {
	b, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	info, err := st.Put(ctx, prefix+LeaseName, bytes.NewReader(b), cond)
	if err != nil {
		return "", err
	}
	return info.ETag, nil
}

// TakeLease creates the lease, or rewrites it when this node holds it or
// when it is older than staleAfter by the store's own clock.
func TakeLease(ctx context.Context, st Store, prefix string, me Lease, staleAfter time.Duration) (string, error) {
	for range takeAttempts {
		etag, err := putLease(ctx, st, prefix, me, PutCondition{IfNoneMatch: true})
		if !errors.Is(err, ErrPrecondition) {
			return etag, err
		}
		cur, info, err := ReadLease(ctx, st, prefix)
		if errors.Is(err, ErrNotFound) {
			continue // released in between
		}
		if err != nil {
			return "", err
		}
		if cur.Node != me.Node && info.Date.Sub(cur.RenewedAt) <= staleAfter {
			return "", &HeldError{Node: cur.Node}
		}
		etag, err = putLease(ctx, st, prefix, me, PutCondition{IfMatch: info.ETag})
		if errors.Is(err, ErrPrecondition) {
			return "", &HeldError{Node: cur.Node}
		}
		return etag, err
	}
	return "", fmt.Errorf("worldsync: the lease kept changing while taking it")
}

func RenewLease(ctx context.Context, st Store, prefix string, me Lease, etag string) (string, error) {
	next, err := putLease(ctx, st, prefix, me, PutCondition{IfMatch: etag})
	if errors.Is(err, ErrPrecondition) {
		return "", ErrLeaseLost
	}
	return next, err
}

// ReleaseLease deletes the lease if it is still the one with etag. The check
// and the delete are two requests; a takeover needs ten minutes without a
// renewal, which a holder that is releasing has just renewed.
func ReleaseLease(ctx context.Context, st Store, prefix, etag string) error {
	info, err := st.Head(ctx, prefix+LeaseName)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.ETag != etag {
		return ErrLeaseLost
	}
	return st.Delete(ctx, prefix+LeaseName)
}
