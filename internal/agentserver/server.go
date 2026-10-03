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

// Package agentserver is the gRPC endpoint the in-game agents connect to, and
// the only writer of the agent registry. A stream's identity comes from its
// bearer token (grpcauth), never from its messages.
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
	DefaultPort = 9443
	// shutdownGrace exists because every RPC is a long-lived stream, so an
	// unbounded GracefulStop would wait for the agents' own deadlines.
	shutdownGrace = 5 * time.Second

	// An agent opens exactly one stream per connection.
	MaxConcurrentStreams uint32 = 8

	// grpc-go's default is two minutes.
	ConnectionTimeout = 30 * time.Second

	// SendDeadline exists because stream.Send blocks on the client's
	// flow-control window and observes no context.
	SendDeadline = 30 * time.Second

	// The largest legitimate message is a roster at RosterMaxEntries, under
	// half a megabyte; grpc-go's default is 4 MiB.
	MaxMessageBytes = 1 << 20

	// MaxConnectionIdle is not a partition detector, and no server keepalive
	// is set on purpose: phase.Inputs.AgentSilent sees a quiet stream within
	// two report intervals and keeps StartDrain, while a keepalive-broken
	// stream would only get StreamDownGrace. The keepalive is on the agent.
	MaxConnectionIdle = 5 * time.Minute

	// A legitimate agent's measured peak is 2 (make-before-break renewal);
	// 8 is loose on purpose, since too low costs a working agent its session.
	// hack/agent-test.sh asserts the peak against this constant.
	MaxConnectionsPerPeer = 8

	// FleetConnectionsPerAgent is the per-peer bound once the fleet passes
	// ExpectedAgents * FleetConnectionsPerAgent connections. Using the same
	// number for both makes the bound converge instead of oscillate; 4 is an
	// agent holding both RPCs at once, each renewing, which no agent does.
	FleetConnectionsPerAgent = 4

	// The agents send no keepalive, so this throttles only a client that
	// pings in a loop.
	MinKeepaliveInterval = 30 * time.Second
)

// ProxyFleet narrows *proxyreg.Fleet to what ProxySession reads, so the handler
// cannot start writing into the fan-out the controllers own.
type ProxyFleet interface {
	Join(ctx context.Context, namespace, group, podUID string) (<-chan *agentpb.OperatorToProxy, func(), error)
	Move(namespace, playerUUID, targetServer string)
	// SetInterest reports nothing: an agent speaking for a session that no
	// longer exists is ordinary, not an error.
	SetInterest(podUID string, wanted bool)
	SendState(ctx context.Context, namespace string)
}

type StatusSource interface {
	Status(ctx context.Context, namespace string, audience netstate.Audience, target string) (*agentpb.StatusResult, error)
}

// ServerFanout joins by namespace, not by group: a backend mirrors the whole
// network.
type ServerFanout interface {
	Join(ctx context.Context, namespace, podUID string) (<-chan *agentpb.OperatorToServer, func(), error)
	SetInterest(podUID string, wanted bool)
}

// Options carries the three durations the operator dictates to its agents;
// both sides derive their thresholds from them.
type Options struct {
	Addr     string
	Provider *certs.Provider
	Auth     *grpcauth.Authenticator
	Agents   *agent.Registry
	Proxies  ProxyFleet
	Servers  ServerFanout
	State    netstate.Source
	Writer   ClusterWriter
	// Status nil refuses StatusRequest as unavailable.
	Status         StatusSource
	ReportInterval time.Duration
	RenewAfter     time.Duration
	// HardDeadline must be above RenewAfter, or a well-behaved agent would be
	// cut off mid-renewal.
	HardDeadline time.Duration
	// Fleet nil means no fleet bound; see PeerLimiter.Expect.
	Fleet func() (int, bool)
	// Clock does not drive HardDeadline, which runs on time.AfterFunc.
	Clock func() time.Time
}

