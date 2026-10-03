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

package agentserver

import (
	"context"
	"testing"
)

// A superseded stream's hard deadline fires while the new stream is serving;
// tested here because end to end the two deadlines lie milliseconds apart.
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

	s.cancel("pod-uid-1", firstGen)
	if err := second.Err(); err != nil {
		t.Errorf("a stale deadline cut the fresh stream: %v", err)
	}

	s.cancel("pod-uid-1", secondGen)
	if second.Err() == nil {
		t.Error("the current generation must still be cancellable")
	}
}

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

	_, _, superseded := s.enter(context.Background(), "pod-uid-1")
	if superseded {
		t.Error("a reconnect after a completed disconnect displaced nothing")
	}
}

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

// Pruning is only safe because a generation is never handed out twice.
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
	if _, ok := s.generation["pod-uid-1"]; ok {
		t.Fatal("the generation of a departed stream was kept")
	}

	fresh, freshGen, superseded := s.enter(context.Background(), "pod-uid-1")
	if superseded {
		t.Error("a reconnect into an empty map displaced something")
	}
	if freshGen <= staleGen {
		t.Fatalf("generation %d does not exceed the earlier %d; the counter restarted",
			freshGen, staleGen)
	}

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

	if !s.leave("pod-uid-1", freshGen) {
		t.Fatal("the live stream could not report its disconnect")
	}
	if len(s.current) != 0 || len(s.generation) != 0 {
		t.Errorf("current=%d generation=%d entries left behind, want none",
			len(s.current), len(s.generation))
	}
}

func TestNoGenerationIsEverZero(t *testing.T) {
	s := newSessions()

	if _, gen, _ := s.enter(context.Background(), "pod-uid-1"); gen == 0 {
		t.Error("the first generation is zero, which is what a forgotten pod reads as")
	}
	if got := s.generation["pod-uid-never-seen"]; got != 0 {
		t.Errorf("an unknown pod reads as generation %d, want the zero value", got)
	}
	if s.leave("pod-uid-never-seen", 1) {
		t.Error("a stream claimed the disconnect of a pod the map never knew")
	}
}
