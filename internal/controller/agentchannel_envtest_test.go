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

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/proxyreg"
	"github.com/spawnery/spawnery/internal/serverreg"
	"github.com/spawnery/spawnery/internal/testenv"
)

// The certificate is issued for the service the operator runs behind in a real
// cluster, not for the throwaway namespace this test runs in: an agent pins
// exactly these names, so the fixture has to serve them.
const (
	channelService          = "spawnery-operator"
	channelServiceNamespace = "spawnery-system"
	channelServerName       = channelService + "." + channelServiceNamespace + ".svc"
)

// channelFixture is the whole agent channel in one namespace: certificates,
// the authenticator, the gRPC service and a Server reconciler whose
// Bootstrapper hands out the very CA the service is serving.
type channelFixture struct {
	*fixture
	cs   *kubernetes.Clientset
	addr string
	ca   []byte
}

func newChannelFixture(t *testing.T) *channelFixture {
	t.Helper()
	base := newFixture(t)

	cs, err := kubernetes.NewForConfig(testenv.Config(t))
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	// The certificates run on the wall clock, not the fixture's frozen one:
	// the TLS stack on both ends checks validity against real time.
	store := &certs.Store{
		Client:    base.c,
		Namespace: base.ns,
		Name:      certs.SecretName,
		DNSNames:  certs.ServingDNSNames(channelService, channelServiceNamespace),
		Clock:     time.Now,
	}
	provider := certs.NewProvider(store)
	bundle, err := store.Ensure(base.ctx)
	if err != nil {
		t.Fatalf("ensure the TLS bundle: %v", err)
	}
	if err := provider.Set(bundle); err != nil {
		t.Fatalf("publish the TLS bundle: %v", err)
	}

	// The same CA the service serves is the one the bootstrap writes into the
	// namespace, exactly as main.go wires it.
	base.reconc.Bootstrap = &Bootstrapper{Client: base.c, Reader: base.c, CA: provider.CABundle}

	srv := agentserver.New(agentserver.Options{
		// Port 0: the kernel picks a free one, so parallel packages do not
		// collide.
		Addr:     "127.0.0.1:0",
		Provider: provider,
		Auth: &grpcauth.Authenticator{
			Reviews:  cs.AuthenticationV1().TokenReviews(),
			Pods:     &grpcauth.ClientPodChecker{Client: base.c},
			Audience: podspec.AgentTokenAudience,
		},
		Agents:  base.agents,
		Proxies: proxyreg.New(proxyreg.Options{Reader: base.c}),
		// The backend side's fan-out, which ServerSession joins. This fixture
		// found the wiring on its own: agentserver.New panics without one, and
		// that panic is what this line answers rather than a compile error.
		Servers: serverreg.New(serverreg.Options{
			State: netstate.Source{Reader: base.c, Agents: base.agents},
		}),
		State:          netstate.Source{Reader: base.c, Agents: base.agents},
		Writer:         agentserver.KubeWriter{Client: base.c},
		ReportInterval: 5 * time.Second,
		RenewAfter:     8 * time.Minute,
		HardDeadline:   10 * time.Minute,
		Clock:          time.Now,
	})

	serverCtx, cancel := context.WithCancel(base.ctx)
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

	return &channelFixture{fixture: base, cs: cs, addr: srv.Addr(), ca: provider.CABundle()}
}

type channelStream = grpc.BidiStreamingClient[agentpb.ServerMessage, agentpb.OperatorToServer]

// dialAgentFor opens a ServerSession the way the agent in that pod would: a
// freshly minted, audience-bound, pod-bound token over TLS against the pinned
// CA. Nothing here is a stand-in for the real path.
func (f *channelFixture) dialAgentFor(pod *corev1.Pod) (channelStream, func()) {
	f.t.Helper()

	tr, err := f.cs.CoreV1().ServiceAccounts(f.ns).CreateToken(f.ctx,
		podspec.ServerServiceAccountName,
		&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{
			Audiences:         []string{podspec.AgentTokenAudience},
			ExpirationSeconds: ptr.To(int64(600)),
			BoundObjectRef: &authnv1.BoundObjectReference{
				Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID,
			},
		}}, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatalf("TokenRequest for %s: %v", pod.Name, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.ca) {
		f.t.Fatal("CA bundle unusable")
	}
	conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{
			RootCAs:    pool,
			ServerName: channelServerName,
			MinVersion: tls.VersionTLS13,
		})))
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	streamCtx := metadata.AppendToOutgoingContext(f.ctx, "authorization", "Bearer "+tr.Status.Token)
	stream, err := agentpb.NewAgentServiceClient(conn).ServerSession(streamCtx)
	if err != nil {
		_ = conn.Close()
		f.t.Fatalf("open ServerSession: %v", err)
	}
	return stream, func() { _ = conn.Close() }
}

func (f *channelFixture) sendHelloReady(stream channelStream) {
	f.t.Helper()
	if err := stream.Send(&agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_Hello{Hello: &agentpb.Hello{Version: "0.1.0", Ready: true}},
	}); err != nil {
		f.t.Fatalf("send Hello: %v", err)
	}
}

func (f *channelFixture) sendPlayerCount(stream channelStream, players, slots int32) {
	f.t.Helper()
	if err := stream.Send(&agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_PlayerCount{
			PlayerCount: &agentpb.PlayerCount{Players: players, Slots: slots},
		},
	}); err != nil {
		f.t.Fatalf("send PlayerCount: %v", err)
	}
}