// Server serves AgentService.
type Server struct {
	agentpb.UnimplementedAgentServiceServer

	opts     Options
	sessions *sessions
	// requestRate is a bucket separate from grpcauth's; see requestLimiter.
	requestRate *requestLimiter
	addr        atomic.Pointer[string]
}

func New(opts Options) *Server {
	if opts.Addr == "" {
		opts.Addr = fmt.Sprintf(":%d", DefaultPort)
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	// Refused here rather than as a panic inside a handler minutes later.
	if opts.Proxies == nil {
		panic("agentserver: no proxy fleet")
	}
	if opts.Servers == nil {
		panic("agentserver: no server fanout")
	}
	if opts.State.Reader == nil {
		panic("agentserver: no network state source")
	}
	if opts.Writer == nil {
		panic("agentserver: no cluster writer")
	}
	return &Server{opts: opts, sessions: newSessions(), requestRate: newRequestLimiter(opts.Clock)}
}

// Addr is empty until Start has bound the listener.
func (s *Server) Addr() string {
	if a := s.addr.Load(); a != nil {
		return *a
	}
	return ""
}

func (s *Server) NeedLeaderElection() bool { return true }

func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("agentserver")

	listener, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.opts.Addr, err)
	}
	bound := listener.Addr().String()
	s.addr.Store(&bound)
	s.opts.Agents.MarkServing()

	// Logged at powers of ten only, so a flooding peer cannot amplify itself
	// through the operator's log.
	limited := NewPeerLimiter(listener, MaxConnectionsPerPeer, func(ev ConnEvent) {
		if !ev.Refused || !isPowerOfTen(ev.Refusals) {
			return
		}
		logger.Info("refusing connections at a limit",
			"peer", ev.Peer, "bound", ev.Bound, "limit", ev.Limit,
			"open", ev.Open, "total", ev.Total, "refused", ev.Refusals)
	})
	limited.Expect(s.opts.Fleet)

	// GetCertificate rather than a fixed certificate: the provider rotates it.
	creds := credentials.NewTLS(&tls.Config{
		GetCertificate: s.opts.Provider.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	})
	grpcServer := grpc.NewServer(
		grpc.Creds(creds),
		grpc.StreamInterceptor(s.opts.Auth.StreamInterceptor()),
		grpc.MaxConcurrentStreams(MaxConcurrentStreams),
		grpc.MaxRecvMsgSize(MaxMessageBytes),
		grpc.ConnectionTimeout(ConnectionTimeout),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: MaxConnectionIdle,
		}),
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
				if err := sendBounded(SendDeadline, "an answer", func() error { return stream.Send(answer) }); err != nil {
					return err
				}
			}
		case msg, ok := <-outbox:
			if !ok {
				// A renewal cancels ctx and closes the outbox together; ctx.Err()
				// tells a supersede from an agent that fell behind.
				if ctx.Err() != nil {
					return status.Error(codes.Unavailable, "session ended, reconnect with a fresh token")
				}
				return status.Error(codes.ResourceExhausted, "server fell behind, reconnect for a fresh state")
			}
			if err := sendBounded(SendDeadline, "a message", func() error { return stream.Send(msg) }); err != nil {
				return err
			}
		}
	}
}

// sendBounded exists because stream.Send blocks on the client's flow-control
// window and observes no context; only the handler returning ends it. The
// buffer of one lets the abandoned goroutine finish once the stream closes.
func sendBounded(deadline time.Duration, what string, send func() error) error {
	sent := make(chan error, 1)
	go func() { sent <- send() }()
	select {
	case err := <-sent:
		return err
	case <-time.After(deadline):
		return status.Errorf(codes.DeadlineExceeded,
			"the agent did not read %s within %s", what, deadline)
	}
}

