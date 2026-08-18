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
// exists. Two of its steady-state early returns handle that themselves --
// NetworkNotFound and NetworkNotAccepted both return before reconcileReplicas
// runs, the group is not gone in either of them so the earlier forget calls
// (which fire only when the ProxyGroup itself is gone or being deleted) do
// not catch them, and each calls forget explicitly instead. ExposeNotImplemented
// shares the same path and the same forget call, but the CRD's enum is closed
// and exposeImplemented agrees with it, so no object reaching this reconciler
// can take that branch; it is named here for the reader who greps for the
// reason, not as a case this steady state actually sees.
//
// Read that as two cases handled, not as the whole of the gap. Every error
// return above reconcileReplicas has the identical shape and does not forget
// -- a failed read, the status write, Bootstrap.Ensure, the ConfigMap, the
// Service, the first pods() call -- and the next early return added here will
// not forget either unless somebody remembers this rule. known-issues.md
// files the count and declines to maintain it, which is the right way round.
//
// What that costs is worth stating exactly, because it is not only an entry
// that outlives its usefulness. Nothing advances since: it is written once,
// when the pod is first seen diverging, and read on every later call. So a
// pod still diverging across a five-minute Bootstrap.Ensure outage is
// measured from before the outage, now.Sub(since) is 300s on the first pass
// that resumes, the grace is cleared on that pass, and a Warning fires for a
// divergence nobody watched. That is a false positive, and it is precisely
// the outcome rejecting a TTL was meant to prevent -- a TTL would let a stale
// first-seen timestamp survive a gap and fire the instant observation
// resumes -- arriving through the door the un-forgotten returns leave open.
//
// Forgetting is the honest response where it is done: the measurement is
// void, so it restarts, and the cost runs the safe way -- a Network briefly
// unaccepted delays a genuine report by at most one grace period. The fix
// that would close the rest is structural rather than more forget calls, and
// known-issues.md carries it: have the entry record when it was last
// observed, and treat one unobserved on the previous pass as void. The type
// would then enforce the property instead of each exit remembering to.
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
