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
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/podspec"
)

// dialProxy is dialAgent for ProxySession, kept apart rather than generified for two callers.
func dialProxy(t *testing.T, ctx context.Context, addr string, ca []byte, token string) (
	grpc.BidiStreamingClient[agentpb.ProxyMessage, agentpb.OperatorToProxy], func()) {
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
	stream, err := agentpb.NewAgentServiceClient(conn).ProxySession(streamCtx)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("open ProxySession: %v", err)
	}
	return stream, func() { _ = conn.Close() }
}

func TestAProxyReceivesItsIntervalDeadlineAndFullSync(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-aaaa")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if first.GetReportInterval() == nil {
		t.Fatalf("first message = %+v, want a ReportInterval", first)
	}
	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if second.GetSessionDeadline() == nil {
		t.Fatalf("second message = %+v, want a SessionDeadline", second)
	}
	third, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if third.GetFullSync() == nil {
		t.Fatalf("third message = %+v, want a FullSync", third)
	}
}

// recvRegister skips anything before the first RegisterServer: a registration's position
// in the stream is proxyreg's business, asserted there.
func recvRegister(t *testing.T, stream interface {
	Recv() (*agentpb.OperatorToProxy, error)
}) *agentpb.RegisterServer {
	t.Helper()
	for i := 0; i < 5; i++ {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if r := msg.GetRegisterServer(); r != nil {
			return r
		}
	}
	t.Fatal("no RegisterServer in the first five messages")
	return nil
}

func TestARegistrationReachesAConnectedProxy(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-bbbb")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()

	for i := 0; i < 3; i++ {
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
	}

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-aaaa", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
		},
		Status: spawneryv1alpha1.ServerStatus{Address: "10.0.0.1:25565"},
	}
	if err := f.proxies.Register(f.ctx, srv); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := recvRegister(t, stream).GetServer().GetName(); got != "lobby-aaaa" {
		t.Errorf("received a RegisterServer for %q, want lobby-aaaa", got)
	}
}

func TestAProxyPlayerCountAgainstItsLimitIsAccepted(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-cccc")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	for i := 0; i < 3; i++ {
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
	}

	if err := stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_PlayerCount{
			PlayerCount: &agentpb.PlayerCount{Players: 7, Slots: 500},
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	waitFor(t, func() bool {
		snap := f.agents.Lookup(string(pod.UID))
		return snap.Players == 7 && snap.Slots == 500
	})

	// A registration arriving after the report proves the handler loop is still running;
	// the registry check above cannot tell that from a returned handler.
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-bbbb", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
		},
		Status: spawneryv1alpha1.ServerStatus{Address: "10.0.0.2:25565"},
	}
	if err := f.proxies.Register(f.ctx, srv); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := recvRegister(t, stream).GetServer().GetName(); got != "lobby-bbbb" {
		t.Errorf("received a RegisterServer for %q, want lobby-bbbb", got)
	}
}

// proxyreg's tests prove the fan-out closes the outbox; this proves the closed outbox
// ends the gRPC stream. The overflow is not flow control (forty small messages fit the
// 64KB window) but Register enqueuing faster than the consumer can marshal and send.
func TestAProxyThatFallsBehindIsDisconnected(t *testing.T) {
	f := newServerFixtureWithProxyOutbox(t, 2)
	pod := f.proxyPod("gateway-dddd")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()

	for i := 0; i < 3; i++ {
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
	}

	// The stall: no more Recv calls from here on.
	for i := 0; i < 40; i++ {
		srv := &spawneryv1alpha1.Server{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("flood-%02d", i), Namespace: f.ns},
			Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
			Status:     spawneryv1alpha1.ServerStatus{Address: "10.0.0.9:25565"},
		}
		if err := f.proxies.Register(f.ctx, srv); err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
	}

	done2 := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				done2 <- err
				return
			}
		}
	}()
	select {
	case err := <-done2:
		if code := status.Code(err); code != codes.ResourceExhausted {
			t.Errorf("code = %s, want ResourceExhausted", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a proxy that fell behind was never disconnected")
	}
}

// Asserts the code, not just the end: a deadline misreported as ResourceExhausted
// would still end the stream.
func TestTheHardDeadlineClosesAProxyStream(t *testing.T) {
	f := newServerFixtureWithDeadline(t, 300*time.Millisecond, 600*time.Millisecond)
	pod := f.proxyPod("gateway-eeee")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()

	// The setup messages arrive before the deadline starts counting.
	for i := 0; i < 3; i++ {
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
	}

	done2 := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				done2 <- err
				return
			}
		}
	}()
	select {
	case err := <-done2:
		if code := status.Code(err); code != codes.Unavailable {
			t.Errorf("code = %s, want Unavailable (a deadline, not a slow proxy)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the operator did not close the proxy stream at the hard deadline")
	}
}

