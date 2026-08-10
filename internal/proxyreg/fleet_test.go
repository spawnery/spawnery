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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
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
	return &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: group, Namespace: ns},
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
