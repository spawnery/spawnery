package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// createProxyPod returns the pod UID the agent registry is keyed on.
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

func orphanReconciler(f *fixture) *OrphanReconciler {
	return &OrphanReconciler{
		Client: f.rc,
		Agents: f.agents,
		Clock:  f.clock.Now,
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

// A Server exists before its pod and writes status.PodName only after it, so
// the Server named by the pod's label is enough to keep the pod.
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

	// After Create(pod), before status.PodName is written.
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

// A proxy has no CR of its own; nothing else would remove the pod.
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

// Proxy pods carry no server label and must not face the Server check.
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

// Sweep's switch has no default: a managed pod with an unknown role reaches
// the loop body and must not be acted on there.
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

// Both delete things and must not fight: the pod and the Ready phase survive
// every round.
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

// A draining pod carries a deletion timestamp and still has players; its UID
// must reach liveUIDs before Sweep skips it, or its agent is forgotten.
func TestSweepKeepsTheAgentOfADrainingPod(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)
	f.createProxyGroup("gateway")
	uid := f.createProxyPod("gateway-abcd", "gateway")
	f.agents.Connect(uid, agent.RoleProxy)

	// A bound pod keeps its deletion timestamp because envtest runs no
	// kubelet: a draining proxy, as seen from here.
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

// Both in one test, so a sweep that deleted everything fails.
func TestTheSweepRemovesAnExpiredBoostAndLeavesALiveOne(t *testing.T) {
	f := newFixture(t)
	past := metav1.NewTime(f.clock.now.Add(-time.Minute))
	future := metav1.NewTime(f.clock.now.Add(time.Hour))
	for name, expires := range map[string]metav1.Time{"stale": past, "live": future} {
		if err := f.c.Create(f.ctx, &spawneryv1alpha1.ScaleBoost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
			Spec: spawneryv1alpha1.ScaleBoostSpec{
				GroupRef:  spawneryv1alpha1.ObjectRef{Name: f.group.Name},
				Replicas:  1,
				ExpiresAt: &expires,
			},
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	if err := orphanReconciler(f).Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	boosts := &spawneryv1alpha1.ScaleBoostList{}
	if err := f.c.List(f.ctx, boosts, ctrlclientInNamespace(f.ns)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(boosts.Items) != 1 || boosts.Items[0].Name != "live" {
		t.Fatalf("boosts = %v, want only the live one", boosts.Items)
	}
}

func TestTheSweepLeavesABoostWithNoExpiry(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Create(f.ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "forever", Namespace: f.ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			Replicas: 1,
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := orphanReconciler(f).Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	boosts := &spawneryv1alpha1.ScaleBoostList{}
	if err := f.c.List(f.ctx, boosts, ctrlclientInNamespace(f.ns)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(boosts.Items) != 1 {
		t.Fatalf("boosts = %v, want the one with no expiry left standing", boosts.Items)
	}
}
