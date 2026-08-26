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
// watching*, and it is the type that enforces that rather than the caller.
// Each entry carries when it was last observed as well as when it first
// diverged, and an entry not observed for longer than divergenceObservationStep
// is void: the next observation restarts it. Time nothing watched is time this
// measurement does not spend.
//
// That rule exists because Reconcile does not call observe on every pass for a
// group that still exists. Every error return above reconcileReplicas returns
// without it -- a failed ProxyGroup or Network read, the status write,
// Bootstrap.Ensure, the ConfigMap, the Service, the first pods() call -- and
// so will the next early return somebody adds. Before the rule, an entry stored
// only when the divergence was first seen and nothing advanced it, so a pod
// still diverging across a five-minute Bootstrap.Ensure outage was measured
// from before the outage: now.Sub(since) was 300s on the first pass that
// resumed, the grace was cleared on that pass, and a Warning fired for a
// divergence nobody had watched.
//
// The cost of the rule is the same one forgetting has always had and it runs
// the safe way: a genuine, continuous divergence that spans a gap restarts its
// clock and reports up to one grace period later than it might have. The
// failure mode of getting divergenceObservationStep *too small* is the quiet
// one -- ordinary jitter would void a real divergence on every pass and it
// would never report -- which is why that constant is four resync intervals
// against a sixty-second grace rather than something tight.
//
// NetworkNotFound and NetworkNotAccepted still call forget explicitly, and
// that is now belt to this braces rather than the mechanism: those two paths
// know the measurement is void a pass earlier than the gap rule would work it
// out, and saying so where it is known costs nothing. ExposeNotImplemented
// shares the same path and the same call; the CRD's enum is closed and
// exposeImplemented agrees with it, so no object reaching this reconciler can
// take that branch. It is named here for the reader who greps for the reason.
//
// Safe for concurrent use: one instance is shared by every reconcile of
// every group, the same guarantee expectations makes.
type readinessDivergence struct {
	mu      sync.Mutex
	now     func() time.Time
	byGroup map[string]map[types.UID]divergenceEntry
}

// divergenceEntry accumulates watched time rather than storing a start.
// watched only ever grows by what one pass can account for, so wall-clock time
// during which nothing observed this group cannot enter it.
type divergenceEntry struct {
	watched      time.Duration
	lastObserved time.Time
}

// divergenceObservationStep is the most a single pass may contribute.
//
// A ProxyGroup requeues every ResyncInterval on its successful path, so in
// steady state each pass contributes that; four times it absorbs a slow pass
// and scheduler jitter. What it bounds is the other case: an outage during
// which nothing observed, after which the first pass back would otherwise
// contribute the whole outage.
//
// Capping rather than discarding is deliberate, and the alternative was tried
// first. Voiding an entry whose last observation was too long ago has a failure
// mode that is silent: if passes ever drift further apart than the bound --
// a loaded operator, a raised resync interval -- every entry is voided on every
// pass and a real divergence is never reported at all. Capping has no such
// cliff. Passes 25 seconds apart still contribute 20 each, so the report
// arrives late rather than never, and the worst an outage can do is overcount
// by one step against a sixty-second grace instead of by its whole length.
const divergenceObservationStep = 4 * ResyncInterval

func newReadinessDivergence(now func() time.Time) *readinessDivergence {
	return &readinessDivergence{now: now, byGroup: make(map[string]map[types.UID]divergenceEntry)}
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
			m = make(map[types.UID]divergenceEntry)
			d.byGroup[group] = m
		}
		e, tracked := m[uid]
		if !tracked {
			// The pass that first sees a divergence has watched none of it
			// yet, so it contributes nothing and cannot report.
			m[uid] = divergenceEntry{lastObserved: now}
			continue
		}
		// The whole point of this type is on this line: a pass accounts for
		// the time since the previous pass, and for no more than one pass's
		// worth of it. Time during which nothing looked at this group cannot
		// enter the measurement, however much of it there was.
		step := now.Sub(e.lastObserved)
		if step > divergenceObservationStep {
			step = divergenceObservationStep
		}
		e.watched += step
		e.lastObserved = now
		m[uid] = e
		if e.watched >= grace {
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
