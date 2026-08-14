/*
Copyright The Spawnery Authors.

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

package controller

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// readinessDivergence tracks, per ProxyGroup, how long each of its pods'
// actual readiness has disagreed with the readiness the operator most
// recently asserted for it.
//
// Scoped per group by the same byGroup shape as expectations, and for the
// same reason: one instance is shared across every ProxyGroup this
// reconciler serves, so a bare map keyed on pod UID alone would have no way
// to tell "this pod is gone" from "this pod belongs to some other group I
// have not looked at this pass." observe is always given the complete live
// pod list of the one group it is reconciling, so an entry that pass does
// not renew -- because the pod agreed again, or because the pod is no
// longer in the list at all -- is exactly the entry that must not survive
// it.
//
// An entry measures how long a pod has been diverging *while something was
// watching*. Observation is what starts and sustains that clock, and
// Reconcile does not call observe on every pass for a group that still
// exists: NetworkNotFound, NetworkNotAccepted and ExposeNotImplemented all
// return before reconcileReplicas runs. The group is not gone in any of
// those three cases, so it is not caught by the earlier forget calls in
// Reconcile that fire only when the ProxyGroup itself is gone or being
// deleted -- each of the three calls forget explicitly on its own instead.
// Unlike expectations this needs no TTL to cover a gap like this one: a TTL
// would let a stale first-seen timestamp survive the gap and then fire the
// moment observation resumes, reporting a span of divergence nobody
// actually watched. Forgetting is the honest response -- the measurement is
// void, so it restarts -- and the cost is bounded in the safe direction: a
// Network briefly unaccepted delays a genuine report by at most one grace
// period.
//
// Safe for concurrent use: one instance is shared by every reconcile of
// every group, the same guarantee expectations makes.
type readinessDivergence struct {
	mu      sync.Mutex
	now     func() time.Time
	byGroup map[string]map[types.UID]time.Time
}

func newReadinessDivergence(now func() time.Time) *readinessDivergence {
	return &readinessDivergence{now: now, byGroup: make(map[string]map[types.UID]time.Time)}
}

// observe records the current agreement for every pod in diverging -- true
// for a pod whose actual readiness disagrees with what was just asserted for
// it, false for one that agrees -- and returns the UIDs of every pod that
// has disagreed for at least grace, continuously: a pod past the threshold
// is returned on every call from the one that first crosses it onward, not
// only the call that crosses it, because the caller's flank detection is
// what turns that into a one-time event.
//
// A pod absent from diverging is dropped the same as one reported false.
// diverging is built from the group's complete live pod list on every call,
// so absence here means the pod is no longer there to have a readiness at
// all, not merely that this pass forgot to check it.
func (d *readinessDivergence) observe(group string, diverging map[types.UID]bool, grace time.Duration) []types.UID {
	d.mu.Lock()
	defer d.mu.Unlock()

	m := d.byGroup[group]
	now := d.now()
	var expired []types.UID
	for uid, mismatched := range diverging {
		if !mismatched {
			if m != nil {
				delete(m, uid)
			}
			continue
		}
		if m == nil {
			m = make(map[types.UID]time.Time)
			d.byGroup[group] = m
		}
		since, tracked := m[uid]
		if !tracked {
			m[uid] = now
			continue
		}
		if now.Sub(since) >= grace {
			expired = append(expired, uid)
		}
	}
	// Anything this group was tracking that diverging did not even mention
	// is a pod that left the live list entirely between one call and the
	// next; nothing is left to report a readiness for, so its entry goes
	// too rather than sitting in the map for the rest of the process's life.
	for uid := range m {
		if _, present := diverging[uid]; !present {
			delete(m, uid)
		}
	}
	if len(m) == 0 {
		delete(d.byGroup, group)
	}
	return expired
}

// forget drops a group entirely, matching expectations.forget: a group
// deleted while one of its pods was mid-divergence has no later observe call
// coming to prune that entry on its own, since observe only ever prunes a
// group's map when it is given that same group's current pod list, and a
// deleted group never produces one again.
func (d *readinessDivergence) forget(group string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.byGroup, group)
}
