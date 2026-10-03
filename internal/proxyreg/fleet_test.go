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

package proxyreg_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/proxyreg"
)

const (
	ns    = "minecraft"
	group = "gateway"
)

// registered builds a Server with status.registered set, the flag the fan-out
// reads, plus an address to route to.
func registered(name, address string) *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
		},
		Status: spawneryv1alpha1.ServerStatus{
			Phase:      "Ready",
			Registered: true,
			Address:    address,
		},
	}
}

func proxyGroup(fallbacks ...string) *spawneryv1alpha1.ProxyGroup {
	return proxyGroupNamed(group, fallbacks...)
}

func proxyGroupNamed(name string, fallbacks ...string) *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Replicas:   1,
			Image:      "example/velocity:1",
			Expose:     spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort},
			Routing:    spawneryv1alpha1.RoutingSpec{FallbackGroups: fallbacks},
		},
	}
}

func newReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	// Without WithStatusSubresource, Status().Update on the fake client
	// silently writes nothing.
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).
		WithObjects(objects...).Build()
}

func newFleet(t *testing.T, objects ...client.Object) *proxyreg.Fleet {
	t.Helper()
	return proxyreg.New(proxyreg.Options{Reader: newReader(t, objects...)})
}

func newFleetWithOutboxSize(t *testing.T, size int, objects ...client.Object) *proxyreg.Fleet {
	t.Helper()
	return proxyreg.New(proxyreg.Options{Reader: newReader(t, objects...), OutboxSize: size})
}

// Every message this package produces is in the outbox before the call that
// produced it returns, so recv never waits.
func recv(t *testing.T, outbox <-chan *agentpb.OperatorToProxy) *agentpb.OperatorToProxy {
	t.Helper()
	select {
	case msg, ok := <-outbox:
		if !ok {
			t.Fatal("outbox closed")
		}
		return msg
	default:
		t.Fatal("outbox empty")
		return nil
	}
}

func drain(t *testing.T, outbox <-chan *agentpb.OperatorToProxy) []*agentpb.OperatorToProxy {
	t.Helper()
	var got []*agentpb.OperatorToProxy
	for {
		select {
		case msg, ok := <-outbox:
			if !ok {
				return got
			}
			got = append(got, msg)
		default:
			return got
		}
	}
}

func TestFullSyncIsTheFirstMessage(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"), registered("lobby-aaaa", "10.0.0.1:25565"))

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	sync := recv(t, outbox).GetFullSync()
	if sync == nil {
		t.Fatal("first message is not a FullSync")
	}
	if len(sync.GetServers()) != 1 {
		t.Fatalf("FullSync carries %d servers, want 1", len(sync.GetServers()))
	}
	got := sync.GetServers()[0]
	if got.GetName() != "lobby-aaaa" || got.GetAddress() != "10.0.0.1:25565" || got.GetGroup() != "lobby" {
		t.Errorf("server = %+v, want name/address/group lobby-aaaa, 10.0.0.1:25565, lobby", got)
	}
}

// The flag, not the phase, decides; they disagree for one reconcile after a
// deregistration.
func TestFullSyncOmitsUnregisteredAndAddresslessServers(t *testing.T) {
	unregistered := registered("lobby-bbbb", "10.0.0.2:25565")
	unregistered.Status.Registered = false
	addressless := registered("lobby-cccc", "")

	f := newFleet(t, proxyGroup("lobby"), unregistered, addressless)

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	if servers := recv(t, outbox).GetFullSync().GetServers(); len(servers) != 0 {
		t.Errorf("FullSync carries %d servers, want none", len(servers))
	}
}

// Draining servers are re-announced after every FullSync, or a proxy
// reconnecting mid-drain would undo the deregistration.
func TestJoinRepeatsTheDrainsAfterTheFullSync(t *testing.T) {
	draining := registered("lobby-dddd", "10.0.0.4:25565")
	draining.Status.Phase = "Draining"
	draining.Status.Registered = false

	f := newFleet(t, proxyGroup("lobby", "fallback"), draining)

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	if recv(t, outbox).GetFullSync() == nil {
		t.Fatal("first message is not a FullSync")
	}
	drain := recv(t, outbox).GetDrainPlayers()
	if drain == nil {
		t.Fatal("second message is not a DrainPlayers")
	}
	if drain.GetFromServer() != "lobby-dddd" {
		t.Errorf("fromServer = %q, want lobby-dddd", drain.GetFromServer())
	}
	if len(drain.GetToGroups()) != 2 || drain.GetToGroups()[0] != "lobby" {
		t.Errorf("toGroups = %v, want the ProxyGroup's fallback list", drain.GetToGroups())
	}
}

