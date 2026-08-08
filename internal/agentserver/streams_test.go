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

package agentserver

import (
	"context"
	"testing"
)

// A hard deadline belongs to the stream that armed it. Once that stream has
// been superseded its timer is a loaded gun pointing at the fresh stream: the
// two overlap by design, so the deadline of the old one fires while the new
// one is serving. The generation guard is what unloads it.
//
// This is tested here rather than end to end because the two deadlines lie
// only milliseconds apart in any fixture that runs in reasonable time, so the
// end-to-end version would be a race dressed up as a test.
func TestAStaleGenerationCannotCancelTheFreshStream(t *testing.T) {
	s := newSessions()

	first, firstGen, superseded := s.enter(context.Background(), "pod-uid-1")
	if superseded {
		t.Fatal("the first stream displaced nothing and must not claim otherwise")
	}

	second, secondGen, superseded := s.enter(context.Background(), "pod-uid-1")
	if !superseded {
		t.Fatal("the second stream displaced a live one and must report it")
	}
	if secondGen == firstGen {
		t.Fatalf("generation %d was handed out twice", secondGen)
	}
	if first.Err() == nil {
		t.Error("entering did not cancel the stream it replaced")
	}

	// The superseded stream's hard deadline fires. It must be a no-op.
	s.cancel("pod-uid-1", firstGen)
	if err := second.Err(); err != nil {
		t.Errorf("a stale deadline cut the fresh stream: %v", err)
	}

	// The current generation still ends its own stream.
	s.cancel("pod-uid-1", secondGen)
	if second.Err() == nil {
		t.Error("the current generation must still be cancellable")
	}
}

// Only the current stream may report the disconnect. A superseded one that
// reported it would undo the registration of the stream that replaced it.
func TestOnlyTheCurrentStreamMayLeave(t *testing.T) {
	s := newSessions()
	_, firstGen, _ := s.enter(context.Background(), "pod-uid-1")
	_, secondGen, _ := s.enter(context.Background(), "pod-uid-1")

	if s.leave("pod-uid-1", firstGen) {
		t.Error("a superseded stream claimed the disconnect")
	}
	if !s.leave("pod-uid-1", secondGen) {
		t.Error("the current stream could not report its disconnect")
	}

	// After the current stream left, the pod reconnects from scratch: nothing
	// is displaced, so readiness may not be carried over.
	_, _, superseded := s.enter(context.Background(), "pod-uid-1")
	if superseded {
		t.Error("a reconnect after a completed disconnect displaced nothing")
	}
}

// The maps must not outlive the streams they describe. An operator that has
// churned through a few thousand pods would otherwise carry an entry for every
// one of them until it restarts.
func TestLeavingLeavesNothingBehind(t *testing.T) {
	s := newSessions()

	for _, uid := range []string{"pod-uid-1", "pod-uid-2", "pod-uid-3"} {
		_, gen, _ := s.enter(context.Background(), uid)
		if !s.leave(uid, gen) {
			t.Fatalf("the only stream of %s could not report its disconnect", uid)
		}
	}

	if len(s.current) != 0 || len(s.generation) != 0 {
		t.Errorf("current=%d generation=%d entries left behind, want none",
			len(s.current), len(s.generation))
	}
}

// Pruning is only safe because a generation is never handed out twice. This is
// the reason the counter is one for the whole process instead of one per pod:
// with a per-pod counter the entry below would be re-created at generation one,
// and the zombie from the first cycle — still holding generation one — would
// pass every guard and tear down a live stream.
func TestAForgottenPodDoesNotRestartItsGenerations(t *testing.T) {
	s := newSessions()

	_, staleGen, _ := s.enter(context.Background(), "pod-uid-1")
	// A second pod in between, so a per-pod counter and a global one could not
	// happen to agree by accident.
	_, otherGen, _ := s.enter(context.Background(), "pod-uid-2")
	if !s.leave("pod-uid-1", staleGen) {
		t.Fatal("the current stream could not report its disconnect")
	}
	if !s.leave("pod-uid-2", otherGen) {
		t.Fatal("the current stream of the second pod could not report its disconnect")
	}
	// The entry really is gone, which is the situation the rest of this test is
	// about: what a stale generation does against a map that forgot the pod.
	if _, ok := s.generation["pod-uid-1"]; ok {
		t.Fatal("the generation of a departed stream was kept")
	}

	// The pod comes back, and the entry is built from scratch. The counter kept
	// counting, so this is strictly above anything issued before — not merely
	// different from it.
	fresh, freshGen, superseded := s.enter(context.Background(), "pod-uid-1")
	if superseded {
		t.Error("a reconnect into an empty map displaced something")
	}
	if freshGen <= staleGen {
		t.Fatalf("generation %d does not exceed the earlier %d; the counter restarted",
			freshGen, staleGen)
	}

	// The zombie from the first cycle finally gets around to its teardown.
	// Neither half of it may touch the stream that is serving now.
	s.cancel("pod-uid-1", staleGen)
	if err := fresh.Err(); err != nil {
		t.Errorf("a zombie's deadline cut the live stream: %v", err)
	}
	if s.leave("pod-uid-1", staleGen) {
		t.Error("a zombie claimed the disconnect of the live stream")
	}
	if _, ok := s.current["pod-uid-1"]; !ok {
		t.Error("a zombie's leave removed the live stream from the map")
	}

	// And once the live stream ends properly, nothing is left over for this pod
	// either: the maps are bounded by live streams, not by pods ever seen.
	if !s.leave("pod-uid-1", freshGen) {
		t.Fatal("the live stream could not report its disconnect")
	}
	if len(s.current) != 0 || len(s.generation) != 0 {
		t.Errorf("current=%d generation=%d entries left behind, want none",
			len(s.current), len(s.generation))
	}
}

// The zero value is load-bearing: the counter is incremented before it is read,
// so nothing is ever issued as generation zero, and a pod the map has forgotten
// reads as exactly that. Anyone tempted to start the counter at zero — or to
// make it per pod again — breaks the guards in leave and cancel, which is why
// this is pinned rather than left as a comment.
func TestNoGenerationIsEverZero(t *testing.T) {
	s := newSessions()

	if _, gen, _ := s.enter(context.Background(), "pod-uid-1"); gen == 0 {
		t.Error("the first generation is zero, which is what a forgotten pod reads as")
	}
	if got := s.generation["pod-uid-never-seen"]; got != 0 {
		t.Errorf("an unknown pod reads as generation %d, want the zero value", got)
	}
	// So a stale generation against a pod the map never knew is a no-op in both
	// directions, with no entry to compare against.
	if s.leave("pod-uid-never-seen", 1) {
		t.Error("a stream claimed the disconnect of a pod the map never knew")
	}
}
