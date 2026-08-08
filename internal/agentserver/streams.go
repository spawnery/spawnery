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
	// generation names the stream currently registered for a pod, so a
	// superseded one can tell it is no longer the current one.
	//
	// The numbers come from nextGeneration below: ONE counter for the whole
	// process, not one per pod. Do not "simplify" it back to a counter per pod.
	// A generation identifies a stream globally and for all time, and that is
	// what makes an entry safe to delete — which leave does. The zero value
	// carries that weight and is load-bearing on purpose: nextGeneration is
	// incremented before it is read, so the first generation ever handed out is
	// 1, a pod with no entry reads as 0, and 0 therefore matches nothing that
	// was ever issued. A missing entry and a stale generation can never be
	// confused.
	//
	// Per pod the two would be in tension by construction: deleting an entry
	// would restart that pod's count at 1, and a slow zombie still holding
	// generation 1 would pass every guard here and tear down the live stream
	// that reused the number. Globally there is no number to reuse.
	generation map[string]uint64
	// nextGeneration is the counter. Guarded by mu, never decremented.
	nextGeneration uint64
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
	s.nextGeneration++
	gen := s.nextGeneration
	s.generation[podUID] = gen
	s.current[podUID] = cancel
	return ctx, gen, superseded
}

// leave reports whether this stream was still the current one. Only then may
// the caller mark the pod disconnected.
//
// It is also where both maps are pruned. A pod whose current stream has ended
// leaves nothing behind: the entry is gone, and because generations are never
// reused, its absence cannot be confused with a fresh one. Without this the
// generation map would grow by one entry for every pod the operator ever saw
// and only end with the process.
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
