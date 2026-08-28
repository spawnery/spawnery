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

// Package serverreg is every live backend session, and the path the operator
// uses to send one anything.
//
// Until this package existed there was no such path. ServerSession sent a
// ReportInterval and a SessionDeadline when a stream opened and never sent
// again; internal/proxyreg.Fleet, with its Join, broadcast, snapshot and
// Resync, has always been the proxies' alone.
//
// # The session machinery here is Fleet's, duplicated rather than extracted
//
// This comment is the argument, not an apology for one.
//
// Fleet is eighteen functions and they split almost exactly in half. Nine are
// this machinery -- New, Join, leave, close, send, broadcast, Resync, Start,
// NeedLeaderElection. Nine are the proxy protocol: fallback lists, drain
// orders, registered-server construction, the lastReady memo, and a snapshot
// scoped to one group rather than a namespace. A backend needs none of the
// second half. A generic carrying the proxy's per-session hooks would be a
// parameterised version of the harder case serving the easier one, and
// building it would have put a refactor of the drain's delivery path inside a
// milestone about reporting.
//
// What is genuinely shared is the picture, and the picture *is* shared: both
// packages build theirs through netstate.Source, so the two cannot come to
// disagree about what a network looks like -- which is the promise the plugin
// API makes and the only one a divergence here would break.
//
// The two rules that matter in the eighty lines below are stated in both
// places on purpose, because they are the ones a reader has to get right:
// build a session's first message under the same lock a broadcast takes, so
// nothing can overtake it; and cut a session that falls behind rather than
// dropping its message, because an agent serving a mirror it cannot know is
// wrong looks healthy the whole time.
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
	// DefaultResyncInterval is how often every live session is re-sent its
	// state. The same figure proxyreg uses, and for the same reason: a state
	// is only as true as its last delivery.
	DefaultResyncInterval = 30 * time.Second
	// DefaultOutboxSize is how far a session may fall behind before it is cut.
	DefaultOutboxSize = 8
)

// Options configures a Registry.
type Options struct {
	// State builds what a session is sent on join and on every resync.
	State netstate.Source
	// ResyncInterval is how often Start re-syncs. Zero means the default.
	ResyncInterval time.Duration
	// OutboxSize bounds a session's queue. Zero means the default.
	//
	// Smaller than proxyreg's 64 on purpose: that queue absorbs a burst of
	// per-server registrations during a rollout, and this one carries one
	// message per resync. A backend that cannot read eight of those is not
	// reading its stream at all.
	OutboxSize int
}

// session is one live backend stream's queue.
type session struct {
	namespace string
	outbox    chan *agentpb.OperatorToServer
	// closed guards against a double close: a session ends either because its
	// stream left or because it fell behind, and both can happen.
	closed bool
}

// Registry is every live backend session. Safe for concurrent use.
type Registry struct {
	mu sync.Mutex
	// sessions is keyed by pod UID, the same key the agent registry uses.
	sessions map[string]*session
	opts     Options
}

// New creates a Registry.
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
// removes it. The first message on the channel is always the network state.
//
// That guarantee is the shape of this function. Everything between the lock
// and the unlock -- building the state, filling the queue, entering the
// session -- happens where no resync can run, because Resync takes the same
// mutex. So a resync cannot overtake the state it would be repeating, and the
// ordering is a property of the code rather than of a test that has to win a
// race to observe it.
//
// The Registry closes the channel if the session falls too far behind. A
// caller that reads a closed channel must end its stream; see send.
func (r *Registry) Join(ctx context.Context, namespace, podUID string) (<-chan *agentpb.OperatorToServer, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	state, err := r.opts.State.Build(ctx, namespace)
	if err != nil {
		return nil, nil, err
	}

	s := &session{
		namespace: namespace,
		outbox:    make(chan *agentpb.OperatorToServer, r.opts.OutboxSize+1),
	}
	s.outbox <- stateMessage(state)
	if previous, ok := r.sessions[podUID]; ok {
		// A second stream from one pod supersedes the first, which is what
		// makes a make-before-break renewal work: the new session is entered
		// here and the old one's reader sees a closed channel and ends.
		r.close(previous)
	}
	r.sessions[podUID] = s

	return s.outbox, func() { r.leave(podUID, s) }, nil
}

// leave removes a session, unless a later one has already replaced it.
func (r *Registry) leave(podUID string, s *session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions[podUID] != s {
		// Superseded by a renewal. The map already holds the newer session and
		// closing on this one's behalf would cut the live stream.
		return
	}
	r.close(s)
	delete(r.sessions, podUID)
}

// close ends a session's channel at most once. Callers hold r.mu.
func (r *Registry) close(s *session) {
	if s.closed {
		return
	}
	s.closed = true
	close(s.outbox)
}

// send queues a message, or cuts the session loose if its queue is full.
//
// Dropping instead would leave the agent serving a mirror it has no way of
// knowing is stale, looking healthy the whole time, until the next resync
// happened to get through. Closing is loud: the stream ends, the agent
// reconnects, and it is rebuilt from a fresh state. proxyreg made this same
// choice for the same reason.
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

// Resync re-sends every live session its namespace's state.
func (r *Registry) Resync(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Built once per namespace rather than once per session: a namespace with
	// twenty backends would otherwise do twenty identical List passes on every
	// tick, and they cannot differ -- the lock is held throughout.
	built := make(map[string]*agentpb.NetworkState)
	for podUID, s := range r.sessions {
		state, ok := built[s.namespace]
		if !ok {
			var err error
			state, err = r.opts.State.Build(ctx, s.namespace)
			if err != nil {
				// One unreadable namespace must not stop the others. The next
				// tick tries again and the session keeps its last known state
				// until then, which is the correct answer while the operator
				// cannot read.
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

// NeedLeaderElection makes this leader-bound, for the reason proxyreg gives:
// only the leader holds the streams these messages go to.
func (r *Registry) NeedLeaderElection() bool { return true }

func stateMessage(state *agentpb.NetworkState) *agentpb.OperatorToServer {
	return &agentpb.OperatorToServer{
		Message: &agentpb.OperatorToServer_NetworkState{NetworkState: state},
	}
}
