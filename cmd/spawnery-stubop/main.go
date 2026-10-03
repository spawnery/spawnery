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

// Command spawnery-stubop is an operator-shaped counterpart for the agents.
// hack/agent-test.sh runs it on the host, points a containerised agent at it,
// and reads the JSON event trace on its stdout; it is test-only and never
// enters an image.
//
// It is passive by default: unlike the real operator it never cancels a
// stream, so every close in the trace is the agent's own doing. --supersede
// cancels the displaced stream the way internal/agentserver does.
// --mute-after accepts streams and never answers them. --proxy sends a
// FullSync, which opens a proxy's readiness gate; --full-sync-after delays it
// and --set-ready-after later closes the gate again. --require-token refuses a
// stream without this stub's token, since an agent that stopped attaching
// credentials would otherwise pass every other check.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/certs"
)

const (
	// certificateLifetime only has to outlive one test run. Nothing renews it.
	certificateLifetime = 24 * time.Hour
	// The container runs as uid 10001 and reads these through a bind mount.
	worldReadable = 0o644
	worldEnter    = 0o755
	// supersedeGrace is a backstop against a displaced handler that will not
	// return.
	supersedeGrace = 2 * time.Second
	// See enter for why this is not zero.
	retirementHeadStart = 250 * time.Millisecond

	// The one backend --proxy syncs, at an unroutable address on purpose.
	// hack/agent-test.sh checks syncedGroup against its SYNCED_GROUP.
	syncedName    = "lobby-0"
	syncedAddress = "10.255.255.1:25565"
	syncedGroup   = "lobby"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// names collects the repeatable --san flag.
type names []string

func (n *names) String() string { return strings.Join(*n, ",") }

func (n *names) Set(value string) error {
	if value == "" {
		return fmt.Errorf("an empty SAN would match nothing")
	}
	*n = append(*n, value)
	return nil
}

// run returns the process exit code: 0 on a clean SIGTERM, 1 on anything else.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spawnery-stubop", flag.ContinueOnError)
	fs.SetOutput(stderr)

	dir := fs.String("dir", "", "directory to write ca.crt and token into (required)")
	var sans names
	fs.Var(&sans, "san", "DNS name to put in the serving certificate; repeatable (default stubop)")
	listen := fs.String("listen", ":9443", "address to serve AgentService on")
	reportInterval := fs.Int("report-interval", 1, "seconds the agent is told to wait between player count reports")
	renewAfter := fs.Int("renew-after", 5, "seconds after which the agent is told to renew its session")
	hardDeadline := fs.Int("hard-deadline", 20, "seconds the agent is told its session may live at most")
	supersede := fs.Bool("supersede", false,
		"cancel the displaced stream when a new one opens, the way the real operator does")
	muteAfter := fs.Int("mute-after", -1,
		"accept streams from this index on and never answer them, the way an operator "+
			"blocked between the cancel and its first Send does; negative disables")
	proxy := fs.Bool("proxy", false,
		"send a FullSync on every proxy stream, the way an operator with a registered backend does")
	requireToken := fs.Bool("require-token", false,
		"refuse a stream that does not present the token this stub wrote, the way "+
			"internal/grpcauth's interceptor does")
	fullSyncAfter := fs.Int("full-sync-after", 0,
		"seconds to hold the FullSync back after the opening messages, so a test can "+
			"probe the readiness gate while the proxy has no server list yet")
	deafenAfter := fs.Duration("deafen-after", 0,
		"after this long, stop reading and writing on every connection without closing "+
			"any of them; 0 disables. The one fault with no clock on either side -- see "+
			"deafness in deafen.go for what it does and does not reproduce, and "+
			"OperatorChannel's keepalive for what is meant to end the wait")
	setReadyAfter := fs.Duration("set-ready-after", 0,
		"after a proxy's FullSync, wait this long and then tell it to stop being ready; "+
			"0 disables. Used by hack/agent-test.sh phase 4 to prove the gate closes on the "+
			"operator's word rather than only at shutdown")
	rotateCA := fs.Bool("rotate-ca", false,
		"write a two-PEM ca.crt built with internal/certs and serve a certificate signed by "+
			"the second of the two, the way an agent sees mid-rotation. Used by "+
			"hack/agent-test.sh phase 6 to prove OperatorChannel.trustManager trusts every "+
			"certificate the bundle holds and not only the first")

	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *dir == "" {
		_, _ = fmt.Fprintln(stderr, "--dir is required: the agent reads its CA and token from a directory")
		return 1
	}
	if len(sans) == 0 {
		sans = names{"stubop"}
	}

	var material *material
	var err error
	if *rotateCA {
		material, err = materialiseRotated(*dir, sans)
	} else {
		material, err = materialise(*dir, sans)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "could not write the agent's credentials: %v\n", err)
		return 1
	}

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "listen on %s: %v\n", *listen, err)
		return 1
	}

	// TLS 1.3 as a floor, matching internal/agentserver.
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{material.Certificate},
		MinVersion:   tls.VersionTLS13,
	})
	events := newRecorder(stdout)

	// A zero limit counts connections without refusing any. Connections, not
	// streams: SessionLoop opens a fresh channel per attempt, so an agent
	// leaking one per reconnect shows only here. agent-test.sh asserts the
	// peak.
	counted := agentserver.NewPeerLimiter(listener, 0, func(ev agentserver.ConnEvent) {
		events.record("connection", map[string]any{
			"peer": ev.Peer,
			"open": ev.Open,
			"peak": ev.Peak,
		})
	})

	// Outside the counter: a deafened connection still counts.
	var serving net.Listener = counted
	if *deafenAfter > 0 {
		deaf := &deafness{}
		serving = deaf.listener(counted)
		time.AfterFunc(*deafenAfter, func() {
			deaf.on.Store(true)
			events.record("deafened", map[string]any{"after": deafenAfter.String()})
		})
	}

	served := &stub{
		events:         events,
		reportInterval: int32(*reportInterval),
		renewAfter:     int32(*renewAfter),
		hardDeadline:   int32(*hardDeadline),
		supersede:      *supersede,
		muteAfter:      *muteAfter,
		proxy:          *proxy,
		fullSyncAfter:  time.Duration(*fullSyncAfter) * time.Second,
		setReadyAfter:  *setReadyAfter,
		token:          material.Token,
	}

	// The operator's keepalive enforcement, the one server option mirrored
	// here: it can GOAWAY an agent that pings too eagerly.
	options := []grpc.ServerOption{
		grpc.Creds(creds),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             agentserver.MinKeepaliveInterval,
			PermitWithoutStream: false,
		}),
	}
	if *requireToken {
		options = append(options, grpc.StreamInterceptor(served.requireBearer))
	}
	server := grpc.NewServer(options...)
	agentpb.RegisterAgentServiceServer(server, served)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		// GracefulStop would wait for streams the agent never closes.
		server.Stop()
	}()

	_, _ = fmt.Fprintf(stderr, "serving AgentService on %s for %v\n", listener.Addr(), []string(sans))
	if err := server.Serve(serving); err != nil {
		_, _ = fmt.Fprintf(stderr, "serve: %v\n", err)
		return 1
	}
	return 0
}

