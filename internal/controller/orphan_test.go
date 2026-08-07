package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

func orphanReconciler(f *fixture) *OrphanReconciler {
	return &OrphanReconciler{
		Client:   f.c,
		Recorder: record.NewFakeRecorder(100),
		Agents:   f.agents,
	}
}

func TestSweepDeletesAPodWithoutItsServer(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	stray := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lobby-ghost",
			Namespace: f.ns,
			Labels:    podspec.ServerLabels("production", "lobby", "lobby-ghost"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "minecraft", Image: "paper"}},
		},
	}
	if err := f.c.Create(f.ctx, stray); err != nil {
		t.Fatalf("create stray pod: %v", err)
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-ghost", Namespace: f.ns}, &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("stray pod survived the sweep: %v", err)
	}
}

func TestSweepKeepsAPodThatHasItsServer(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("the sweep deleted a pod that has its Server")
	}
}

func TestSweepIgnoresForeignPods(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "someone-elses-pod",
			Namespace: f.ns,
			Labels:    map[string]string{"app": "postgres"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "db", Image: "postgres"}},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create foreign pod: %v", err)
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "someone-elses-pod", Namespace: f.ns}, &corev1.Pod{}); err != nil {
		t.Fatalf("the sweep touched a pod it does not manage: %v", err)
	}
}

func TestSweepDeletesAServerWithoutItsGroup(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "gone-x1", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "does-not-exist"},
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	got := &spawneryv1alpha1.Server{}
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "gone-x1", Namespace: f.ns}, got)
	if err == nil && got.DeletionTimestamp.IsZero() {
		t.Fatal("the server of a deleted group was not removed")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get server: %v", err)
	}
}

func TestSweepForgetsAgentsOfVanishedPods(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	f.agents.Connect("pod-uid-that-never-existed", agent.RoleServer)
	if !f.agents.Lookup("pod-uid-that-never-existed").Known {
		t.Fatal("precondition: the agent must be known")
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if f.agents.Lookup("pod-uid-that-never-existed").Known {
		t.Error("the registry still knows an agent whose pod does not exist")
	}
}

// TestSweepKeepsAPodWhoseServerHasNotRecordedTheNameYet pins the window the
// Server controller and the sweep share: a Server is created before its pod
// ever exists (Reconcile only builds the pod once the Server object is
// already there), and status.PodName is only written after the pod. If the
// sweep ran on the pod's label alone without checking for a live Server, or
// if it needed status.PodName to agree, a sweep landing in that gap could
// delete a pod that its Server was about to adopt. It must not: the Server
// object existing under the name the pod's label carries is enough.
func TestSweepKeepsAPodWhoseServerHasNotRecordedTheNameYet(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-p3nd", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			GroupGeneration: f.group.Generation,
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create server: %v", err)
	}

	// Simulate the window right after Create(pod) but before
	// Status().Update(srv): the pod exists and carries the Server's name, but
	// srv.Status.PodName is still empty.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lobby-p3nd",
			Namespace: f.ns,
			Labels:    podspec.ServerLabels("production", "lobby", "lobby-p3nd"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "minecraft", Image: "paper"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	if got := f.server("lobby-p3nd"); got.Status.PodName != "" {
		t.Fatalf("precondition: status.PodName = %q, want empty", got.Status.PodName)
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, ok := f.pod("lobby-p3nd"); !ok {
		t.Fatal("the sweep deleted a pod about to be adopted by its Server")
	}
}

// TestSweepAndServerControllerConverge runs the sweep and the Server
// controller in alternation across many passes on a healthy, Ready server.
// Both can delete things — the sweep a pod without a Server, the Server
// controller a pod it decides to replace — and they must not fight: the pod
// and the Ready phase must survive every round.
func TestSweepAndServerControllerConverge(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	f.createServer("lobby-x7k2")
	uid := bringUpNamed(t, f, "lobby-x7k2")

	for i := 0; i < 20; i++ {
		if err := o.Sweep(f.ctx); err != nil {
			t.Fatalf("pass %d: Sweep: %v", i, err)
		}
		f.reconcile("lobby-x7k2")

		if _, ok := f.pod("lobby-x7k2"); !ok {
			t.Fatalf("pass %d: the pod of a healthy Ready server was deleted", i)
		}
		if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
			t.Fatalf("pass %d: phase = %q, want Ready", i, got)
		}
		if !f.agents.Lookup(uid).Known {
			t.Fatalf("pass %d: the sweep forgot the agent of a pod that still exists", i)
		}
	}
}
