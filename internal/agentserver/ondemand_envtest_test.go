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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// One test per bound, each asserting which one fired, and each reading the
// cluster back afterwards where the verb writes: an answer is not evidence
// that anything was created or deleted, and the two failure modes -- saying
// yes without writing, and writing without saying so -- are both invisible to
// a test that only inspects the response.

// askOverTheWire asks on a real proxy stream and returns the answer.
//
// A proxy session and not a server one: the plugin that starts and stops
// private servers runs on a proxy, so that is where these requests come from.
// The request's id is this helper's, because every caller wants the same
// thing from it -- the answer to the ask it just made.
func askOverTheWire(
	t *testing.T, f *serverFixture, pod *corev1.Pod, req *agentpb.CloudRequest,
) *agentpb.CloudResponse {
	t.Helper()
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the opening message never arrived: %v", err)
	}

	req.Id = 23
	if err := stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_CloudRequest{CloudRequest: req},
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
			if resp.GetId() != req.GetId() {
				t.Fatalf("answered id %d, want the %d the agent asked with", resp.GetId(), req.GetId())
			}
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatal("no CloudResponse arrived within ten seconds")
		}
	}
}

func startOverTheWire(
	t *testing.T, f *serverFixture, pod *corev1.Pod, group, key string,
) *agentpb.CloudResponse {
	t.Helper()
	return askOverTheWire(t, f, pod, &agentpb.CloudRequest{
		Request: &agentpb.CloudRequest_StartServer{
			StartServer: &agentpb.StartServerRequest{Group: group, Key: key},
		},
	})
}

func stopOverTheWire(
	t *testing.T, f *serverFixture, pod *corev1.Pod, server string,
) *agentpb.CloudResponse {
	t.Helper()
	return askOverTheWire(t, f, pod, &agentpb.CloudRequest{
		Request: &agentpb.CloudRequest_StopServer{
			StopServer: &agentpb.StopServerRequest{Server: server},
		},
	})
}

func makeOnDemandGroup(t *testing.T, f *serverFixture, name string, maxInstances int32) {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:   spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:         spawneryv1alpha1.ServerGroupOnDemand,
			Image:        "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:   10,
			MaxInstances: ptr.To(maxInstances),
			Storage:      &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("2Gi")},
		},
	}
	if err := f.c.Create(f.ctx, g); err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
}

func makeEphemeralGroup(t *testing.T, f *serverFixture, name string) {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 10,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 3},
		},
	}
	if err := f.c.Create(f.ctx, g); err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
}

func member(t *testing.T, f *serverFixture, name string) *spawneryv1alpha1.Server {
	t.Helper()
	var srv spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: name}, &srv); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return &srv
}

func setPhase(t *testing.T, f *serverFixture, srv *spawneryv1alpha1.Server, p phase.Phase) {
	t.Helper()
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Status.Phase = string(p)
	if err := f.c.Status().Patch(f.ctx, srv, patch); err != nil {
		t.Fatalf("set the phase of %s: %v", srv.Name, err)
	}
}

// holdWhileStopping puts the drain finalizer on a member, so that a stop
// leaves the object behind with a deletion timestamp instead of removing it
// at once.
//
// That is what a stop does in a live cluster -- the Server controller holds
// the object until the players on it have been moved -- and envtest runs no
// controller, so without this the whole stopping window is a state these
// tests could never reach. The finalizer is taken off again at cleanup, or
// the namespace would never finish deleting.
func holdWhileStopping(t *testing.T, f *serverFixture, name string) {
	t.Helper()
	srv := member(t, f, name)
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Finalizers = append(srv.Finalizers, controller.ServerFinalizer)
	if err := f.c.Patch(f.ctx, srv, patch); err != nil {
		t.Fatalf("hold %s: %v", name, err)
	}
	t.Cleanup(func() {
		var held spawneryv1alpha1.Server
		if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: name}, &held); err != nil {
			return
		}
		patch := client.MergeFrom(held.DeepCopy())
		held.Finalizers = nil
		_ = f.c.Patch(f.ctx, &held, patch)
	})
}

func TestStartCreatesTheMemberAndEchoesItsName(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")

	resp := startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if resp.GetError() != nil {
		t.Fatalf("refused: %s", resp.GetError().GetMessage())
	}
	if got := resp.GetStartServer().GetServer(); got != "private-servers-c0ffee" {
		t.Fatalf("server = %q", got)
	}
	if resp.GetStartServer().GetAlreadyRunning() {
		t.Fatal("already_running on the first start")
	}

	// An answer is not evidence that anything was written.
	srv := member(t, f, "private-servers-c0ffee")
	if srv.Spec.Key != "c0ffee" {
		t.Errorf("spec.key = %q, want c0ffee", srv.Spec.Key)
	}
	if srv.Spec.GroupRef.Name != "private-servers" {
		t.Errorf("spec.groupRef = %q", srv.Spec.GroupRef.Name)
	}
	// Owned by its group, so deleting the group takes its members with it.
	if len(srv.OwnerReferences) != 1 || srv.OwnerReferences[0].Kind != "ServerGroup" {
		t.Errorf("ownerReferences = %+v, want the group", srv.OwnerReferences)
	}
}

