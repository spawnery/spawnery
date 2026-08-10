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
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/podspec"
)

// dialProxy opens a ProxySession the way a real Velocity agent would.
// It is dialAgent with one line changed; the two are kept apart rather than
// generified because the stream types differ and the duplication is four
// lines against an abstraction nobody would read.
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
		conn.Close()
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

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got := msg.GetRegisterServer().GetServer().GetName(); got != "lobby-aaaa" {
		t.Errorf("received %+v, want a RegisterServer for lobby-aaaa", msg)
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		if snap := f.agents.Lookup(string(pod.UID)); snap.Players == 7 && snap.Slots == 500 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the report never landed: %+v", f.agents.Lookup(string(pod.UID)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
