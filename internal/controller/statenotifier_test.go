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

package controller

import (
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

type recordingState struct {
	mu         sync.Mutex
	namespaces []string
}

func (r *recordingState) Changed(namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.namespaces = append(r.namespaces, namespace)
}

func (r *recordingState) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.namespaces)
}

// settled reconciles until a pass tells the proxies nothing and returns how
// many pushes it took, so the test that follows counts only what it did itself.
func settled(t *testing.T, f *fixture, state *recordingState, name string) int {
	t.Helper()
	for i := 0; i < 10; i++ {
		before := state.count()
		f.reconcile(name)
		if state.count() == before {
			return before
		}
	}
	t.Fatalf("%s never stopped changing for the proxies", name)
	return 0
}

func newFixtureWithState(t *testing.T) (*fixture, *recordingState) {
	t.Helper()
	f := newFixture(t)
	state := &recordingState{}
	f.reconc.State = state
	return f, state
}

func TestANewServerIsToldToTheProxies(t *testing.T) {
	f, state := newFixtureWithState(t)
	f.createServer("lobby-a")

	f.reconcile("lobby-a")

	if state.count() == 0 {
		t.Fatal("a server nobody had seen yet told the proxies nothing")
	}
	if got := state.namespaces[0]; got != f.ns {
		t.Errorf("told namespace %q, want %q", got, f.ns)
	}
}

func TestAServerTheSchedulerCannotPlaceIsToldToTheProxiesOnce(t *testing.T) {
	f, state := newFixtureWithState(t)
	f.createServer("lobby-a")
	base := settled(t, f, state, "lobby-a")

	f.markUnschedulable(t, "lobby-a", insufficientMemory)
	f.reconcile("lobby-a")
	f.reconcile("lobby-a")
	if got := state.count() - base; got != 1 {
		t.Fatalf("pushes = %d once the status said PodUnschedulable, want 1", got)
	}

	f.reconcile("lobby-a")
	f.reconcile("lobby-a")
	if got := state.count() - base; got != 1 {
		t.Errorf("pushes = %d after further passes that changed nothing, want 1", got)
	}
}

func TestAServerBoundToANodeIsToldToTheProxies(t *testing.T) {
	f, state := newFixtureWithState(t)
	f.createServer("lobby-a")
	base := settled(t, f, state, "lobby-a")
	pod, ok := f.pod("lobby-a")
	if !ok {
		t.Fatal("no pod")
	}

	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		Target:     corev1.ObjectReference{Kind: "Node", Name: "node-1"},
	}
	if err := f.c.SubResource("binding").Create(f.ctx, pod, binding); err != nil {
		t.Fatalf("bind: %v", err)
	}
	f.reconcile("lobby-a")

	if got := state.count() - base; got != 1 {
		t.Errorf("pushes = %d after the pod was bound, want 1", got)
	}
}

func TestAPlayerCountAloneTellsTheProxiesNothing(t *testing.T) {
	f, state := newFixtureWithState(t)
	uid := bringUpReady(t, f, "lobby-a")
	base := settled(t, f, state, "lobby-a")

	f.clock.Advance(time.Minute)
	if err := f.agents.ReportPlayers(uid, 12, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-a")

	srv := f.server("lobby-a")
	if srv.Status.Players != 12 {
		t.Fatalf("status.players = %d, want 12: the count never reached the status, so this test "+
			"proves nothing", srv.Status.Players)
	}
	if got := state.count() - base; got != 0 {
		t.Errorf("pushes = %d for a player count, want 0: counts travel on the resync", got)
	}
}

func TestADeletedServerIsToldToTheProxiesOnce(t *testing.T) {
	f, state := newFixtureWithState(t)
	f.createServer("lobby-a")
	settled(t, f, state, "lobby-a")
	if err := f.c.Delete(f.ctx, f.server("lobby-a")); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	key := types.NamespacedName{Name: "lobby-a", Namespace: f.ns}
	for i := 0; i < 10; i++ {
		err := f.c.Get(f.ctx, key, &spawneryv1alpha1.Server{})
		if apierrors.IsNotFound(err) {
			break
		}
		f.reconcile("lobby-a")
	}
	if err := f.c.Get(f.ctx, key, &spawneryv1alpha1.Server{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the server is still there: %v", err)
	}

	before := state.count()
	f.reconcile("lobby-a")
	if got := state.count() - before; got != 1 {
		t.Errorf("pushes = %d for the server's disappearance, want 1", got)
	}
	f.reconcile("lobby-a")
	if got := state.count() - before; got != 1 {
		t.Errorf("pushes = %d after a second pass for a server that is gone, want still 1", got)
	}
}

func TestAServerNeverSeenAndNotFoundTellsTheProxiesNothing(t *testing.T) {
	f, state := newFixtureWithState(t)

	f.reconcile("never-created")

	if got := state.count(); got != 0 {
		t.Errorf("pushes = %d for a server that never existed, want 0", got)
	}
}
