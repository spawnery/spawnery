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
	e.expectCreated("ns/lobby", "lobby-aaaa")

	creates, deletes, _ := e.pending("ns/lobby")
	if creates != 1 || len(deletes) != 0 {
		t.Fatalf("pending = (%d, %v), want (1, empty)", creates, deletes)
	}

	e.observe("ns/lobby", []ServerView{{Name: "lobby-aaaa", Phase: phase.Pending}})
	if creates, _, _ := e.pending("ns/lobby"); creates != 0 {
		t.Errorf("creates = %d once the cache shows it, want 0", creates)
	}
}

func TestExpectationsExpire(t *testing.T) {
	e, clock := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")

	clock.Advance(expectationTTL - time.Second)
	e.observe("ns/lobby", nil)
	if creates, _, _ := e.pending("ns/lobby"); creates != 1 {
		t.Errorf("creates = %d before the TTL, want 1", creates)
	}

	clock.Advance(2 * time.Second)
	e.observe("ns/lobby", nil)
	if creates, _, _ := e.pending("ns/lobby"); creates != 0 {
		t.Errorf("creates = %d after the TTL, want 0: a lost watch event must "+
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
	// Condemned is an independent node-level signal, not evidence that the
	// reserved removal has landed: a server this reconciler already
	// reserved an ordinary delete for can have its node cordoned before the
	// cache shows any consequence of that delete. Clearing the reservation
	// on Condemned alone would drop the guard that keeps condemned() from
	// re-listing the same server on the next pass.
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
	e.expectCreated("ns/lobby", "lobby-aaaa")
	e.expectCreated("ns/arena", "arena-bbbb")

	if creates, _, _ := e.pending("ns/arena"); creates != 1 {
		t.Errorf("arena creates = %d, want 1", creates)
	}
	e.observe("ns/lobby", []ServerView{{Name: "lobby-aaaa"}})
	if creates, _, _ := e.pending("ns/arena"); creates != 1 {
		t.Errorf("arena creates = %d after observing lobby, want 1", creates)
	}
}

func TestForgetDropsAGroupEntirely(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")
	e.expectDeleted("ns/lobby", "lobby-bbbb")

	e.forget("ns/lobby")

	creates, deletes, _ := e.pending("ns/lobby")
	if creates != 0 || len(deletes) != 0 {
		t.Errorf("pending = (%d, %v) after forget, want (0, empty)", creates, deletes)
	}
}

// TestPendingSeparatesCreatesFromDeletes exercises the type's primary read API
// with both kinds in one group at once. It is the arithmetic task 6 sizes the
// group from: a delete counted as a create inflates alive by two and hides a
// shortfall the group actually has.
func TestPendingSeparatesCreatesFromDeletes(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")
	e.expectCreated("ns/lobby", "lobby-bbbb")
	e.expectDeleted("ns/lobby", "lobby-cccc")

	creates, deletes, _ := e.pending("ns/lobby")
	if creates != 2 {
		t.Errorf("creates = %d, want 2", creates)
	}
	if len(deletes) != 1 || !deletes["lobby-cccc"] {
		t.Errorf("deletes = %v, want exactly lobby-cccc", deletes)
	}
}

func TestExpectedRetireCountsUntilTheCacheShowsIt(t *testing.T) {
	// Without this reservation a second server can be nominated while the
	// first patch has not reached the cache, and maxUnavailable is exceeded
	// by one. The window is small; the standard here is not smallness.
	e := newExpectations(time.Now)
	e.expectRetired("ns/g", "a")

	_, _, retires := e.pending("ns/g")
	if !retires["a"] {
		t.Fatal("a retirement the cache has not shown is not reserved")
	}

	// The cache still shows the old spec: still reserved.
	e.observe("ns/g", []ServerView{{Name: "a"}})
	if _, _, retires = e.pending("ns/g"); !retires["a"] {
		t.Error("the reservation was dropped before the cache caught up")
	}

	// The cache shows the patch: the reservation has done its job.
	e.observe("ns/g", []ServerView{{Name: "a", Retire: true}})
	if _, _, retires = e.pending("ns/g"); retires["a"] {
		t.Error("the reservation outlived the observation")
	}
}

// TestObservePodsClearsACreateReservation pins observePods -- observe's pod-
// shaped counterpart for the ProxyGroup controller, which lists pods rather
// than the Server CRs observe reads.
func TestObservePodsClearsACreateReservation(t *testing.T) {
	e := newExpectations(func() time.Time { return time.Unix(0, 0) })
	e.expectCreated("gateway", "gateway-aaaa")

	pending, _, _ := e.pending("gateway")
	if pending != 1 {
		t.Fatalf("pending = %d, want 1 before the pod appears", pending)
	}

	e.observePods("gateway", []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "gateway-aaaa"}}})

	pending, _, _ = e.pending("gateway")
	if pending != 0 {
		t.Errorf("pending = %d, want 0 once the cache shows the pod", pending)
	}
}

func TestObservePodsClearsADeleteReservationWhenThePodIsGone(t *testing.T) {
	e := newExpectations(func() time.Time { return time.Unix(0, 0) })
	e.expectDeleted("gateway", "gateway-aaaa")

	e.observePods("gateway", nil)

	pending, leaving, _ := e.pending("gateway")
	if pending != 0 || len(leaving) != 0 {
		t.Errorf("pending = %d, leaving = %v, want both empty once the pod is gone", pending, leaving)
	}
}

func TestExpectedRetireIsSatisfiedByDisappearance(t *testing.T) {
	// A server that finished retiring and was deleted between the patch and
	// the next list would otherwise hold a slot of the budget forever, or
	// until the TTL — and the TTL is the backstop, not the mechanism.
	e := newExpectations(time.Now)
	e.expectRetired("ns/g", "a")
	e.observe("ns/g", nil)
	if _, _, retires := e.pending("ns/g"); retires["a"] {
		t.Error("a retirement whose server is gone is still reserved")
	}
}
