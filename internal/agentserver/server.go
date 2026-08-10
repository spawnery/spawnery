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
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/grpcauth"
)

const (
	// DefaultPort is where the agents look for the operator.
	DefaultPort = 9443
	// shutdownGrace is how long a shutdown waits for the streams to end by
	// themselves before it cuts them. Every RPC here is a long-lived stream,
	// so an unbounded GracefulStop would wait for the agents' own deadlines.
	shutdownGrace = 5 * time.Second
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
	// ReportInterval is how often an agent should report its player count.
	ReportInterval time.Duration
	// RenewAfter is when an agent should open its next stream — before the
	// current one ends, so the operator never sees the pod as disconnected.
	RenewAfter time.Duration
	// HardDeadline is when the operator closes a stream regardless. It must be
	// above RenewAfter, or a well-behaved agent would be cut off mid-renewal.
	HardDeadline time.Duration
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
	return &Server{opts: opts, sessions: newSessions()}
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

	// GetCertificate rather than a fixed certificate: the provider rotates it
	// underneath us and every handshake must pick up the current one.
	creds := credentials.NewTLS(&tls.Config{
		GetCertificate: s.opts.Provider.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	})
	grpcServer := grpc.NewServer(
		grpc.Creds(creds),
		grpc.StreamInterceptor(s.opts.Auth.StreamInterceptor()),
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
	if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
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
			s.handle(logger, id, msg)
		}
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

	if err := sendFixed(); err != nil {
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
func (s *Server) handle(logger logr.Logger, id grpcauth.Identity, msg *agentpb.ServerMessage) {
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
	}
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
			s.handleProxy(logger, id, msg)
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
func (s *Server) handleProxy(logger logr.Logger, id grpcauth.Identity, msg *agentpb.ProxyMessage) {
	switch m := msg.GetMessage().(type) {
	case *agentpb.ProxyMessage_Hello:
		// A proxy's readiness is not carried here. The agent serves the pod's
		// readiness probe itself (design 6.6), so the kubelet has already
		// written the answer where the ProxyGroup controller reads it.
		logger.V(1).Info("proxy connected", "version", m.Hello.GetVersion())
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
	case *agentpb.ProxyMessage_PlayerJoinedServer:
		// Accepted and ignored. Nothing in milestones 3 or 4 consumes it —
		// player counts come from the servers — and it is on the wire for the
		// dashboard in project 4.
		logger.V(1).Info("player joined a server",
			"player", m.PlayerJoinedServer.GetPlayer(), "server", m.PlayerJoinedServer.GetServer())
	}
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
