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
	"github.com/spawnery/spawnery/internal/netstatus"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/proxyreg"
	"github.com/spawnery/spawnery/internal/serverreg"
	"github.com/spawnery/spawnery/internal/testenv"
)

func dialAgent(t *testing.T, ctx context.Context, addr string, ca []byte, token string) (
	grpc.BidiStreamingClient[agentpb.ServerMessage, agentpb.OperatorToServer], func()) {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("CA bundle unusable")
	}
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

// newServerFixtureWithProxyOutbox gives a queue small enough to overflow after a
// handful of registrations rather than a flow-control window's worth.
func newServerFixtureWithProxyOutbox(t *testing.T, outboxSize int) *serverFixture {
	return newFixture(t, 8*time.Minute, 10*time.Minute, outboxSize)
}

func newFixture(t *testing.T, renewAfter, hardDeadline time.Duration, proxyOutboxSize int) *serverFixture {
	return newFixtureWithProxies(t, renewAfter, hardDeadline, proxyOutboxSize, nil)
}

// newFixtureWithProxies lets a test wrap Options.Proxies; wrap nil keeps the real *Fleet.
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
	state := netstate.Source{Reader: c, Agents: registry}
	// The real writer, not a stub: a retire that patches nothing would pass every
	// assertion about the answer.
	writer := agentserver.KubeWriter{Client: c, Clock: now}
	fleet := proxyreg.New(proxyreg.Options{Reader: c, OutboxSize: proxyOutboxSize, State: state})
	servers := serverreg.New(serverreg.Options{State: state})
	var proxies agentserver.ProxyFleet = fleet
	if wrap != nil {
		proxies = wrap(fleet)
	}
	srv := agentserver.New(agentserver.Options{
		Addr:     "127.0.0.1:0",
		Provider: provider,
		Auth: &grpcauth.Authenticator{
			// The operator's own RBAC, not testenv.Client: this is the one call needing
			// tokenreviews create, and admin rights would prove nothing. f.cs stays admin
			// because minting ServiceAccount tokens is a right the operator must not have.
			Reviews:  restrictedCS(t).AuthenticationV1().TokenReviews(),
			Pods:     &grpcauth.ClientPodChecker{Client: c},
			Audience: podspec.AgentTokenAudience,
		},
		Agents:         registry,
		Proxies:        proxies,
		Servers:        servers,
		State:          state,
		Writer:         writer,
		Status:         netstatus.Source{Reader: c, Agents: registry, Metrics: fixtureMetrics{}, Clock: now},
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

// The authenticator insists on the managed-by label and a role label matching
// the session being opened.
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

// The TokenRequest API refuses to bind one ServiceAccount's token to a pod running
// under another, so sa is explicit.
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

// proxyPod exists only so a proxy token can be minted; see token.
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

// awaitSession returns once the operator's first message arrives, which it sends only
// after registering the stream.
func awaitSession(t *testing.T, stream serverStream) {
	t.Helper()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the operator never confirmed the session: %v", err)
	}
}

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

// Dropping the stream would be a reconnect loop the agent could trigger at will.
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

// Make-before-break keeps a renewal from dropping the server out of Ready.
func TestASecondStreamSupersedesTheFirstWithoutLosingState(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")

	first, closeFirst := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	mustSend(t, first, hello(true))
	mustSend(t, first, playerCount(3, 100))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 3 })

	second, closeSecond := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeSecond()
	// Without the wait the new stream may not have reached the server yet, and the
	// old one's disconnect would be correctly reported.
	awaitSession(t, second)
	mustSend(t, second, hello(true))

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

// The new stream registers before its own Hello, and a reconcile sampling "connected
// but not ready" in between would deregister the server. It never says Hello, so only
// the readiness carried over from the displaced stream can keep the flag up.
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

// Carry-over is tied to displacing a live stream: after a real break the agent may
// have restarted, and only its own Hello may say it is ready.
func TestAReconnectAfterARealBreakStartsUnready(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")

	first, closeFirst := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	mustSend(t, first, hello(true))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })

	closeFirst()
	waitFor(t, func() bool { return !f.agents.Lookup(string(pod.UID)).Connected })

	second, closeSecond := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeSecond()
	awaitSession(t, second)
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Connected })

	if f.agents.Lookup(string(pod.UID)).Ready {
		t.Error("a silent reconnect came back ready without the agent saying so")
	}
}