// material is what the stub hands the agent: the serving certificate it
// presents, and the token it will accept in any spelling of the header.
type material struct {
	Certificate tls.Certificate
	Token       string
}

func materialise(dir string, sans []string) (*material, error) {
	if err := os.MkdirAll(dir, worldEnter); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll applies the umask and leaves an existing directory's mode alone.
	if err := os.Chmod(dir, worldEnter); err != nil {
		return nil, fmt.Errorf("chmod %s: %w", dir, err)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the CA key: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "spawnery-stubop"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(certificateLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign the CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse the CA certificate: %w", err)
	}

	servingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the serving key: %w", err)
	}
	servingTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: sans[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(certificateLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     sans,
	}
	servingDER, err := x509.CreateCertificate(rand.Reader, servingTemplate, ca, &servingKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign the serving certificate: %w", err)
	}
	serving, err := x509.ParseCertificate(servingDER)
	if err != nil {
		return nil, fmt.Errorf("parse the serving certificate: %w", err)
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("draw a token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	if err := write(filepath.Join(dir, "ca.crt"), caPEM); err != nil {
		return nil, err
	}
	// No trailing newline: the agent sends the file's bytes as the token.
	if err := write(filepath.Join(dir, "token"), []byte(token)); err != nil {
		return nil, err
	}

	return &material{
		Certificate: tls.Certificate{
			Certificate: [][]byte{servingDER},
			PrivateKey:  servingKey,
			Leaf:        serving,
		},
		Token: token,
	}, nil
}

// materialiseRotated is materialise's counterpart for --rotate-ca: it writes a
// two-PEM ca.crt and serves a certificate signed by the second of the two. It
// goes through internal/certs so the agent sees what production publishes.
func materialiseRotated(dir string, sans []string) (*material, error) {
	if err := os.MkdirAll(dir, worldEnter); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	// See materialise: MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(dir, worldEnter); err != nil {
		return nil, fmt.Errorf("chmod %s: %w", dir, err)
	}

	now := time.Now()
	firstCertPEM, firstKeyPEM, err := certs.IssueCA(now)
	if err != nil {
		return nil, fmt.Errorf("issue the first CA: %w", err)
	}
	secondCertPEM, secondKeyPEM, err := certs.IssueCA(now)
	if err != nil {
		return nil, fmt.Errorf("issue the second CA: %w", err)
	}

	// The bundle is taken mid-rotation, before SwitchToNext, so the agent
	// keeps this unchanged bundle across the switch to the second CA.
	b := &certs.Bundle{CACertPEM: firstCertPEM, CAKeyPEM: firstKeyPEM}
	b = b.WithNextCA(secondCertPEM, secondKeyPEM)
	bundlePEM := b.PublishedCA()

	switched, err := b.SwitchToNext(now, sans)
	if err != nil {
		return nil, fmt.Errorf("switch to the second CA: %w", err)
	}
	servingCert, err := switched.TLSCertificate()
	if err != nil {
		return nil, fmt.Errorf("load the certificate signed by the second CA: %w", err)
	}

	if err := write(filepath.Join(dir, "ca.crt"), bundlePEM); err != nil {
		return nil, err
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("draw a token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	// No trailing newline: see materialise.
	if err := write(filepath.Join(dir, "token"), []byte(token)); err != nil {
		return nil, err
	}

	return &material{Certificate: servingCert, Token: token}, nil
}

// write puts the file where the agent can read it whatever the umask says.
func write(path string, contents []byte) error {
	if err := os.WriteFile(path, contents, worldReadable); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, worldReadable); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// recorder writes the observed events as one JSON object per line, so the test
// script can tail the file and assert on the order of what it finds there.
type recorder struct {
	mu  sync.Mutex
	out io.Writer
	seq int
}

func newRecorder(w io.Writer) *recorder {
	return &recorder{out: w}
}

// record writes {"kind":..., "seq":..., ...fields} and flushes. seq is a total
// order over the events of all streams.
func (r *recorder) record(kind string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	event := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		event[key] = value
	}
	event["kind"] = kind
	event["seq"] = r.seq
	r.seq++

	line, err := json.Marshal(event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not record a %s event: %v\n", kind, err)
		return
	}
	if _, err := r.out.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "could not write a %s event: %v\n", kind, err)
		return
	}
	if file, ok := r.out.(*os.File); ok {
		_ = file.Sync()
	}
}

