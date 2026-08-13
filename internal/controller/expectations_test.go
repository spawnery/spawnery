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

	"github.com/spawnery/spawnery/internal/phase"
)

func newTestExpectations() (*expectations, *testClock) {
	clock := &testClock{now: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	return newExpectations(clock.Now), clock
}

func TestExpectedCreateCountsUntilTheCacheShowsIt(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")

	creates, deletes := e.pending("ns/lobby")
	if creates != 1 || len(deletes) != 0 {
		t.Fatalf("pending = (%d, %v), want (1, empty)", creates, deletes)
	}

	e.observe("ns/lobby", []ServerView{{Name: "lobby-aaaa", Phase: phase.Pending}})
	if creates, _ := e.pending("ns/lobby"); creates != 0 {
		t.Errorf("creates = %d once the cache shows it, want 0", creates)
	}
}

func TestExpectationsExpire(t *testing.T) {
	e, clock := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")

	clock.Advance(expectationTTL - time.Second)
	e.observe("ns/lobby", nil)
	if creates, _ := e.pending("ns/lobby"); creates != 1 {
		t.Errorf("creates = %d before the TTL, want 1", creates)
	}

	clock.Advance(2 * time.Second)
	e.observe("ns/lobby", nil)
	if creates, _ := e.pending("ns/lobby"); creates != 0 {
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

			_, deletes := e.pending("ns/lobby")
			if len(deletes) != tc.want {
				t.Errorf("pending deletes = %v, want %d entries", deletes, tc.want)
			}
		})
	}
}

func TestExpectationsAreKeptPerGroup(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")
	e.expectCreated("ns/arena", "arena-bbbb")

	if creates, _ := e.pending("ns/arena"); creates != 1 {
		t.Errorf("arena creates = %d, want 1", creates)
	}
	e.observe("ns/lobby", []ServerView{{Name: "lobby-aaaa"}})
	if creates, _ := e.pending("ns/arena"); creates != 1 {
		t.Errorf("arena creates = %d after observing lobby, want 1", creates)
	}
}

func TestForgetDropsAGroupEntirely(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")
	e.expectDeleted("ns/lobby", "lobby-bbbb")

	e.forget("ns/lobby")

	creates, deletes := e.pending("ns/lobby")
	if creates != 0 || len(deletes) != 0 {
		t.Errorf("pending = (%d, %v) after forget, want (0, empty)", creates, deletes)
	}
}
