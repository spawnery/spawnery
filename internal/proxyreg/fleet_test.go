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

package proxyreg_test

import (
	"context"
	"strings"
	"testing"
	"time"

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

// registered builds a Server in the state the fan-out cares about: the flag
// applyDecision writes next to its Register call, plus an address to route to.
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

// proxyGroupNamed is proxyGroup with the group name broken out, for tests that
// need two distinct ProxyGroups in one namespace — proxyGroup alone can only
// ever produce one, since it hardcodes the package-level group constant.
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
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	// WithStatusSubresource, or Status().Update on the fake client silently
	// writes nothing and the resync test would pass for the wrong reason.
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).
		WithObjects(objects...).Build()
}

func newFleet(t *testing.T, objects ...client.Object) *proxyreg.Fleet {
	t.Helper()
	return proxyreg.New(proxyreg.Options{Reader: newReader(t, objects...)})
}

// newFleetWithOutboxSize is for the overflow test: it needs a queue depth
// small enough to reach with a handful of Registers rather than thousands.
func newFleetWithOutboxSize(t *testing.T, size int, objects ...client.Object) *proxyreg.Fleet {
	t.Helper()
	return proxyreg.New(proxyreg.Options{Reader: newReader(t, objects...), OutboxSize: size})
}

// recv takes one message or fails. Nothing here blocks in practice: every
// message this package produces is already in the outbox before the call that
// produced it returns.
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

// drain returns every message currently queued on outbox without blocking for
// more. It is for tests that assert on how many messages went out rather than
// on the shape of just one.
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

// A server the operator never told the proxies about must not appear, and the
// flag is what records that — not the phase. The two disagree for exactly one
// reconcile after a deregistration, and in that window the flag is the one
// that matches what was sent.
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

// Section 5.2 of the main design: after every FullSync the draining servers are
// re-announced, or a proxy reconnecting mid-drain would undo the
// deregistration and start sending players back.
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

// Make-before-break: the agent opens its next stream before the current one
// ends, so two Joins for one pod overlap. The displaced session's leave must
// not remove the fresh one — the same hazard sessions.leave guards against on
// the gRPC side, for the same reason.
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

// The other half of make-before-break: a second Join for a pod that already
// has a live session must close the first session's outbox, not just stop
// delivering to it. Join's own doc comment promises that a closed channel
// means "end your stream" — a consumer that never sees the close would range
// over the channel forever. Leaving the close to the displaced session's own
// leave call would work only if that call happens to run after the second
// Join, which make-before-break does not guarantee.
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

