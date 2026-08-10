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

package controller

import (
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

// createProxyGroup adds a ProxyGroup to the fixture's network. Task 8's sweep
// tests use it too — both files are in package controller.
func (f *fixture) createProxyGroup(name string, mutate ...func(*spawneryv1alpha1.ProxyGroup)) *spawneryv1alpha1.ProxyGroup {
	f.t.Helper()
	group := &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: f.network.Name},
			Replicas:   2,
			Image:      "ghcr.io/spawnery/velocity:3.4.0-0.2.0",
			Expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			Routing: spawneryv1alpha1.RoutingSpec{FallbackGroups: []string{"lobby"}},
		},
	}
	for _, m := range mutate {
		m(group)
	}
	if err := f.c.Create(f.ctx, group); err != nil {
		f.t.Fatalf("create ProxyGroup: %v", err)
	}
	return group
}

func proxyGroupReconciler(f *fixture) *ProxyGroupReconciler {
	return &ProxyGroupReconciler{
		Client: f.c,
		Scheme: testenv.Scheme(f.t),
		Agents: f.agents,
		Bootstrap: &Bootstrapper{
			Client: f.c, Reader: f.c,
			CA: func() []byte { return []byte("test-ca") },
		},
		AgentEndpoint: "spawnery-operator.spawnery-system.svc:9443",
	}
}

func (f *fixture) reconcileProxyGroup(r *ProxyGroupReconciler, name string) {
	f.t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	}); err != nil {
		f.t.Fatalf("reconcile ProxyGroup %s: %v", name, err)
	}
}

func (f *fixture) proxyPods(group string) []corev1.Pod {
	f.t.Helper()
	pods := &corev1.PodList{}
	if err := f.c.List(f.ctx, pods, client.InNamespace(f.ns), client.MatchingLabels{
		podspec.LabelRole:  podspec.RoleProxy,
		podspec.LabelGroup: group,
	}); err != nil {
		f.t.Fatalf("list proxy pods: %v", err)
	}
	live := make([]corev1.Pod, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.DeletionTimestamp.IsZero() {
			live = append(live, p)
		}
	}
	return live
}

func (f *fixture) proxyGroup(name string) *spawneryv1alpha1.ProxyGroup {
	f.t.Helper()
	group := &spawneryv1alpha1.ProxyGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, group); err != nil {
		f.t.Fatalf("get ProxyGroup %s: %v", name, err)
	}
	return group
}

func TestProxyGroupCreatesItsPodsAndService(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want the group's 2 replicas", len(pods))
	}

	svc := &corev1.Service{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway", Namespace: f.ns}, svc); err != nil {
		t.Fatalf("get Service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("Service type = %q, want NodePort", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("Service ports = %+v, want exactly the Minecraft port", svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].Port != podspec.MinecraftPort || svc.Spec.Ports[0].NodePort != 30001 {
		t.Errorf("Service port = %+v, want 25565 on node port 30001", svc.Spec.Ports[0])
	}
	// A selector that does not match the pods produces a Service with no
	// endpoints — reachable, silent, and wrong.
	for k, v := range svc.Spec.Selector {
		if pods[0].Labels[k] != v {
			t.Errorf("Service selector %s=%q does not match the pods", k, v)
		}
	}
	if svc.Spec.Selector[podspec.LabelRole] != podspec.RoleProxy {
		t.Error("the Service selector must pin the proxy role, or it would also select server pods")
	}
}

// Milestone 6 owns the other two strategies. Until then the refusal has to be
// visible on the object rather than buried in a log line — a ProxyGroup that
// silently does nothing is indistinguishable from an operator that is down.
func TestProxyGroupRefusesLoadBalancer(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeLoadBalancer}
	})

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonExposeNotImplemented) {
		t.Errorf("conditions = %+v, want Accepted=False/%s",
			group.Status.Conditions, spawneryv1alpha1.ReasonExposeNotImplemented)
	}
	if n := len(f.proxyPods("gateway")); n != 0 {
		t.Errorf("proxy pods = %d, want none for a strategy that is not implemented", n)
	}
}

// With NodePort the address a player needs is a node's, and the operator has
// no right to read Node objects. hostIP on a running proxy pod is the address
// of a node that demonstrably has a proxy on it.
func TestProxyGroupAddressComesFromAReadyPodsHostIP(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("no proxy pods to mark ready")
	}
	pod := &pods[0]
	pod.Status.Phase = corev1.PodRunning
	pod.Status.HostIP = "192.168.1.10"
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(f.clock.Now()),
	}}
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")

	// Creating past the replica count on a steady-state resync is the primary
	// risk of a reconciler that manages pods directly rather than through a
	// per-pod CR: ReadyReplicas alone would not catch a reconciler that
	// created two more pods on this second pass instead of leaving the two
	// that already exist alone.
	if n := len(f.proxyPods("gateway")); n != 2 {
		t.Errorf("proxy pods = %d, want the steady-state resync to leave the count at 2", n)
	}

	group := f.proxyGroup("gateway")
	if group.Status.Address != "192.168.1.10:30001" {
		t.Errorf("status.address = %q, want 192.168.1.10:30001", group.Status.Address)
	}
	if group.Status.ReadyReplicas != 1 {
		t.Errorf("status.readyReplicas = %d, want 1", group.Status.ReadyReplicas)
	}
}

// Empty is the truthful answer while nothing is ready: there is nowhere to
// connect yet, and a node address for a proxy that is not serving would send
// players at a closed port.
func TestProxyGroupAddressIsEmptyWithNoReadyPod(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	if got := f.proxyGroup("gateway").Status.Address; got != "" {
		t.Errorf("status.address = %q, want empty while no proxy is ready", got)
	}
}

// Scale-down has to remove the newest proxies, not just any two of the
// three: an older proxy has had longer to collect players, and this
// milestone has no way to move them off before deleting it. A test that only
// counts survivors would stay green even if the comparator or the loop
// direction in reconcileReplicas were inverted.
func TestProxyGroupScalesDown(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 3
	})
	f.reconcileProxyGroup(r, "gateway")
	before := f.proxyPods("gateway")
	if len(before) != 3 {
		t.Fatalf("proxy pods = %d, want 3", len(before))
	}
	// Sorted the same way ProxyGroupReconciler.pods sorts its own list —
	// oldest first, ties broken by name — so the expected survivor is
	// determined the same way the reconciler determines it, not guessed.
	sort.Slice(before, func(i, j int) bool {
		if before[i].CreationTimestamp.Equal(&before[j].CreationTimestamp) {
			return before[i].Name < before[j].Name
		}
		return before[i].CreationTimestamp.Before(&before[j].CreationTimestamp)
	})
	oldest := before[0].Name

	group = f.proxyGroup("gateway")
	group.Spec.Replicas = 1
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("scale down: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")

	after := f.proxyPods("gateway")
	if len(after) != 1 {
		t.Fatalf("proxy pods = %d, want 1 after scaling down", len(after))
	}
	if after[0].Name != oldest {
		t.Errorf("survivor = %s, want the oldest pod %s — scale-down must remove the newest replicas first",
			after[0].Name, oldest)
	}
}

// The bootstrap has to run before the first pod, or the pod would mount a
// ConfigMap that does not exist and never start.
func TestProxyGroupBootstrapsTheNamespace(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	sa := &corev1.ServiceAccount{}
	key := types.NamespacedName{Name: podspec.ProxyServiceAccountName, Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, sa); err != nil {
		t.Fatalf("the proxy ServiceAccount was not bootstrapped: %v", err)
	}
}
