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