// sessionPrologue is shared by both sessions so the enter/Supersede/OpenStreams
// sequence cannot drift between roles; no client-visible test would catch it.
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
		if s.sessions.leave(id.PodUID, gen) {
			s.opts.Agents.Disconnect(id.PodUID)
		}
		logger.V(1).Info("session ended", "after", s.opts.Clock().Sub(openedAt))
	}

	if superseded {
		// The displaced stream was still live, so the agent process never
		// went away and its readiness carries over.
		s.opts.Agents.Supersede(id.PodUID, role)
	} else {
		s.opts.Agents.Connect(id.PodUID, role)
	}
	OpenStreams.WithLabelValues(string(role)).Inc()

	if err := sendBounded(s.opts.HardDeadline, "its opening messages", sendFixed); err != nil {
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

// handle ignores unknown branches so a newer agent works against an older
// operator.
func (s *Server) handle(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	msg *agentpb.ServerMessage,
) *agentpb.OperatorToServer {
	switch m := msg.GetMessage().(type) {
	case *agentpb.ServerMessage_Hello:
		// The agent repeats Ready on every connect, so an operator restart
		// cannot leave a server in Starting.
		if m.Hello.GetReady() {
			s.opts.Agents.MarkReady(id.PodUID)
		}
	case *agentpb.ServerMessage_EventInterest:
		s.opts.Servers.SetInterest(id.PodUID, m.EventInterest.GetWanted())
	case *agentpb.ServerMessage_Ready:
		s.opts.Agents.MarkReady(id.PodUID)
	case *agentpb.ServerMessage_PlayerCount:
		if err := s.opts.Agents.ReportPlayers(id.PodUID,
			m.PlayerCount.GetPlayers(), m.PlayerCount.GetSlots()); err != nil {
			// Discard, keep the stream: dropping it would be a reconnect
			// loop the agent could trigger at will.
			RejectedReports.WithLabelValues(string(agent.RoleServer)).Inc()
			logger.V(1).Info("discarded a player count", "reason", err.Error())
		} else if err := s.opts.Agents.ReportTicks(id.PodUID,
			m.PlayerCount.GetTps(), m.PlayerCount.GetMspt()); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleServer)).Inc()
			logger.V(1).Info("discarded a tick report", "reason", err.Error())
		} else if err := s.opts.Agents.ReportPlayableSlots(id.PodUID,
			m.PlayerCount.GetPlayableSlots()); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleServer)).Inc()
			logger.V(1).Info("discarded a playable figure", "reason", err.Error())
		} else if err := s.opts.Agents.ReportHeap(id.PodUID,
			m.PlayerCount.GetHeapUsedBytes(), m.PlayerCount.GetHeapMaxBytes()); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleServer)).Inc()
			logger.V(1).Info("discarded a heap report", "reason", err.Error())
		}
	case *agentpb.ServerMessage_CloudRequest:
		return &agentpb.OperatorToServer{
			Message: &agentpb.OperatorToServer_CloudResponse{
				CloudResponse: s.answerCloudRequest(ctx, logger, id, m.CloudRequest),
			},
		}
	}
	return nil
}

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

	// Joined after the fixed messages: the agent needs the deadline before it
	// processes a server list.
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
				if err := sendBounded(SendDeadline, "an answer", func() error { return stream.Send(answer) }); err != nil {
					return err
				}
			}
		case msg, ok := <-outbox:
			if !ok {
				// A renewal cancels ctx and closes the outbox together, and Go
				// picks between ready cases arbitrarily; ctx.Err() tells a
				// supersede or shutdown from a proxy that fell behind.
				if ctx.Err() != nil {
					return status.Error(codes.Unavailable, "session ended, reconnect with a fresh token")
				}
				return status.Error(codes.ResourceExhausted, "proxy fell behind, reconnect for a fresh sync")
			}
			if err := sendBounded(SendDeadline, "a message", func() error { return stream.Send(msg) }); err != nil {
				return err
			}
		}
	}
}

