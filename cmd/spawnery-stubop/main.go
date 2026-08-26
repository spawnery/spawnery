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

// Command spawnery-stubop is an operator-shaped counterpart for the Paper
// agent, and nothing else. hack/agent-test.sh runs it on the host, points a
// containerised agent at it, and reads its stdout; it is test-only and never
// enters the image.
//
// It is deliberately *passive* by default. The real operator supersedes: on a
// second stream for the same pod, internal/agentserver cancels the displaced
// stream's context itself. A stub that imitated that would close the outgoing
// stream on its own, and the assertion "the first stream closed only after the
// second greeted" would then be measuring the stub rather than the agent. So by
// default this one accepts a stream, records what arrives on it, and never
// cancels one: every close in the recorded trace is the agent's own doing.
//
// --supersede turns the other half on, because passivity also hides everything
// the operator's own retirement order can provoke in the agent. It cancels the
// displaced stream exactly where sessions.enter() does — at the handler entry
// of the replacement, before the first Send — answers the cancelled stream with
// the same Unavailable, and waits for it to be gone before it answers the
// replacement, so the order the agent sees is the operator's rather than
// whichever write won a scheduling race (see enter). What is asserted under it
// is not the overlap but the rate: an agent that mistakes that cancellation for
// a breakage it owes a reconnect opens a stream a second, forever.
//
// --mute-after is the third thing an operator can do, and the only one that is
// not an event: accept a stream and then say nothing on it. It is a silence
// rather than a failure, so no error reaches the agent and no deadline of the
// operator's ever fires — see muted for where the real operator does this. What
// is asserted under it is a lower bound on the rate: an agent with no bound of
// its own stops opening streams entirely, which is the one failure an upper
// bound reads as success.
//
// --proxy adds the one thing the proxy direction has that the server direction
// does not: a FullSync after the two opening messages, which is what opens the
// proxy's readiness gate. --full-sync-after holds it back by that many seconds,
// so hack/agent-test.sh can probe the gate's port while the agent is connected
// and has deliberately not been given a server list yet. That pair of probes —
// closed before, open after — is the only place the gate's precondition is
// observable at all; a unit test can see that bind() was called and nothing
// more.
//
// --set-ready-after is the other end of that gate, and the only end the
// operator drives on its own: some time after the FullSync, the stub tells the
// proxy to stop being ready. The opening is a decision the agent reaches from a
// server list; the closing is a decision the operator makes and the agent obeys,
// and nothing outside a unit test had ever put that message in front of the real
// jar. What hack/agent-test.sh asserts under it is again the port -- closed
// while the agent is connected, synced, and still answering on 25565.
//
// --proxy gates the FullSync and nothing else. Both rpcs are served
// unconditionally, because refusing one would make an agent that opened the
// wrong rpc look like a connection failure rather than naming itself.
//
// It records, verbatim, what the agent put in the authorization header, and by
// default accepts any of it: what a stream carried is a fact for the test to
// assert on, not a reason for this process to refuse one.
//
// --require-token turns that into a refusal, because for one obligation an
// assertion on the recorded header is not enough. The Velocity agent attaches
// its credentials in ProxyRole.open(), and deleting that call compiles, leaves
// every JUnit test green, and streams unauthenticated — so the only cover it
// has is at this level. A shell comparison in hack/agent-test.sh does catch it,
// and is also the single line a later editor is most likely to prune as
// redundant. Under the flag the uncredentialed stream never opens at all, which
// is what internal/grpcauth does in a cluster and what leaves the agent
// reconnecting into a refusal rather than running unauthenticated.
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
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/certs"
)

