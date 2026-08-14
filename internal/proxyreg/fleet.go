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

// Package proxyreg is the port the controllers reach the proxies through. It
// owns every live proxy session and turns a registration decision into
// messages on them.
//
// It is the mirror of internal/agent: that package is what the gRPC server
// writes into and the controllers read from, this one is what the controllers
// write into and the gRPC server reads from. Neither lives inside
// internal/agentserver, so that neither direction has to know about TLS,
// tokens or streams.
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
	"github.com/spawnery/spawnery/internal/phase"
)

const (
	// DefaultResyncInterval is how often every live session is re-sent the
	// construction Join builds. See Resync for why it is not optional.
	DefaultResyncInterval = 30 * time.Second
	// DefaultOutboxSize is how far a session may fall behind before it is cut.
	DefaultOutboxSize = 64
)

// Options configures a Fleet.
type Options struct {
	// Reader lists Server objects and reads ProxyGroups. The manager's cached
	// client: FullSync is a read of the informer's indexer, not an API round
	// trip, which is what makes it cheap enough to build under the mutex.
	Reader client.Reader
	// ResyncInterval is how often Start re-syncs every session. Zero means
	// DefaultResyncInterval.
	ResyncInterval time.Duration
	// OutboxSize bounds a session's queue. Zero means DefaultOutboxSize.
	OutboxSize int
}

// session is one live proxy stream's queue.
type session struct {
	namespace string
	group     string
	outbox    chan *agentpb.OperatorToProxy
	// closed guards against a double close. A session is closed either because
	// its stream left or because it fell behind, and both can happen.
	closed bool
	// lastReady is the readiness this session was last told to have, and
	// whether it has been told at all. The operator asserts the desired state
	// on every reconcile; without this the same message would go out every
	// five seconds for the whole of a drain.
	//
	// Suppressing the reconcile's repeats is all it does. It is not a claim
	// that the agent was told once and that settles it: Resync re-sends this
	// value on every tick, which is what bounds a disagreement between the
	// agent's gate and this memo to one interval. Read the two together —
	// this decides the rate, Resync makes it periodic.
	//
	// It belongs to the session and not to the Fleet on purpose: a new stream
	// starts without one, so the state is re-asserted on reconnect without the
	// operator having to know a reconnect happened.
	lastReady    bool
	lastReadySet bool
}

// Fleet is every live proxy session. Safe for concurrent use.
type Fleet struct {
	mu sync.Mutex
	// sessions is keyed by pod UID, the same key the agent registry uses.
	sessions map[string]*session
	opts     Options
}