func TestStartTwiceIsAnAnswerAndNotASecondServer(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")

	startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	resp := startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if resp.GetError() != nil {
		t.Fatalf("the second start was refused: %s", resp.GetError().GetMessage())
	}
	if !resp.GetStartServer().GetAlreadyRunning() {
		t.Fatal("already_running is false on the second start")
	}

	var servers spawneryv1alpha1.ServerList
	if err := f.c.List(f.ctx, &servers, client.InNamespace(f.ns)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(servers.Items) != 1 {
		t.Fatalf("%d servers exist, want 1", len(servers.Items))
	}
}

func TestStartRefusesPastTheCeiling(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 1)
	pod := f.proxyPod("gateway-aaaa")

	startOverTheWire(t, f, pod, "private-servers", "one")
	resp := startOverTheWire(t, f, pod, "private-servers", "two")
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
	// And the refusal is a refusal: nothing was created past the ceiling.
	var srv spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "private-servers-two"},
		&srv); err == nil {
		t.Fatal("the member past the ceiling was created anyway")
	}
}

// A member whose run is over holds no slot. Counting it would make a group
// drift closed as its players' servers ended, and the owner of the next key
// would be refused by servers nobody is on.
func TestAMemberWhoseRunIsOverDoesNotHoldASlot(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 1)
	pod := f.proxyPod("gateway-aaaa")

	startOverTheWire(t, f, pod, "private-servers", "one")
	setPhase(t, f, member(t, f, "private-servers-one"), phase.Finished)

	resp := startOverTheWire(t, f, pod, "private-servers", "two")
	if resp.GetError() != nil {
		t.Fatalf("refused while the only other member was Finished: %s", resp.GetError().GetMessage())
	}
	member(t, f, "private-servers-two")
}

// The same key again after its run ended starts a fresh member rather than
// reporting the corpse: its world is on the claim, and refusing here would
// leave the owner waiting out a retention they cannot see.
func TestStartReplacesAMemberWhoseRunIsOver(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")

	startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	first := member(t, f, "private-servers-c0ffee")
	setPhase(t, f, first, phase.Failed)

	resp := startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if resp.GetError() != nil {
		t.Fatalf("refused: %s", resp.GetError().GetMessage())
	}
	if resp.GetStartServer().GetAlreadyRunning() {
		t.Fatal("already_running for a member whose run was over")
	}
	if second := member(t, f, "private-servers-c0ffee"); second.UID == first.UID {
		t.Fatal("the failed member was reported rather than replaced")
	}
}

// Ruling: a member carrying a deletion timestamp is not already running.
//
// A stop leaves the object alive while its players are moved. Answering
// "already running" for it would tell a player's plugin the server is up
// while it is on its way out, and the plugin would send them there. It is
// UNAVAILABLE rather than REFUSED because the very same request succeeds once
// the member is gone.
func TestStartOnAStoppingMemberIsUnavailable(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")

	startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	holdWhileStopping(t, f, "private-servers-c0ffee")
	stopOverTheWire(t, f, pod, "private-servers-c0ffee")
	if member(t, f, "private-servers-c0ffee").DeletionTimestamp.IsZero() {
		t.Fatal("the member is not stopping, so this test would assert nothing")
	}

	resp := startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_UNAVAILABLE {
		t.Fatalf("reason = %v (%s), want UNAVAILABLE for a member that is still stopping",
			got, resp.GetError().GetMessage())
	}
	if resp.GetStartServer().GetAlreadyRunning() {
		t.Fatal("a member on its way out was reported as already running")
	}
}

// The corpse of a terminal member is deleted and then in the way: its own
// drain finalizer holds the object until the controller lets go. The fresh
// member cannot be created yet, and the caller is told to ask again rather
// than told the dead one is theirs.
func TestStartBehindALingeringCorpseIsUnavailable(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")

	startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	holdWhileStopping(t, f, "private-servers-c0ffee")
	setPhase(t, f, member(t, f, "private-servers-c0ffee"), phase.Finished)

	resp := startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_UNAVAILABLE {
		t.Fatalf("reason = %v (%s), want UNAVAILABLE while the corpse is still held",
			got, resp.GetError().GetMessage())
	}
	if resp.GetStartServer().GetAlreadyRunning() {
		t.Fatal("a member whose run was over was reported as already running")
	}
}