// stub serves AgentService and records.
type stub struct {
	agentpb.UnimplementedAgentServiceServer

	events  *recorder
	streams atomic.Int64

	reportInterval int32
	renewAfter     int32
	hardDeadline   int32

	// current is the stream a new one would displace; the real operator keys
	// it by pod UID, but the test runs one agent per stub.
	supersede bool
	// muteAfter is the first stream index that is never answered; negative
	// disables.
	muteAfter int
	mu        sync.Mutex
	current   *live

	proxy         bool
	fullSyncAfter time.Duration
	setReadyAfter time.Duration

	token string
}

// requireBearer refuses a stream that does not present this stub's token, and
// records the refusal: from outside, a refused agent looks like one that never
// connected.
func (s *stub) requireBearer(
	srv any,
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	authorization := authorizationOf(ss.Context())
	if subtle.ConstantTimeCompare([]byte(authorization), []byte("Bearer "+s.token)) != 1 {
		s.events.record("stream_rejected", map[string]any{
			"authorization": authorization,
			"rpc":           info.FullMethod,
		})
		return status.Error(codes.Unauthenticated, "this stream presented no usable bearer token")
	}
	return handler(srv, ss)
}

// muted reports whether this stream is one the stub accepts and then says
// nothing on. The real operator does that when it blocks between cancelling
// the displaced stream and its first Send, before any deadline is armed, so
// only the agent's own bound ends the wait.
func (s *stub) muted(index int64) bool {
	return s.muteAfter >= 0 && index >= int64(s.muteAfter)
}