const (
	// certificateLifetime only has to outlive one test run. Nothing renews it.
	certificateLifetime = 24 * time.Hour
	// worldReadable, because the container runs as uid 10001 and reads these
	// two files through a bind mount of the directory they are written to.
	worldReadable = 0o644
	worldEnter    = 0o755
	// supersedeGrace bounds the wait in enter below. It is a backstop against a
	// displaced handler that will not return, not a timing assumption: the
	// handler it waits for is already selecting on the context it just
	// cancelled.
	supersedeGrace = 2 * time.Second
	// retirementHeadStart is how long enter holds the replacement back after
	// the displaced stream is gone. See enter for why it is not zero.
	retirementHeadStart = 250 * time.Millisecond

	// The one backend --proxy syncs. The address is deliberately unroutable:
	// nothing in this harness is supposed to connect to it, and a reachable one
	// would invite a later test to depend on a backend that is not there.
	syncedName    = "lobby-0"
	syncedAddress = "10.255.255.1:25565"
	syncedGroup   = "lobby"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// names collects a repeatable flag. The serving certificate needs one SAN per
// name the agent might dial it by, and under a container runtime that is not
// the host's own hostname.
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

	// TLS 1.3 as a floor, matching internal/agentserver: a stub that accepted
	// less could pass while the agent negotiated something the real operator
	// would refuse.
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{material.Certificate},
		MinVersion:   tls.VersionTLS13,
	})
	events := newRecorder(stdout)

	// Counted, never refused -- the zero limit, which is the mode this stub
	// wants and the operator never uses.
	//
	// streams_opened counts what the operator was asked to serve, and a stream
	// is not a connection: SessionLoop builds a fresh ManagedChannel per
	// attempt, so a make-before-break renewal holds two connections at once
	// and an agent leaking one per reconnect moved no number this trace
	// carried. The operator publishes the same count as
	// spawnery_agent_open_connections, but that needs an operator and a
	// cluster; here is where it can be read against a real agent, over a real
	// socket, on one host -- which is what agent-test.sh asserts the peak from
	// and what MaxConnectionsPerPeer is derived from. The Kotlin unit tests
	// cannot: their in-process transport opens no socket at all.
	counted := agentserver.NewPeerLimiter(listener, 0, func(ev agentserver.ConnEvent) {
		events.record("connection", map[string]any{
			"peer": ev.Peer,
			"open": ev.Open,
			"peak": ev.Peak,
		})
	})

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

	options := []grpc.ServerOption{grpc.Creds(creds)}
	if *requireToken {
		options = append(options, grpc.StreamInterceptor(served.requireBearer))
	}
	server := grpc.NewServer(options...)
	agentpb.RegisterAgentServiceServer(server, served)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		// Stop, not GracefulStop: every RPC here is a stream the agent means to
		// hold open, and this process is meant to end when the test does.
		server.Stop()
	}()

	_, _ = fmt.Fprintf(stderr, "serving AgentService on %s for %v\n", listener.Addr(), []string(sans))
	if err := server.Serve(counted); err != nil {
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

// materialise generates the CA and serving certificate and writes ca.crt and
// token into dir.
func materialise(dir string, sans []string) (*material, error) {
	if err := os.MkdirAll(dir, worldEnter); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone and applies the umask
	// to a new one, and the bind-mounted directory has to be enterable by a
	// uid that is not this one.
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
	// No trailing newline: the agent sends the file's bytes as the token, so
	// anything written here is part of the header the operator would match.
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
// two-PEM ca.crt and serves a certificate signed by the second of the two,
// which is the shape internal/certs.Bundle.PublishedCA produces mid-rotation
// and OperatorChannel.trustManager is meant to survive.
//
// It is built from internal/certs rather than by hand, on purpose: a fixture
// that minted its own bundle with a different code path would prove that path
// agrees with itself, not that the agent survives what production actually
// publishes.
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

	// The bundle as it stands mid-rotation: the first CA still signs, the
	// second is only published. PublishedCA is the same call
	// internal/controller makes to write the ConfigMap in that state, and its
	// result -- first CA then second -- is fixed into bundlePEM below before
	// SwitchToNext runs, because the claim under test is that mounting *this*
	// unchanged bundle keeps trusting the serving certificate across the
	// switch from the first CA to the second.
	b := &certs.Bundle{CACertPEM: firstCertPEM, CAKeyPEM: firstKeyPEM}
	b = b.WithNextCA(secondCertPEM, secondKeyPEM)
	bundlePEM := b.PublishedCA()

	// SwitchToNext is the step that actually moves the signature: the second
	// CA promotes to signing and a fresh serving certificate is cut with it.
	// This is what the operator does at the end of the overlap, and it is the
	// only thing under test here that the agent cannot see directly -- it only
	// ever sees the certificate this produces.
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
	// No trailing newline: see materialise, same reason.
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

// newRecorder returns a recorder writing one JSON object per line to w.
func newRecorder(w io.Writer) *recorder {
	return &recorder{out: w}
}

// record writes {"kind":..., "seq":..., ...fields} and flushes.
//
// seq is the whole reason this is not just an encoder: several streams record
// concurrently, and the overlap assertion needs a total order over the events
// rather than over the lines of one stream. Taking it under the same lock that
// serialises the write is what makes the two agree.
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
		// Only a caller passing something unmarshalable can reach this, and
		// every caller is in this file.
		fmt.Fprintf(os.Stderr, "could not record a %s event: %v\n", kind, err)
		return
	}
	if _, err := r.out.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "could not write a %s event: %v\n", kind, err)
		return
	}
	// A reader tailing the file has to see events as they happen; an os.File
	// write is already a syscall, but the fsync removes the page cache from
	// the question entirely.
	if file, ok := r.out.(*os.File); ok {
		_ = file.Sync()
	}
}

