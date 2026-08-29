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

// Package agentserver is the gRPC endpoint the in-game agents connect to. It
// is the only writer of the agent registry: every message an agent sends ends
// up as registry state, and the controllers read nothing but snapshots of it.
//
// The identity of a stream never comes from its messages — grpcauth derives it
// from the bearer token — so a compromised agent can only ever lie about
// itself.
package agentserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/netstate"
)

const (
	// DefaultPort is where the agents look for the operator.
	DefaultPort = 9443
	// shutdownGrace is how long a shutdown waits for the streams to end by
	// themselves before it cuts them. Every RPC here is a long-lived stream,
	// so an unbounded GracefulStop would wait for the agents' own deadlines.
	shutdownGrace = 5 * time.Second

	// MaxConcurrentStreams bounds streams on ONE connection. An agent opens
	// exactly one -- proto/spawnery/agent/v1alpha1 has two RPCs and a session
	// uses one of them -- so this is generous by an order of magnitude. What it
	// does not bound is how many connections a pod may open: that is
	// MaxConnectionsPerPeer below, and PeerLimiter's doc comment says why none
	// of the bounds that came before it reaches that far.
	MaxConcurrentStreams uint32 = 8

	// ConnectionTimeout bounds how long a half-finished handshake holds
	// resources. grpc-go's default is two minutes.
	ConnectionTimeout = 30 * time.Second

	// MaxConnectionIdle reaps a connection carrying no stream. An agent's
	// session stream is long-lived, so a connection that has been idle this
	// long has lost its agent.
	//
	// It is not a partition detector and must not be read as one. A
	// black-holed connection carries a live stream, so this never fires on
	// one, and no keepalive Time is set either, so the transport learns a
	// partitioned peer is gone only when TCP's own retransmission gives up --
	// minutes, on a default Linux. Measured 2026-08-26 from the agent's side
	// through a freezable relay: over 200 seconds, and twice not at all
	// within 213.
	//
	// **The operator does not need the transport to tell it, and setting a
	// keepalive here would make it worse rather than better.** The agent's
	// reports stop the instant its peer does, and phase.Inputs.AgentSilent
	// reads "the stream is up and has gone quiet" for what it is, within twice
	// the report interval -- ten seconds at the default, against the 65 a
	// keepalive could manage at the fastest this server's own
	// MinKeepaliveInterval permits a client to be asked. Worse than slower: a
	// keepalive that broke the stream would turn AgentSilent into an ordinary
	// broken stream, which phase tolerates for StreamDownGrace and which does
	// *not* carry StartDrain -- so the twenty-second window for moving players
	// off a backend that will never answer again would be traded for a socket
	// closed sooner. HardDeadline already bounds how long that socket can lie.
	//
	// The agent is the end with no application signal, because nothing arrives
	// on a healthy stream between one operator instruction and the next. That
	// is why the keepalive is on the client and only on the client; see
	// OperatorChannel.KEEPALIVE_SECONDS, and hack/agent-test.sh's seventh
	// phase, which measured the agent giving up 64 seconds after a stub went
	// deaf.
	MaxConnectionIdle = 5 * time.Minute

	// MaxConnectionsPerPeer bounds how many connections one peer address may
	// hold at once. PeerLimiter says what a peer is, why the bound sits on the
	// listener, and what it does and does not close. This is where the number
	// comes from.
	//
	// A legitimate agent's peak is 2, and that is measured rather than read
	// off SessionLoop. Renewal is make-before-break and every attempt builds
	// its own ManagedChannel, so the replacement's connection and the outgoing
	// one overlap for the length of a handover. cmd/spawnery-stubop counts
	// connections for exactly this (see its zero-limit PeerLimiter); against
	// the pinned 0.2.1 images on 2026-08-26 the high-water mark was 2 in every
	// run, over roughly seventy renewals and four paths:
	//
	//	Paper, plain renewal            17 renewals   peak 2
	//	Paper, --supersede              17 renewals   peak 2
	//	Paper, --mute-after             8 give-ups    peak 2
	//	Velocity, --proxy               18 renewals   peak 2
	//
	// The give-up path is the interesting one: it drops to 0 between attempts,
	// because an operator that never answered leaves nothing to hand over.
	// Nothing observed 3.
	//
	// Eight is four times that, and the factor is a judgement where the peak
	// is a measurement. It is loose on purpose, because the two directions
	// cost differently. Too high costs a bounded multiple of one connection --
	// the attack is *unbounded*, so it is already defeated at any small finite
	// number, and 8 versus 4 is not a security difference. Too low costs a
	// working agent its session, on a bound nothing in the fleet can see
	// coming. It also leaves room for the one legitimate shape that would
	// exceed 2 without anything being wrong: an agent holding both of
	// AgentService's RPCs at once, each renewing, which is 4. A pod is one
	// role today, so no agent does.
	//
	// hack/agent-test.sh asserts the peak against this constant, so an agent
	// change that raises it fails there rather than in a cluster.
	MaxConnectionsPerPeer = 8

	// FleetConnectionsPerAgent is the bound every peer falls back to once the
	// connections open across the fleet pass what its pod count can account
	// for. PeerLimiter says why a fleet bound has to work by withdrawing slack
	// rather than by capping a total; this is where the number comes from.
	//
	// Four is twice a legitimate agent's measured peak of 2, and it is not a
	// judgement in the way MaxConnectionsPerPeer's factor of four is. It is
	// the one legitimate shape that has ever been argued to exceed the peak,
	// written down beside the peak itself: an agent holding both of
	// AgentService's RPCs at once, each renewing. No agent does that today, so
	// nothing in the fleet reaches it, and that is the property this number is
	// chosen for -- when the fleet bound binds, it must refuse only
	// connections no working agent would have asked for.
	//
	// It doubles as the multiplier on the pod count: the ceiling that turns
	// the bound on is ExpectedAgents * FleetConnectionsPerAgent. The two being
	// the same number is what makes the bound converge -- a fleet held at
	// four per peer sits exactly at its own ceiling and stops there, instead
	// of oscillating around a ceiling it can pass.
	FleetConnectionsPerAgent = 4

	// MinKeepaliveInterval is how often a client may ping. The agents send no
	// keepalive at all -- agent/common's SessionLoop says so in its own
	// comment: "the channel underneath has no keepalive, no idle timeout" --
	// so this cannot throttle a legitimate agent. It bounds a client that
	// decides to ping in a loop.
	MinKeepaliveInterval = 30 * time.Second
)