func TestRegisterReachesOnlyItsOwnNamespace(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	mine, leaveMine, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveMine()
	other, leaveOther, err := f.Join(context.Background(), "elsewhere", group, "uid-2")
	if err != nil {
		t.Fatalf("Join other: %v", err)
	}
	defer leaveOther()

	recv(t, mine)  // the FullSync
	recv(t, other) // the FullSync

	if err := f.Register(context.Background(), registered("lobby-eeee", "10.0.0.5:25565")); err != nil {
		t.Fatalf("Register: %v", err)
	}

	msg := recv(t, mine).GetRegisterServer()
	if msg == nil || msg.GetServer().GetName() != "lobby-eeee" {
		t.Fatalf("the session in the namespace got %+v", msg)
	}
	select {
	case msg := <-other:
		t.Fatalf("a session in another namespace received %+v", msg)
	default:
	}
}

func TestLeaveStopsDelivery(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	recv(t, outbox)
	leave()

	if err := f.Register(context.Background(), registered("lobby-ffff", "10.0.0.6:25565")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := <-outbox; ok {
		t.Error("a message reached a session that had left")
	}
}

// Make-before-break: the displaced session's leave must not remove its
// successor.
func TestASupersededSessionDoesNotRemoveItsSuccessor(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	_, leaveFirst, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("first Join: %v", err)
	}
	second, leaveSecond, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("second Join: %v", err)
	}
	defer leaveSecond()

	leaveFirst()
	recv(t, second) // the FullSync

	if err := f.Register(context.Background(), registered("lobby-gggg", "10.0.0.7:25565")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if recv(t, second).GetRegisterServer() == nil {
		t.Error("the successor session stopped receiving when its predecessor left")
	}
}

func TestRegisterWithNoSessionsIsNotAnError(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))
	if err := f.Register(context.Background(), registered("lobby-hhhh", "10.0.0.8:25565")); err != nil {
		t.Errorf("Register with no proxies connected: %v", err)
	}
}

// A superseding Join must close the predecessor's outbox: its own leave may run
// before the second Join.
func TestASupersedingJoinClosesThePredecessorsOutbox(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	first, _, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("first Join: %v", err)
	}
	recv(t, first) // the FullSync

	second, leaveSecond, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("second Join: %v", err)
	}
	defer leaveSecond()
	recv(t, second) // the FullSync

	if _, ok := <-first; ok {
		t.Error("the displaced session's outbox is still open")
	}
}

func TestResyncHealsARegistrationTheCacheHadNotSeen(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).
		WithObjects(proxyGroup("lobby")).Build()
	f := proxyreg.New(proxyreg.Options{Reader: reader})

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	if servers := recv(t, outbox).GetFullSync().GetServers(); len(servers) != 0 {
		t.Fatalf("FullSync carries %d servers, want none", len(servers))
	}

	late := registered("lobby-iiii", "10.0.0.9:25565")
	if err := reader.Create(context.Background(), late); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := reader.Status().Update(context.Background(), late); err != nil {
		t.Fatalf("status update: %v", err)
	}

	f.Resync(context.Background())

	sync := recv(t, outbox).GetFullSync()
	if sync == nil {
		t.Fatal("the resync did not send a FullSync")
	}
	if len(sync.GetServers()) != 1 || sync.GetServers()[0].GetName() != "lobby-iiii" {
		t.Errorf("resynced FullSync = %+v, want the late registration", sync.GetServers())
	}
}

