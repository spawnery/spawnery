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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func newTestDivergence() (*readinessDivergence, *testClock) {
	clock := &testClock{now: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
	return newReadinessDivergence(clock.Now), clock
}

const testGrace = 60 * time.Second

// pass is one steady-state reconcile of one group: the clock moves by the
// resync interval and observe is called with the group's whole live pod list.
// Written as a helper because the property under test is about *sequences* of
// passes, and a test that spelled each one out would bury that in noise.
func pass(d *readinessDivergence, clock *testClock, group string, diverging map[types.UID]bool) []types.UID {
	clock.Advance(ResyncInterval)
	return d.observe(group, diverging, testGrace)
}

func TestADivergenceReportsOnceItHasBeenWatchedForTheWholeGrace(t *testing.T) {
	d, clock := newTestDivergence()
	diverging := map[types.UID]bool{"pod-a": true}

	// The steady state: a pass every ResyncInterval. Nothing may report before
	// the grace has actually elapsed under observation.
	elapsed := time.Duration(0)
	for elapsed < testGrace {
		if stale := pass(d, clock, "ns/gateway", diverging); len(stale) != 0 {
			t.Fatalf("reported %v after %s of a %s grace", stale, elapsed, testGrace)
		}
		elapsed += ResyncInterval
	}

	if stale := pass(d, clock, "ns/gateway", diverging); len(stale) != 1 || stale[0] != "pod-a" {
		t.Errorf("stale = %v after %s of continuous observation, want [pod-a]", stale, elapsed+ResyncInterval)
	}
}

// TestAGapInObservationCannotBeSpentOnTheGrace is the defect
// docs/known-issues.md files under milestone 4c-2 as "a deferred structural fix
// to readinessDivergence".
//
// An entry measures how long a pod has diverged *while something was watching*.
// Reconcile does not call observe on every pass: every error return above
// reconcileReplicas — a failed read, the status write, Bootstrap.Ensure, the
// ConfigMap, the Service, the first pods() call — returns without it. Since the
// entry stored only when the divergence was first seen and nothing advanced it,
// a pod still diverging across a five-minute outage was measured from before
// the outage, and the first pass that resumed found the whole grace already
// elapsed and fired a Warning about a stretch nobody watched.
//
// The two forget calls on the NetworkNotFound and NetworkNotAccepted paths
// handled two exits by hand. The cap handles all of them, including the ones
// nobody has written yet.
func TestAGapInObservationCannotBeSpentOnTheGrace(t *testing.T) {
	d, clock := newTestDivergence()
	diverging := map[types.UID]bool{"pod-a": true}

	// Seen diverging once, then observation stops: Bootstrap.Ensure is failing
	// and every pass returns before reportReadinessDivergence.
	if stale := pass(d, clock, "ns/gateway", diverging); len(stale) != 0 {
		t.Fatalf("reported on the very first observation: %v", stale)
	}
	clock.Advance(5 * time.Minute)

	// The pass that resumes may account for one pass's worth of that gap and no
	// more. Reporting here would be a Warning about five minutes nobody saw.
	if stale := pass(d, clock, "ns/gateway", diverging); len(stale) != 0 {
		t.Errorf("stale = %v on the first pass after a five-minute gap in observation. "+
			"The grace measures watched time, and nothing watched this pod for those "+
			"five minutes", stale)
	}

	// It must also not abandon what it had. Count the passes it takes from here
	// and check the watched time against what the constants say it should be:
	// the gap contributed at most one step, so what remains is a grace minus
	// that, and the answer must fall in that window rather than at either edge
	// of it. Derived from the constants rather than fitted to the answer, so
	// changing either constant moves the expectation with it.
	watched := divergenceObservationStep // what the resuming pass could add
	for range int(testGrace / ResyncInterval * 2) {
		stale := pass(d, clock, "ns/gateway", diverging)
		if len(stale) > 0 {
			break
		}
		watched += ResyncInterval
	}
	if watched < testGrace-divergenceObservationStep || watched > testGrace {
		t.Errorf("reported after %s of accountable watched time; want it inside "+
			"[%s, %s] — earlier means the gap was spent on the grace, later means "+
			"the measurement was thrown away rather than paused",
			watched, testGrace-divergenceObservationStep, testGrace)
	}
}

// TestPassesFurtherApartThanOneStepStillReport is the failure mode that made
// capping the right answer and voiding the wrong one. An earlier version of
// this file voided any entry whose last observation was older than the bound,
// which is silent when it is wrong: let passes drift further apart than the
// bound -- a loaded operator, a raised resync interval -- and every entry is
// voided on every pass, so a real divergence is never reported at all. A
// capped step degrades instead of disappearing.
func TestPassesFurtherApartThanOneStepStillReport(t *testing.T) {
	d, clock := newTestDivergence()
	diverging := map[types.UID]bool{"pod-a": true}
	slow := divergenceObservationStep * 2

	for range 20 {
		clock.Advance(slow)
		if stale := d.observe("ns/gateway", diverging, testGrace); len(stale) > 0 {
			return
		}
	}
	t.Errorf("twenty passes %s apart never reported a continuous divergence. A pass "+
		"slower than divergenceObservationStep (%s) must still contribute that much; "+
		"contributing nothing makes a real divergence invisible forever",
		slow, divergenceObservationStep)
}

// TestAPodThatAgreesAgainClearsItsEntry and the two below pin behaviour the
// restructure must not change. They pass before it as well as after; they are
// here because the change rewrites observe's body, and a rewrite with no
// standing tests under it is a rewrite nobody can check.
func TestAPodThatAgreesAgainClearsItsEntry(t *testing.T) {
	d, clock := newTestDivergence()

	pass(d, clock, "ns/gateway", map[types.UID]bool{"pod-a": true})
	pass(d, clock, "ns/gateway", map[types.UID]bool{"pod-a": false})

	clock.Advance(2 * testGrace)
	if stale := pass(d, clock, "ns/gateway", map[types.UID]bool{"pod-a": true}); len(stale) != 0 {
		t.Errorf("stale = %v: agreeing again must drop the entry, so the later divergence "+
			"starts its own clock rather than inheriting the first one's", stale)
	}
}

func TestAPodThatLeavesTheListIsDropped(t *testing.T) {
	d, clock := newTestDivergence()

	pass(d, clock, "ns/gateway", map[types.UID]bool{"pod-a": true})
	// pod-a is gone from the group's live list entirely.
	pass(d, clock, "ns/gateway", map[types.UID]bool{"pod-b": true})

	if _, tracked := d.byGroup["ns/gateway"]["pod-a"]; tracked {
		t.Error("pod-a is still tracked after leaving the live pod list; nothing is left to " +
			"report a readiness for it, and the entry would sit there for the life of the process")
	}
}

func TestTwoGroupsDoNotShareAClock(t *testing.T) {
	d, clock := newTestDivergence()
	diverging := map[types.UID]bool{"pod-a": true}

	// One group is observed throughout; the other joins late. The shared
	// instance must not let the first group's passes advance the second's
	// measurement.
	for elapsed := time.Duration(0); elapsed < testGrace; elapsed += ResyncInterval {
		pass(d, clock, "ns/gateway", diverging)
	}
	if stale := d.observe("ns/other", diverging, testGrace); len(stale) != 0 {
		t.Errorf("stale = %v for a group on its first observation, want none", stale)
	}
}