// ProxyFleet is the one thing ProxySession needs from the proxy fan-out.
// *proxyreg.Fleet has a much wider surface — Register, Deregister, Drain,
// Resync — that belongs to the controllers, which write into the fan-out.
// ProxySession only ever reads from it, and giving it the concrete *Fleet
// would let a future change to this handler start writing too without the
// compiler ever objecting. Narrowing the type here is what keeps that true.
//
// It also matters for what a test can observe. Fleet.Join's guard against a
// displaced handler is only sound because ProxySession passes it the
// enter-derived context, not stream.Context(); with the concrete type there
// was no seam to substitute a fake and watch which one arrives.
type ProxyFleet interface {
	// Join is *proxyreg.Fleet.Join: see its doc comment for the contract.
	Join(ctx context.Context, namespace, group, podUID string) (<-chan *agentpb.OperatorToProxy, func(), error)
	// Move is *proxyreg.Fleet.Move: it broadcasts and reports nothing, which
	// is why it returns nothing here either.
	Move(namespace, playerUUID, targetServer string)
}

// ServerFanout is the backend side's counterpart, narrowed to the one method
// ServerSession calls for the reasons ProxyFleet gives above -- a package that
// depended on the whole of internal/serverreg would drag its resync ticker and
// its metric into every test that opens a stream.
//
// A backend joins by namespace and not by group: its mirror is the whole
// network, where a proxy's FullSync is scoped to what its own group routes to.
type ServerFanout interface {
	// Join is *serverreg.Registry.Join: see its doc comment for the contract.
	Join(ctx context.Context, namespace, podUID string) (<-chan *agentpb.OperatorToServer, func(), error)
}