// stub serves AgentService and records. Unless --supersede says otherwise it
// never cancels a stream: see the package comment for why that is the design
// and not an omission.
type stub struct {
	agentpb.UnimplementedAgentServiceServer

	events  *recorder
	streams atomic.Int64

	reportInterval int32
	renewAfter     int32
	hardDeadline   int32

	// supersede and everything below it are the --supersede half. current is
	// the stream a new one would displace; there is one because the test points
	// one agent at this process, where the real operator keys it by pod UID.
	supersede bool
	// muteAfter is the first stream index that is accepted and never answered.
	// Negative means every stream is answered. See muted.
	muteAfter int
	mu        sync.Mutex
	current   *live

	// proxy is the --proxy half: whether a proxy stream is sent a FullSync at
	// all, and fullSyncAfter is how long it is held back once it is.
	// setReadyAfter is the --set-ready-after half: how long after that FullSync
	// the proxy is told to stop being ready, or zero for never.
	proxy         bool
	fullSyncAfter time.Duration
	setReadyAfter time.Duration

	// token is what --require-token demands in the header. Set always, read only
	// by requireBearer, which is only installed under the flag.
	token string
}

// requireBearer refuses a stream that does not present this stub's token,
// before the handler that would have recorded it ever runs. Installed only
// under --require-token; see the package comment for the one obligation that
// needs it.
//
// The refusal is recorded, so a run that fails this way says why in the trace
// rather than only through a silent agent: an agent whose credentials are
// refused reconnects on its backoff, which from the outside is indistinguishable
// from an agent that never reached the operator at all.
//
// The comparison is constant-time for no reason that matters here — nothing is
// attacking a test double — but a token comparison written the other way is a
// pattern worth not leaving in the tree to be copied.
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
// nothing on.
//
// This is the other half of what an operator can do to an agent, and the half
// that has no failure of its own to make it visible. internal/agentserver
// cancels the displaced stream inside sessions.enter (server.go:179) and only
// then calls Agents.Supersede (server.go:193), increments a metric and sends
// (server.go:200, :207); its hard-deadline rescue is armed after both Sends
// (server.go:218). So an operator goroutine that blocks anywhere in between has
// retired the agent's outgoing stream, accepted its replacement, and armed
// nothing that will ever end either wait. Nothing on the operator's side
// resolves that, which is why the agent needs a bound of its own and why this
// flag exists to hold it to one.
func (s *stub) muted(index int64) bool {
	return s.muteAfter >= 0 && index >= int64(s.muteAfter)
}