// A superseded stream can reach Join after its successor did; its context is
// cancelled by then and it must leave the live session alone.
func TestJoinRefusesAnAlreadyCancelledContext(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	live, leaveLive, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveLive()
	recv(t, live) // the FullSync

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := f.Join(cancelled, ns, group, "uid-1"); err == nil {
		t.Fatal("Join with an already-cancelled context returned no error")
	}

	if err := f.Register(context.Background(), registered("lobby-zzzz", "10.0.0.20:25565")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if recv(t, live).GetRegisterServer() == nil {
		t.Error("the live session stopped receiving after a cancelled Join targeted its pod")
	}
}

func TestDrainSendsEachSessionItsOwnGroupsFallbacks(t *testing.T) {
	f := newFleet(t,
		proxyGroupNamed("alpha", "fallback-a"),
		proxyGroupNamed("beta", "fallback-b1", "fallback-b2"))

	alpha, leaveAlpha, err := f.Join(context.Background(), ns, "alpha", "uid-alpha")
	if err != nil {
		t.Fatalf("Join alpha: %v", err)
	}
	defer leaveAlpha()
	recv(t, alpha) // the FullSync

	beta, leaveBeta, err := f.Join(context.Background(), ns, "beta", "uid-beta")
	if err != nil {
		t.Fatalf("Join beta: %v", err)
	}
	defer leaveBeta()
	recv(t, beta) // the FullSync

	srv := registered("lobby-drain", "10.0.0.30:25565")
	srv.Status.Phase = "Draining"
	if err := f.Drain(context.Background(), srv); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	alphaDrain := recv(t, alpha).GetDrainPlayers()
	if alphaDrain == nil {
		t.Fatal("alpha's session did not receive a DrainPlayers")
	}
	if got := alphaDrain.GetToGroups(); len(got) != 1 || got[0] != "fallback-a" {
		t.Errorf("alpha's toGroups = %v, want [fallback-a]", got)
	}
	if alphaDrain.GetFromServer() != "lobby-drain" {
		t.Errorf("alpha's fromServer = %q, want lobby-drain", alphaDrain.GetFromServer())
	}

	betaDrain := recv(t, beta).GetDrainPlayers()
	if betaDrain == nil {
		t.Fatal("beta's session did not receive a DrainPlayers")
	}
	if got := betaDrain.GetToGroups(); len(got) != 2 || got[0] != "fallback-b1" || got[1] != "fallback-b2" {
		t.Errorf("beta's toGroups = %v, want [fallback-b1 fallback-b2] — "+
			"a union across the namespace would have leaked alpha's fallback in here", got)
	}
}

func TestDeregisterCarriesTheServerAndOnlyItsNamespace(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	mine, leaveMine, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveMine()
	other, leaveOther, err := f.Join(context.Background(), "elsewhere", group, "uid-2")
	if err != nil {
		t.Fatalf("Join other: %v", err)
	}
	defer leaveOther()
	recv(t, mine)  // the FullSync
	recv(t, other) // the FullSync

	if err := f.Deregister(context.Background(), registered("lobby-dereg", "10.0.0.40:25565")); err != nil {
		t.Fatalf("Deregister: %v", err)
	}

	msg := recv(t, mine).GetUnregisterServer()
	if msg == nil || msg.GetName() != "lobby-dereg" {
		t.Fatalf("the session in the namespace got %+v", msg)
	}
	select {
	case msg := <-other:
		t.Fatalf("a session in another namespace received %+v", msg)
	default:
	}
}

func TestAFullOutboxCutsTheSession(t *testing.T) {
	f := newFleetWithOutboxSize(t, 1, proxyGroup("lobby"))

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	// Capacity is OutboxSize+len(initial) = 2, and the FullSync already holds
	// one slot.
	if err := f.Register(context.Background(), registered("lobby-iiii", "10.0.0.9:25565")); err != nil {
		t.Fatalf("Register 1: %v", err)
	}
	if err := f.Register(context.Background(), registered("lobby-jjjj", "10.0.0.10:25565")); err != nil {
		t.Fatalf("Register 2: %v", err)
	}

	if recv(t, outbox).GetFullSync() == nil {
		t.Fatal("first message is not a FullSync")
	}
	if recv(t, outbox).GetRegisterServer() == nil {
		t.Fatal("second message is not the RegisterServer that fit")
	}

	if _, ok := <-outbox; ok {
		t.Error("the outbox was not closed after a message overflowed it")
	}
}

func TestSetReadyReachesOneSessionOnly(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	a, leaveA, err := f.Join(context.Background(), ns, group, "pod-a")
	if err != nil {
		t.Fatalf("Join a: %v", err)
	}
	defer leaveA()
	recv(t, a) // the FullSync

	b, leaveB, err := f.Join(context.Background(), ns, group, "pod-b")
	if err != nil {
		t.Fatalf("Join b: %v", err)
	}
	defer leaveB()
	recv(t, b) // the FullSync

	if err := f.SetReady(context.Background(), "pod-a", false); err != nil {
		t.Fatalf("SetReady: %v", err)
	}

	msg := recv(t, a)
	setReady := msg.GetSetReady()
	if setReady == nil {
		t.Fatalf("pod-a got %T, want a SetReady", msg.GetMessage())
	}
	if setReady.GetReady() {
		t.Error("pod-a was told ready=true, want false")
	}
	select {
	case msg := <-b:
		t.Fatalf("pod-b is a different proxy and must not be told anything, got %+v", msg)
	default:
	}
}

func TestSetReadyIsNotRepeatedForTheSameState(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	s, leave, err := f.Join(context.Background(), ns, group, "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	recv(t, s) // the FullSync

	for i := 0; i < 3; i++ {
		if err := f.SetReady(context.Background(), "pod-a", false); err != nil {
			t.Fatalf("SetReady %d: %v", i, err)
		}
	}

	if got := drain(t, s); len(got) != 1 {
		t.Errorf("sent %d messages for three identical assertions, want 1", len(got))
	}
}

func TestSetReadyIsReassertedOnANewStream(t *testing.T) {
	// The memo lives on the session, so a reconnect re-asserts an unchanged
	// value.
	f := newFleet(t, proxyGroup("lobby"))

	s, leave, err := f.Join(context.Background(), ns, group, "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	recv(t, s) // the FullSync
	if err := f.SetReady(context.Background(), "pod-a", false); err != nil {
		t.Fatalf("SetReady: %v", err)
	}
	drain(t, s)
	leave()

	s2, leave2, err := f.Join(context.Background(), ns, group, "pod-a")
	if err != nil {
		t.Fatalf("Join after reconnect: %v", err)
	}
	defer leave2()
	recv(t, s2) // the FullSync
	if err := f.SetReady(context.Background(), "pod-a", false); err != nil {
		t.Fatalf("SetReady after reconnect: %v", err)
	}
	if got := drain(t, s2); len(got) != 1 {
		t.Errorf("the new stream got %d messages, want the state re-asserted once", len(got))
	}
}

func TestSetReadyToAnUnknownPodIsNotAnError(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))
	if err := f.SetReady(context.Background(), "pod-gone", false); err != nil {
		t.Errorf("SetReady to an unknown pod = %v, want nil", err)
	}
}

// SetReady never repeats a value on one session, so Resync is what bounds a
// divergence between the agent's gate and the memo.
func TestResyncReassertsTheLastReadiness(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	s, leave, err := f.Join(context.Background(), ns, group, "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	recv(t, s) // the FullSync
	if err := f.SetReady(context.Background(), "pod-a", false); err != nil {
		t.Fatalf("SetReady: %v", err)
	}
	drain(t, s)

	f.Resync(context.Background())

	got := drain(t, s)
	if len(got) != 2 {
		t.Fatalf("the resync sent %d messages, want the FullSync and the readiness", len(got))
	}
	// A SetReady(true) ahead of the FullSync would open the gate of an agent
	// with no server list.
	if got[0].GetFullSync() == nil {
		t.Errorf("the resync's first message is %T, want the FullSync", got[0].GetMessage())
	}
	setReady := got[1].GetSetReady()
	if setReady == nil {
		t.Fatalf("the resync's second message is %T, want the readiness", got[1].GetMessage())
	}
	if setReady.GetReady() {
		t.Error("the resync re-asserted ready=true on a session last told false")
	}
}

func TestResyncAssertsNoReadinessOnASessionNeverTold(t *testing.T) {
	// A default sent here would be an assertion the operator never made.
	f := newFleet(t, proxyGroup("lobby"))

	s, leave, err := f.Join(context.Background(), ns, group, "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	drain(t, s)

	f.Resync(context.Background())

	got := drain(t, s)
	if len(got) != 1 || got[0].GetFullSync() == nil {
		t.Fatalf("the resync sent %d messages, want the FullSync alone", len(got))
	}
}

// The fallback list has three spellings:
// ProxyGroup.spec.routing.fallbackGroups, SPAWNERY_FALLBACK_GROUPS in the pod
// spec, and DrainPlayers.toGroups. A proxy resolving a join against one and a
// drain against another moves players onto a group it has no server for. The
// comparison lives here because podspec imports no other internal package.
func TestTheFallbackListHasOneSourceForJoinAndForDrain(t *testing.T) {
	// Unsorted and more than one: this is an ordered try-list.
	fallbacks := []string{"zulu", "alpha", "mike"}
	pg := proxyGroup(fallbacks...)
	f := newFleet(t, pg)

	session, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	recv(t, session) // the FullSync

	srv := registered("lobby-drain", "10.0.0.40:25565")
	srv.Status.Phase = "Draining"
	if err := f.Drain(context.Background(), srv); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	drainMsg := recv(t, session).GetDrainPlayers()
	if drainMsg == nil {
		t.Fatal("the session did not receive a DrainPlayers")
	}

	pod, err := podspec.BuildProxyPod(
		&spawneryv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns}},
		pg, group+"-0", "spawnery-operator.spawnery-system.svc:9443", nil)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	env, found := "", false
	for _, c := range pod.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == podspec.EnvFallbackGroups {
				env, found = e.Value, true
			}
		}
	}
	if !found {
		t.Fatalf("the proxy pod carries no %s at all", podspec.EnvFallbackGroups)
	}

	if want := strings.Join(drainMsg.GetToGroups(), ","); env != want {
		t.Errorf("%s = %q but DrainPlayers.toGroups = %v; a join and a drain would resolve "+
			"against different lists", podspec.EnvFallbackGroups, env, drainMsg.GetToGroups())
	}
	// Against the CRD too, so the two cannot agree by being equally wrong.
	if want := strings.Join(fallbacks, ","); env != want {
		t.Errorf("%s = %q, spec.routing.fallbackGroups = %v", podspec.EnvFallbackGroups, env, fallbacks)
	}
}

