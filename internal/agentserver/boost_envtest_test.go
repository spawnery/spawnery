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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/podspec"
)

// One test per bound, and every one of them reads the cluster back afterwards.
// An answer is not evidence that an object was or was not created, and "said
// yes and wrote nothing" is exactly as broken as "said no and wrote anyway".

// makeGroup creates an ephemeral group a boost can move.
func makeGroup(t *testing.T, f *serverFixture, name string, minR, maxR int32) {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "example/paper:1",
			MaxPlayers: 100,
			Scaling: &spawneryv1alpha1.ScalingSpec{
				MinReplicas: minR,
				MaxReplicas: maxR,
				SpareSlots:  10,
			},
		},
	}
	if err := f.c.Create(f.ctx, g); err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
}

// ask sends one CloudRequest on a stream and returns the answer to it.
func ask(t *testing.T, f *serverFixture, pod *corev1.Pod, id uint64,
	request *agentpb.CloudRequest,
) *agentpb.CloudResponse {
	t.Helper()
	stream, done := dialAgent(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the opening message never arrived: %v", err)
	}
	request.Id = id
	if err := stream.Send(&agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_CloudRequest{CloudRequest: request},
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if resp := msg.GetCloudResponse(); resp != nil && resp.GetId() == id {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatal("no answer arrived within ten seconds")
		}
	}
}

// boostRequest and stopRequest keep the envelope out of every test body.
func boostRequest(group string, replicas int32, seconds int64) *agentpb.CloudRequest {
	return &agentpb.CloudRequest{Request: &agentpb.CloudRequest_Boost{
		Boost: &agentpb.BoostRequest{Group: group, Replicas: replicas, DurationSeconds: seconds},
	}}
}

func stopRequest(group string) *agentpb.CloudRequest {
	return &agentpb.CloudRequest{Request: &agentpb.CloudRequest_StopBoost{
		StopBoost: &agentpb.StopBoostRequest{Group: group},
	}}
}

func boostsOn(t *testing.T, f *serverFixture, group string) []spawneryv1alpha1.ScaleBoost {
	t.Helper()
	var list spawneryv1alpha1.ScaleBoostList
	// Namespace-scoped, always: envtest shares one control plane across the
	// whole package and never cleans up, so a cluster-wide list would make one
	// test's leftovers another's answer.
	if err := f.c.List(f.ctx, &list, client.InNamespace(f.ns)); err != nil {
		t.Fatalf("list boosts: %v", err)
	}
	var mine []spawneryv1alpha1.ScaleBoost
	for _, b := range list.Items {
		if b.Spec.GroupRef.Name == group {
			mine = append(mine, b)
		}
	}
	return mine
}

func TestABoostIsCreatedOwnedByItsGroupAndExpires(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeGroup(t, f, "lobby", 1, 6)

	before := time.Now()
	resp := ask(t, f, pod, 1, boostRequest("lobby", 2, 0))

	if resp.GetBoost().GetReplicas() != 2 {
		t.Fatalf("answer = %+v, want a BoostResult for 2", resp.GetResult())
	}
	// The default, resolved by the operator rather than by the agent: the
	// request asked for zero seconds.
	expires := time.Unix(resp.GetBoost().GetExpiresAtUnix(), 0)
	if got := expires.Sub(before); got < 55*time.Minute || got > 65*time.Minute {
		t.Errorf("expiry is %s away, want about the one-hour default", got)
	}

	boosts := boostsOn(t, f, "lobby")
	if len(boosts) != 1 {
		t.Fatalf("the operator answered yes and the namespace holds %d boosts", len(boosts))
	}
	b := boosts[0]
	if b.Spec.Replicas != 2 {
		t.Errorf("boost replicas = %d, want 2", b.Spec.Replicas)
	}
	if b.Spec.ExpiresAt == nil {
		t.Fatal("the boost has no expiry, so nothing will ever end it")
	}
	// The owner reference is what makes a deleted group take its boosts with
	// it. Without it a boost outlives the group it names and counts for
	// nothing while sitting in the namespace.
	if len(b.OwnerReferences) != 1 || b.OwnerReferences[0].Name != "lobby" {
		t.Errorf("owner references = %+v, want one naming the group", b.OwnerReferences)
	}
}

func TestTwoBoostsAddRatherThanReplace(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeGroup(t, f, "lobby", 1, 9)

	ask(t, f, pod, 1, boostRequest("lobby", 2, 0))
	ask(t, f, pod, 2, boostRequest("lobby", 1, 0))

	// Two objects and not one edited: "somebody else already boosted this" has
	// to be a non-event rather than a race between two people typing.
	if got := boostsOn(t, f, "lobby"); len(got) != 2 {
		t.Fatalf("the namespace holds %d boosts, want two that add", len(got))
	}
}

func TestABoostBeyondTheCeilingIsRefusedAndNamesTheRoom(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	// Room for two: the ceiling is three and the floor is one.
	makeGroup(t, f, "lobby", 1, 3)

	resp := ask(t, f, pod, 1, boostRequest("lobby", 6, 0))

	if got := resp.GetError().GetReason(); got != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED past the ceiling", got)
	}
	// The number, so an admin can retype something that works rather than
	// guess. A bare refusal makes them try five, then four, then three.
	if msg := resp.GetError().GetMessage(); !strings.Contains(msg, "room for 2") {
		t.Errorf("message = %q, did not say how much room there is", msg)
	}
	if got := boostsOn(t, f, "lobby"); len(got) != 0 {
		t.Errorf("a refused boost created %d objects anyway", len(got))
	}
}

