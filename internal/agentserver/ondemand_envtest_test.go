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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// Every test reads the cluster back where the verb writes: an answer is not evidence
// that anything was created or deleted.

// A proxy session: the plugin that starts and stops private servers runs on a proxy.
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

// holdWhileStopping adds the drain finalizer the Server controller would hold in a live
// cluster, so a stop leaves the member Terminating; envtest runs no controller. Cleanup
// removes it, or the namespace never finishes deleting.
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

	srv := member(t, f, "private-servers-c0ffee")
	if srv.Spec.Key != "c0ffee" {
		t.Errorf("spec.key = %q, want c0ffee", srv.Spec.Key)
	}
	if srv.Spec.GroupRef.Name != "private-servers" {
		t.Errorf("spec.groupRef = %q", srv.Spec.GroupRef.Name)
	}
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
	var srv spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "private-servers-two"},
		&srv); err == nil {
		t.Fatal("the member past the ceiling was created anyway")
	}
}

// Counting a finished member would let a group drift closed under servers nobody is on.
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

// Its world is on the claim; refusing would make the owner wait out a retention they
// cannot see.
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

// Answering "already running" would send players to a member on its way out.
// UNAVAILABLE, not REFUSED: the same request succeeds once the member is gone.
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

// The corpse's own drain finalizer holds it until the controller lets go, so the caller
// is told to ask again rather than handed the dead member.
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

// On-demand group "a" with key "b-xyz" composes exactly what ephemeral group "a-b" names
// its member "a-b-xyz"; already_running would send the player to that lobby.
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

	held := member(t, f, "a-b-xyz")
	if held.Spec.GroupRef.Name != "a-b" || held.Spec.Key != "" {
		t.Errorf("the lobby server was rewritten: groupRef=%q key=%q",
			held.Spec.GroupRef.Name, held.Spec.Key)
	}
	if !held.DeletionTimestamp.IsZero() {
		t.Error("the lobby server was asked to go by a refused request")
	}
}

// No operator write produces such a server; an admin or another controller can.
func TestStartRefusesAMemberOfThisGroupWhoseKeyDoesNotComposeItsName(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	odd := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers-c0ffee", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "private-servers"},
			Key:      "tea",
		},
	}
	if err := f.c.Create(f.ctx, odd); err != nil {
		t.Fatalf("create the server: %v", err)
	}

	pod := f.proxyPod("gateway-aaaa")

	resp := startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v (%s), want REFUSED for a name whose holder has another key",
			got, resp.GetError().GetMessage())
	}
	if resp.GetStartServer() != nil {
		t.Fatalf("answered with a server: %+v", resp.GetStartServer())
	}

	held := member(t, f, "private-servers-c0ffee")
	if held.Spec.Key != "tea" || !held.DeletionTimestamp.IsZero() {
		t.Errorf("the server was touched by a refused request: key=%q deleted=%v",
			held.Spec.Key, !held.DeletionTimestamp.IsZero())
	}
}