// The mirror comes after the FullSync: ProxyRole opens the readiness gate on
// the FullSync, and a DrainPlayers has to follow the list it names servers
// from.
func TestAJoiningProxyIsSentTheNetworkStateAfterItsFullSync(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	start := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	reader := newReader(t, proxyGroup(), registered("lobby-0", "10.0.0.1:25565"))
	f := proxyreg.New(proxyreg.Options{
		Reader: reader,
		State: netstate.Source{
			Reader: reader,
			Agents: agent.New(func() time.Time { return start }, 5*time.Second, start),
		},
	})

	outbox, leave, err := f.Join(context.Background(), ns, group, "proxy-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	first := <-outbox
	if first.GetFullSync() == nil {
		t.Fatalf("the first message was %T, want the FullSync", first.GetMessage())
	}

	var state *agentpb.NetworkState
	for len(outbox) > 0 {
		if s := (<-outbox).GetNetworkState(); s != nil {
			state = s
		}
	}
	if state == nil {
		t.Fatal("no NetworkState followed the FullSync")
	}
	if len(state.GetServers()) != 1 || state.GetServers()[0].GetName() != "lobby-0" {
		t.Errorf("servers = %v, want lobby-0", state.GetServers())
	}
}

func TestSendStateCarriesADoorToEveryProxyOfTheNamespace(t *testing.T) {
	start := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	lobby := registered("lobby-0", "10.0.0.1:25565")
	lobby.Status.PodUID = "pod-lobby"
	reader := newReader(t, proxyGroup(), lobby)
	agents := agent.New(func() time.Time { return start }, 5*time.Second, start)
	f := proxyreg.New(proxyreg.Options{Reader: reader, State: netstate.Source{Reader: reader, Agents: agents}})

	here, leaveHere, err := f.Join(context.Background(), ns, group, "proxy-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveHere()
	elsewhere, leaveElsewhere, err := f.Join(context.Background(), "other", group, "proxy-b")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveElsewhere()
	drain(t, here)
	drain(t, elsewhere)

	agents.Connect("pod-lobby", agent.RoleServer)
	if _, err := agents.ReportAcceptJoins("pod-lobby", ns, false, false); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}
	f.SendState(context.Background(), ns)

	got := drain(t, here)
	if len(got) != 1 || got[0].GetNetworkState() == nil {
		t.Fatalf("sent %v, want one NetworkState", got)
	}
	if servers := got[0].GetNetworkState().GetServers(); len(servers) != 1 || !servers[0].GetJoinsClosed() {
		t.Errorf("servers = %v, want lobby-0 with its door closed", servers)
	}
	if other := drain(t, elsewhere); len(other) != 0 {
		t.Errorf("a proxy of another namespace was sent %v", other)
	}
}

// A proxy's mirror is the whole namespace, private servers included: the plugin
// that asks for one runs on the proxy.
func TestAJoiningProxyIsSentPrivateServersAndTheirGroup(t *testing.T) {
	maxInstances := int32(300)
	private := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type: spawneryv1alpha1.ServerGroupOnDemand, MaxInstances: &maxInstances,
		},
	}
	member := registered("private-servers-c0ffee", "10.0.0.2:25565")
	member.Spec.GroupRef.Name = "private-servers"
	member.Spec.Key = "c0ffee"
	start := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	reader := newReader(t, proxyGroup(), private, member)
	f := proxyreg.New(proxyreg.Options{
		Reader: reader,
		State: netstate.Source{
			Reader: reader,
			Agents: agent.New(func() time.Time { return start }, 5*time.Second, start),
		},
	})

	outbox, leave, err := f.Join(context.Background(), ns, group, "proxy-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	var state *agentpb.NetworkState
	for len(outbox) > 0 {
		if s := (<-outbox).GetNetworkState(); s != nil {
			state = s
		}
	}
	if state == nil {
		t.Fatal("no NetworkState was sent")
	}
	if len(state.GetServers()) != 1 || state.GetServers()[0].GetName() != "private-servers-c0ffee" {
		t.Errorf("servers = %v, want the private server", state.GetServers())
	}
	var kind agentpb.GroupState_Kind
	for _, g := range state.GetGroups() {
		if g.GetName() == "private-servers" {
			kind = g.GetKind()
		}
	}
	if kind != agentpb.GroupState_ON_DEMAND {
		t.Errorf("private-servers kind = %v, want ON_DEMAND (groups: %v)", kind, state.GetGroups())
	}
}

