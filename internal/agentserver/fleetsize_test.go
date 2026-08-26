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

package agentserver

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/spawnery/spawnery/internal/podspec"
)

func managedPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "games",
			Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func foreignPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "games"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func counterOver(objects ...client.Object) *FleetCounter {
	return &FleetCounter{
		Pods: fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build(),
	}
}

// failingReader is a client.Reader whose List always fails, which is the state
// a counter is in before the manager's cache has synced.
type failingReader struct{ client.Reader }

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("the cache has not synced")
}

// TestTheCountIsUnknownUntilItSucceeds is the distinction the whole type
// exists for. An operator that has not counted yet must not be told its fleet
// is empty: PeerLimiter.Expect turns unknown into no bound at all and zero
// into a real, if floored, one.
func TestTheCountIsUnknownUntilItSucceeds(t *testing.T) {
	counter := &FleetCounter{Pods: failingReader{}}
	if size, known := counter.Size(); known {
		t.Fatalf("a counter that has never run reported %d as known", size)
	}
	if err := counter.count(context.Background()); err == nil {
		t.Fatal("a failing list was reported as a successful count")
	}
	if size, known := counter.Size(); known {
		t.Fatalf("a failed count reported %d as known", size)
	}
}

// TestAnEmptyClusterIsCountedRatherThanUnknown is the other side of it. Zero
// managed pods is an answer, not an absence.
func TestAnEmptyClusterIsCountedRatherThanUnknown(t *testing.T) {
	counter := counterOver()
	if err := counter.count(context.Background()); err != nil {
		t.Fatalf("count: %v", err)
	}
	size, known := counter.Size()
	if !known {
		t.Fatal("an empty cluster was reported as an uncounted one")
	}
	if size != 0 {
		t.Errorf("counted %d pods in an empty cluster", size)
	}
}

// TestOnlyManagedPodsThatCanHoldAConnectionAreCounted pins both narrowings.
// The label one keeps a cluster's other workloads out of a number that bounds
// our own agents. The phase one keeps a cluster that retains its failures from
// raising the ceiling by every pod that ever died -- which is the same as
// having no ceiling, arrived at quietly.
func TestOnlyManagedPodsThatCanHoldAConnectionAreCounted(t *testing.T) {
	counter := counterOver(
		managedPod("lobby-0", corev1.PodRunning),
		managedPod("lobby-1", corev1.PodPending),
		managedPod("lobby-2", corev1.PodFailed),
		managedPod("lobby-3", corev1.PodSucceeded),
		foreignPod("someone-elses"),
	)
	if err := counter.count(context.Background()); err != nil {
		t.Fatalf("count: %v", err)
	}
	size, known := counter.Size()
	if !known {
		t.Fatal("a successful count reported unknown")
	}
	// Running and Pending: the Pending one holds nothing yet and is counted
	// anyway, because counting high only ever loosens the bound.
	if size != 2 {
		t.Errorf("counted %d pods, want the 2 that can hold a connection", size)
	}
}

// TestALastGoodCountSurvivesAFailedOne is what keeps a cache hiccup from
// turning the fleet bound off. The alternative -- dropping back to unknown --
// would mean a single failed list handed the fleet its slack back.
func TestALastGoodCountSurvivesAFailedOne(t *testing.T) {
	counter := counterOver(managedPod("lobby-0", corev1.PodRunning))
	if err := counter.count(context.Background()); err != nil {
		t.Fatalf("count: %v", err)
	}

	counter.Pods = failingReader{}
	if err := counter.count(context.Background()); err == nil {
		t.Fatal("a failing list was reported as a successful count")
	}
	size, known := counter.Size()
	if !known || size != 1 {
		t.Errorf("after a failed count the size is (%d, %v), want (1, true)", size, known)
	}
}

// TestTheCounterStopsWithItsContext guards the Start loop's exit. A Runnable
// that ignored cancellation would hold the manager's shutdown open for an
// interval, every time.
func TestTheCounterStopsWithItsContext(t *testing.T) {
	counter := counterOver()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := counter.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, known := counter.Size(); !known {
		t.Error("Start returned without counting once; a fresh leader would be blind for an interval")
	}
}