// A ceiling held by a leaving member clears by itself, so UNAVAILABLE: REFUSED would
// tell the plugin not to ask again, and asking again shortly is what works.
func TestStartIsUnavailableWhileTheCeilingIsHeldByAMemberThatIsGoing(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 1)
	pod := f.proxyPod("gateway-aaaa")

	startOverTheWire(t, f, pod, "private-servers", "one")
	holdWhileStopping(t, f, "private-servers-one")
	stopOverTheWire(t, f, pod, "private-servers-one")
	if member(t, f, "private-servers-one").DeletionTimestamp.IsZero() {
		t.Fatal("the member is not going, so this test would assert nothing")
	}

	resp := startOverTheWire(t, f, pod, "private-servers", "two")
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_UNAVAILABLE {
		t.Fatalf("reason = %v (%s), want UNAVAILABLE while the only slot is held by a member that is going",
			got, resp.GetError().GetMessage())
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

// REFUSED, not NOT_FOUND: a bad key is the caller's to fix, a missing group is not.
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

// The boost headroom check must refuse a third group type too, or the ScaleBoost is
// created, counted, and changes nothing.
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

func deleteOverTheWire(
	t *testing.T, f *serverFixture, pod *corev1.Pod, group, key string,
) *agentpb.CloudResponse {
	t.Helper()
	return askOverTheWire(t, f, pod, &agentpb.CloudRequest{
		Request: &agentpb.CloudRequest_DeleteServer{
			DeleteServer: &agentpb.DeleteServerRequest{Group: group, Key: key},
		},
	})
}

// worldOf creates a member's data claim the way the Server controller does.
func worldOf(t *testing.T, f *serverFixture, group, name string) {
	t.Helper()
	var g spawneryv1alpha1.ServerGroup
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: group}, &g); err != nil {
		t.Fatalf("get group %s: %v", group, err)
	}
	srv := &spawneryv1alpha1.Server{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns}}
	srv.Spec.Key = strings.TrimPrefix(name, group+"-")
	if err := f.c.Create(f.ctx, podspec.BuildDataClaim(&g, srv)); err != nil {
		t.Fatalf("create the claim of %s: %v", name, err)
	}
}

// holdClaim keeps a claim Terminating after its deletion, as pvc-protection
// does while a pod still mounts it.
func holdClaim(t *testing.T, f *serverFixture, name string) {
	t.Helper()
	key := client.ObjectKey{Namespace: f.ns, Name: podspec.DataClaimName(name)}
	var c corev1.PersistentVolumeClaim
	if err := f.c.Get(f.ctx, key, &c); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	patch := client.MergeFrom(c.DeepCopy())
	c.Finalizers = append(c.Finalizers, "test.spawnery.cloud/hold")
	if err := f.c.Patch(f.ctx, &c, patch); err != nil {
		t.Fatalf("hold claim: %v", err)
	}
	t.Cleanup(func() {
		var held corev1.PersistentVolumeClaim
		if err := f.c.Get(f.ctx, key, &held); err != nil {
			return
		}
		patch := client.MergeFrom(held.DeepCopy())
		held.Finalizers = nil
		_ = f.c.Patch(f.ctx, &held, patch)
	})
}

func claimGoing(t *testing.T, f *serverFixture, name string) bool {
	t.Helper()
	var c corev1.PersistentVolumeClaim
	err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: podspec.DataClaimName(name)}, &c)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	return !c.DeletionTimestamp.IsZero()
}

func serverGoing(t *testing.T, f *serverFixture, name string) bool {
	t.Helper()
	var srv spawneryv1alpha1.Server
	err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: name}, &srv)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		t.Fatalf("get server: %v", err)
	}
	return !srv.DeletionTimestamp.IsZero()
}

func wantReason(t *testing.T, resp *agentpb.CloudResponse, want agentpb.RequestError_Reason) {
	t.Helper()
	if got := resp.GetError().GetReason(); got != want || resp.GetError() == nil {
		t.Fatalf("reason = %v (%s), want %v", got, resp.GetError().GetMessage(), want)
	}
}

func TestDeleteRemovesARunningMemberAndItsWorld(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")
	startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	worldOf(t, f, "private-servers", "private-servers-c0ffee")

	resp := deleteOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if resp.GetError() != nil {
		t.Fatalf("refused: %s", resp.GetError().GetMessage())
	}
	if got := resp.GetDeleteServer(); got.GetServer() != "private-servers-c0ffee" || !got.GetWorld() {
		t.Fatalf("result = %+v, want the member's name and world=true", got)
	}
	if !serverGoing(t, f, "private-servers-c0ffee") {
		t.Error("the member is still there and not going")
	}
	if !claimGoing(t, f, "private-servers-c0ffee") {
		t.Error("the world is still there and not going")
	}
}