// Two group names and two keys can compose one server name: on-demand group
// "a" with key "b-xyz" composes exactly what ephemeral group "a-b" calls its
// member "a-b-xyz". No race is needed, and the answer must not be
// already_running -- a plugin told that sends its player to a server that is
// not theirs, which here is a lobby.
func TestStartRefusesANameAnotherGroupsServerAlreadyHas(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "a", 2)
	makeEphemeralGroup(t, f, "a-b")
	lobby := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "a-b-xyz", Namespace: f.ns},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "a-b"}},
	}
	if err := f.c.Create(f.ctx, lobby); err != nil {
		t.Fatalf("create the other group's server: %v", err)
	}

	pod := f.proxyPod("gateway-aaaa")

	resp := startOverTheWire(t, f, pod, "a", "b-xyz")
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v (%s), want REFUSED for a name another group already has",
			got, resp.GetError().GetMessage())
	}
	if resp.GetStartServer() != nil {
		t.Fatalf("answered with a server: %+v", resp.GetStartServer())
	}

	// And the other group's server is untouched: neither adopted nor deleted.
	held := member(t, f, "a-b-xyz")
	if held.Spec.GroupRef.Name != "a-b" || held.Spec.Key != "" {
		t.Errorf("the lobby server was rewritten: groupRef=%q key=%q",
			held.Spec.GroupRef.Name, held.Spec.Key)
	}
	if !held.DeletionTimestamp.IsZero() {
		t.Error("the lobby server was asked to go by a refused request")
	}
}

func TestStartRefusesAGroupThatIsNotOnDemand(t *testing.T) {
	f := newServerFixture(t)
	makeEphemeralGroup(t, f, "lobby")
	pod := f.proxyPod("gateway-aaaa")

	resp := startOverTheWire(t, f, pod, "lobby", "c0ffee")
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
	var srv spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "lobby-c0ffee"}, &srv); err == nil {
		t.Fatal("a member was created in a group whose sizing pass would condemn it")
	}
}

// A key that cannot be part of a name is REFUSED and not NOT_FOUND: the
// caller has to be able to tell "your key is wrong" from "your group is not
// here", because only one of the two is theirs to fix.
func TestStartRefusesAKeyNoNameCanBeBuiltFrom(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")

	resp := startOverTheWire(t, f, pod, "private-servers", "C0FFEE")
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", got)
	}
}

func TestStartOnAGroupThisNetworkDoesNotHaveIsNotFound(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-aaaa")

	resp := startOverTheWire(t, f, pod, "a-group-nobody-has", "c0ffee")
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_NOT_FOUND {
		t.Fatalf("reason = %v, want NOT_FOUND", got)
	}
}

// The other half of the audit in §3.7: the boost headroom check refuses
// anything that is not an ephemeral group with scaling, and a third type must
// not slip past it into a ScaleBoost that is created, counted, and changes
// nothing.
func TestBoostRefusesAnOnDemandGroup(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")

	resp := askOverTheWire(t, f, pod, &agentpb.CloudRequest{
		Request: &agentpb.CloudRequest_Boost{
			Boost: &agentpb.BoostRequest{Group: "private-servers", Replicas: 1},
		},
	})
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
}

func TestStopDeletesTheMember(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")
	startOverTheWire(t, f, pod, "private-servers", "c0ffee")

	resp := stopOverTheWire(t, f, pod, "private-servers-c0ffee")
	if resp.GetError() != nil {
		t.Fatalf("refused: %s", resp.GetError().GetMessage())
	}
	if got := resp.GetStopServer().GetServer(); got != "private-servers-c0ffee" {
		t.Fatalf("server = %q, want the name the caller asked with", got)
	}
	var srv spawneryv1alpha1.Server
	err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "private-servers-c0ffee"}, &srv)
	if err == nil && srv.DeletionTimestamp.IsZero() {
		t.Fatal("the member is still there and not going")
	}
}

// The mistake this refusal exists for: a caller naming a lobby would
// otherwise delete it.
func TestStopRefusesAServerThatIsNotAnInstance(t *testing.T) {
	f := newServerFixture(t)
	makeEphemeralGroup(t, f, "lobby")
	makeServer(t, f, "lobby-abc")
	pod := f.proxyPod("gateway-aaaa")

	resp := stopOverTheWire(t, f, pod, "lobby-abc")
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
	var srv spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "lobby-abc"}, &srv); err != nil {
		t.Fatalf("the lobby server was deleted by a refused request: %v", err)
	}
	if !srv.DeletionTimestamp.IsZero() {
		t.Fatal("the lobby server was asked to go by a refused request")
	}
}

func TestStoppingAServerThisNetworkDoesNotHaveIsNotFound(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-aaaa")

	resp := stopOverTheWire(t, f, pod, "a-server-nobody-has")
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_NOT_FOUND {
		t.Fatalf("reason = %v, want NOT_FOUND", got)
	}
}
