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

package serverreg_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
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

func TestASessionThatFallsBehindIsCutRatherThanSilentlyStale(t *testing.T) {
	// Dropping the message instead would leave the agent serving a mirror it
	// has no way of knowing is stale, looking healthy the whole time.
	r := newRegistry(t, serverreg.Options{OutboxSize: 1}, group("ns", "lobby"))

	outbox, leave, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	// Read nothing, and resync until the queue is past its bound.
	for i := 0; i < 5; i++ {
		r.Resync(context.Background())
	}

	// Drain what is buffered, then assert the channel is *closed* rather than
	// merely empty. A bare `for range outbox` would block forever if the cut
	// never happened, so this test would hang rather than fail -- and a test
	// whose failure mode is a hang tells whoever runs it nothing.
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
	// A double close is the bug this guards: leave closes the channel, and a
	// resync that still held the session would close it a second time.
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
	// The make-before-break renewal: the new session is entered and the old
	// one's reader sees a closed channel and ends its stream.
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
