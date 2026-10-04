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

package serverreg_test

import (
	"context"
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
	"github.com/spawnery/spawnery/internal/serverreg"
)

func group(ns, name string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "example/paper:1",
			MaxPlayers: 100,
		},
	}
}

func newRegistry(t *testing.T, opts serverreg.Options, objects ...client.Object) *serverreg.Registry {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	start := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	opts.State = netstate.Source{
		Reader: fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&spawneryv1alpha1.Server{}).
			WithObjects(objects...).Build(),
		Agents: agent.New(func() time.Time { return start }, 5*time.Second, start),
	}
	return serverreg.New(opts)
}

func TestAJoiningServerIsSentTheStateFirst(t *testing.T) {
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"))

	outbox, leave, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	first := <-outbox
	state := first.GetNetworkState()
	if state == nil {
		t.Fatalf("the first message was %T, want the network state", first.GetMessage())
	}
	if len(state.GetGroups()) != 1 || state.GetGroups()[0].GetName() != "lobby" {
		t.Errorf("groups = %v, want lobby", state.GetGroups())
	}
}

// Private servers are left out on both paths that build a picture: the state a
// session opens with and the one every resync repeats.
func TestABackendIsNeverSentPrivateServers(t *testing.T) {
	maxInstances := int32(300)
	private := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers", Namespace: "ns"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type: spawneryv1alpha1.ServerGroupOnDemand, MaxInstances: &maxInstances,
		},
	}
	member := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers-c0ffee", Namespace: "ns"},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "private-servers"},
			Key:      "c0ffee",
		},
		Status: spawneryv1alpha1.ServerStatus{Phase: "Ready", Registered: true},
	}
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"), private, member)

	outbox, leave, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	r.Resync(context.Background())

	for _, when := range []string{"on join", "on resync"} {
		state := (<-outbox).GetNetworkState()
		if state == nil {
			t.Fatalf("no network state %s", when)
		}
		if len(state.GetServers()) != 0 {
			t.Errorf("%s: servers = %v, want no private server", when, state.GetServers())
		}
		if len(state.GetGroups()) != 1 || state.GetGroups()[0].GetName() != "lobby" {
			t.Errorf("%s: groups = %v, want lobby alone", when, state.GetGroups())
		}
	}
}

func TestASessionThatFallsBehindIsCutRatherThanSilentlyStale(t *testing.T) {
	r := newRegistry(t, serverreg.Options{OutboxSize: 1}, group("ns", "lobby"))

	outbox, leave, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	for i := 0; i < 5; i++ {
		r.Resync(context.Background())
	}

	// Assert *closed*, not merely empty: a bare `for range outbox` would hang
	// rather than fail if the cut never happened.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-outbox:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("the outbox was still open after five resyncs past its bound: " +
				"a session that falls behind must be cut, not left serving a mirror " +
				"it cannot know is stale")
		}
	}
}

func TestLeavingRemovesTheSessionAndAResyncAfterItDoesNotPanic(t *testing.T) {
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"))

	_, leave, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	leave()

	r.Resync(context.Background())
}

func TestResyncReachesEverySessionWithItsOwnNamespacesState(t *testing.T) {
	r := newRegistry(t, serverreg.Options{},
		group("ns-one", "lobby"), group("ns-two", "arena"))

	one, leaveOne, err := r.Join(context.Background(), "ns-one", "pod-one")
	if err != nil {
		t.Fatalf("Join one: %v", err)
	}
	defer leaveOne()
	two, leaveTwo, err := r.Join(context.Background(), "ns-two", "pod-two")
	if err != nil {
		t.Fatalf("Join two: %v", err)
	}
	defer leaveTwo()

	<-one
	<-two
	r.Resync(context.Background())

	if got := (<-one).GetNetworkState().GetGroups()[0].GetName(); got != "lobby" {
		t.Errorf("ns-one was sent %q, want its own group", got)
	}
	if got := (<-two).GetNetworkState().GetGroups()[0].GetName(); got != "arena" {
		t.Errorf("ns-two was sent %q, want its own group", got)
	}
}

func TestASecondStreamFromOnePodSupersedesTheFirst(t *testing.T) {
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"))

	first, _, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("first Join: %v", err)
	}
	<-first

	second, leave, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("second Join: %v", err)
	}
	defer leave()

	closed := false
	superseded := time.After(5 * time.Second)
	for !closed {
		select {
		case _, open := <-first:
			closed = !open
		case <-superseded:
			t.Fatal("the superseded session's channel stayed open, so its stream would never end")
		}
	}
	if (<-second).GetNetworkState() == nil {
		t.Error("the superseding session did not get its own state")
	}
}

// cloudEventsIn is bounded so a test proving nothing arrives fails instead of
// hanging. It skips the NetworkState every stream opens with.
func cloudEventsIn(ch <-chan *agentpb.OperatorToServer) []*agentpb.CloudEvent {
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

func TestAnEventReachesOnlyTheSessionsThatWantOne(t *testing.T) {
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"))
	watching, leaveA, err := r.Join(context.Background(), "ns", "pod-watching")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveA()
	quiet, leaveB, err := r.Join(context.Background(), "ns", "pod-quiet")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveB()
	r.SetInterest("pod-watching", true)

	r.Publish("ns", &agentpb.CloudEvent{Kind: "ReadyGatePassed", Subject: "lobby-a"})

	if got := cloudEventsIn(watching); len(got) != 1 || got[0].GetSubject() != "lobby-a" {
		t.Errorf("the interested session got %+v, want one event for lobby-a", got)
	}
	if got := cloudEventsIn(quiet); len(got) != 0 {
		t.Errorf("a session that never asked for events received %+v", got)
	}
}

func TestInterestIsForgottenWithTheSession(t *testing.T) {
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"))
	_, leave, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	r.SetInterest("pod-a", true)
	leave()

	if r.Interested("pod-a") {
		t.Error("interest outlived the session that declared it")
	}
}

func TestAnEventDoesNotCrossANamespace(t *testing.T) {
	// Publish takes the namespace as an argument rather than from a token.
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"), group("other", "lobby"))
	other, leave, err := r.Join(context.Background(), "other", "pod-other")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	r.SetInterest("pod-other", true)

	r.Publish("ns", &agentpb.CloudEvent{Kind: "ReadyGatePassed", Subject: "lobby-a"})

	if got := cloudEventsIn(other); len(got) != 0 {
		t.Errorf("an event reached a session in another namespace: %+v", got)
	}
}

func TestInterestForAPodWithNoSessionIsIgnored(t *testing.T) {
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"))

	r.SetInterest("pod-that-never-joined", true)

	if r.Interested("pod-that-never-joined") {
		t.Error("interest was recorded for a pod with no session")
	}
}

func TestSendReachesOnlyTheNamedSession(t *testing.T) {
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"))
	a, leaveA, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer leaveA()
	b, leaveB, err := r.Join(context.Background(), "ns", "pod-b")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer leaveB()
	<-a
	<-b

	msg := &agentpb.OperatorToServer{Message: &agentpb.OperatorToServer_ExecuteCommand{
		ExecuteCommand: &agentpb.ExecuteCommand{Id: 1, Command: "list"}}}
	if !r.Send("pod-a", msg) {
		t.Fatal("Send to a live session reported failure")
	}
	if got := <-a; got.GetExecuteCommand().GetId() != 1 {
		t.Errorf("pod-a got %v", got)
	}
	select {
	case got := <-b:
		t.Errorf("pod-b got %v, want nothing", got)
	default:
	}
	if r.Send("pod-gone", msg) {
		t.Error("Send to no session reported success")
	}
}
