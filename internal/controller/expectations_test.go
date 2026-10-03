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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spawnery/spawnery/internal/phase"
)

func newTestExpectations() (*expectations, *testClock) {
	clock := &testClock{now: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	return newExpectations(clock.Now), clock
}

func TestExpectedCreateCountsUntilTheCacheShowsIt(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa", 0)

	creates, deletes, _ := e.pending("ns/lobby")
	if len(creates) != 1 || len(deletes) != 0 {
		t.Fatalf("pending = (%v, %v), want (1, empty)", creates, deletes)
	}

	e.observe("ns/lobby", []ServerView{{Name: "lobby-aaaa", Phase: phase.Pending}})
	if creates, _, _ := e.pending("ns/lobby"); len(creates) != 0 {
		t.Errorf("creates = %v once the cache shows it, want empty", creates)
	}
}

func TestExpectationsExpire(t *testing.T) {
	e, clock := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa", 0)

	clock.Advance(expectationTTL - time.Second)
	e.observe("ns/lobby", nil)
	if creates, _, _ := e.pending("ns/lobby"); len(creates) != 1 {
		t.Errorf("creates = %v before the TTL, want 1", creates)
	}

	clock.Advance(2 * time.Second)
	e.observe("ns/lobby", nil)
	if creates, _, _ := e.pending("ns/lobby"); len(creates) != 0 {
		t.Errorf("creates = %v after the TTL, want empty: a lost watch event must "+
			"delay the group, not blind it", creates)
	}
}

func TestExpectedDeleteIsSatisfiedByDisappearanceOrDeparture(t *testing.T) {
	for _, tc := range []struct {
		name  string
		views []ServerView
		want  int
	}{
		{"still there, unchanged", []ServerView{{Name: "lobby-aaaa", Phase: phase.Ready}}, 1},
		{"gone from the cache", nil, 0},
		{"draining", []ServerView{{Name: "lobby-aaaa", Phase: phase.Draining}}, 0},
		{"terminating", []ServerView{{Name: "lobby-aaaa", Phase: phase.Terminating}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newTestExpectations()
			e.expectDeleted("ns/lobby", "lobby-aaaa")

			e.observe("ns/lobby", tc.views)

			_, deletes, _ := e.pending("ns/lobby")
			if len(deletes) != tc.want {
				t.Errorf("pending deletes = %v, want %d entries", deletes, tc.want)
			}
		})
	}
}

func TestExpectedDeleteIsNotSatisfiedByCondemnedAlone(t *testing.T) {
	// Condemned is a node-level signal, not proof the reserved delete landed;
	// clearing on it would let condemned() re-list the same server.
	e, _ := newTestExpectations()
	e.expectDeleted("ns/lobby", "lobby-aaaa")

	e.observe("ns/lobby", []ServerView{{Name: "lobby-aaaa", Phase: phase.Ready, Condemned: true}})

	_, deletes, _ := e.pending("ns/lobby")
	if len(deletes) != 1 {
		t.Fatalf("pending deletes = %v, want the reservation still held", deletes)
	}
}

func TestExpectationsAreKeptPerGroup(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa", 0)
	e.expectCreated("ns/arena", "arena-bbbb", 0)

	if creates, _, _ := e.pending("ns/arena"); len(creates) != 1 {
		t.Errorf("arena creates = %v, want 1", creates)
	}
	e.observe("ns/lobby", []ServerView{{Name: "lobby-aaaa"}})
	if creates, _, _ := e.pending("ns/arena"); len(creates) != 1 {
		t.Errorf("arena creates = %v after observing lobby, want 1", creates)
	}
}

func TestForgetDropsAGroupEntirely(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa", 0)
	e.expectDeleted("ns/lobby", "lobby-bbbb")

	e.forget("ns/lobby")

	creates, deletes, _ := e.pending("ns/lobby")
	if len(creates) != 0 || len(deletes) != 0 {
		t.Errorf("pending = (%v, %v) after forget, want (empty, empty)", creates, deletes)
	}
}

// A delete counted as a create inflates alive by two and hides a shortfall.
func TestPendingSeparatesCreatesFromDeletes(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa", 0)
	e.expectCreated("ns/lobby", "lobby-bbbb", 0)
	e.expectDeleted("ns/lobby", "lobby-cccc")

	creates, deletes, _ := e.pending("ns/lobby")
	if len(creates) != 2 {
		t.Errorf("creates = %v, want 2", creates)
	}
	if len(deletes) != 1 || !deletes["lobby-cccc"] {
		t.Errorf("deletes = %v, want exactly lobby-cccc", deletes)
	}
}

// The first two assertions report rather than halt, so the third (no delete
// among the creates) can still be the one that fails.
func TestPendingNamesItsCreates(t *testing.T) {
	e := newExpectations(func() time.Time { return time.Unix(0, 0) })
	e.expectCreated("ns/survival", "survival-0", 0)
	e.expectCreated("ns/survival", "survival-2", 0)
	e.expectDeleted("ns/survival", "survival-5")

	creates, deletes, _ := e.pending("ns/survival")
	if len(creates) != 2 || !creates["survival-0"] || !creates["survival-2"] {
		t.Errorf("creates = %v, want survival-0 and survival-2", creates)
	}
	if len(deletes) != 1 || !deletes["survival-5"] {
		t.Errorf("deletes = %v, want survival-5", deletes)
	}
	if creates["survival-5"] {
		t.Error("a delete reservation must not appear among the creates")
	}
}

func TestExpectedRetireCountsUntilTheCacheShowsIt(t *testing.T) {
	// Without the reservation a second server can be nominated before the
	// first patch reaches the cache, exceeding maxUnavailable by one.
	e := newExpectations(time.Now)
	e.expectRetired("ns/g", "a")

	_, _, retires := e.pending("ns/g")
	if !retires["a"] {
		t.Fatal("a retirement the cache has not shown is not reserved")
	}

	e.observe("ns/g", []ServerView{{Name: "a"}})
	if _, _, retires = e.pending("ns/g"); !retires["a"] {
		t.Error("the reservation was dropped before the cache caught up")
	}

	e.observe("ns/g", []ServerView{{Name: "a", Retire: true}})
	if _, _, retires = e.pending("ns/g"); retires["a"] {
		t.Error("the reservation outlived the observation")
	}
}

func TestObservePodsClearsACreateReservation(t *testing.T) {
	e := newExpectations(func() time.Time { return time.Unix(0, 0) })
	e.expectCreated("gateway", "gateway-aaaa", 0)

	pending, _, _ := e.pending("gateway")
	if len(pending) != 1 {
		t.Fatalf("pending = %v, want 1 before the pod appears", pending)
	}

	e.observePods("gateway", []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "gateway-aaaa"}}})

	pending, _, _ = e.pending("gateway")
	if len(pending) != 0 {
		t.Errorf("pending = %v, want empty once the cache shows the pod", pending)
	}
}