// New creates a Fleet.
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
// removes it. The first message on the channel is always the FullSync.
//
// That guarantee is the whole point of this function's shape. Everything
// between the lock and the unlock — reading the servers, building the messages,
// filling the queue, entering the session — happens where no broadcast can run,
// because every broadcast takes the same mutex. A RegisterServer therefore
// cannot overtake the FullSync it belongs after, and the ordering is a property
// of the code rather than of a test that has to win a race to notice.
//
// The Fleet closes the channel if the session falls too far behind. A caller
// that reads a closed channel must end its stream; see send.
func (f *Fleet) Join(ctx context.Context, namespace, group, podUID string) (<-chan *agentpb.OperatorToProxy, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// This looks like a redundant guard on a context nothing downstream reads
	// again — until you ask why it could ever be cancelled here at all.
	// sessions.enter cancels a superseded stream's derived context, but that
	// cancellation cannot interrupt the two blocking stream.Send calls
	// sessionPrologue makes before ProxySession ever reaches Join: those are
	// bound to stream.Context(), a different context that enter never
	// touches. So the superseded stream (call it B) can still reach Join
	// after its successor (C) already has — B.enter's cancellation raced
	// nothing C was waiting on, B's two sends simply finished late. Without
	// this check, B would find C installed as "previous" below, close C's
	// live outbox, and install itself in its place: a healthy session killed
	// and mislabelled as having fallen behind. The check is sound because
	// enter happens-before Join within one handler, whichever of the three
	// ways this context ends is the one we observe here: a successor's enter
	// (the case above), the stream's own context ending, or the hard-deadline
	// timer calling sessions.cancel. In the first, a successor already holds
	// the map entry and it, not us, is the one that must keep it. In the
	// other two, this session is ending on its own — nothing has displaced
	// it, but nothing here needs installing either, so returning early is
	// still the right call.
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	initial, err := f.snapshot(ctx, namespace, group)
	if err != nil {
		return nil, nil, err
	}

	// The queue is sized to hold the initial burst on top of the steady-state
	// budget, so a namespace that happens to be draining many servers at once
	// cannot cut a session off at the moment it joins.
	s := &session{
		namespace: namespace,
		group:     group,
		outbox:    make(chan *agentpb.OperatorToProxy, f.opts.OutboxSize+len(initial)),
	}
	for _, msg := range initial {
		s.outbox <- msg
	}
	// A second Join for a pod already registered supersedes the first, the same
	// make-before-break case leave guards against. Close the displaced session
	// here rather than leaving it to leave: once the map entry is overwritten,
	// the old session's own leave call finds a different pointer at this key and
	// exits at the identity guard without closing anything, so the channel would
	// otherwise stay open forever and a caller ranging over it would never see
	// it end.
	if previous, ok := f.sessions[podUID]; ok {
		f.close(previous)
	}
	f.sessions[podUID] = s

	return s.outbox, func() { f.leave(podUID, s) }, nil
}

// leave removes a session, if it is still the one registered for that pod.
//
// The guard matters for the same reason it does in agentserver's sessions:
// make-before-break means an agent opens its next stream before the current
// one ends, so a displaced session's leave runs after its successor has
// already entered. Without the identity check it would remove the live one.
func (f *Fleet) leave(podUID string, s *session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessions[podUID] != s {
		return
	}
	delete(f.sessions, podUID)
	f.close(s)
}

// close closes a session's outbox at most once. Callers hold f.mu.
func (f *Fleet) close(s *session) {
	if s.closed {
		return
	}
	s.closed = true
	close(s.outbox)
}

// send queues a message, or cuts the session loose if its queue is full.
//
// Dropping the message instead would leave the proxy routing on a list it has
// no way of knowing is wrong, looking healthy the whole time, until the next
// resync. Closing is loud: the agent's stream ends, it reconnects, and it is
// rebuilt from a fresh FullSync. A proxy that cannot accept a queue this deep
// is not serving players either.
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
		// status.registered, not phase Ready. The flag is the record of what
		// this operator actually told the proxies — applyDecision writes it in
		// the same block that calls Register — while the phase is the record of
		// what the server is. They disagree for one reconcile after a
		// deregistration, and there the flag is the one that matches the wire.
		if srv.Status.Registered && srv.Status.Address != "" {
			sync.Servers = append(sync.Servers, registeredServer(srv))
		}
		if srv.Status.Phase == string(phase.Draining) {
			draining = append(draining, srv)
		}
	}
	// Sorted so two snapshots of the same state are the same bytes, which is
	// what lets an agent treat an unchanged resync as a no-op.
	sort.Slice(sync.Servers, func(i, j int) bool { return sync.Servers[i].GetName() < sync.Servers[j].GetName() })
	sort.Slice(draining, func(i, j int) bool { return draining[i].Name < draining[j].Name })

	out := []*agentpb.OperatorToProxy{{
		Message: &agentpb.OperatorToProxy_FullSync{FullSync: sync},
	}}
	fallbacks := f.fallbacks(ctx, namespace, group)
	for _, srv := range draining {
		out = append(out, drainMessage(srv, fallbacks))
	}
	return out, nil
}

