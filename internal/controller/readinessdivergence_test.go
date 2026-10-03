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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func newTestDivergence() (*readinessDivergence, *testClock) {
	clock := &testClock{now: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
	return newReadinessDivergence(clock.Now), clock
}

const testGrace = 60 * time.Second

func pass(d *readinessDivergence, clock *testClock, group string, diverging map[types.UID]bool) []types.UID {
	clock.Advance(ResyncInterval)
	return d.observe(group, diverging, testGrace)
}

func TestADivergenceReportsOnceItHasBeenWatchedForTheWholeGrace(t *testing.T) {
	d, clock := newTestDivergence()
	diverging := map[types.UID]bool{"pod-a": true}

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

// Reconcile skips observe on every early error return, so unwatched time must not count toward the grace.
func TestAGapInObservationCannotBeSpentOnTheGrace(t *testing.T) {
	d, clock := newTestDivergence()
	diverging := map[types.UID]bool{"pod-a": true}

	// Observation stops, as when every pass returns before reportReadinessDivergence.
	if stale := pass(d, clock, "ns/gateway", diverging); len(stale) != 0 {
		t.Fatalf("reported on the very first observation: %v", stale)
	}
	clock.Advance(5 * time.Minute)

	if stale := pass(d, clock, "ns/gateway", diverging); len(stale) != 0 {
		t.Errorf("stale = %v on the first pass after a five-minute gap in observation. "+
			"The grace measures watched time, and nothing watched this pod for those "+
			"five minutes", stale)
	}

	// The gap contributed at most one step, so the report must land within one step of a full grace.
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

// Voiding stale entries instead of capping their step would never report once passes drift apart.
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
	pass(d, clock, "ns/gateway", map[types.UID]bool{"pod-b": true})

	if _, tracked := d.byGroup["ns/gateway"]["pod-a"]; tracked {
		t.Error("pod-a is still tracked after leaving the live pod list; nothing is left to " +
			"report a readiness for it, and the entry would sit there for the life of the process")
	}
}

func TestTwoGroupsDoNotShareAClock(t *testing.T) {
	d, clock := newTestDivergence()
	diverging := map[types.UID]bool{"pod-a": true}

	for elapsed := time.Duration(0); elapsed < testGrace; elapsed += ResyncInterval {
		pass(d, clock, "ns/gateway", diverging)
	}
	if stale := d.observe("ns/other", diverging, testGrace); len(stale) != 0 {
		t.Errorf("stale = %v for a group on its first observation, want none", stale)
	}
}