// live is one stream under --supersede: what ends it, and how to tell that it
// has ended.
type live struct {
	cancel context.CancelFunc
	// done is closed when the handler has returned.
	done chan struct{}
}

// enter registers a new stream and retires the one it replaces, where
// internal/agentserver's sessions.enter does: at the handler entry of the
// replacement, before anything is sent on it.
//
// It then holds the replacement back until the displaced stream is gone and a
// little beyond. The two events travel different connections and nothing
// orders them; on loopback the agent would almost always see the new stream
// first, so the stub forces the other order, which a real cluster can produce.
func (s *stub) enter(current *live) {
	s.mu.Lock()
	displaced := s.current
	s.current = current
	s.mu.Unlock()

	if displaced == nil {
		return
	}
	displaced.cancel()
	select {
	case <-displaced.done:
	case <-time.After(supersedeGrace):
	}
	// grpc-go writes the trailers after the handler returns.
	time.Sleep(retirementHeadStart)
}

type recorderLike interface {
	record(kind string, fields map[string]any)
}

// answerCloudRequest answers every connect with ordered=true and the target it
// was given; what is under test is the agent's round trip.
func answerCloudRequest(of func(map[string]any) map[string]any, events recorderLike, req *agentpb.CloudRequest) *agentpb.CloudResponse {
	c := req.GetConnect()
	events.record("cloud_request", of(map[string]any{
		"id":     req.GetId(),
		"player": c.GetPlayerUuid(),
		"server": c.GetServer(),
		"group":  c.GetGroup(),
	}))
	target := c.GetServer()
	if target == "" {
		target = c.GetGroup()
	}
	return &agentpb.CloudResponse{
		Id: req.GetId(),
		Result: &agentpb.CloudResponse_Connect{
			Connect: &agentpb.ConnectResult{Ordered: true, Target: target},
		},
	}
}

// networkState is the mirror both agent kinds are sent on connect.
func networkState() *agentpb.NetworkState {
	return &agentpb.NetworkState{
		Groups: []*agentpb.GroupState{{
			Name: syncedGroup, Kind: agentpb.GroupState_EPHEMERAL,
			Replicas: 1, ReadyReplicas: 1, FreeSlots: 100,
		}},
		Servers: []*agentpb.ServerState{{
			Name: syncedName, Group: syncedGroup, Phase: "Ready",
			Players: 0, Slots: 100, Registered: true,
		}},
	}
}

func (s *stub) ServerSession(stream agentpb.AgentService_ServerSessionServer) error {
	return serveSession(s, stream, []*agentpb.OperatorToServer{
		{Message: &agentpb.OperatorToServer_ReportInterval{
			ReportInterval: &agentpb.ReportInterval{Seconds: s.reportInterval},
		}},
		{Message: &agentpb.OperatorToServer_SessionDeadline{
			SessionDeadline: &agentpb.SessionDeadline{
				RenewAfterSeconds:   s.renewAfter,
				HardDeadlineSeconds: s.hardDeadline,
			},
		}},
		{Message: &agentpb.OperatorToServer_NetworkState{NetworkState: networkState()}},
	}, nil, s.observeServer)
}

func (s *stub) ProxySession(stream agentpb.AgentService_ProxySessionServer) error {
	var later []delayed[agentpb.OperatorToProxy]
	if s.proxy {
		later = append(later, delayed[agentpb.OperatorToProxy]{
			message: &agentpb.OperatorToProxy{
				Message: &agentpb.OperatorToProxy_FullSync{
					FullSync: &agentpb.FullSync{
						Servers: []*agentpb.RegisteredServer{
							{Name: syncedName, Address: syncedAddress, Group: syncedGroup},
						},
					},
				},
			},
			after:   s.fullSyncAfter,
			kind:    "full_sync_sent",
			failure: "full_sync_failed",
		})
		// After the FullSync: against an unsynced proxy, SetReady{false} would
		// close a gate that never opened.
		if s.setReadyAfter > 0 {
			later = append(later, delayed[agentpb.OperatorToProxy]{
				message: &agentpb.OperatorToProxy{
					Message: &agentpb.OperatorToProxy_SetReady{
						SetReady: &agentpb.SetReady{Ready: false},
					},
				},
				after:   s.setReadyAfter,
				kind:    "set_ready_sent",
				failure: "set_ready_failed",
			})
		}
	}
	return serveSession(s, stream, []*agentpb.OperatorToProxy{
		{Message: &agentpb.OperatorToProxy_ReportInterval{
			ReportInterval: &agentpb.ReportInterval{Seconds: s.reportInterval},
		}},
		{Message: &agentpb.OperatorToProxy_SessionDeadline{
			SessionDeadline: &agentpb.SessionDeadline{
				RenewAfterSeconds:   s.renewAfter,
				HardDeadlineSeconds: s.hardDeadline,
			},
		}},
		// Ahead of the FullSync, so it could disturb the gate's opening if
		// anything does.
		{Message: &agentpb.OperatorToProxy_NetworkState{NetworkState: networkState()}},
	}, later, s.observeProxy)
}

