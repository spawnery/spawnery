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
	"sync"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// sessions tracks one live stream per pod. A second stream for the same pod
// supersedes the first: otherwise tearing down the zombie would wipe the state
// of the fresh one and the server would fall out of Ready for no reason.
type sessions struct {
	mu      sync.Mutex
	current map[string]context.CancelFunc
	// generation comes from one process-wide counter, not one per pod: leave
	// deletes entries, and a per-pod count restarting at 1 would let a zombie
	// still holding generation 1 tear down the live stream that reused it.
	generation     map[string]uint64
	nextGeneration uint64
	answers        map[string]*mailbox
}

// mailboxSize is far above what the request rate limit lets a pod have
// outstanding.
const mailboxSize = 64

// mailbox holds a pod's answers until whichever of its streams is current
// sends them.
type mailbox struct{ ch chan *agentpb.CloudResponse }

func newMailbox() *mailbox { return &mailbox{ch: make(chan *agentpb.CloudResponse, mailboxSize)} }

// put never blocks; the agent's deadline covers an answer it drops.
func (m *mailbox) put(answer *agentpb.CloudResponse) bool {
	select {
	case m.ch <- answer:
		return true
	default:
		AnswersDropped.Inc()
		return false
	}
}

func newSessions() *sessions {
	return &sessions{
		current:    make(map[string]context.CancelFunc),
		generation: make(map[string]uint64),
		answers:    make(map[string]*mailbox),
	}
}

// enter reports whether a still-live stream was displaced: a make-before-break
// renewal keeps the agent's readiness, a reconnect after a disconnect does not.
func (s *sessions) enter(parent context.Context, podUID string) (context.Context, uint64, bool, *mailbox) {
	ctx, cancel := context.WithCancel(parent)

	s.mu.Lock()
	defer s.mu.Unlock()
	previous, superseded := s.current[podUID]
	if superseded {
		previous()
	}
	s.nextGeneration++
	gen := s.nextGeneration
	s.generation[podUID] = gen
	s.current[podUID] = cancel
	// A displaced stream was still live, so the agent process that asked is
	// the one listening on this stream. Any other process gets a fresh
	// mailbox: its request ids start over, and an old answer must not
	// complete a new request.
	box, kept := s.answers[podUID]
	if !superseded || !kept {
		box = newMailbox()
		s.answers[podUID] = box
	}
	return ctx, gen, superseded, box
}

// leave reports whether this stream was still the current one. Only then may
// the caller mark the pod disconnected.
func (s *sessions) leave(podUID string, gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation[podUID] != gen {
		return false
	}
	delete(s.current, podUID)
	delete(s.generation, podUID)
	delete(s.answers, podUID)
	return true
}

func (s *sessions) cancel(podUID string, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation[podUID] != gen {
		return
	}
	if cancel, ok := s.current[podUID]; ok {
		cancel()
	}
}

// sendAnswer leaves an answer this stream could not take for the stream that
// replaces it. A send that times out may still land; the agent ignores an
// answer for an id it no longer holds.
func sendAnswer(box *mailbox, answer *agentpb.CloudResponse, send func(*agentpb.CloudResponse) error) error {
	if err := sendBounded(SendDeadline, "an answer", func() error { return send(answer) }); err != nil {
		box.put(answer)
		return err
	}
	return nil
}