// live is one stream under --supersede: what ends it, and how to tell that it
// has ended.
type live struct {
	cancel context.CancelFunc
	// done is closed when the handler has returned, which is when its status
	// has gone out on the wire.
	done chan struct{}
}

// enter registers a new stream and retires the one it replaces, which is what
// internal/agentserver's sessions.enter does and where it does it: at the
// handler entry of the replacement, before a byte has gone out on it.
//
// It then holds the replacement back until the displaced stream is gone and a
// little beyond, and that is the deliberate part rather than an accident of
// implementation.
//
// The two things the agent has to order — the displaced stream's status and the
// replacement's first message — travel different connections, and nothing on
// either side orders them. Cancelling and sending on the next line leaves it to
// whichever goroutine the Go scheduler picks and whichever of the agent's two
// transport threads the JVM runs first, and measured, that lands consistently
// on the side where the agent has already handed over before it hears about the
// cancellation. Asserting against that would be asserting that the agent keeps
// winning a coin toss neither end controls: the real operator has a registry
// write, a metric and two Sends between enter() and the agent hearing anything
// on the new stream, and a real cluster has two network paths rather than one
// loopback. So the stub puts the agent on the losing side on purpose. What it
// then measures is the only thing worth measuring here — that the agent is
// right either way.
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
	// The handler has returned; grpc-go writes its trailers after that, so the
	// status is still only on its way out. Nothing here can observe it arrive.
	time.Sleep(retirementHeadStart)
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
	}, nil, s.observeServer)
}

// ProxySession is ServerSession's counterpart, and structurally the same: the
// same opening messages, the same receive loops, the same events. The one
// difference is the FullSync, which has no equivalent on the server side —
// see the package comment.
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
		// Behind the FullSync rather than beside it, because the message the
		// agent is meant to obey here only means anything to a proxy that has
		// already opened its gate: a SetReady{false} against an unsynced proxy
		// closes a port nothing ever bound, and the phase asserting on it would
		// pass without the agent doing a thing.
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
	}, later, s.observeProxy)
}

// delayed is a message the stub sends on a schedule of its own rather than in
// answer to anything: what to send, how long to wait first, and the event kinds
// recorded once the send has returned or failed.
//
// They come as a sequence, and `after` is counted from the previous send rather
// than from the stream opening: --set-ready-after is documented as a wait after
// the FullSync, and a second offset measured from the same origin as the first
// would silently become an absolute schedule the moment either flag moved.
//
// kind is recorded *after* Send returns, never before, because the whole point
// of the flags is that a test can order its own observations against them.
type delayed[Out any] struct {
	message *Out
	after   time.Duration
	kind    string
	failure string
}