// handleProxy answers on the requesting stream, not through the fan-out: a
// renewal has already failed the request on the agent's side, and the
// successor never minted that id.
func (s *Server) handleProxy(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	msg *agentpb.ProxyMessage,
) *agentpb.OperatorToProxy {
	switch m := msg.GetMessage().(type) {
	case *agentpb.ProxyMessage_Hello:
		// A proxy's readiness is not carried here: the agent serves its own
		// readiness probe. The read timeout is, because it lives in a file
		// the operator never reads; zero comes from older agents and is ignored.
		logger.V(1).Info("proxy connected", "version", m.Hello.GetVersion(),
			"readTimeoutMillis", m.Hello.GetReadTimeoutMillis())
		s.opts.Agents.ReportReadTimeout(id.PodUID, id.Namespace,
			time.Duration(m.Hello.GetReadTimeoutMillis())*time.Millisecond)
	case *agentpb.ProxyMessage_PlayerCount:
		if err := s.opts.Agents.ReportPlayers(id.PodUID,
			m.PlayerCount.GetPlayers(), m.PlayerCount.GetSlots()); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleProxy)).Inc()
			logger.V(1).Info("discarded a player count", "reason", err.Error())
		} else if err := s.opts.Agents.ReportHeap(id.PodUID,
			m.PlayerCount.GetHeapUsedBytes(), m.PlayerCount.GetHeapMaxBytes()); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleProxy)).Inc()
			logger.V(1).Info("discarded a heap report", "reason", err.Error())
		}
	case *agentpb.ProxyMessage_Heartbeat:
		// The stream is its own liveness signal; a second path would be a
		// second truth about the same fact.
	case *agentpb.ProxyMessage_BackendPlayers:
		if reason, ok := backendsRefusal(m.BackendPlayers.GetPlayers()); !ok {
			RejectedReports.WithLabelValues(string(agent.RoleProxy)).Inc()
			logger.V(1).Info("discarded a backend report", "reason", reason)
			break
		}
		if err := s.opts.Agents.ReportBackends(id.PodUID, id.Namespace,
			m.BackendPlayers.GetPlayers()); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleProxy)).Inc()
			logger.V(1).Info("discarded a backend report", "reason", err.Error())
		}
	case *agentpb.ProxyMessage_PlayerRoster:
		if reason, ok := rosterRefusal(m.PlayerRoster); !ok {
			RejectedReports.WithLabelValues(string(agent.RoleProxy)).Inc()
			logger.V(1).Info("discarded a roster report", "reason", reason)
			break
		}
		entries := make([]agent.RosterEntry, 0, len(m.PlayerRoster.GetPlayers()))
		for _, p := range m.PlayerRoster.GetPlayers() {
			// The reader keys on UUID, so a second empty one would silently
			// replace the first.
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
			// No player name: this is the one message that identifies a person.
			logger.V(1).Info("discarded a roster report", "reason", err.Error())
		}
	case *agentpb.ProxyMessage_EventInterest:
		s.opts.Proxies.SetInterest(id.PodUID, m.EventInterest.GetWanted())
	case *agentpb.ProxyMessage_PlayerJoinedServer:
		// Accepted and ignored: nothing consumes it yet.
		logger.V(1).Info("player joined a server",
			"player", m.PlayerJoinedServer.GetPlayer(), "server", m.PlayerJoinedServer.GetServer())
	case *agentpb.ProxyMessage_CloudRequest:
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_CloudResponse{
				CloudResponse: s.answerCloudRequest(ctx, logger, id, m.CloudRequest),
			},
		}
	}
	return nil
}

// recvPump exists because Recv blocks and the handler selects on three other
// channels.
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

// seconds never returns zero: the protocol counts whole seconds, and zero
// would tell the agent to report in a tight loop.
func seconds(d time.Duration) int32 {
	if s := d.Truncate(time.Second); s > 0 {
		return int32(s / time.Second)
	}
	return 1
}
