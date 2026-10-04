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

// Package proxyreg is the port the controllers reach the proxies through. It
// owns every live proxy session and turns a registration decision into messages
// on them.
//
// It mirrors internal/agent in the opposite direction. Neither lives inside
// internal/agentserver, so neither direction has to know about TLS, tokens or
// streams.
package proxyreg

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/phase"
)

const (
	// DefaultResyncInterval is how often every live session is re-sent its
	// snapshot; see Resync for why it is not optional.
	DefaultResyncInterval = 30 * time.Second
	// DefaultOutboxSize is how far a session may fall behind before it is cut.
	DefaultOutboxSize = 64
)

type Options struct {
	// Reader should be the manager's cached client: snapshot reads it under the
	// mutex.
	Reader client.Reader
	// ResyncInterval is how often Start re-syncs every session. Zero means
	// DefaultResyncInterval.
	ResyncInterval time.Duration
	// OutboxSize bounds a session's queue. Zero means DefaultOutboxSize.
	OutboxSize int
	// State builds the NetworkState a session is sent after its FullSync.
	// Optional, unlike the FullSync: a proxy without the mirror still routes.
	State netstate.Source
}

type session struct {
	namespace string
	group     string
	outbox    chan *agentpb.OperatorToProxy
	closed    bool
	// lastReady suppresses re-sending the same readiness on every reconcile;
	// Resync re-asserts it each interval. It lives on the session so a
	// reconnect re-asserts the state without the operator noticing the
	// reconnect.
	lastReady    bool
	lastReadySet bool
	// wantsEvents is whether this agent last reported anybody reading cloud
	// events.
	wantsEvents bool
}

type Fleet struct {
	mu sync.Mutex
	// keyed by pod UID, the same key the agent registry uses
	sessions map[string]*session
	opts     Options
}

func New(opts Options) *Fleet {
	if opts.ResyncInterval <= 0 {
		opts.ResyncInterval = DefaultResyncInterval
	}
	if opts.OutboxSize <= 0 {
		opts.OutboxSize = DefaultOutboxSize
	}
	return &Fleet{sessions: make(map[string]*session), opts: opts}
}

// Join enters a session and returns its outbox together with the function that
// removes it. The first message on the channel is always the FullSync: it is
// built and queued under the mutex every broadcast takes, so no broadcast can
// overtake it.
//
// The Fleet closes the channel if the session falls too far behind. A caller
// that reads a closed channel must end its stream; see send.
func (f *Fleet) Join(ctx context.Context, namespace, group, podUID string) (<-chan *agentpb.OperatorToProxy, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// sessions.enter cannot interrupt sessionPrologue's two stream.Send calls,
	// so a superseded stream can reach Join after its successor did. Its
	// context is cancelled by then; going on would close the successor's live
	// outbox.
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	initial, err := f.snapshot(ctx, namespace, group)
	if err != nil {
		return nil, nil, err
	}

	// Sized for the initial burst on top of the steady-state budget, so joining
	// while many servers drain cannot cut the session.
	s := &session{
		namespace: namespace,
		group:     group,
		outbox:    make(chan *agentpb.OperatorToProxy, f.opts.OutboxSize+len(initial)),
	}
	for _, msg := range initial {
		s.outbox <- msg
	}
	// Closed here because the displaced session's own leave will find a
	// different pointer at this key and close nothing.
	if previous, ok := f.sessions[podUID]; ok {
		f.close(previous)
	}
	f.sessions[podUID] = s

	return s.outbox, func() { f.leave(podUID, s) }, nil
}

// leave removes a session only if it is still the one registered for that pod:
// under make-before-break a displaced session's leave runs after its successor
// entered.
func (f *Fleet) leave(podUID string, s *session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessions[podUID] != s {
		return
	}
	delete(f.sessions, podUID)
	f.close(s)
}

// Callers hold f.mu.
func (f *Fleet) close(s *session) {
	if s.closed {
		return
	}
	s.closed = true
	close(s.outbox)
}

// send queues a message, or cuts the session loose if its queue is full.
// Dropping it instead would leave the proxy routing on a stale list that looks
// healthy; closing makes the agent reconnect and get a fresh FullSync.
//
// Callers hold f.mu.
func (f *Fleet) send(s *session, msg *agentpb.OperatorToProxy) {
	if s.closed {
		return
	}
	select {
	case s.outbox <- msg:
	default:
		SessionsCut.Inc()
		f.close(s)
	}
}