// serveSession is the body both rpcs share.
//
// One body rather than two copies, and that is the milestone's own lesson
// applied to the harness: everything asserted about reconnect rates, retirement
// order and silence is measured through this code, so a second copy that drifted
// from it would make the proxy phases quietly measure something else than the
// server phases claim to. What genuinely differs between the two rpcs — the
// message types, the opening messages, the FullSync, how a message is recorded —
// arrives as arguments.
func serveSession[In, Out any](
	s *stub,
	stream grpc.BidiStreamingServer[In, Out],
	opening []*Out,
	later []delayed[Out],
	observe func(func(map[string]any) map[string]any, *In),
) error {
	index := s.streams.Add(1) - 1
	// Verbatim. The script compares this against "Bearer <token>" character for
	// character, the way internal/grpcauth's interceptor does, so nothing here
	// may trim, split or case-fold it.
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

	// A muted stream skips every Send and nothing else: it is registered, it
	// displaced whatever came before it, and it stays open. That is the point —
	// the agent holds a stream the operator accepted and will never answer. The
	// scheduled messages are skipped with them, because a muted operator that
	// still handed out a server list, or still asserted a readiness, would not
	// be silent.
	if !muted {
		for _, message := range opening {
			if err := stream.Send(message); err != nil {
				s.events.record("stream_closed", of(map[string]any{"error": err.Error()}))
				return nil
			}
		}

		if len(later) > 0 {
			// One goroutine, because the hold has to overlap the receive loop
			// below rather than precede it. The agent's Hello arrives while the
			// timer runs, and a stub that slept before its first Recv would
			// record the greeting only once the FullSync had already gone out —
			// collapsing the two events the test needs to probe between into
			// one moment.
			//
			// One for the whole sequence rather than one each, and that is what
			// keeps it the only sender left once the messages above are on the
			// wire: nothing here ever calls Send concurrently. The deferred
			// stop-and-wait is what keeps that true through the handler's
			// return: grpc-go finishes the stream when the handler returns, and
			// a goroutine still holding it would be sending on a stream that no
			// longer exists.
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
		// The passive loop, and deliberately still a plain blocking Recv: the
		// only thing that ends this stream is the agent, so there is no second
		// event to select on and no way for a cancellation to be recorded in
		// place of the agent's own close.
		for {
			message, err := stream.Recv()
			if err != nil {
				s.events.record("stream_closed", of(map[string]any{"error": closeReason(err)}))
				// nil, not the error: the agent ending a stream it renewed is
				// the expected outcome here, not a failure of the stub.
				return nil
			}
			observe(of, message)
		}
	}

	// Receiving blocks, so under --supersede it runs in its own goroutine and
	// the context does the cancelling — the same shape, for the same reason, as
	// internal/agentserver's ServerSession.
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
			observe(of, message)
		}
	}
}

// observeServer records one message from a server agent.
func (s *stub) observeServer(of func(map[string]any) map[string]any, message *agentpb.ServerMessage) {
	switch body := message.Message.(type) {
	case *agentpb.ServerMessage_Hello:
		s.hello(of, body.Hello)
	case *agentpb.ServerMessage_Ready:
		s.events.record("ready", of(map[string]any{}))
	case *agentpb.ServerMessage_PlayerCount:
		s.playerCount(of, body.PlayerCount)
	default:
		s.events.record("unknown", of(map[string]any{}))
	}
}

// observeProxy records one message from a proxy agent.
//
// There is no Ready: a proxy's readiness reaches the operator through the
// kubelet's probe on the agent's own port and nowhere else, which is why
// hack/agent-test.sh probes that port rather than reading this trace for it.
func (s *stub) observeProxy(of func(map[string]any) map[string]any, message *agentpb.ProxyMessage) {
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
		// Recorded rather than ignored, because this is the one message whose
		// absence looks exactly like an agent that works. It is what tells the
		// operator a player is *arriving* at a backend, which neither the
		// backend nor the proxy's own player list can see, and an agent that
		// silently stopped sending it would leave every trace here looking
		// perfectly healthy while a drain deleted a pod under somebody.
		//
		// The map is sorted into a slice of pairs, not written as an object:
		// a JSON object's key order is not a thing a test can assert on, and
		// this trace exists to be asserted on.
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
	case *agentpb.ProxyMessage_Heartbeat:
		s.events.record("heartbeat", of(map[string]any{}))
	default:
		s.events.record("unknown", of(map[string]any{}))
	}
}

// hello and playerCount are what the two observers share. The field names are
// the ones hack/agent-test.sh's jq reads, and it reads them the same way in
// both directions — so recording them in one place is what keeps a proxy phase's
// assertion about `slots` meaning what the server phase's does.
func (s *stub) hello(of func(map[string]any) map[string]any, hello *agentpb.Hello) {
	s.events.record("hello", of(map[string]any{
		"version": hello.GetVersion(),
		"ready":   hello.GetReady(),
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