func TestAnExistingBoostCountsAgainstTheRoomLeft(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	// Room for four.
	makeGroup(t, f, "lobby", 1, 5)

	// Three of it, taken.
	if r := ask(t, f, pod, 1, boostRequest("lobby", 3, 0)); r.GetBoost() == nil {
		t.Fatalf("the first boost was refused: %+v", r.GetResult())
	}
	// Two more would be six against a ceiling of five.
	resp := ask(t, f, pod, 2, boostRequest("lobby", 2, 0))

	if got := resp.GetError().GetReason(); got != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED: the live boost has to count", got)
	}
	if msg := resp.GetError().GetMessage(); !strings.Contains(msg, "room for 1") {
		t.Errorf("message = %q, want the one remaining", msg)
	}
}

func TestABoostLongerThanTheBoundIsRefused(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeGroup(t, f, "lobby", 1, 9)

	resp := ask(t, f, pod, 1,
		boostRequest("lobby", 1, int64((agentserver.BoostMaxDuration+time.Hour).Seconds())))

	if got := resp.GetError().GetReason(); got != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED past the duration bound", got)
	}
	// The sentence that sends somebody to the file they should be editing.
	// Without it the bound is an obstacle rather than an answer.
	if msg := resp.GetError().GetMessage(); !strings.Contains(msg, "own file") {
		t.Errorf("message = %q, did not point at the lasting way to do this", msg)
	}
	if got := boostsOn(t, f, "lobby"); len(got) != 0 {
		t.Errorf("a boost refused for its length was created anyway: %d", len(got))
	}
}

func TestABoostOnAPersistentGroupIsRefusedRatherThanIgnored(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	replicas := int32(2)
	if err := f.c.Create(f.ctx, &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupPersistent,
			Image:      "example/paper:1",
			MaxPlayers: 100,
			Replicas:   &replicas,
			Storage: &spawneryv1alpha1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
		},
	}); err != nil {
		t.Fatalf("create the persistent group: %v", err)
	}

	resp := ask(t, f, pod, 1, boostRequest("survival", 1, 0))

	// Refused and not created: nothing adds a boost to a persistent group's
	// size, so the object would exist, be counted in status.boostedReplicas,
	// and change nothing at all.
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED for a group no boost can move", got)
	}
	if got := boostsOn(t, f, "survival"); len(got) != 0 {
		t.Errorf("a boost was created on a group it cannot move: %d", len(got))
	}
}

func TestABoostOfNothingIsRefused(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeGroup(t, f, "lobby", 1, 9)

	resp := ask(t, f, pod, 1, boostRequest("lobby", 0, 0))

	if got := resp.GetError().GetReason(); got != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED for a boost of nothing", got)
	}
	if got := boostsOn(t, f, "lobby"); len(got) != 0 {
		t.Errorf("a boost of zero created %d objects", len(got))
	}
}

func TestABoostCannotReachAnotherNetworksGroupOfTheSameName(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeGroup(t, f, "lobby", 1, 9)

	other := "elsewhere-" + f.ns
	if err := f.c.Create(f.ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: other},
	}); err != nil {
		t.Fatalf("create the other namespace: %v", err)
	}

	ask(t, f, pod, 1, boostRequest("lobby", 1, 0))

	// The agent's own namespace got it, and the other one has nothing --
	// including no boost named for a group it does not have.
	if got := boostsOn(t, f, "lobby"); len(got) != 1 {
		t.Fatalf("the agent's own namespace holds %d boosts, want one", len(got))
	}
	var elsewhere spawneryv1alpha1.ScaleBoostList
	if err := f.c.List(f.ctx, &elsewhere, client.InNamespace(other)); err != nil {
		t.Fatalf("list the other namespace: %v", err)
	}
	if len(elsewhere.Items) != 0 {
		t.Errorf("a boost landed in another namespace: %+v", elsewhere.Items)
	}
}

func TestStopRemovesEveryBoostOnTheGroupAndSaysHowMany(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeGroup(t, f, "lobby", 1, 9)
	makeGroup(t, f, "arena", 1, 9)

	ask(t, f, pod, 1, boostRequest("lobby", 1, 0))
	ask(t, f, pod, 2, boostRequest("lobby", 2, 0))
	ask(t, f, pod, 3, boostRequest("arena", 1, 0))

	resp := ask(t, f, pod, 4, stopRequest("lobby"))

	if got := resp.GetStopBoost().GetRemoved(); got != 2 {
		t.Fatalf("removed = %d, want the two on lobby", got)
	}
	if got := boostsOn(t, f, "lobby"); len(got) != 0 {
		t.Errorf("stop left %d boosts on the group", len(got))
	}
	// And it did not reach past the group it was given. Two verbs exist so
	// that a typo cannot confuse a group with a server; a stop that swept the
	// namespace would give that back.
	if got := boostsOn(t, f, "arena"); len(got) != 1 {
		t.Errorf("stop on lobby removed arena's boosts too: %d left", len(got))
	}
}

func TestStoppingAGroupWithNoBoostsAnswersZero(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeGroup(t, f, "lobby", 1, 9)

	resp := ask(t, f, pod, 1, stopRequest("lobby"))

	// An ordinary answer and not an error: it is what an admin who expected
	// boosts needs to hear.
	if resp.GetError() != nil {
		t.Fatalf("a group with no boosts was an error: %+v", resp.GetError())
	}
	if got := resp.GetStopBoost().GetRemoved(); got != 0 {
		t.Errorf("removed = %d, want zero", got)
	}
}