func TestDeleteRemovesTheWorldOfAStoppedMember(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")
	worldOf(t, f, "private-servers", "private-servers-c0ffee")

	resp := deleteOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if resp.GetError() != nil {
		t.Fatalf("refused: %s", resp.GetError().GetMessage())
	}
	if !resp.GetDeleteServer().GetWorld() {
		t.Error("world = false for a delete that removed a world")
	}
	if !claimGoing(t, f, "private-servers-c0ffee") {
		t.Error("the world of a stopped member survived its delete")
	}
}

func TestDeleteOfNothingIsNotFound(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")

	wantReason(t, deleteOverTheWire(t, f, pod, "private-servers", "c0ffee"), agentpb.RequestError_NOT_FOUND)
}

// A counted group's names can collide with a key's composed name; the group
// type is what keeps a delete away from its worlds.
func TestDeleteRefusesAGroupThatIsNotOnDemand(t *testing.T) {
	f := newServerFixture(t)
	makeEphemeralGroup(t, f, "lobby")
	pod := f.proxyPod("gateway-aaaa")

	wantReason(t, deleteOverTheWire(t, f, pod, "lobby", "c0ffee"), agentpb.RequestError_REFUSED)
}

func TestDeleteOnAGroupThisNetworkDoesNotHaveIsNotFound(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-aaaa")

	wantReason(t, deleteOverTheWire(t, f, pod, "private-servers", "c0ffee"), agentpb.RequestError_NOT_FOUND)
}

func TestDeleteRefusesAKeyNoNameCanBeBuiltFrom(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")

	wantReason(t, deleteOverTheWire(t, f, pod, "private-servers", "NOT A KEY!"), agentpb.RequestError_REFUSED)
}

func TestDeleteLeavesAClaimItDidNotMake(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: podspec.DataClaimName("private-servers-c0ffee"), Namespace: f.ns,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create foreign claim: %v", err)
	}

	wantReason(t, deleteOverTheWire(t, f, pod, "private-servers", "c0ffee"), agentpb.RequestError_REFUSED)
	if claimGoing(t, f, "private-servers-c0ffee") {
		t.Fatal("a claim this operator did not make was deleted")
	}
}

func TestDeleteTwiceWhileGoingSucceeds(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")
	startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	holdWhileStopping(t, f, "private-servers-c0ffee")

	for i := 0; i < 2; i++ {
		if resp := deleteOverTheWire(t, f, pod, "private-servers", "c0ffee"); resp.GetError() != nil {
			t.Fatalf("delete %d refused: %s", i+1, resp.GetError().GetMessage())
		}
	}
}

func TestStartWhileTheWorldIsBeingDeletedIsUnavailable(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")
	worldOf(t, f, "private-servers", "private-servers-c0ffee")
	holdClaim(t, f, "private-servers-c0ffee")
	deleteOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if !claimGoing(t, f, "private-servers-c0ffee") {
		t.Fatal("the world is not being deleted, so this test would assert nothing")
	}

	wantReason(t, startOverTheWire(t, f, pod, "private-servers", "c0ffee"), agentpb.RequestError_UNAVAILABLE)
}

// A world from before the key label existed: this operator made it, but the
// admission policy would refuse its deletion, so the writer says so first.
func TestDeleteRefusesAWorldWithoutItsKeyLabel(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := f.proxyPod("gateway-aaaa")
	worldOf(t, f, "private-servers", "private-servers-c0ffee")
	var c corev1.PersistentVolumeClaim
	key := client.ObjectKey{Namespace: f.ns, Name: podspec.DataClaimName("private-servers-c0ffee")}
	if err := f.c.Get(f.ctx, key, &c); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	patch := client.MergeFrom(c.DeepCopy())
	delete(c.Labels, podspec.LabelKey)
	if err := f.c.Patch(f.ctx, &c, patch); err != nil {
		t.Fatalf("drop the key label: %v", err)
	}

	wantReason(t, deleteOverTheWire(t, f, pod, "private-servers", "c0ffee"), agentpb.RequestError_REFUSED)
	if claimGoing(t, f, "private-servers-c0ffee") {
		t.Fatal("a world without its key label was deleted")
	}
}
