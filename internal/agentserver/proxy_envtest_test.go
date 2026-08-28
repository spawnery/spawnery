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

// dialProxy opens a ProxySession the way a real Velocity agent would.
// It is dialAgent with a handful of lines changed — the stream types and the
// RPC name — and the two are kept apart rather than generified: the
// duplication is about twenty-five lines, but the alternative is a helper
// generic over both message-pair types for a dial function that never grows a
// third caller.
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

// A proxy agent connects and is told everything it needs before it is asked
// for anything. Every assertion is on what the client received.
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

// A registration made after the session is up reaches it.
// recvRegister reads past anything that is not a RegisterServer and returns
// the first one, or fails.
//
// It exists because the snapshot a proxy is sent on join grew a NetworkState
// in 7b-3, and three tests here read "the next message" and meant "the
// registration". What they assert is that a registration reaches a connected
// proxy, never that it arrives in a particular position -- the position is
// proxyreg's own business and its own test asserts it there.
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

// The rule the proto comment now states. A proxy reporting a real player count
// against its own limit must not be discarded.
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

	// The registry is the operator's side, so poll it rather than assert once.
	waitFor(t, func() bool {
		snap := f.agents.Lookup(string(pod.UID))
		return snap.Players == 7 && snap.Slots == 500
	})

	// The wire-side complement: an accepted report must not tear the stream
	// down, the same guarantee TestPlayerCountAboveSlotsIsDiscardedButKeepsTheStream
	// proves for ServerSession. A registration landing on this stream after
	// the report is what proves the handler loop is still running rather than
	// having returned — the registry check above cannot tell the two apart.
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
	// The registration surviving is what says the stream did: a session cut
	// for a rejected report would have ended before it arrived.
	if got := recvRegister(t, stream).GetServer().GetName(); got != "lobby-bbbb" {
		t.Errorf("received a RegisterServer for %q, want lobby-bbbb", got)
	}
}

// The contract the whole backpressure design in proxyreg rests on: a session
// that falls behind is cut loose rather than left to grow without bound.
// proxyreg's own tests prove the fan-out closes the outbox; this proves the
// closed outbox actually ends the gRPC stream a real proxy is holding.
//
// A small OutboxSize is what makes this deterministic, but not because it
// starves the stream's flow control — forty small RegisterServer messages
// are a few KB against a default window of 64KB, nowhere near enough to make
// the forwarding Send block. What actually overflows the queue is the gap
// between the two sides of it: f.proxies.Register is a pure in-memory
// broadcast under one mutex, so forty back-to-back calls enqueue about as
// fast as a Go channel send allows, while the only consumer draining the
// other end has to marshal and write each message to the wire before it can
// take the next one. Against a three-slot queue (OutboxSize 2 plus the
// initial FullSync), that consumer cannot keep up, and the client stalling
// its own receive after the setup messages just removes the one thing that
// would otherwise eventually slow the producer down to match.
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

	// The stall: no more Recv calls from here on. Comfortably more
	// registrations than the queue could ever hold even generously drained.
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