// Options configures the server. The three durations are what the operator
// dictates to its agents; both sides derive their thresholds from them, so
// they are never guessed twice.
type Options struct {
	// Addr is the listen address. Port 0 lets the kernel pick, which is what
	// the tests use.
	Addr     string
	Provider *certs.Provider
	Auth     *grpcauth.Authenticator
	Agents   *agent.Registry
	// Proxies is the fan-out every proxy session joins. Required for
	// ProxySession; a nil one is a programming error, not a runtime state.
	// The narrow ProxyFleet interface, not *proxyreg.Fleet: see its doc
	// comment for why.
	Proxies ProxyFleet
	// Servers is the fan-out every backend session joins. Required for
	// ServerSession, and refused in New for the reason a nil Proxies is: a nil
	// here surfaces as a panic inside a session, minutes after start and in a
	// goroutine, rather than as a startup error.
	Servers ServerFanout
	// State builds the picture a request is resolved against. Required for
	// CloudRequest and refused in New with the two fan-outs, for the reason
	// they are: a nil here is a panic inside a session rather than at start.
	State netstate.Source
	// ReportInterval is how often an agent should report its player count.
	ReportInterval time.Duration
	// RenewAfter is when an agent should open its next stream — before the
	// current one ends, so the operator never sees the pod as disconnected.
	RenewAfter time.Duration
	// HardDeadline is when the operator closes a stream regardless. It must be
	// above RenewAfter, or a well-behaved agent would be cut off mid-renewal.
	HardDeadline time.Duration
	// Fleet answers how many agents this operator ought to be serving, and
	// whether it knows yet. It is what turns the per-peer connection bound
	// into one the fleet as a whole cannot multiply: see PeerLimiter.Expect,
	// which this is handed to unchanged. Nil means no fleet bound.
	Fleet func() (int, bool)
	// Clock reads the wall clock for the session duration this server logs.
	// It does not drive HardDeadline: that runs on time.AfterFunc against the
	// real clock, so a test cannot shorten the deadline by moving Clock
	// forward — it has to configure a shorter HardDeadline instead.
	Clock func() time.Time
}

// Server serves AgentService.
type Server struct {
	agentpb.UnimplementedAgentServiceServer

	opts     Options
	sessions *sessions
	// requestRate bounds how often one pod may ask for something. Its own
	// bucket and not grpcauth's: see requestLimiter for why two.
	requestRate *requestLimiter
	// addr is the address the kernel actually handed out. It is written once
	// by Start and read from other goroutines, hence the atomic.
	addr atomic.Pointer[string]
}