// delayed is a message the stub sends on a schedule of its own. `after` counts
// from the previous send in the sequence, not from the stream opening. kind is
// recorded after Send returns, so a test can order its observations against
// it.
type delayed[Out any] struct {
	message *Out
	after   time.Duration
	kind    string
	failure string
}

// serveSession is the body both rpcs share.
func serveSession[In, Out any](
	s *stub,
	stream grpc.BidiStreamingServer[In, Out],
	opening []*Out,
	later []delayed[Out],
	// observe records one received message; a non-nil return is sent on
	// this stream.
	observe func(func(map[string]any) map[string]any, *In) *Out,
) error {
	index := s.streams.Add(1) - 1
	// Verbatim: the script compares it character for character.
	authorization := authorizationOf(stream.Context())

	of := func(fields map[string]any) map[string]any {
		fields["stream"] = index
		fields["authorization"] = authorization
		return fields
	}
	muted := s.muted(index)
	s.events.record("stream_opened", of(map[string]any{"muted": muted}))

	ctx := stream.Context()
	if s.supersede {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		done := make(chan struct{})
		defer close(done)
		s.enter(&live{cancel: cancel, done: done})
	}

	// A muted stream skips every Send, scheduled ones included, and nothing
	// else.
	if !muted {
		for _, message := range opening {
			if err := stream.Send(message); err != nil {
				s.events.record("stream_closed", of(map[string]any{"error": err.Error()}))
				return nil
			}
		}

		if len(later) > 0 {
			// The wait must overlap the receive loop below, so the Hello is
			// recorded before the FullSync goes out. One goroutine for the whole
			// sequence keeps Send single-threaded, and it is stopped before the
			// handler returns and grpc-go finishes the stream.
			sendCtx, stopSending := context.WithCancel(ctx)
			sent := make(chan struct{})
			go func() {
				defer close(sent)
				for _, next := range later {
					select {
					case <-time.After(next.after):
					case <-sendCtx.Done():
						return
					}
					if err := stream.Send(next.message); err != nil {
						s.events.record(next.failure, of(map[string]any{"error": err.Error()}))
						return
					}
					s.events.record(next.kind, of(map[string]any{}))
				}
			}()
			defer func() {
				stopSending()
				<-sent
			}()
		}
	}

	if !s.supersede {
		// Only the agent ends a passive stream, so a blocking Recv suffices.
		for {
			message, err := stream.Recv()
			if err != nil {
				s.events.record("stream_closed", of(map[string]any{"error": closeReason(err)}))
				return nil
			}
			if answer := observe(of, message); answer != nil {
				if err := stream.Send(answer); err != nil {
					s.events.record("stream_closed", of(map[string]any{"error": err.Error()}))
					return nil
				}
			}
		}
	}

	received := make(chan *In)
	errs := make(chan error, 1)
	go func() {
		defer close(received)
		for {
			message, err := stream.Recv()
			if err != nil {
				errs <- err
				return
			}
			select {
			case received <- message:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			s.events.record("stream_closed", of(map[string]any{"error": "superseded"}))
			return status.Error(codes.Unavailable, "session ended, reconnect with a fresh token")
		case err := <-errs:
			s.events.record("stream_closed", of(map[string]any{"error": closeReason(err)}))
			return nil
		case message := <-received:
			if answer := observe(of, message); answer != nil {
				if err := stream.Send(answer); err != nil {
					s.events.record("stream_closed", of(map[string]any{"error": err.Error()}))
					return nil
				}
			}
		}
	}
}

func (s *stub) observeServer(of func(map[string]any) map[string]any, message *agentpb.ServerMessage) *agentpb.OperatorToServer {
	switch body := message.Message.(type) {
	case *agentpb.ServerMessage_Hello:
		s.hello(of, body.Hello)
	case *agentpb.ServerMessage_Ready:
		s.events.record("ready", of(map[string]any{}))
	case *agentpb.ServerMessage_PlayerCount:
		s.playerCount(of, body.PlayerCount)
	case *agentpb.ServerMessage_EventInterest:
		s.events.record("event_interest", of(map[string]any{"wanted": body.EventInterest.GetWanted()}))
	case *agentpb.ServerMessage_CloudRequest:
		return &agentpb.OperatorToServer{
			Message: &agentpb.OperatorToServer_CloudResponse{
				CloudResponse: answerCloudRequest(of, s.events, body.CloudRequest),
			},
		}
	default:
		s.events.record("unknown", of(map[string]any{}))
	}
	return nil
}

// observeProxy has no Ready case: a proxy's readiness is only visible on the
// probe port.
func (s *stub) observeProxy(of func(map[string]any) map[string]any, message *agentpb.ProxyMessage) *agentpb.OperatorToProxy {
	switch body := message.Message.(type) {
	case *agentpb.ProxyMessage_Hello:
		s.hello(of, body.Hello)
	case *agentpb.ProxyMessage_PlayerCount:
		s.playerCount(of, body.PlayerCount)
	case *agentpb.ProxyMessage_PlayerJoinedServer:
		s.events.record("player_joined_server", of(map[string]any{
			"player": body.PlayerJoinedServer.GetPlayer(),
			"server": body.PlayerJoinedServer.GetServer(),
		}))
	case *agentpb.ProxyMessage_BackendPlayers:
		// Sorted pairs rather than an object, for an assertable order.
		names := make([]string, 0, len(body.BackendPlayers.GetPlayers()))
		for name := range body.BackendPlayers.GetPlayers() {
			names = append(names, name)
		}
		sort.Strings(names)
		pairs := make([]map[string]any, 0, len(names))
		for _, name := range names {
			pairs = append(pairs, map[string]any{
				"server":  name,
				"players": body.BackendPlayers.GetPlayers()[name],
			})
		}
		s.events.record("backend_players", of(map[string]any{"backends": pairs}))
	case *agentpb.ProxyMessage_PlayerRoster:
		// Sorted by UUID, for an assertable order.
		entries := body.PlayerRoster.GetPlayers()
		players := make([]map[string]any, 0, len(entries))
		for _, p := range entries {
			players = append(players, map[string]any{
				"uuid":   p.GetUuid(),
				"name":   p.GetName(),
				"server": p.GetServer(),
			})
		}
		sort.Slice(players, func(i, j int) bool {
			return players[i]["uuid"].(string) < players[j]["uuid"].(string)
		})
		s.events.record("player_roster", of(map[string]any{"players": players}))
	case *agentpb.ProxyMessage_Heartbeat:
		s.events.record("heartbeat", of(map[string]any{}))
	case *agentpb.ProxyMessage_EventInterest:
		s.events.record("event_interest", of(map[string]any{"wanted": body.EventInterest.GetWanted()}))
	case *agentpb.ProxyMessage_CloudRequest:
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_CloudResponse{
				CloudResponse: answerCloudRequest(of, s.events, body.CloudRequest),
			},
		}
	default:
		s.events.record("unknown", of(map[string]any{}))
	}
	return nil
}

// hello and playerCount are shared by both observers; the field names are the
// ones hack/agent-test.sh's jq reads.
func (s *stub) hello(of func(map[string]any) map[string]any, hello *agentpb.Hello) {
	s.events.record("hello", of(map[string]any{
		"version": hello.GetVersion(),
		"ready":   hello.GetReady(),
		// Only a real Velocity with a real velocity.toml can show this.
		"readTimeoutMillis": hello.GetReadTimeoutMillis(),
	}))
}

func (s *stub) playerCount(of func(map[string]any) map[string]any, count *agentpb.PlayerCount) {
	s.events.record("player_count", of(map[string]any{
		"players": count.GetPlayers(),
		"slots":   count.GetSlots(),
	}))
}

// closeReason is empty for the agent's own clean half-close, which is what a
// renewal looks like from here and is not an error.
func closeReason(err error) string {
	if errors.Is(err, io.EOF) {
		return ""
	}
	return err.Error()
}

func authorizationOf(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