// fallbacks reads one ProxyGroup's fallback list.
//
// Deliberately per group rather than a union across the namespace: two
// ProxyGroups of one network may route to different fallbacks, and a union
// would tell a proxy to move players onto a group it has no server for.
//
// A missing ProxyGroup is not an error. A proxy pod outlives its group by
// however long the orphan sweep takes, and an empty list is the honest answer
// for that window.
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
// the session because DrainPlayers differs per ProxyGroup.
func (f *Fleet) broadcast(namespace string, build func(*session) *agentpb.OperatorToProxy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.namespace != namespace {
			continue
		}
		f.send(s, build(s))
	}
}

// Register implements controller.Registrar.
//
// It returns nil when no proxy is connected. That is not a degraded state: a
// Network with no ProxyGroup is legitimate, and it is the state every server
// ran in through milestone 2.
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

// SetReady tells one proxy whether it should be taking new connections.
//
// The proxy's readiness is what the kubelet probes and therefore what decides
// whether the Service keeps its endpoint, so this is how the operator takes a
// proxy out of rotation without touching the sessions already on it.
//
// A pod with no live stream is not an error: it is not taking connections
// either, and the next reconcile asserts the state again if it comes back.
//
// The memo below suppresses a repeat of a value this session already carries,
// which is what keeps a five-second reconcile from re-sending the same message
// for the whole of a drain. It is not the only thing that ever asserts: Resync
// re-sends the memo's value on every tick, so the assertion is periodic at the
// resync interval rather than once per change. That is deliberate — see
// Resync — and the proto's SetReady comment describes the pair of them.
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

// readyMessage is one readiness assertion on the wire. SetReady and Resync
// share it so the two paths that assert readiness cannot come to mean
// different things.
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
// This is not in section 5.2 of the main design, and it is not redundant with
// the ordering invariant in Join. That invariant closes the window between a
// broadcast and a session entering. It cannot close the one between the Server
// controller writing a status and the cache snapshot reads from catching up:
//
//	Deregister for X is broadcast and queued on a session
//	the same session rejoins
//	its FullSync is built from a cache that still shows X as registered
//	the proxy ends up with X in its list after being told to drop it
//
// Nothing else would ever correct that. The window is short and this closes it
// within one interval. Do not remove it because "the list is derived from the
// CRs anyway" — that is exactly the reasoning that leaves it broken.
//
// The readiness assertion rides along for the same kind of reason, and it is
// the half of it that is not about the cache. SetReady sends only on a change,
// so if the agent's gate and this session's memo ever disagree — a lost race
// inside the agent, a message applied and then undone, a bug neither end has
// found yet — nothing else re-states the desired value, and the disagreement
// lasts as long as the session does. A pod stuck Ready while its drain
// deadline runs is the expensive direction of that: it keeps taking players
// who are then disconnected when it is deleted. Re-asserting here bounds any
// such divergence to one interval (DefaultResyncInterval, 30s) whatever caused
// it, which is a property no fix to a particular cause can give.
//
// After the snapshot, not before it: on an agent whose first FullSync failed,
// a SetReady(true) arriving first would open its readiness gate while it still
// has no server list, and every player routed there would be disconnected with
// "no available server".
//
// A session never told anything is sent nothing, rather than a default: the
// operator has no readiness for a pod it has not decided about, and inventing
// ready=true here would assert one.
//
// It is exported rather than only ticked, so tests drive it instead of
// sleeping.
func (f *Fleet) Resync(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for podUID, s := range f.sessions {
		messages, err := f.snapshot(ctx, s.namespace, s.group)
		if err != nil {
			// One unreadable namespace must not stop the others. The next tick
			// tries again, and the session keeps its last known list until then
			// — which is the correct answer while the operator cannot read.
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

// NeedLeaderElection makes this leader-bound, for the same reason the agent
// endpoint is: only the leader holds the streams these messages go to.
func (f *Fleet) NeedLeaderElection() bool { return true }
