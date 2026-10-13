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

	"github.com/spawnery/spawnery/internal/agentpb"
)

// A superseded stream's hard deadline fires while the new stream is serving;
// tested here because end to end the two deadlines lie milliseconds apart.
func TestAStaleGenerationCannotCancelTheFreshStream(t *testing.T) {
	s := newSessions()

	first, firstGen, superseded, _ := s.enter(context.Background(), "pod-uid-1")
	if superseded {
		t.Fatal("the first stream displaced nothing and must not claim otherwise")
	}

	second, secondGen, superseded, _ := s.enter(context.Background(), "pod-uid-1")
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
	_, firstGen, _, _ := s.enter(context.Background(), "pod-uid-1")
	_, secondGen, _, _ := s.enter(context.Background(), "pod-uid-1")

	if s.leave("pod-uid-1", firstGen) {
		t.Error("a superseded stream claimed the disconnect")
	}
	if !s.leave("pod-uid-1", secondGen) {
		t.Error("the current stream could not report its disconnect")
	}

	_, _, superseded, _ := s.enter(context.Background(), "pod-uid-1")
	if superseded {
		t.Error("a reconnect after a completed disconnect displaced nothing")
	}
}

func TestLeavingLeavesNothingBehind(t *testing.T) {
	s := newSessions()

	for _, uid := range []string{"pod-uid-1", "pod-uid-2", "pod-uid-3"} {
		_, gen, _, _ := s.enter(context.Background(), uid)
		if !s.leave(uid, gen) {
			t.Fatalf("the only stream of %s could not report its disconnect", uid)
		}
	}

	if len(s.current) != 0 || len(s.generation) != 0 || len(s.answers) != 0 {
		t.Errorf("current=%d generation=%d answers=%d entries left behind, want none",
			len(s.current), len(s.generation), len(s.answers))
	}
}

// The agent process behind a displaced stream is still running and still
// waiting; see enter.
func TestARenewalHandsTheDisplacedStreamsAnswersOn(t *testing.T) {
	s := newSessions()
	_, _, _, first := s.enter(context.Background(), "pod-uid-1")
	_, _, _, second := s.enter(context.Background(), "pod-uid-1")

	if !first.put(&agentpb.CloudResponse{Id: 7}) {
		t.Fatal("an answer for a pod with room in its mailbox was refused")
	}

	select {
	case got := <-second.ch:
		if got.GetId() != 7 {
			t.Errorf("the renewed stream received answer %d, want 7", got.GetId())
		}
	default:
		t.Error("the answer the displaced stream owed never reached the renewed one")
	}
}

// A stream that ended without being displaced may belong to a process that
// died, whose request ids a new process reuses.
func TestAFreshStreamInheritsNoAnswers(t *testing.T) {
	s := newSessions()
	_, gen, _, old := s.enter(context.Background(), "pod-uid-1")
	old.put(&agentpb.CloudResponse{Id: 3})
	s.leave("pod-uid-1", gen)

	_, _, _, fresh := s.enter(context.Background(), "pod-uid-1")

	select {
	case got := <-fresh.ch:
		t.Errorf("a fresh stream received answer %d meant for a stream before it", got.GetId())
	default:
	}
}

func TestAFullMailboxRefusesRatherThanBlocks(t *testing.T) {
	s := newSessions()
	_, _, _, box := s.enter(context.Background(), "pod-uid-1")
	for i := 0; i < cap(box.ch); i++ {
		box.put(&agentpb.CloudResponse{Id: uint64(i + 1)})
	}

	if box.put(&agentpb.CloudResponse{Id: 999}) {
		t.Error("a full mailbox accepted another answer")
	}
}

// Pruning is only safe because a generation is never handed out twice.
func TestAForgottenPodDoesNotRestartItsGenerations(t *testing.T) {
	s := newSessions()

	_, staleGen, _, _ := s.enter(context.Background(), "pod-uid-1")
	// A second pod in between, so a per-pod counter and a global one could not
	// happen to agree by accident.
	_, otherGen, _, _ := s.enter(context.Background(), "pod-uid-2")
	if !s.leave("pod-uid-1", staleGen) {
		t.Fatal("the current stream could not report its disconnect")
	}
	if !s.leave("pod-uid-2", otherGen) {
		t.Fatal("the current stream of the second pod could not report its disconnect")
	}
	if _, ok := s.generation["pod-uid-1"]; ok {
		t.Fatal("the generation of a departed stream was kept")
	}

	fresh, freshGen, superseded, _ := s.enter(context.Background(), "pod-uid-1")
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

	if _, gen, _, _ := s.enter(context.Background(), "pod-uid-1"); gen == 0 {
		t.Error("the first generation is zero, which is what a forgotten pod reads as")
	}
	if got := s.generation["pod-uid-never-seen"]; got != 0 {
		t.Errorf("an unknown pod reads as generation %d, want the zero value", got)
	}
	if s.leave("pod-uid-never-seen", 1) {
		t.Error("a stream claimed the disconnect of a pod the map never knew")
	}
}
