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

package agentserver_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/proxyreg"
	"github.com/spawnery/spawnery/internal/serverreg"
	"github.com/spawnery/spawnery/internal/testenv"
)

// dialAgent opens a ServerSession the way a real agent would: TLS against the
// pinned CA, token in the authorization header.
func dialAgent(t *testing.T, ctx context.Context, addr string, ca []byte, token string) (
	grpc.BidiStreamingClient[agentpb.ServerMessage, agentpb.OperatorToServer], func()) {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("CA bundle unusable")
	}
	// The agent pins this CA and nothing else — that is the whole point of the
	// operator issuing its own certificate.
	creds := credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: "spawnery-operator.spawnery-system.svc",
		MinVersion: tls.VersionTLS13,
	})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	stream, err := agentpb.NewAgentServiceClient(conn).ServerSession(streamCtx)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("open ServerSession: %v", err)
	}
	return stream, func() { _ = conn.Close() }
}

type serverFixture struct {
	t       *testing.T
	ctx     context.Context
	c       client.Client
	cs      *kubernetes.Clientset
	ns      string
	agents  *agent.Registry
	proxies *proxyreg.Fleet
	addr    string
	ca      []byte
}

func newServerFixture(t *testing.T) *serverFixture {
	return newFixture(t, 8*time.Minute, 10*time.Minute, 0)
}

func newServerFixtureWithDeadline(t *testing.T, renewAfter, hardDeadline time.Duration) *serverFixture {
	return newFixture(t, renewAfter, hardDeadline, 0)
}

// newServerFixtureWithProxyOutbox is for the closed-outbox test: it needs a
// queue small enough to overflow after a handful of registrations rather than
// however many it takes to exhaust a real stream's flow-control window.
func newServerFixtureWithProxyOutbox(t *testing.T, outboxSize int) *serverFixture {
	return newFixture(t, 8*time.Minute, 10*time.Minute, outboxSize)
}

func newFixture(t *testing.T, renewAfter, hardDeadline time.Duration, proxyOutboxSize int) *serverFixture {
	return newFixtureWithProxies(t, renewAfter, hardDeadline, proxyOutboxSize, nil)
}

// newFixtureWithProxies is newFixture with one more knob: what the server's
// Options.Proxies actually is. Every other fixture wants the real *Fleet
// wired straight through — wrap nil gives them exactly that — but a test that
// needs to observe what ProxySession hands Join needs a fake sitting in that
// one slot without losing the rest of the scaffolding (certs, auth, the real
// fleet still available for f.proxies.Register and friends).
func newFixtureWithProxies(t *testing.T, renewAfter, hardDeadline time.Duration, proxyOutboxSize int,
	wrap func(*proxyreg.Fleet) agentserver.ProxyFleet) *serverFixture {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	cs, err := kubernetes.NewForConfig(testenv.Config(t))
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if err := c.Create(ctx, sa); err != nil {
			t.Fatalf("create ServiceAccount %s: %v", name, err)
		}
	}

	now := func() time.Time { return time.Now() }
	store := &certs.Store{
		Client: c, Namespace: ns, Name: certs.SecretName,
		// The SANs must match what dialAgent asks for, not the test namespace.
		DNSNames: certs.ServingDNSNames("spawnery-operator", "spawnery-system"),
		Clock:    now,
	}
	provider := certs.NewProvider(store)
	bundle, err := store.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := provider.Set(bundle); err != nil {
		t.Fatalf("Set: %v", err)
	}

	registry := agent.New(now, 5*time.Second, now())
	// The picture both fan-outs send, built once, exactly as the operator
	// binary builds it.
	state := netstate.Source{Reader: c, Agents: registry}
	fleet := proxyreg.New(proxyreg.Options{Reader: c, OutboxSize: proxyOutboxSize, State: state})
	servers := serverreg.New(serverreg.Options{State: state})
	var proxies agentserver.ProxyFleet = fleet
	if wrap != nil {
		proxies = wrap(fleet)
	}
	srv := agentserver.New(agentserver.Options{
		// Port 0: the kernel picks a free one, so parallel packages do not
		// collide.
		Addr:     "127.0.0.1:0",
		Provider: provider,
		Auth: &grpcauth.Authenticator{
			// The operator's own view, not the harness's: this is the one call in the
			// tree that needs `authentication.k8s.io/tokenreviews: create`, and under
			// testenv.Client it would have been granted everything and proved nothing.
			// f.cs stays admin because the harness mints ServiceAccount tokens, which
			// the operator has no right to do and must not acquire.
			Reviews:  restrictedCS(t).AuthenticationV1().TokenReviews(),
			Pods:     &grpcauth.ClientPodChecker{Client: c},
			Audience: podspec.AgentTokenAudience,
		},
		Agents:         registry,
		Proxies:        proxies,
		Servers:        servers,
		ReportInterval: 5 * time.Second,
		RenewAfter:     renewAfter,
		HardDeadline:   hardDeadline,
		Clock:          now,
	})

	serverCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		if err := srv.Start(serverCtx); err != nil {
			t.Logf("agent server stopped: %v", err)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("the agent server never bound a port")
		}
		time.Sleep(10 * time.Millisecond)
	}

	return &serverFixture{
		t: t, ctx: ctx, c: c, cs: cs, ns: ns,
		agents: registry, proxies: fleet, addr: srv.Addr(), ca: provider.CABundle(),
	}
}