// Tested on its own rather than assumed from serverreg's: the two fan-outs
// carry different session structs and message types.
func proxyCloudEventsIn(ch <-chan *agentpb.OperatorToProxy) []*agentpb.CloudEvent {
	var got []*agentpb.CloudEvent
	deadline := time.After(250 * time.Millisecond)
	for {
		select {
		case msg := <-ch:
			if ev := msg.GetCloudEvent(); ev != nil {
				got = append(got, ev)
			}
		case <-deadline:
			return got
		}
	}
}

func TestAnEventReachesOnlyTheProxySessionsThatWantOne(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))
	watching, leaveA, err := f.Join(context.Background(), ns, group, "uid-watching")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveA()
	quiet, leaveB, err := f.Join(context.Background(), ns, group, "uid-quiet")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveB()
	f.SetInterest("uid-watching", true)

	f.Publish(ns, &agentpb.CloudEvent{Kind: "ReadyGatePassed", Subject: "lobby-a"})

	if got := proxyCloudEventsIn(watching); len(got) != 1 || got[0].GetSubject() != "lobby-a" {
		t.Errorf("the interested session got %+v, want one event for lobby-a", got)
	}
	if got := proxyCloudEventsIn(quiet); len(got) != 0 {
		t.Errorf("a session that never asked for events received %+v", got)
	}
}

func TestProxyInterestIsForgottenWithTheSession(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))
	_, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	f.SetInterest("uid-1", true)
	leave()

	if f.Interested("uid-1") {
		t.Error("interest outlived the session that declared it")
	}
}

func TestAnEventDoesNotCrossANamespaceOnTheProxySide(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))
	other, leave, err := f.Join(context.Background(), "somewhere-else", group, "uid-other")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	f.SetInterest("uid-other", true)

	f.Publish(ns, &agentpb.CloudEvent{Kind: "ReadyGatePassed", Subject: "lobby-a"})

	if got := proxyCloudEventsIn(other); len(got) != 0 {
		t.Errorf("an event reached a session in another namespace: %+v", got)
	}
}

func TestProxyInterestForAPodWithNoSessionIsIgnored(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	f.SetInterest("uid-that-never-joined", true)

	if f.Interested("uid-that-never-joined") {
		t.Error("interest was recorded for a pod with no session")
	}
}