// The proxy twin of TestTheHardDeadlineClosesTheStream: the net under an
// agent that ignores renewAfter is the same one under a server or a proxy,
// because both sessions share the one hard-deadline timer sessionPrologue
// starts.
//
// Asserting the code, not just that the stream ended, is what makes this
// discriminating rather than a liveness check: ProxySession has two teardown
// codes now, and a deadline that fired but was misreported as
// ResourceExhausted — "fell behind" — would still pass a test that only
// checked Recv returned an error.
func TestTheHardDeadlineClosesAProxyStream(t *testing.T) {
	f := newServerFixtureWithDeadline(t, 300*time.Millisecond, 600*time.Millisecond)
	pod := f.proxyPod("gateway-eeee")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()

	// The three setup messages are what prove the session runs, and they
	// arrive before the deadline can have started counting.
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

// A renewal is not a proxy falling behind. sessions.enter cancels the
// displaced stream's context, and Fleet.Join closes its outbox a few lines
// later in the same call — both select cases in the displaced ProxySession
// can be ready by the time it gets back to the select, and without checking
// ctx.Err() first it could report ResourceExhausted ("fell behind") for a
// stream that was actually just superseded.
//
// This asserts the invariant the fix guarantees, not a reproduction of the
// race itself: ctx.Done() wins that race on essentially every run in
// practice (confirmed by reverting the ctx.Err() check locally and running
// this test dozens of times without a single failure), so the test cannot be
// relied on to catch a regression here on its own. It is still worth having —
// it is the documented contract, it is one of the three exit paths this round
// asked for coverage on, and a future change that made the race wider (for
// instance, real work between the two select cases) would start failing it.
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

	// The first stream is now superseded; it must end, and the code the
	// client sees must say so, not claim it fell behind.
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

	// The live stream must not have been torn down by the superseded one
	// leaving: a registration reaching it proves the handler loop is still
	// running.
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-gggg", Namespace: f.ns},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
		Status:     spawneryv1alpha1.ServerStatus{Address: "10.0.0.3:25565"},
	}
	if err := f.proxies.Register(f.ctx, srv); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Reaching the live stream is what says the superseded one leaving did
	// not take it down with it.
	if got := recvRegister(t, second).GetServer().GetName(); got != "lobby-gggg" {
		t.Errorf("received a RegisterServer for %q, want lobby-gggg", got)
	}
}

// TestABackendReportReachesTheRegistryUnderTheAuthenticatedNamespace is the
// end-to-end of the drain gap's operator half: a proxy says which backends its
// players are attached to, and the Server controller's own registry can answer
// for one of them.
//
// The namespace is the assertion worth making here rather than in a unit test.
// It comes from the authenticated identity and never from the message, which
// is the rule every other fact on this channel follows -- an agent may lie
// about itself and is believed about nothing else -- and only a real token
// against a real API server proves the operator takes it from the right place.
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

	// The registry is written from the receive loop, so poll rather than
	// assume the send has been applied by the time Send returned.
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

	// And it did not land under some other namespace's name, which is what a
	// report trusted about its own scope would have allowed.
	if n, _ := f.agents.AttachedTo("somewhere-else", "lobby-0", time.Time{}); n != 0 {
		t.Errorf("lobby-0 in another namespace = %d, want 0", n)
	}
	if n, _ := f.agents.AttachedTo(f.ns, "lobby-1", time.Time{}); n != 1 {
		t.Errorf("lobby-1 = %d, want 1", n)
	}
}

// A roster sent on the stream reaches the registry, keyed by the namespace the
// token authenticated rather than anything the message said. Modelled on
// TestABackendReportReachesTheRegistryUnderTheAuthenticatedNamespace, which is
// the same shape for the same reason.
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

	// The registry is written from the receive loop, so poll rather than
	// assume the send has been applied by the time Send returned.
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

	// And it did not land under some other namespace's name, which is what a
	// report trusted about its own scope would have allowed.
	if got, _ := f.agents.Roster("somewhere-else"); len(got) != 0 {
		t.Errorf("roster in another namespace = %+v, want none", got)
	}
}

// A connect request over a real stream, answered on the same stream.
//
// This is the wire, not the jar: nothing in either agent calls connect until a
// plugin does, and the first caller is the /cloud command a later milestone
// builds. What it proves is that a CloudRequest survives the channel and comes
// back correlated -- which no unit test on either side can see, because both
// are built from the same generated code and neither crosses a socket.
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
			// The id it minted, echoed. Without that a plugin's future would
			// never complete, and the failure would look like a timeout.
			if resp.GetId() != 7 {
				t.Fatalf("answered id %d, want the 7 the agent asked with", resp.GetId())
			}
			// NOT_FOUND, because no proxy has reported a roster: the player
			// does not exist on this network. That it is a refusal and not a
			// silence is the point.
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