// pod creates a managed server pod and returns it. The full label set matters:
// the authenticator insists on both the managed-by label and a role label that
// matches the session being opened.
func (f *serverFixture) pod(name string) *corev1.Pod {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ns,
			Labels:    podspec.ServerLabels("production", "lobby", name),
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: podspec.ServerServiceAccountName,
			Containers:         []corev1.Container{{Name: "minecraft", Image: "example/paper:1"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		f.t.Fatalf("create pod: %v", err)
	}
	return pod
}

// token mints a pod-bound token the way the kubelet would. sa and audiences
// are explicit rather than assumed to be the server agent's, because a proxy
// pod needs a token bound to its own ServiceAccount: the real TokenRequest
// API refuses to bind a token for one ServiceAccount to a pod running under
// another.
func (f *serverFixture) token(sa string, audiences []string, boundTo *corev1.Pod) string {
	f.t.Helper()
	tr, err := f.cs.CoreV1().ServiceAccounts(f.ns).CreateToken(f.ctx, sa,
		&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{
			Audiences:         audiences,
			ExpirationSeconds: ptr.To(int64(600)),
			BoundObjectRef: &authnv1.BoundObjectReference{
				Kind: "Pod", APIVersion: "v1", Name: boundTo.Name, UID: boundTo.UID,
			},
		}}, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatalf("TokenRequest: %v", err)
	}
	return tr.Status.Token
}

// proxyPod creates a pod running under the proxy ServiceAccount. It exists
// only so a proxy token can be minted at all: the real TokenRequest API
// refuses to bind a token for one ServiceAccount to a pod that runs under a
// different one, so a proxy token needs a genuine proxy pod behind it.
func (f *serverFixture) proxyPod(name string) *corev1.Pod {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ns,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelNetwork:   "production",
				podspec.LabelGroup:     "gateway",
				podspec.LabelRole:      podspec.RoleProxy,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: podspec.ProxyServiceAccountName,
			Containers:         []corev1.Container{{Name: "velocity", Image: "example/velocity:1"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		f.t.Fatalf("create pod: %v", err)
	}
	return pod
}

type serverStream = grpc.BidiStreamingClient[agentpb.ServerMessage, agentpb.OperatorToServer]

func mustSend(t *testing.T, stream serverStream, msg *agentpb.ServerMessage) {
	t.Helper()
	if err := stream.Send(msg); err != nil {
		t.Fatalf("send %T: %v", msg.GetMessage(), err)
	}
}

func hello(ready bool) *agentpb.ServerMessage {
	return &agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_Hello{Hello: &agentpb.Hello{Version: "0.1.0", Ready: ready}},
	}
}