// snapshot builds what a session is sent on join and on every resync: the full
// registered list, followed by one DrainPlayers per draining server.
//
// Callers hold f.mu.
func (f *Fleet) snapshot(ctx context.Context, namespace, group string) ([]*agentpb.OperatorToProxy, error) {
	servers := &spawneryv1alpha1.ServerList{}
	if err := f.opts.Reader.List(ctx, servers, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list servers in %s: %w", namespace, err)
	}

	sync := &agentpb.FullSync{}
	var draining []*spawneryv1alpha1.Server
	for i := range servers.Items {
		srv := &servers.Items[i]
		// status.registered, not phase Ready: the flag records what the proxies
		// were told, and the two disagree for one reconcile after a
		// deregistration.
		if srv.Status.Registered && srv.Status.Address != "" {
			sync.Servers = append(sync.Servers, registeredServer(srv))
		}
		if srv.Status.Phase == string(phase.Draining) {
			draining = append(draining, srv)
		}
	}
	// Sorted so an unchanged state is the same bytes and an agent can skip it.
	sort.Slice(sync.Servers, func(i, j int) bool { return sync.Servers[i].GetName() < sync.Servers[j].GetName() })
	sort.Slice(draining, func(i, j int) bool { return draining[i].Name < draining[j].Name })

	out := []*agentpb.OperatorToProxy{{
		Message: &agentpb.OperatorToProxy_FullSync{FullSync: sync},
	}}
	fallbacks := f.fallbacks(ctx, namespace, group)
	for _, srv := range draining {
		out = append(out, drainMessage(srv, fallbacks))
	}

	// Last: ProxyRole opens the readiness gate on the FullSync, and the drains
	// refer to its list. A state that cannot be built is skipped: losing the
	// mirror must not cost routing.
	if f.opts.State.Reader != nil {
		state, err := f.opts.State.Build(ctx, namespace, netstate.ForProxies)
		if err != nil {
			log.FromContext(ctx).V(1).Info("skipped a proxy's network state",
				"namespace", namespace, "reason", err.Error())
		} else {
			out = append(out, &agentpb.OperatorToProxy{
				Message: &agentpb.OperatorToProxy_NetworkState{NetworkState: state},
			})
		}
	}
	return out, nil
}

// fallbacks reads one ProxyGroup's fallback list. Per group, not a union: two
// ProxyGroups may route to different fallbacks.
//
// A missing ProxyGroup yields an empty list; a proxy pod outlives its group
// until the orphan sweep.
func (f *Fleet) fallbacks(ctx context.Context, namespace, group string) []string {
	pg := &spawneryv1alpha1.ProxyGroup{}
	key := types.NamespacedName{Name: group, Namespace: namespace}
	if err := f.opts.Reader.Get(ctx, key, pg); err != nil {
		return nil
	}
	return pg.Spec.Routing.FallbackGroups
}

func registeredServer(srv *spawneryv1alpha1.Server) *agentpb.RegisteredServer {
	return &agentpb.RegisteredServer{
		Name:    srv.Name,
		Address: srv.Status.Address,
		Group:   srv.Spec.GroupRef.Name,
	}
}

func drainMessage(srv *spawneryv1alpha1.Server, fallbacks []string) *agentpb.OperatorToProxy {
	return &agentpb.OperatorToProxy{
		Message: &agentpb.OperatorToProxy_DrainPlayers{
			DrainPlayers: &agentpb.DrainPlayers{
				FromServer: srv.Name,
				ToGroups:   fallbacks,
			},
		},
	}
}

// broadcast delivers one message to every session in a namespace. build takes
// the session because DrainPlayers differs per ProxyGroup; a nil result sends
// nothing.
func (f *Fleet) broadcast(namespace string, build func(*session) *agentpb.OperatorToProxy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.namespace != namespace {
			continue
		}
		if msg := build(s); msg != nil {
			f.send(s, msg)
		}
	}
}

// SetInterest records whether this session's agent has anybody to show events
// to. An unknown pod is ignored: a report from a just-displaced session would
// otherwise leak an entry per reconnect.
func (f *Fleet) SetInterest(podUID string, wanted bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[podUID]; ok {
		s.wantsEvents = wanted
	}
}

// Interested reports what SetInterest last recorded. Exported for tests.
func (f *Fleet) Interested(podUID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[podUID]
	return ok && s.wantsEvents
}

// Publish sends one event to every session in the namespace that asked for
// events. It implements cloudevent.Publisher and reports nothing: a feed nobody is
// watching must not be able to fail a reconcile.
func (f *Fleet) Publish(namespace string, ev *agentpb.CloudEvent) {
	f.broadcast(namespace, func(s *session) *agentpb.OperatorToProxy {
		if !s.wantsEvents {
			return nil
		}
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_CloudEvent{CloudEvent: ev},
		}
	})
}

