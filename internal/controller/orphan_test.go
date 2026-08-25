package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// createProxyPod adds a managed proxy pod belonging to the named group, and
// returns its UID — the key the agent registry is on.
func (f *fixture) createProxyPod(name, group string) string {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ns,
			Labels:    podspec.ProxyLabels("production", group),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "velocity", Image: "velocity"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		f.t.Fatalf("create proxy pod: %v", err)
	}
	return string(pod.UID)
}

// `createProxyGroup` comes from Task 7's proxygroup_controller_test.go — both
// files are in package controller, so it is already available here.

func orphanReconciler(f *fixture) *OrphanReconciler {
	return &OrphanReconciler{
		Client: f.rc,
		Agents: f.agents,
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

// The defect this task exists to remove: a proxy agent connects, one sweep
// runs, and the registry has forgotten it. Nothing logs a reason, because from
// the sweep's point of view the pod never existed — it filtered on role=server
// and then pruned every registry key not in that list.
func TestSweepKeepsAConnectedProxyAgent(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)
	f.createProxyGroup("gateway")
	uid := f.createProxyPod("gateway-abcd", "gateway")

	f.agents.Connect(uid, agent.RoleProxy)

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if snap := f.agents.Lookup(uid); !snap.Known || !snap.Connected {
		t.Errorf("the sweep forgot a connected proxy agent: %+v", snap)
	}
}

// The mirror of the Server case. A proxy has no CR of its own, so its group is
// what owns it, and nothing else would ever remove the pod.
func TestSweepDeletesAProxyPodWhoseGroupIsGone(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)
	f.createProxyPod("gateway-orphan", "does-not-exist")

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway-orphan", Namespace: f.ns}, &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a proxy pod whose group is gone survived the sweep: %v", err)
	}
}

// The widened list must not make proxy pods candidates for the Server check.
// They carry no server label and never will, and deleting them for that would
// be the same bug pointing the other way.
func TestSweepKeepsAProxyPodThatHasItsGroup(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)
	f.createProxyGroup("gateway")
	f.createProxyPod("gateway-abcd", "gateway")

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway-abcd", Namespace: f.ns}, &corev1.Pod{}); err != nil {
		t.Fatalf("the sweep deleted a proxy pod that has its group: %v", err)
	}
}

// The switch in Sweep has no default case, so an unrecognised role is a
// silent no-op by construction — nothing matches, err stays nil, nothing is
// deleted. Nothing but this test would catch a regression that changed that:
// a `default:` branch added later that deletes, or a new role value routed
// into the wrong case by copy-paste. The pod is still managed-by Spawnery, so
// it must reach the loop body; it just must not be acted on once there.
func TestSweepIgnoresAManagedPodWithAnUnknownRole(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mystery-pod",
			Namespace: f.ns,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelRole:      "loadbalancer",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "mystery-pod", Namespace: f.ns}, &corev1.Pod{}); err != nil {
		t.Fatalf("the sweep deleted a managed pod whose role it does not recognise: %v", err)
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

// TestSweepKeepsTheAgentOfADrainingPod pins an ordering docs/known-issues.md
// files as "the deletionTimestamp skip in Sweep is covered by no test; it
// concerns only an already-deleting orphaned pod, where a second Delete is
// harmless."
//
// The skip is indeed harmless. What is not harmless is the line above it:
// liveUIDs records the pod *before* the skip, and a UID missing from liveUIDs
// has its agent forgotten at the bottom of Sweep. A terminating proxy pod is
// exactly the pod that is still serving people — a drain is a pod with a
// deletion timestamp and players still on it — so forgetting its agent would
// throw away the operator's knowledge of those players, which is what decides
// occupancy, the disruption budget, and when the drain may end.
//
// So the test is not about the skip. It is about the two lines being in this
// order, which nothing said and nothing checked.
func TestSweepKeepsTheAgentOfADrainingPod(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)
	f.createProxyGroup("gateway")
	uid := f.createProxyPod("gateway-abcd", "gateway")
	f.agents.Connect(uid, agent.RoleProxy)

	// A pod bound to a node keeps its deletion timestamp: the API server waits
	// for a kubelet's confirmation and envtest runs none. That is what a
	// draining proxy looks like from here.
	pod := &corev1.Pod{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: "gateway-abcd"}, pod); err != nil {
		t.Fatalf("get the pod: %v", err)
	}
	pod.Spec.NodeName = "node-a"
	if err := f.c.SubResource("binding").Create(f.ctx, pod, &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.ns, Name: pod.Name},
		Target:     corev1.ObjectReference{Kind: "Node", Name: "node-a"},
	}); err != nil {
		t.Fatalf("bind the pod: %v", err)
	}
	if err := f.c.Delete(f.ctx, pod); err != nil {
		t.Fatalf("delete the pod: %v", err)
	}
	if err := f.c.Get(f.ctx, types.NamespacedName{Namespace: f.ns, Name: "gateway-abcd"}, pod); err != nil {
		t.Fatalf("the pod did not survive its own deletion, so it is not terminating: %v", err)
	}
	if pod.DeletionTimestamp.IsZero() {
		t.Fatal("the pod has no deletion timestamp, so this test is not about a draining pod")
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if snap := f.agents.Lookup(uid); !snap.Known {
		t.Errorf("the sweep forgot the agent of a pod that is terminating but still there: %+v. "+
			"A draining proxy has players on it until the drain ends, and this registry entry "+
			"is what the operator knows about them", snap)
	}
}