func playerCount(players, slots int32) *agentpb.ServerMessage {
	return &agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_PlayerCount{
			PlayerCount: &agentpb.PlayerCount{Players: players, Slots: slots},
		},
	}
}

// awaitSession blocks until the operator's first message arrives on a stream.
// The operator sends it only after registering the stream, so this is the
// point from which the new stream is the live one.
//
// A real agent has to wait for it too: make-before-break means dropping the
// old stream after the new one is established, never before. Dropped earlier,
// the operator has not seen the new stream at all and the disconnect it
// records is simply the truth.
func awaitSession(t *testing.T, stream serverStream) {
	t.Helper()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the operator never confirmed the session: %v", err)
	}
}

// waitFor polls until the condition holds. Message handling is concurrent with
// the test, so a direct comparison would be flaky.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the condition never held within three seconds")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHelloWithReadyMarksTheAgentReady(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeConn()

	if err := stream.Send(&agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_Hello{Hello: &agentpb.Hello{Version: "0.1.0", Ready: true}},
	}); err != nil {
		t.Fatalf("send Hello: %v", err)
	}

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })
	snap := f.agents.Lookup(string(pod.UID))
	if !snap.Connected {
		t.Error("the registry does not see the stream")
	}
}

// The operator dictates the interval, so both sides derive the staleness
// threshold from the same number.
func TestOperatorSendsIntervalAndDeadlineOnConnect(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeConn()

	var gotInterval, gotDeadline bool
	for range 2 {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch m := msg.GetMessage().(type) {
		case *agentpb.OperatorToServer_ReportInterval:
			gotInterval = true
			if m.ReportInterval.GetSeconds() != 5 {
				t.Errorf("ReportInterval = %ds, want 5s", m.ReportInterval.GetSeconds())
			}
		case *agentpb.OperatorToServer_SessionDeadline:
			gotDeadline = true
			if m.SessionDeadline.GetRenewAfterSeconds() >= m.SessionDeadline.GetHardDeadlineSeconds() {
				t.Errorf("renewAfter %d must be below hardDeadline %d",
					m.SessionDeadline.GetRenewAfterSeconds(),
					m.SessionDeadline.GetHardDeadlineSeconds())
			}
		}
	}
	if !gotInterval || !gotDeadline {
		t.Errorf("interval=%v deadline=%v, want both", gotInterval, gotDeadline)
	}
}

func TestPlayerCountReachesTheRegistry(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	mustSend(t, stream, playerCount(7, 100))

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 7 })
	if got := f.agents.Lookup(string(pod.UID)).Slots; got != 100 {
		t.Errorf("Slots = %d, want 100", got)
	}
}

// Spec 5.2: discard, do not disconnect. Dropping the stream would be a
// reconnect loop the agent could trigger at will.
func TestPlayerCountAboveSlotsIsDiscardedButKeepsTheStream(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	mustSend(t, stream, playerCount(5, 100))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 5 })

	mustSend(t, stream, playerCount(4000, 100))
	mustSend(t, stream, playerCount(6, 100))

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 6 })
	if !f.agents.Lookup(string(pod.UID)).Connected {
		t.Error("the stream was dropped over a bad report")
	}
}

func TestDisconnectIsVisibleInTheRegistry(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))

	mustSend(t, stream, hello(true))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })

	closeConn()
	waitFor(t, func() bool { return !f.agents.Lookup(string(pod.UID)).Connected })
	if f.agents.Lookup(string(pod.UID)).Ready {
		t.Error("a broken stream left the agent marked ready")
	}
}

