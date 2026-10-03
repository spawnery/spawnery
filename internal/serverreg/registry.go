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

// Package serverreg is every live backend session, and the path the operator
// uses to send one anything.
//
// The session machinery duplicates proxyreg.Fleet's rather than sharing a
// generic: half of Fleet is proxy protocol a backend does not need. The network
// picture itself is shared through netstate.Source.
package serverreg

import (
	"context"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/netstate"
)

const (
	// DefaultResyncInterval matches proxyreg's: a state is only as true as its
	// last delivery.
	DefaultResyncInterval = 30 * time.Second
	DefaultOutboxSize     = 8
)

// Options configures a Registry.
type Options struct {
	State          netstate.Source
	ResyncInterval time.Duration
	// OutboxSize bounds a session's queue. Zero means the default. Smaller than
	// proxyreg's 64: this queue carries one message per resync, not a rollout's
	// burst of registrations.
	OutboxSize int
}

type session struct {
	namespace string
	outbox    chan *agentpb.OperatorToServer
	closed    bool
	// wantsEvents lives on the session so a new stream starts without it and it
	// cannot outlive the stream that reported it.
	wantsEvents bool
}

// Registry is every live backend session. Safe for concurrent use.
type Registry struct {
	mu sync.Mutex
	// sessions is keyed by pod UID, the same key the agent registry uses.
	sessions map[string]*session
	opts     Options
}

func New(opts Options) *Registry {
	if opts.ResyncInterval <= 0 {
		opts.ResyncInterval = DefaultResyncInterval
	}
	if opts.OutboxSize <= 0 {
		opts.OutboxSize = DefaultOutboxSize
	}
	return &Registry{sessions: make(map[string]*session), opts: opts}
}

// Join enters a session and returns its outbox together with the function that
// removes it. The first message on the channel is always the network state:
// it is built under the mutex Resync takes, so no resync can overtake it.
//
// The Registry closes the channel if the session falls too far behind. A
// caller that reads a closed channel must end its stream; see send.
func (r *Registry) Join(ctx context.Context, namespace, podUID string) (<-chan *agentpb.OperatorToServer, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	state, err := r.opts.State.Build(ctx, namespace, netstate.ForServers)
	if err != nil {
		return nil, nil, err
	}

	s := &session{
		namespace: namespace,
		outbox:    make(chan *agentpb.OperatorToServer, r.opts.OutboxSize+1),
	}
	s.outbox <- stateMessage(state)
	if previous, ok := r.sessions[podUID]; ok {
		// A second stream from one pod supersedes the first (make-before-break
		// renewal); the old reader sees a closed channel and ends.
		r.close(previous)
	}
	r.sessions[podUID] = s

	return s.outbox, func() { r.leave(podUID, s) }, nil
}

func (r *Registry) leave(podUID string, s *session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[podUID] != s {
		return
	}
	r.close(s)
	delete(r.sessions, podUID)
}

// Callers hold r.mu.
func (r *Registry) close(s *session) {
	if s.closed {
		return
	}
	s.closed = true
	close(s.outbox)
}

// send queues a message, or cuts the session loose if its queue is full.
// Dropping instead would leave the agent serving a stale mirror while looking
// healthy; a cut stream reconnects and is rebuilt from a fresh state.
//
// Callers hold r.mu.
func (r *Registry) send(s *session, msg *agentpb.OperatorToServer) {
	if s.closed {
		return
	}
	select {
	case s.outbox <- msg:
	default:
		SessionsCut.Inc()
		r.close(s)
	}
}

// broadcast sends one message to every session in a namespace. A build that
// returns nil skips that session.
func (r *Registry) broadcast(namespace string, build func(*session) *agentpb.OperatorToServer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if s.namespace != namespace {
			continue
		}
		if msg := build(s); msg != nil {
			r.send(s, msg)
		}
	}
}

// SetInterest records whether this session's agent has anybody to show events
// to. An unknown pod is ignored: the report may come from a session a renewal
// just displaced.
func (r *Registry) SetInterest(podUID string, wanted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[podUID]; ok {
		s.wantsEvents = wanted
	}
}

// Interested reports what SetInterest last recorded. Exported for tests.
func (r *Registry) Interested(podUID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[podUID]
	return ok && s.wantsEvents
}

// Publish sends one event to every session in the namespace that asked for
// events. It implements cloudevent.Sink. It reports nothing: a feed nobody
// watches must not fail a reconcile, and a missed event is ordinary.
func (r *Registry) Publish(namespace string, ev *agentpb.CloudEvent) {
	r.broadcast(namespace, func(s *session) *agentpb.OperatorToServer {
		if !s.wantsEvents {
			return nil
		}
		return &agentpb.OperatorToServer{
			Message: &agentpb.OperatorToServer_CloudEvent{CloudEvent: ev},
		}
	})
}

func (r *Registry) Resync(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	built := make(map[string]*agentpb.NetworkState)
	for podUID, s := range r.sessions {
		state, ok := built[s.namespace]
		if !ok {
			var err error
			state, err = r.opts.State.Build(ctx, s.namespace, netstate.ForServers)
			if err != nil {
				// One unreadable namespace must not stop the others; the session keeps
				// its last state until the next tick.
				log.FromContext(ctx).V(1).Info("skipped a server resync",
					"pod", podUID, "namespace", s.namespace, "reason", err.Error())
				continue
			}
			built[s.namespace] = state
		}
		r.send(s, stateMessage(state))
	}
}

// Start runs the resync ticker until ctx ends. It implements manager.Runnable.
func (r *Registry) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.opts.ResyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.Resync(ctx)
		}
	}
}

// NeedLeaderElection is true: only the leader holds the streams.
func (r *Registry) NeedLeaderElection() bool { return true }

func stateMessage(state *agentpb.NetworkState) *agentpb.OperatorToServer {
	return &agentpb.OperatorToServer{
		Message: &agentpb.OperatorToServer_NetworkState{NetworkState: state},
	}
}