// Move asks the proxies of a namespace to move one player to one server.
// Broadcast, because nothing here knows which proxy has the player; a proxy
// without that player ignores it. No outcome is reported; see
// agentpb.ConnectResult.
func (f *Fleet) Move(namespace, playerUUID, targetServer string) {
	f.broadcast(namespace, func(*session) *agentpb.OperatorToProxy {
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_MovePlayer{
				MovePlayer: &agentpb.MovePlayer{
					PlayerUuid:   playerUUID,
					TargetServer: targetServer,
				},
			},
		}
	})
}

// SendState sends every session in a namespace a fresh NetworkState now
// rather than at the next Resync.
func (f *Fleet) SendState(ctx context.Context, namespace string) {
	if f.opts.State.Reader == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	state, err := f.opts.State.Build(ctx, namespace, netstate.ForProxies)
	if err != nil {
		log.FromContext(ctx).V(1).Info("skipped a proxy state push",
			"namespace", namespace, "reason", err.Error())
		return
	}
	msg := &agentpb.OperatorToProxy{
		Message: &agentpb.OperatorToProxy_NetworkState{NetworkState: state},
	}
	for _, s := range f.sessions {
		if s.namespace == namespace {
			f.send(s, msg)
		}
	}
}

// Register implements controller.Registrar. With no proxy connected it is a
// no-op: a Network without a ProxyGroup is legitimate.
func (f *Fleet) Register(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	f.broadcast(srv.Namespace, func(*session) *agentpb.OperatorToProxy {
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_RegisterServer{
				RegisterServer: &agentpb.RegisterServer{Server: registeredServer(srv)},
			},
		}
	})
	return nil
}

// Deregister implements controller.Registrar.
func (f *Fleet) Deregister(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	f.broadcast(srv.Namespace, func(*session) *agentpb.OperatorToProxy {
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_UnregisterServer{
				UnregisterServer: &agentpb.UnregisterServer{Name: srv.Name},
			},
		}
	})
	return nil
}

// Drain implements controller.Registrar.
func (f *Fleet) Drain(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	f.broadcast(srv.Namespace, func(s *session) *agentpb.OperatorToProxy {
		return drainMessage(srv, f.fallbacks(ctx, s.namespace, s.group))
	})
	return nil
}

// SetReady tells one proxy whether it should be taking new connections; its
// readiness decides whether the Service keeps its endpoint. A pod with no live
// stream is not an error.
//
// A repeat of the memoized value is not sent; Resync re-asserts it each
// interval.
func (f *Fleet) SetReady(ctx context.Context, podUID string, ready bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	s, ok := f.sessions[podUID]
	if !ok {
		return nil
	}
	if s.lastReadySet && s.lastReady == ready {
		return nil
	}
	s.lastReady, s.lastReadySet = ready, true
	f.send(s, readyMessage(ready))
	return nil
}

func readyMessage(ready bool) *agentpb.OperatorToProxy {
	return &agentpb.OperatorToProxy{
		Message: &agentpb.OperatorToProxy_SetReady{
			SetReady: &agentpb.SetReady{Ready: ready},
		},
	}
}

// Resync re-sends every live session the same construction Join builds,
// followed by the readiness this session was last told to have.
//
// Not redundant with Join's ordering: a FullSync built from a cache that has
// not yet seen a deregistration keeps that server in the proxy's list, and
// nothing else would correct it. Likewise SetReady sends only on a change, so
// re-asserting here bounds any disagreement between the agent's gate and the
// memo to one interval.
//
// Readiness goes after the snapshot: a ready proxy with no server list
// disconnects every player with "no available server", and older or foreign
// agents do not guard against that. A session never told a readiness is sent
// none.
func (f *Fleet) Resync(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for podUID, s := range f.sessions {
		messages, err := f.snapshot(ctx, s.namespace, s.group)
		if err != nil {
			// One unreadable namespace must not stop the others; the session
			// keeps its last list until the next tick.
			log.FromContext(ctx).V(1).Info("skipped a proxy resync",
				"pod", podUID, "namespace", s.namespace, "reason", err.Error())
			continue
		}
		for _, msg := range messages {
			f.send(s, msg)
		}
		if s.lastReadySet {
			f.send(s, readyMessage(s.lastReady))
		}
	}
}

// Start runs the resync ticker until ctx ends. It implements manager.Runnable.
func (f *Fleet) Start(ctx context.Context) error {
	ticker := time.NewTicker(f.opts.ResyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			f.Resync(ctx)
		}
	}
}

// NeedLeaderElection: only the leader holds the streams these messages go to.
func (f *Fleet) NeedLeaderElection() bool { return true }