// Make-before-break: this is what keeps a renewal from dropping the server out
// of Ready every ten minutes.
func TestASecondStreamSupersedesTheFirstWithoutLosingState(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")

	first, closeFirst := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	mustSend(t, first, hello(true))
	mustSend(t, first, playerCount(3, 100))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 3 })

	second, closeSecond := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeSecond()
	// Established first, dropped second: the order the agent owes the
	// operator. Without the wait the new stream may not have reached the
	// server yet, and the disconnect the old one then reports is correct.
	awaitSession(t, second)
	mustSend(t, second, hello(true))

	// The superseded stream ends, and that must not tear down the new one.
	closeFirst()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !f.agents.Lookup(string(pod.UID)).Connected {
			t.Fatal("the superseded stream disconnected the live one")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !f.agents.Lookup(string(pod.UID)).Ready {
		t.Error("the new stream lost the ready state")
	}
}

// The window this closes is narrow but it opens on every renewal of every
// server: the new stream registers before its own Hello arrives, and a
// reconcile that samples "connected but not ready" in between treats it as an
// immediate readiness loss — deregistering the server from the proxies. So
// readiness is sampled continuously across the handover, and the new stream
// deliberately never says Hello: only the readiness carried over from the
// stream it displaced can keep the flag up.
func TestASupersedingStreamNeverLetsReadinessDrop(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")

	first, closeFirst := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeFirst()
	mustSend(t, first, hello(true))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })

	var flickered, samples atomic.Int64
	stop := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if !f.agents.Lookup(string(pod.UID)).Ready {
				flickered.Add(1)
			}
			samples.Add(1)
			runtime.Gosched()
		}
	}()

	second, closeSecond := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeSecond()
	// A report the registry accepts proves the new stream is the live one:
	// ReportPlayers refuses anything without a live stream behind it.
	mustSend(t, second, playerCount(11, 100))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 11 })

	close(stop)
	watcher.Wait()

	if samples.Load() == 0 {
		t.Fatal("the watcher never sampled the registry")
	}
	if n := flickered.Load(); n != 0 {
		t.Errorf("readiness was false in %d of %d samples across the handover, want none",
			n, samples.Load())
	}
	if !f.agents.Lookup(string(pod.UID)).Ready {
		t.Error("the superseding stream ended up unready")
	}
}

// The complement of the test above, and the reason the carry-over is tied to
// displacing a live stream rather than granted to every stream: after a real
// break the agent process may have restarted, and only its own Hello may say
// it is ready again. A reconnect that stays silent has to stay unready.
func TestAReconnectAfterARealBreakStartsUnready(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")

	first, closeFirst := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	mustSend(t, first, hello(true))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })

	// The break has to be complete before the new stream arrives, or this
	// would be the make-before-break case again.
	closeFirst()
	waitFor(t, func() bool { return !f.agents.Lookup(string(pod.UID)).Connected })

	second, closeSecond := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeSecond()
	// No Hello on this stream: nothing may speak for the agent but the agent.
	awaitSession(t, second)
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Connected })

	if f.agents.Lookup(string(pod.UID)).Ready {
		t.Error("a silent reconnect came back ready without the agent saying so")
	}
}

// The hard deadline is the net under an agent that ignores renewAfter.
func TestTheHardDeadlineClosesTheStream(t *testing.T) {
	f := newServerFixtureWithDeadline(t, 300*time.Millisecond, 600*time.Millisecond)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	// The operator's own first message is what proves the session runs, and it
	// arrives before the deadline can have started counting. Readiness would
	// be the wrong signal here: the teardown at the deadline clears it, so a
	// slow TokenReview could push the wait past the 600 ms and leave the test
	// waiting for a flag that correct code has already taken back.
	awaitSession(t, stream)

	// Recv returns the error once the operator hangs up.
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the operator did not close the stream at the hard deadline")
	}
}

// An unknown branch must be ignored, not fatal: a newer agent against an older
// operator has to keep working.
func TestAnEmptyMessageIsIgnored(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })

	// A ServerMessage with no branch set is what an unknown future branch
	// decodes to on this operator.
	mustSend(t, stream, &agentpb.ServerMessage{})
	mustSend(t, stream, playerCount(4, 100))

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 4 })
	if !f.agents.Lookup(string(pod.UID)).Connected {
		t.Error("an unknown message tore down the stream")
	}
}