// New creates the server. It does not listen yet; Start does.
//
// It panics if opts.Proxies is nil — a programming error the caller must fix,
// not a runtime state the server can run without.
func New(opts Options) *Server {
	if opts.Addr == "" {
		opts.Addr = fmt.Sprintf(":%d", DefaultPort)
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	// Refused here rather than at the first proxy stream: a nil fleet would
	// surface as a panic inside a gRPC handler, minutes after start and in a
	// goroutine, instead of as a startup error.
	if opts.Proxies == nil {
		panic("agentserver: no proxy fleet")
	}
	if opts.Servers == nil {
		panic("agentserver: no server fanout")
	}
	if opts.State.Reader == nil {
		panic("agentserver: no network state source")
	}
	return &Server{opts: opts, sessions: newSessions(), requestRate: newRequestLimiter(opts.Clock)}
}

// Addr is the address the listener actually bound, empty until Start has one.
// With port 0 that is the only way to learn where the server ended up.
func (s *Server) Addr() string {
	if a := s.addr.Load(); a != nil {
		return *a
	}
	return ""
}

// NeedLeaderElection makes this a leader-bound runnable. Only the leader may
// hold agent streams: two operators writing the same registry would each see
// half the fleet.
func (s *Server) NeedLeaderElection() bool { return true }

// Start listens and serves until ctx ends. It is a manager Runnable.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("agentserver")

	listener, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.opts.Addr, err)
	}
	bound := listener.Addr().String()
	s.addr.Store(&bound)

	// Bounded per peer before the TLS handshake, which is the expensive half
	// of a connection and the half the attack is after. PeerLimiter's doc
	// comment carries the argument for the cut and for the key.
	//
	// The log is deliberately not one line per refusal. A peer that is over
	// its bound is by definition one opening connections as fast as it can, so
	// a line each would hand it the operator's log as the amplifier the
	// connections themselves no longer are. One line at the first refusal and
	// then at every power of ten keeps an episode legible -- first, tenth,
	// hundredth -- while the count an operator actually alerts on lives in
	// spawnery_agent_connections_refused_total.
	limited := NewPeerLimiter(listener, MaxConnectionsPerPeer, func(ev ConnEvent) {
		if !ev.Refused || !isPowerOfTen(ev.Refusals) {
			return
		}
		logger.Info("refusing connections at a limit",
			"peer", ev.Peer, "bound", ev.Bound, "limit", ev.Limit,
			"open", ev.Open, "total", ev.Total, "refused", ev.Refusals)
	})
	// The fleet bound, when the operator can count its own pods. Nil is the
	// tests' wiring and means peers are bounded and the fleet is not; Expect's
	// doc comment says why unknown has to fail open.
	limited.Expect(s.opts.Fleet)

	// GetCertificate rather than a fixed certificate: the provider rotates it
	// underneath us and every handshake must pick up the current one.
	creds := credentials.NewTLS(&tls.Config{
		GetCertificate: s.opts.Provider.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	})
	grpcServer := grpc.NewServer(
		grpc.Creds(creds),
		grpc.StreamInterceptor(s.opts.Auth.StreamInterceptor()),
		grpc.MaxConcurrentStreams(MaxConcurrentStreams),
		grpc.ConnectionTimeout(ConnectionTimeout),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: MaxConnectionIdle,
		}),
		// PermitWithoutStream is false: a client with no active stream has no
		// reason to ping, and the agents never do.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             MinKeepaliveInterval,
			PermitWithoutStream: false,
		}),
	)
	agentpb.RegisterAgentServiceServer(grpcServer, s)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		logger.Info("stopping the agent endpoint")
		graceful := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(graceful)
		}()
		select {
		case <-graceful:
		case <-time.After(shutdownGrace):
			// The streams are meant to outlive any single shutdown, so
			// waiting for them to end on their own would hang the manager.
			logger.Info("cutting the remaining agent streams")
			grpcServer.Stop()
		}
	}()

	logger.Info("serving agents", "addr", bound)
	if err := grpcServer.Serve(limited); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve agents: %w", err)
	}
	<-stopped
	return nil
}