// The scenario the resync exists for, played out exactly: a registration is
// broadcast while the reader still shows the old world, the session's FullSync
// is therefore built without it, and nothing else would ever correct that.
func TestResyncHealsARegistrationTheCacheHadNotSeen(t *testing.T) {
	scheme := runtime.NewScheme()
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

	// The world moves on without the session having been told.
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

// The cross-session race described on Join's own doc comment: a superseded
// stream can still reach Join after its successor already has, because
// sessions.enter's cancellation in agentserver cannot interrupt the blocking
// stream.Send calls sessionPrologue makes before either stream gets here.
// Driving that actual interleaving is not practical from this package — it
// depends on two goroutines racing inside agentserver's gRPC handlers — but
// the contract Join promises is exercisable directly: a caller whose context
// is already cancelled must leave whatever session is currently registered
// for that pod alone, rather than tearing it down and installing nothing
// useful in its place.
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

	// The live session must be exactly as it was: still registered for this
	// pod, and its outbox still open. A Register reaching it proves both —
	// Join's ctx.Err() guard did not close it out from under a caller who was
	// never told the session was gone.
	if err := f.Register(context.Background(), registered("lobby-zzzz", "10.0.0.20:25565")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if recv(t, live).GetRegisterServer() == nil {
		t.Error("the live session stopped receiving after a cancelled Join targeted its pod")
	}
}

// This is the test that proves fallbacks are per-ProxyGroup rather than a
// union across the namespace: two sessions in different groups, with
// different fallback lists, must each see only their own group's list on a
// Drain. A union bug would show up here as beta's session receiving alpha's
// fallback too.
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

// Deregister is otherwise untested in this package: it must carry the right
// server name, and it must reach only sessions in that server's namespace —
// the same scoping TestRegisterReachesOnlyItsOwnNamespace proves for
// Register.
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

// A full queue cuts the session instead of dropping the message: dropping
// would leave a proxy routing on a list it has no way of knowing is wrong,
// looking healthy the whole time, while a closed stream is something the
// agent already knows how to recover from.
func TestAFullOutboxCutsTheSession(t *testing.T) {
	f := newFleetWithOutboxSize(t, 1, proxyGroup("lobby"))

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	// The queue's capacity is OutboxSize+len(initial) = 1+1 = 2, and it already
	// holds the FullSync from Join. The first Register fills the remaining
	// slot; the second finds the queue full and cuts the session rather than
	// dropping the message.
	if err := f.Register(context.Background(), registered("lobby-iiii", "10.0.0.9:25565")); err != nil {
		t.Fatalf("Register 1: %v", err)
	}
	if err := f.Register(context.Background(), registered("lobby-jjjj", "10.0.0.10:25565")); err != nil {
		t.Fatalf("Register 2: %v", err)
	}

	// Drain what made it into the queue before the cut: the FullSync and the
	// one RegisterServer that fit.
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

// SetReady is unlike Register/Deregister/Drain: it targets one pod rather than
// broadcasting to a namespace, and it must not repeat an assertion the session
// already carries — the operator calls it on every five-second reconcile
// whether or not the desired state changed.

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
	// The operator asserts the desired state on every reconcile, five seconds
	// apart. Without the memo a draining proxy would be told the same thing
	// for the whole of its drain.
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
	// The memo lives on the session, so a reconnect starts without one. That
	// is what makes the state survive a stream that broke mid-drain: the
	// operator's next assertion lands even though the value has not changed.
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
	// A proxy whose stream has gone is a proxy that is not taking connections
	// either. The reconcile that asked must not fail because of it — it will
	// assert again on the next pass if the pod comes back.
	f := newFleet(t, proxyGroup("lobby"))
	if err := f.SetReady(context.Background(), "pod-gone", false); err != nil {
		t.Errorf("SetReady to an unknown pod = %v, want nil", err)
	}
}

// The memo means SetReady itself never repeats a value on one session, so the
// resync is the only thing that ever re-states it — and re-stating it is what
// bounds a disagreement between the agent's readiness gate and what the
// operator believes it asserted to one interval instead of to the life of the
// session. The expensive direction of such a disagreement is a pod left Ready
// while its drain deadline runs: it collects players who are then disconnected
// when it is deleted.
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
	// The order is asserted, not just the pair. A SetReady(true) ahead of the
	// FullSync would open the gate of an agent that has no server list, and
	// every player routed there is disconnected with "no available server".
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
	// The operator has no readiness for a pod it has not decided about, and a
	// default sent here would be an assertion it never made — ready=true would
	// put a proxy into the Service's endpoints on a resync tick alone.
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

// The fallback list has three spellings: the CRD field
// ProxyGroup.spec.routing.fallbackGroups, the SPAWNERY_FALLBACK_GROUPS the pod
// spec hands the agent at start, and the DrainPlayers.toGroups the operator
// sends over the channel while a server empties. A proxy that resolves a join
// against one and a drain against the other moves players onto a group it has
// no server for.
//
// They cannot disagree today — internal/podspec.BuildProxyPod and Fleet's own
// fallbacks() both read Spec.Routing.FallbackGroups — and nothing pinned that.
// This is the only place the two are compared; it lives here rather than in
// internal/podspec because podspec deliberately imports no other internal
// package, and the comparison has to happen on the side that can see both.
func TestTheFallbackListHasOneSourceForJoinAndForDrain(t *testing.T) {
	// More than one entry, and deliberately not in sorted order: a single
	// element would pass whether or not order survived, and this is an
	// ordered try-list.
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

// The mirror reaches a proxy, and it reaches it last.
//
// The position is the assertion. ProxyRole opens the pod's readiness gate on
// the FullSync, so a message ahead of that one would be applied by an agent
// that is not yet routable -- and a DrainPlayers has to follow the list it
// names servers from.
func TestAJoiningProxyIsSentTheNetworkStateAfterItsFullSync(t *testing.T) {
	scheme := runtime.NewScheme()
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