// A server pod's token does not carry a Group, and even if it did, the role
// the authenticator saw at TokenReview time was server — a proxy session
// insists on RoleProxy, so this is refused before a single message crosses
// the wire.
func TestAServerTokenOnAProxySessionIsUnauthenticated(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.ca) {
		t.Fatal("CA bundle unusable")
	}
	conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{
			RootCAs:    pool,
			ServerName: "spawnery-operator.spawnery-system.svc",
			MinVersion: tls.VersionTLS13,
		})))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx := metadata.AppendToOutgoingContext(f.ctx, "authorization", "Bearer "+f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	stream, err := agentpb.NewAgentServiceClient(conn).ProxySession(ctx)
	if err != nil {
		t.Fatalf("open ProxySession: %v", err)
	}
	if err := stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_Hello{Hello: &agentpb.Hello{Version: "0.1.0"}},
	}); err != nil {
		// A send may already fail once the server hung up — but SendMsg
		// never carries the wire status; gRPC returns io.EOF once the server
		// has ended the stream, or a client-side Unavailable, and requires
		// the caller to fetch the real reason from RecvMsg. Asserting on
		// err here would be exactly the operator-side/wire-side confusion
		// this whole channel exists to eliminate: the status this test
		// cares about is Recv's, not Send's, on either path.
		if _, recvErr := stream.Recv(); status.Code(recvErr) != codes.Unauthenticated {
			t.Errorf("code = %s, want Unauthenticated", status.Code(recvErr))
		}
		return
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("ProxySession answered a server token")
	}
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Errorf("code = %s, want Unauthenticated", code)
	}
}