// ServerSession is the Paper agent's channel.
func (s *Server) ServerSession(stream agentpb.AgentService_ServerSessionServer) error {
	id, logger, ctx, cleanup, err := s.sessionPrologue(stream.Context(), agent.RoleServer, func() error {
		if err := stream.Send(&agentpb.OperatorToServer{
			Message: &agentpb.OperatorToServer_ReportInterval{
				ReportInterval: &agentpb.ReportInterval{Seconds: seconds(s.opts.ReportInterval)},
			},
		}); err != nil {
			return err
		}
		return stream.Send(&agentpb.OperatorToServer{
			Message: &agentpb.OperatorToServer_SessionDeadline{
				SessionDeadline: &agentpb.SessionDeadline{
					RenewAfterSeconds:   seconds(s.opts.RenewAfter),
					HardDeadlineSeconds: seconds(s.opts.HardDeadline),
				},
			},
		})
	})
	if err != nil {
		return err
	}
	defer cleanup()

	outbox, leaveFanout, err := s.opts.Servers.Join(ctx, id.Namespace, id.PodUID)
	if err != nil {
		return status.Errorf(codes.Unavailable, "join the server fanout: %v", err)
	}
	defer leaveFanout()

	received, errs := recvPump(ctx, stream.Recv)

	for {
		select {
		case <-ctx.Done():
			return status.Error(codes.Unavailable, "session ended, reconnect with a fresh token")
		case err := <-errs:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case msg := <-received:
			if answer := s.handle(ctx, logger, id, msg); answer != nil {
				if err := stream.Send(answer); err != nil {
					return err
				}
			}
		case msg, ok := <-outbox:
			if !ok {
				// The same two-readiness race ProxySession's own branch
				// explains: a renewal cancels this ctx and closes this outbox
				// within a few lines of each other, so both cases can be ready
				// at once and Go picks arbitrarily. ctx.Err() is what tells a
				// supersede apart from an agent that actually fell behind.
				if ctx.Err() != nil {
					return status.Error(codes.Unavailable, "session ended, reconnect with a fresh token")
				}
				// Cut loose. Ending the stream is the point: a mirror the
				// agent cannot know is stale is worse than a reconnect, and
				// the reconnect rebuilds it from a fresh state.
				return status.Error(codes.ResourceExhausted, "server fell behind, reconnect for a fresh state")
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// sendBounded runs a session's opening sends under a bound of its own, and
// gives up on them by returning rather than by cancelling anything.
//
// stream.Send blocks on the client's flow-control window and observes no
// context at all: an agent that opens a stream, completes the handshake and
// then stops reading holds this goroutine, and the only thing that ends a
// blocked Send is the handler returning, which closes the stream. Nothing
// bounded that.
//
// docs/known-issues.md carried this as "the operator's hard-deadline rescue is
// armed after its first two Sends", with moving the time.AfterFunc above them
// as the fix that was necessary but not sufficient. Reading it again on
// 2026-08-24: it is neither. sessions.cancel does reach a handler once the
// handler is in its loop -- ServerSession and ProxySession both select on
// ctx.Done() there -- and it reaches none inside Send, whichever order the
// timer is armed in. The window is the send itself, so the bound belongs on
// the send.
//
// The channel is buffered by one so the goroutine cannot leak on the timeout
// path: the blocked Send returns as soon as the handler's return closes the
// stream, and its result goes into the buffer with nobody left listening.
// That one character is reasoned rather than tested, and deliberately -- see
// TestTheOpeningSendsAreBounded for why no assertion in this package can
// observe it without a goroutine count that would make the suite flaky.
func sendBounded(deadline time.Duration, send func() error) error {
	sent := make(chan error, 1)
	go func() { sent <- send() }()
	select {
	case err := <-sent:
		return err
	case <-time.After(deadline):
		return status.Errorf(codes.DeadlineExceeded,
			"the agent did not read its opening messages within %s", deadline)
	}
}

// sessionPrologue is everything a session does before its main loop, and
// everything ServerSession and ProxySession would otherwise both write out by
// hand: authenticate the stream, register it with make-before-break semantics
// (entering sessions, choosing Connect vs. Supersede, bumping OpenStreams),
// send the two fixed messages every session opens with, and start the
// hard-deadline timer.
//
// It was pulled out for the same reason recvPump was: a divergence here is
// invisible from outside and dangerous on the inside. Two copies of the
// enter/Supersede/OpenStreams sequence could drift so one role's stream
// forgets to bump the gauge it decrements, or registers before deciding
// superseded vs. fresh — either wrong and no test would catch it, because
// both roles look identical to a client watching the wire. sendFixed is the
// one piece left to the caller: the two fixed messages carry the same values
// on every stream, but OperatorToServer and OperatorToProxy are different
// wire types with no message in common, so building them is the caller's job.
// Everything sendFixed does is bracketed by the same Connect/Supersede and
// OpenStreams bookkeeping either session needs, in the same order the
// original two handlers used, so a Send failure here still decrements the
// gauge and reports the disconnect exactly as if the loop below had run and
// exited immediately.
//
// gen is deliberately not among the return values. Both of its uses — leave's
// identity check and the hard-deadline timer's cancel — now live entirely
// inside this function and its returned cleanup; nothing past this point ever
// needs the raw number, and returning it just to satisfy a shape the callers
// do not use would be dead weight.
func (s *Server) sessionPrologue(streamCtx context.Context, role agent.Role, sendFixed func() error) (
	grpcauth.Identity, logr.Logger, context.Context, func(), error) {
	id, ok := grpcauth.IdentityFrom(streamCtx)
	if !ok {
		return grpcauth.Identity{}, logr.Logger{}, nil, nil, status.Error(codes.Unauthenticated, "no identity on the stream")
	}
	logger := log.FromContext(streamCtx).WithValues("pod", id.PodName, "namespace", id.Namespace)
	openedAt := s.opts.Clock()

	ctx, gen, superseded := s.sessions.enter(streamCtx, id.PodUID)
	leave := func() {
		// Only the current stream may report the disconnect. A superseded one
		// must not, or make-before-break would break.
		if s.sessions.leave(id.PodUID, gen) {
			s.opts.Agents.Disconnect(id.PodUID)
		}
		logger.V(1).Info("session ended", "after", s.opts.Clock().Sub(openedAt))
	}

	if superseded {
		// The stream this one displaced was still live, so the agent process
		// never went away: its readiness (for a server) or its place in the
		// fan-out (for a proxy) carries over.
		s.opts.Agents.Supersede(id.PodUID, role)
	} else {
		s.opts.Agents.Connect(id.PodUID, role)
	}
	OpenStreams.WithLabelValues(string(role)).Inc()

	if err := sendBounded(s.opts.HardDeadline, sendFixed); err != nil {
		OpenStreams.WithLabelValues(string(role)).Dec()
		leave()
		return grpcauth.Identity{}, logr.Logger{}, nil, nil, err
	}

	deadline := time.AfterFunc(s.opts.HardDeadline, func() {
		logger.V(1).Info("closing the stream at its hard deadline")
		s.sessions.cancel(id.PodUID, gen)
	})
	cleanup := func() {
		deadline.Stop()
		OpenStreams.WithLabelValues(string(role)).Dec()
		leave()
	}
	return id, logger, ctx, cleanup, nil
}

// handle applies one message. An unknown branch is ignored so a newer agent
// against an older operator keeps working.
// The return is the answer to a request, or nil, for the reason handleProxy's
// own comment gives: an answer belongs to the stream that asked.
func (s *Server) handle(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	msg *agentpb.ServerMessage,
) *agentpb.OperatorToServer {
	switch m := msg.GetMessage().(type) {
	case *agentpb.ServerMessage_Hello:
		// Ready is a state, not an event: the agent repeats it on every
		// connect, so an operator restart cannot leave a server in Starting.
		if m.Hello.GetReady() {
			s.opts.Agents.MarkReady(id.PodUID)
		}
	case *agentpb.ServerMessage_Ready:
		s.opts.Agents.MarkReady(id.PodUID)
	case *agentpb.ServerMessage_PlayerCount:
		if err := s.opts.Agents.ReportPlayers(id.PodUID,
			m.PlayerCount.GetPlayers(), m.PlayerCount.GetSlots()); err != nil {
			// Discard, keep the stream. Spec 5.2: dropping it would be a
			// reconnect loop the agent could trigger at will.
			RejectedReports.WithLabelValues(string(agent.RoleServer)).Inc()
			logger.V(1).Info("discarded a player count", "reason", err.Error())
		}
	case *agentpb.ServerMessage_CloudRequest:
		if c := m.CloudRequest.GetConnect(); c != nil {
			resp := s.answerConnect(ctx, logger, id, m.CloudRequest.GetId(), c)
			return &agentpb.OperatorToServer{
				Message: &agentpb.OperatorToServer_CloudResponse{CloudResponse: resp},
			}
		}
		// Answered rather than ignored, for the reason handleProxy gives.
		return &agentpb.OperatorToServer{
			Message: &agentpb.OperatorToServer_CloudResponse{
				CloudResponse: refuse(m.CloudRequest.GetId(),
					agentpb.RequestError_REASON_UNSPECIFIED,
					"this operator does not know that request"),
			},
		}
	}
	return nil
}

// ProxySession is the Velocity agent's channel. It reads from the fan-out and
// never writes into it: everything a proxy is told is decided by a controller,
// so a compromised proxy cannot make the operator say anything.
func (s *Server) ProxySession(stream agentpb.AgentService_ProxySessionServer) error {
	id, logger, ctx, cleanup, err := s.sessionPrologue(stream.Context(), agent.RoleProxy, func() error {
		if err := stream.Send(&agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_ReportInterval{
				ReportInterval: &agentpb.ReportInterval{Seconds: seconds(s.opts.ReportInterval)},
			},
		}); err != nil {
			return err
		}
		return stream.Send(&agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_SessionDeadline{
				SessionDeadline: &agentpb.SessionDeadline{
					RenewAfterSeconds:   seconds(s.opts.RenewAfter),
					HardDeadlineSeconds: seconds(s.opts.HardDeadline),
				},
			},
		})
	})
	if err != nil {
		return err
	}
	defer cleanup()

	// Joining after the two fixed messages, not before: the FullSync is the
	// first thing whose content depends on the cluster, and the agent needs the
	// deadline in hand before it starts processing a server list.
	outbox, leaveFleet, err := s.opts.Proxies.Join(ctx, id.Namespace, id.Group, id.PodUID)
	if err != nil {
		return status.Errorf(codes.Unavailable, "join the proxy fleet: %v", err)
	}
	defer leaveFleet()

	received, errs := recvPump(ctx, stream.Recv)

	for {
		select {
		case <-ctx.Done():
			return status.Error(codes.Unavailable, "session ended, reconnect with a fresh token")
		case err := <-errs:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case msg := <-received:
			if answer := s.handleProxy(ctx, logger, id, msg); answer != nil {
				if err := stream.Send(answer); err != nil {
					return err
				}
			}
		case msg, ok := <-outbox:
			if !ok {
				// A renewal cancels this session's ctx and closes its outbox
				// within a few lines of each other in the fleet and in
				// sessions.enter, so both select cases can be ready at once —
				// Go picks between them arbitrarily, not in the order the two
				// events actually happened. ctx.Err() is what tells the two
				// apart: non-nil means the session ended for some other
				// reason (superseded, or the server shutting down) and this
				// close is just the fleet catching up, not the proxy actually
				// having fallen behind.
				if ctx.Err() != nil {
					return status.Error(codes.Unavailable, "session ended, reconnect with a fresh token")
				}
				// The fan-out cut us loose. Ending the stream is the point:
				// a partial server list is worse than a reconnect.
				return status.Error(codes.ResourceExhausted, "proxy fell behind, reconnect for a fresh sync")
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// handleProxy applies one message from a proxy. An unknown branch is ignored so
// a newer agent against an older operator keeps working.
// The return is the answer to a request, or nil. It goes back on the stream
// the request arrived on rather than through the fan-out, because an answer
// belongs to one stream and not to the pod: a renewal that displaced the
// asking stream has already failed that request on the agent's side, and
// delivering to the successor would complete an id it never minted.
func (s *Server) handleProxy(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	msg *agentpb.ProxyMessage,
) *agentpb.OperatorToProxy {
	switch m := msg.GetMessage().(type) {
	case *agentpb.ProxyMessage_Hello:
		// A proxy's readiness is not carried here. The agent serves the pod's
		// readiness probe itself (design 6.6), so the kubelet has already
		// written the answer where the ProxyGroup controller reads it.
		//
		// The read timeout is. It is the one thing on this message the
		// operator cannot find out any other way -- it lives in a file the
		// operator never reads, which a configOverlay may lower -- and the
		// operator races it every time a backend's node dies. Zero is what an
		// older agent sends and the registry ignores it, so the fallback stays
		// the value this repository ships. The namespace comes from the
		// authenticated identity, never from the message.
		logger.V(1).Info("proxy connected", "version", m.Hello.GetVersion(),
			"readTimeoutMillis", m.Hello.GetReadTimeoutMillis())
		s.opts.Agents.ReportReadTimeout(id.PodUID, id.Namespace,
			time.Duration(m.Hello.GetReadTimeoutMillis())*time.Millisecond)
	case *agentpb.ProxyMessage_PlayerCount:
		if err := s.opts.Agents.ReportPlayers(id.PodUID,
			m.PlayerCount.GetPlayers(), m.PlayerCount.GetSlots()); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleProxy)).Inc()
			logger.V(1).Info("discarded a player count", "reason", err.Error())
		}
	case *agentpb.ProxyMessage_Heartbeat:
		// Nothing. The stream is its own liveness signal and the registry's
		// staleness rule already derives from ReportInterval. A second liveness
		// path would be a second truth about the same fact.
	case *agentpb.ProxyMessage_BackendPlayers:
		// The namespace comes from the authenticated identity and never from
		// the message, the same rule every other fact on this channel follows:
		// an agent may lie about itself and is believed about nothing else.
		if err := s.opts.Agents.ReportBackends(id.PodUID, id.Namespace,
			m.BackendPlayers.GetPlayers()); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleProxy)).Inc()
			logger.V(1).Info("discarded a backend report", "reason", err.Error())
		}
	case *agentpb.ProxyMessage_PlayerRoster:
		entries := make([]agent.RosterEntry, 0, len(m.PlayerRoster.GetPlayers()))
		for _, p := range m.PlayerRoster.GetPlayers() {
			// An entry with no UUID is dropped rather than stored under "":
			// the reader keys on UUID, so a second empty one would silently
			// replace the first and the roster would show one player where
			// two are on.
			if p.GetUuid() == "" {
				continue
			}
			entries = append(entries, agent.RosterEntry{
				UUID:   p.GetUuid(),
				Name:   p.GetName(),
				Server: p.GetServer(),
			})
		}
		if err := s.opts.Agents.ReportRoster(id.PodUID, id.Namespace, entries); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleProxy)).Inc()
			// V(1), and with no player name in it. This message is the one
			// thing on this channel that identifies a person, and a rejected
			// report is an agent bug rather than something an operator acts on
			// per player.
			logger.V(1).Info("discarded a roster report", "reason", err.Error())
		}
	case *agentpb.ProxyMessage_PlayerJoinedServer:
		// Accepted and ignored. Nothing in milestones 3 or 4 consumes it —
		// player counts come from the servers — and it is on the wire for the
		// dashboard in project 4.
		logger.V(1).Info("player joined a server",
			"player", m.PlayerJoinedServer.GetPlayer(), "server", m.PlayerJoinedServer.GetServer())
	case *agentpb.ProxyMessage_CloudRequest:
		if c := m.CloudRequest.GetConnect(); c != nil {
			resp := s.answerConnect(ctx, logger, id, m.CloudRequest.GetId(), c)
			return &agentpb.OperatorToProxy{
				Message: &agentpb.OperatorToProxy_CloudResponse{CloudResponse: resp},
			}
		}
		// A request kind this operator does not know. Answered rather than
		// ignored: an agent waiting on an id it will never hear about holds
		// its future until the deadline, and the deadline is the slowest way
		// to learn that an operator is older than a plugin.
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_CloudResponse{
				CloudResponse: refuse(m.CloudRequest.GetId(),
					agentpb.RequestError_REASON_UNSPECIFIED,
					"this operator does not know that request"),
			},
		}
	}
	return nil
}

// recvPump moves a stream's receive side onto channels, because Recv blocks
// and the handler has three other things to select on. It is generic over the
// message type: the two sessions differ in nothing else here, and this is the
// part that is easy to get subtly wrong twice.
//
// The goroutine ends when Recv fails or when ctx is done, so it cannot outlive
// its stream.
func recvPump[T any](ctx context.Context, recv func() (T, error)) (<-chan T, <-chan error) {
	received := make(chan T)
	errs := make(chan error, 1)
	go func() {
		defer close(received)
		for {
			msg, err := recv()
			if err != nil {
				errs <- err
				return
			}
			select {
			case received <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	return received, errs
}

// seconds is what goes on the wire. The protocol counts whole seconds, so a
// sub-second duration would round to zero and tell the agent to report in a
// tight loop; one second is the smallest honest answer.
func seconds(d time.Duration) int32 {
	if s := d.Truncate(time.Second); s > 0 {
		return int32(s / time.Second)
	}
	return 1
}
