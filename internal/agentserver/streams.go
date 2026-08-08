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
	"sync"
)

// sessions tracks one live stream per pod. A second stream for the same pod
// supersedes the first: otherwise tearing down the zombie would wipe the state
// of the fresh one and the server would fall out of Ready for no reason.
type sessions struct {
	mu      sync.Mutex
	current map[string]context.CancelFunc
	// generation counts how often a pod connected, so a superseded stream can
	// tell it is no longer the current one. It is never decremented and its
	// entries are never removed: a number that could be handed out twice would
	// let a slow zombie mistake itself for the stream that reused it.
	generation map[string]uint64
}

func newSessions() *sessions {
	return &sessions{
		current:    make(map[string]context.CancelFunc),
		generation: make(map[string]uint64),
	}
}

// enter registers a new stream and cancels the one it replaces. The returned
// context ends when this stream is superseded or the server shuts down.
//
// The third return value reports whether a still-live stream was displaced.
// The caller needs it to tell a make-before-break renewal, where the agent
// process kept running and its readiness still holds, from a genuine reconnect
// after a disconnect, where only the new Hello may say the agent is ready.
func (s *sessions) enter(parent context.Context, podUID string) (context.Context, uint64, bool) {
	ctx, cancel := context.WithCancel(parent)

	s.mu.Lock()
	defer s.mu.Unlock()
	previous, superseded := s.current[podUID]
	if superseded {
		previous()
	}
	s.generation[podUID]++
	gen := s.generation[podUID]
	s.current[podUID] = cancel
	return ctx, gen, superseded
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
	return true
}

// cancel ends a stream from the outside — the hard deadline uses it. It is a
// no-op once the generation has moved on, so a deadline that fires just after
// a renewal cannot cut the fresh stream short.
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