func TestObservePodsClearsADeleteReservationWhenThePodIsGone(t *testing.T) {
	e := newExpectations(func() time.Time { return time.Unix(0, 0) })
	e.expectDeleted("gateway", "gateway-aaaa")

	e.observePods("gateway", nil)

	pending, leaving, _ := e.pending("gateway")
	if len(pending) != 0 || len(leaving) != 0 {
		t.Errorf("pending = %v, leaving = %v, want both empty once the pod is gone", pending, leaving)
	}
}

func TestExpectedRetireIsSatisfiedByDisappearance(t *testing.T) {
	// A server deleted between the patch and the next list would otherwise
	// hold a budget slot until the TTL.
	e := newExpectations(time.Now)
	e.expectRetired("ns/g", "a")
	e.observe("ns/g", nil)
	if _, _, retires := e.pending("ns/g"); retires["a"] {
		t.Error("a retirement whose server is gone is still reserved")
	}
}

func TestPendingNumbersHoldsWhatWasReserved(t *testing.T) {
	e := newExpectations(time.Now)

	e.expectCreated("ns/hub", "hub-dvjk", 1)
	e.expectCreated("ns/hub", "hub-pgqg", 3)

	got := e.pendingNumbers("ns/hub")
	if !got[1] || !got[3] {
		t.Errorf("pendingNumbers = %v, want 1 and 3", got)
	}
	if len(got) != 2 {
		t.Errorf("pendingNumbers = %v, want exactly two entries", got)
	}
}

func TestPendingNumbersSkipsZero(t *testing.T) {
	e := newExpectations(time.Now)

	// Zero means a name without a number; the ephemeral rule never hands out
	// zero.
	e.expectCreated("ns/gateway", "gateway-a1b2", 0)

	if got := e.pendingNumbers("ns/gateway"); len(got) != 0 {
		t.Errorf("pendingNumbers = %v, want empty", got)
	}
}

func TestAnObservedCreateReleasesItsNumber(t *testing.T) {
	e := newExpectations(time.Now)
	e.expectCreated("ns/hub", "hub-dvjk", 1)

	e.observe("ns/hub", []ServerView{{Name: "hub-dvjk", Number: 1}})

	if got := e.pendingNumbers("ns/hub"); len(got) != 0 {
		t.Errorf("pendingNumbers = %v, want empty once the create was seen", got)
	}
}