// TestTheServerBoundsStreamsPerConnection is the one new bound that can be
// observed from outside. An agent opens exactly one stream -- the proto has two
// RPCs and a session uses one of them -- so the limit is generous by design;
// what it stops is a single connection multiplexing an unbounded number.
//
// Be precise about what this does NOT bound, because the convenient reading is
// that it closes the availability gap and it does not: MaxConcurrentStreams is
// per connection, so a pod that opens many connections is untouched by it.
// That is MaxConnectionsPerPeer's job, and
// TestTheServerBoundsConnectionsPerPeer below is where it is proven.
func TestTheServerBoundsStreamsPerConnection(t *testing.T) {
	f := newServerFixture(t)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.ca) {
		t.Fatal("CA bundle unusable")
	}
	creds := credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: "spawnery-operator.spawnery-system.svc",
		MinVersion: tls.VersionTLS13,
	})
	// One connection, many streams: that is the shape the bound governs.
	conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	client := agentpb.NewAgentServiceClient(conn)

	open := func(ctx context.Context, i int) (
		grpc.BidiStreamingClient[agentpb.ServerMessage, agentpb.OperatorToServer], error) {
		pod := f.pod(fmt.Sprintf("lobby-stream-%d", i))
		token := f.token(podspec.ServerServiceAccountName,
			[]string{podspec.AgentTokenAudience}, pod)
		streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		return client.ServerSession(streamCtx)
	}

	for i := 0; i < int(agentserver.MaxConcurrentStreams); i++ {
		stream, err := open(f.ctx, i)
		if err != nil {
			t.Fatalf("stream %d of the permitted %d was refused: %v",
				i, agentserver.MaxConcurrentStreams, err)
		}
		// Hold it open: a stream is only concurrent while it lives.
		if err := stream.Send(&agentpb.ServerMessage{
			Message: &agentpb.ServerMessage_Hello{
				Hello: &agentpb.Hello{Version: "0.1.0", Ready: false},
			},
		}); err != nil {
			t.Fatalf("send Hello on stream %d: %v", i, err)
		}
	}

	// One past the limit. grpc-go's http2Client.NewStream -- which the
	// generated ServerSession(ctx) stub calls synchronously, before this
	// function returns -- mirrors the server's advertised
	// SETTINGS_MAX_CONCURRENT_STREAMS as a stream quota, and once that quota
	// is exhausted it blocks right there, in a select on the caller's context
	// or a quota-available channel, before NewStream ever returns (grpc-go
	// v1.83.0, internal/transport/http2_client.go:756-916, quota check around
	// :1297-1337). So with the bound in force it is open() itself that blocks
	// until over's five-second deadline -- not Recv(), which never even gets
	// called -- and the error this test needs to check comes back from open,
	// not from a later Recv on a stream that opened fine. Without the bound,
	// open() returns immediately with room to spare and Recv() is what would
	// have to be checked instead; both branches below exist because either
	// shape is possible depending on which one actually happens.
	over, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	extra, err := open(over, int(agentserver.MaxConcurrentStreams))
	if err != nil {
		// DeadlineExceeded only, not a wider set: both of the obvious
		// alternatives are codes this codebase genuinely produces for
		// reasons that have nothing to do with the bound, and both can only
		// reach open() after the ninth stream has already cleared the
		// client-side quota gate -- i.e. only once MaxConcurrentStreams has
		// already failed to hold. grpcauth's interceptor returns
		// Unavailable specifically when a TokenReview call itself is
		// unavailable (internal/grpcauth/interceptor.go), deliberately kept
		// apart from Unauthenticated so an agent backs off instead of
		// concluding its credentials are wrong -- an ordinary envtest-load
		// flake here would look identical to a passing bound. And
		// ResourceExhausted is grpc-go's own mapping for ENHANCE_YOUR_CALM
		// and flow-control errors -- exactly the code this task's own new
		// keepalive enforcement policy can produce. Accepting either would
		// let the one failure this test exists to catch report PASS.
		//
		// The narrowing is safe in this fixture and only in this fixture.
		// The two real paths to Unavailable during the quota wait are a
		// GOAWAY and the transport context closing, and neither can fire
		// here: the connection is never idle (eight live streams),
		// MaxConnectionAge is unset, this client sets no keepalive so
		// ENHANCE_YOUR_CALM cannot fire, and the transport context is
		// cancelled only by t.Cleanup. Share a connection across subtests
		// here, or give the client a keepalive, and that argument stops
		// holding -- revisit the narrowing before doing either.
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("stream %d failed to open with %v, want the deadline "+
				"-- a different code means it failed for some other reason "+
				"and this test proves nothing about the bound",
				agentserver.MaxConcurrentStreams+1, err)
		}
		return
	}
	if _, err := extra.Recv(); err == nil {
		t.Errorf("stream %d was served; MaxConcurrentStreams is not in force",
			agentserver.MaxConcurrentStreams+1)
	} else if status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("stream %d failed with %v, want the deadline — a different "+
			"error means it was refused for some other reason and this test "+
			"proves nothing about the bound", agentserver.MaxConcurrentStreams+1, err)
	}
}

// restrictedCS is a clientset acting as the operator does in a cluster, under
// its own ServiceAccount and the ClusterRole config/rbac/role.yaml
// generates. See testenv.RestrictedConfig.
func restrictedCS(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	cs, err := kubernetes.NewForConfig(testenv.RestrictedConfig(t))
	if err != nil {
		t.Fatalf("restricted clientset: %v", err)
	}
	return cs
}