func TestTheHardDeadlineClosesTheStream(t *testing.T) {
	f := newServerFixtureWithDeadline(t, 300*time.Millisecond, 600*time.Millisecond)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	// Not readiness: the teardown at the deadline clears it, so a slow TokenReview could
	// leave the test waiting for a flag correct code already took back.
	awaitSession(t, stream)

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

// A newer agent against an older operator has to keep working.
func TestAnEmptyMessageIsIgnored(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })

	// An unknown future branch decodes to a ServerMessage with no branch set.
	mustSend(t, stream, &agentpb.ServerMessage{})
	mustSend(t, stream, playerCount(4, 100))

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 4 })
	if !f.agents.Lookup(string(pod.UID)).Connected {
		t.Error("an unknown message tore down the stream")
	}
}

// The authenticator saw role server at TokenReview time; a proxy session insists on
// RoleProxy.
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
		// SendMsg never carries the wire status; the reason has to come from RecvMsg.
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

// An agent opens one stream per connection, so the limit is generous; it does not bound
// a pod opening many connections, which is MaxConnectionsPerPeer's job.
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
		// A stream is only concurrent while it lives.
		if err := stream.Send(&agentpb.ServerMessage{
			Message: &agentpb.ServerMessage_Hello{
				Hello: &agentpb.Hello{Version: "0.1.0", Ready: false},
			},
		}); err != nil {
			t.Fatalf("send Hello on stream %d: %v", i, err)
		}
	}

	// One past the limit. grpc-go's NewStream blocks on the stream quota mirrored from
	// SETTINGS_MAX_CONCURRENT_STREAMS, so with the bound in force open() itself blocks
	// until the deadline; without it Recv() would have to be checked instead.
	over, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	extra, err := open(over, int(agentserver.MaxConcurrentStreams))
	if err != nil {
		// DeadlineExceeded only: Unavailable (grpcauth's TokenReview outage) and
		// ResourceExhausted (ENHANCE_YOUR_CALM, flow control) have unrelated causes here and
		// would let a failed bound pass. The narrowing holds only because this connection is
		// never idle, has no MaxConnectionAge and no client keepalive; revisit it if any changes.
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

// Every connection carries a valid token and a live stream: none of MaxConcurrentStreams,
// MaxConnectionIdle or grpcauth's TokenReview-miss rate limit touches that shape.
// One connection per grpc.NewClient: a shared ClientConn would multiplex onto one
// transport, which is the previous test's subject.
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

	// session holds its stream up to the operator's first message: served, not merely requested.
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

	// The one over the bound: closed before TLS, so the client sees Unavailable.
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
		// Asserting the code, not the message ("authentication handshake failed: EOF"):
		// Unauthenticated would mean the connection was served and the token rejected.
		if code := status.Code(err); code != codes.Unavailable && code != codes.DeadlineExceeded {
			t.Errorf("code = %s, want Unavailable or DeadlineExceeded (err: %v)", code, err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("connection %d neither succeeded nor failed",
			agentserver.MaxConnectionsPerPeer+1)
	}
}

func TestAServerAgentReceivesItsNetworkState(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	stream, done := dialAgent(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()

	// Read until the state arrives: the opening sends and the fan-out's join are different
	// code paths with no ordering contract.
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

func TestThePlayableFigureReachesTheRegistry(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-play")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	mustSend(t, stream, &agentpb.ServerMessage{Message: &agentpb.ServerMessage_PlayerCount{
		PlayerCount: &agentpb.PlayerCount{Players: 14, Slots: 100, PlayableSlots: 12},
	}})

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).PlayableSlots == 12 })
	if got := f.agents.Lookup(string(pod.UID)).Players; got != 14 {
		t.Errorf("Players = %d, want 14: players beyond the playable seats are legitimate", got)
	}
}
