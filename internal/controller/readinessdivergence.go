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

package controller

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// readinessDivergence tracks, per ProxyGroup, how long each pod's actual
// readiness has disagreed with the readiness the operator last asserted.
// It counts only watched time: Reconcile skips observe on its early error
// returns, and an outage must not count as divergence somebody saw.
type readinessDivergence struct {
	mu      sync.Mutex
	now     func() time.Time
	byGroup map[string]map[types.UID]divergenceEntry
}

type divergenceEntry struct {
	watched      time.Duration
	lastObserved time.Time
}

// divergenceObservationStep caps what one pass may contribute. Capping rather
// than voiding stale entries keeps a real divergence reportable when passes
// drift further apart; four resyncs absorb a slow pass and jitter.
const divergenceObservationStep = 4 * ResyncInterval

func newReadinessDivergence(now func() time.Time) *readinessDivergence {
	return &readinessDivergence{now: now, byGroup: make(map[string]map[types.UID]divergenceEntry)}
}

// observe returns every pod diverging for at least grace, on every call from
// the crossing onward; the caller's flank detection makes it a one-time event.
// diverging must cover the group's complete live pod list: absent pods are dropped.
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
			m[uid] = divergenceEntry{lastObserved: now}
			continue
		}
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

func (d *readinessDivergence) forget(group string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.byGroup, group)
}