// TestTheServerBoundsConnectionsPerPeer is the other half, and the one that
// closes milestone 2a's availability gap: a single peer cannot hold an
// unbounded number of connections open, no matter how legitimate each one is.
//
// Every connection here carries a valid token and a live stream, which is what
// makes it the real attack rather than a caricature of one. None of the bounds
// that came before this touches that shape -- MaxConcurrentStreams is per
// connection, MaxConnectionIdle never fires on a connection carrying a stream,
// and grpcauth's rate limit throttles TokenReview misses, which a pod
// replaying one valid token does not produce.
//
// One connection per grpc.NewClient, deliberately. A single ClientConn would
// multiplex every stream onto one transport, which is the previous test's
// subject and would prove nothing here.
func TestTheServerBoundsConnectionsPerPeer(t *testing.T) {
	f := newServerFixture(t)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.ca) {
		t.Fatal("CA bundle unusable")
	}
	creds := credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: "spawnery-operator.spawnery-system.svc",
		MinVersion: tls.VersionTLS13,
	})

	// session opens a connection of its own and takes a stream on it as far as
	// the operator's first message, which is the point the stream is really
	// being served rather than merely requested.
	session := func(i int) error {
		conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(creds))
		if err != nil {
			return fmt.Errorf("dial: %w", err)
		}
		t.Cleanup(func() { _ = conn.Close() })

		pod := f.pod(fmt.Sprintf("lobby-conn-%d", i))
		token := f.token(podspec.ServerServiceAccountName,
			[]string{podspec.AgentTokenAudience}, pod)
		ctx := metadata.AppendToOutgoingContext(f.ctx, "authorization", "Bearer "+token)
		stream, err := agentpb.NewAgentServiceClient(conn).ServerSession(ctx)
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		// Recv, not Send: a Send goes into the transport's buffer and would
		// succeed against a connection the listener has already closed.
		if _, err := stream.Recv(); err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		return nil
	}

	for i := 0; i < agentserver.MaxConnectionsPerPeer; i++ {
		if err := session(i); err != nil {
			t.Fatalf("connection %d of the permitted %d was refused: %v",
				i+1, agentserver.MaxConnectionsPerPeer, err)
		}
	}

	// The one over the bound. Its TCP connection is accepted and closed before
	// TLS, so what the client sees is a transport that will not come up --
	// Unavailable, after grpc-go has retried it as far as the context allows.
	over, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(creds))
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		pod := f.pod("lobby-conn-over")
		token := f.token(podspec.ServerServiceAccountName,
			[]string{podspec.AgentTokenAudience}, pod)
		ctx := metadata.AppendToOutgoingContext(over, "authorization", "Bearer "+token)
		stream, err := agentpb.NewAgentServiceClient(conn).ServerSession(ctx)
		if err != nil {
			done <- err
			return
		}
		_, err = stream.Recv()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("connection %d was served; MaxConnectionsPerPeer is not in force",
				agentserver.MaxConnectionsPerPeer+1)
		}
		// Unavailable is what a transport that never came up produces, and the
		// message it came back with says which half refused it:
		//
		//	transport: authentication handshake failed: EOF
		//
		// The connection was closed before TLS -- the listener's doing, not the
		// authenticator's, which is the whole design argument for putting the
		// bound on the listener rather than in a StatsHandler. The code is what
		// is asserted, though, and not the message: Unauthenticated here would
		// mean the connection *was* served and the token then rejected, a
		// different outcome wearing the same failure. The code separates them
		// where the wording would only track grpc-go's phrasing.
		if code := status.Code(err); code != codes.Unavailable && code != codes.DeadlineExceeded {
			t.Errorf("code = %s, want Unavailable or DeadlineExceeded (err: %v)", code, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("connection %d neither succeeded nor failed",
			agentserver.MaxConnectionsPerPeer+1)
	}
}

// A backend receives the mirror. Until 7b-3 this stream carried two messages
// when it opened and nothing ever again.
func TestAServerAgentReceivesItsNetworkState(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	stream, done := dialAgent(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()

	// The two opening messages come first; the state follows. Read until it
	// arrives rather than assuming a position -- the opening sends and the
	// fan-out's join are two different code paths and their order is not a
	// contract this test should pin.
	var state *agentpb.NetworkState
	for i := 0; i < 5 && state == nil; i++ {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		state = msg.GetNetworkState()
	}
	if state == nil {
		t.Fatal("no NetworkState arrived in the first five messages of a server session")
	}
}