// waitForRegistry blocks until the operator has processed what the agent sent.
// The messages travel a real connection and are handled on another goroutine,
// so the reconcile that follows has to wait for them rather than assume them.
func (f *channelFixture) waitForRegistry(uid string) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap := f.agents.Lookup(uid)
		if snap.Connected && snap.Ready && snap.Slots > 0 {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the registry never saw the agent of %s: %+v", uid, snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// This is the milestone in one test: no test may call registry.MarkReady
// here. The only path to Ready is a real agent over a real TLS connection.
func TestAgentOverTheWireBringsAServerToReady(t *testing.T) {
	f := newChannelFixture(t)

	srv := f.createServer("lobby-abcd")
	f.reconcile(srv.Name)

	pod, ok := f.pod(srv.Name)
	if !ok {
		t.Fatal("no pod was created")
	}
	f.setPodRunning(srv.Name, true)

	stream, closeConn := f.dialAgentFor(pod)
	defer closeConn()
	f.sendHelloReady(stream)
	f.sendPlayerCount(stream, 12, 100)
	f.waitForRegistry(string(pod.UID))

	f.reconcile(srv.Name)
	f.reconcile(srv.Name)

	got := f.server(srv.Name)
	if got.Status.Phase != string(phase.Ready) {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.Players != 12 || got.Status.Slots != 100 {
		t.Errorf("status = %d/%d, want 12/100", got.Status.Players, got.Status.Slots)
	}
}

// The bootstrap has to have run before the pod exists, or the kubelet would
// fail to mount a ConfigMap that is not there.
//
// Every assertion here has to be able to tell ServerReconciler's own Ensure
// apart from the fixture's. newFixture reconciles the Network before this test
// body starts, and that reconcile now bootstraps the namespace too, so a test
// that only asked whether the objects exist would stay green with the call it
// guards deleted outright. Two things separate the two callers: the fixture
// bootstraps with the literal "test-ca" while newChannelFixture rewires only
// the ServerReconciler's Bootstrapper to the bundle the gRPC service really
// serves, so the stored ca.crt names which of them wrote it; and the
// ServiceAccounts, which Ensure never updates, are deleted below so that only
// a Create in this reconcile can bring them back.
func TestReconcileBootstrapsTheNamespaceBeforeCreatingThePod(t *testing.T) {
	f := newChannelFixture(t)

	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		if err := f.c.Delete(f.ctx, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		}); err != nil {
			t.Fatalf("delete the fixture's %s ServiceAccount: %v", name, err)
		}
	}

	srv := f.createServer("lobby-abcd")
	f.reconcile(srv.Name)

	cm := &corev1.ConfigMap{}
	if err := f.c.Get(f.ctx, types.NamespacedName{
		Name: podspec.CAConfigMapName, Namespace: f.ns,
	}, cm); err != nil {
		t.Fatalf("the CA ConfigMap is missing although the pod exists: %v", err)
	}
	if got := cm.Data[podspec.CAConfigMapKey]; got != string(f.ca) {
		t.Errorf("ca.crt = %q, want the bundle the agent service serves. Only the "+
			"Bootstrapper this reconciler holds returns that bundle, so anything "+
			"else here means the fixture's Network reconcile wrote the ConfigMap "+
			"and this reconcile did not", got)
	}
	sa := &corev1.ServiceAccount{}
	if err := f.c.Get(f.ctx, types.NamespacedName{
		Name: podspec.ServerServiceAccountName, Namespace: f.ns,
	}, sa); err != nil {
		t.Fatalf("the ServiceAccount is missing: %v", err)
	}
}

// The other half of the ordering rule: as long as the bootstrap cannot run —
// which is the state of every operator between process start and the moment
// the leader has published a CA — no pod may be created at all. A pod started
// against an empty or missing ca.crt does not wait for one; it comes up and
// fails its handshake, and the operator would have to time it out.
func TestReconcileCreatesNoPodWhileTheCAIsMissing(t *testing.T) {
	f := newChannelFixture(t)

	// newChannelFixture's own newFixture(t) already ran a Network reconcile
	// that bootstrapped this namespace with a real CA (that is this task's
	// whole point), so the ConfigMap this test checks for absence is already
	// there. Remove it, or the assertion below observes the fixture's setup
	// rather than what this reconcile does with an empty CA.
	if err := f.c.Delete(f.ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: podspec.CAConfigMapName, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("delete the fixture's CA ConfigMap: %v", err)
	}

	f.reconc.Bootstrap = &Bootstrapper{Client: f.rc, Reader: f.rc, CA: func() []byte { return nil }}

	srv := f.createServer("lobby-abcd")
	f.reconcile(srv.Name)

	if _, ok := f.pod(srv.Name); ok {
		t.Fatal("a pod was created although no CA was available to mount")
	}
	cm := &corev1.ConfigMap{}
	err := f.c.Get(f.ctx, types.NamespacedName{
		Name: podspec.CAConfigMapName, Namespace: f.ns,
	}, cm)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get ConfigMap = %v, want NotFound: an empty CA must never be written", err)
	}

	// And it recovers by itself once the provider has one — no restart, no
	// manual step.
	f.reconc.Bootstrap = &Bootstrapper{Client: f.rc, Reader: f.rc, CA: func() []byte { return f.ca }}
	f.reconcile(srv.Name)
	if _, ok := f.pod(srv.Name); !ok {
		t.Fatal("no pod was created after the CA became available")
	}
}