// A superseded stream must not report ResourceExhausted: both select cases can be ready,
// so ProxySession checks ctx.Err() first. ctx.Done() wins that race almost always, so this
// pins the contract rather than reproducing the race.
func TestASecondProxyStreamSupersedesTheFirstWithoutMisreportingWhy(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-ffff")

	first, closeFirst := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeFirst()
	for i := 0; i < 3; i++ {
		if _, err := first.Recv(); err != nil {
			t.Fatalf("first Recv %d: %v", i, err)
		}
	}

	second, closeSecond := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeSecond()
	for i := 0; i < 3; i++ {
		if _, err := second.Recv(); err != nil {
			t.Fatalf("second Recv %d: %v", i, err)
		}
	}

	done := make(chan error, 1)
	go func() {
		for {
			if _, err := first.Recv(); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		if code := status.Code(err); code != codes.Unavailable {
			t.Errorf("code = %s, want Unavailable (a superseded session, not a slow one)", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the superseded proxy stream never ended")
	}

	// A registration reaching the live stream proves the superseded one did not take it down.
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-gggg", Namespace: f.ns},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
		Status:     spawneryv1alpha1.ServerStatus{Address: "10.0.0.3:25565"},
	}
	if err := f.proxies.Register(f.ctx, srv); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := recvRegister(t, second).GetServer().GetName(); got != "lobby-gggg" {
		t.Errorf("received a RegisterServer for %q, want lobby-gggg", got)
	}
}

// The namespace comes from the authenticated token, never the message; only a real token
// against a real API server proves the operator takes it from there.
func TestABackendReportReachesTheRegistryUnderTheAuthenticatedNamespace(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-aaaa")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the opening message never arrived: %v", err)
	}

	if err := stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_BackendPlayers{
			BackendPlayers: &agentpb.BackendPlayers{
				Players: map[string]int32{"lobby-0": 2, "lobby-1": 1},
			},
		},
	}); err != nil {
		t.Fatalf("send the backend report: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		n, stale := f.agents.AttachedTo(f.ns, "lobby-0", time.Time{})
		if n == 2 && !stale {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lobby-0 = %d stale=%v, want 2 and fresh", n, stale)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if n, _ := f.agents.AttachedTo("somewhere-else", "lobby-0", time.Time{}); n != 0 {
		t.Errorf("lobby-0 in another namespace = %d, want 0", n)
	}
	if n, _ := f.agents.AttachedTo(f.ns, "lobby-1", time.Time{}); n != 1 {
		t.Errorf("lobby-1 = %d, want 1", n)
	}
}

func TestAProxyRosterReachesTheRegistryUnderTheAuthenticatedNamespace(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-aaaa")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the opening message never arrived: %v", err)
	}

	if err := stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_PlayerRoster{
			PlayerRoster: &agentpb.PlayerRoster{
				Players: []*agentpb.RosterEntry{
					{Uuid: "u-alice", Name: "alice", Server: "lobby-0"},
				},
			},
		},
	}); err != nil {
		t.Fatalf("send the roster: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, stale := f.agents.Roster(f.ns)
		if len(got) == 1 && got[0].UUID == "u-alice" && !stale {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("roster = %+v stale=%v, want one fresh entry for alice", got, stale)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if got, _ := f.agents.Roster("somewhere-else"); len(got) != 0 {
		t.Errorf("roster in another namespace = %+v, want none", got)
	}
}

// Proves a CloudRequest crosses the socket and comes back correlated, which no unit
// test on either side can see: both are built from the same generated code.
func TestAConnectRequestIsAnsweredOnTheStreamThatAsked(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-aaaa")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the opening message never arrived: %v", err)
	}

	if err := stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_CloudRequest{
			CloudRequest: &agentpb.CloudRequest{
				Id: 7,
				Request: &agentpb.CloudRequest_Connect{
					Connect: &agentpb.ConnectRequest{
						PlayerUuid: "u-nobody",
						Target:     &agentpb.ConnectRequest_Server{Server: "lobby-0"},
					},
				},
			},
		},
	}); err != nil {
		t.Fatalf("send the request: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if resp := msg.GetCloudResponse(); resp != nil {
			// Without the echoed id a plugin's future never completes and looks like a timeout.
			if resp.GetId() != 7 {
				t.Fatalf("answered id %d, want the 7 the agent asked with", resp.GetId())
			}
			// No proxy has reported a roster, so the player is not on this network.
			if resp.GetError().GetReason() != agentpb.RequestError_NOT_FOUND {
				t.Fatalf("reason = %v, want NOT_FOUND for a player nobody reported",
					resp.GetError().GetReason())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no CloudResponse arrived within ten seconds")
		}
	}
}
