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
}

func newSessions() *sessions {
	return &sessions{
		current:    make(map[string]context.CancelFunc),
		generation: make(map[string]uint64),
	}
}

// enter reports whether a still-live stream was displaced: a make-before-break
// renewal keeps the agent's readiness, a reconnect after a disconnect does not.
func (s *sessions) enter(parent context.Context, podUID string) (context.Context, uint64, bool) {
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
	delete(s.generation, podUID)
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
