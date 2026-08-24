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
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/render"
	"github.com/spawnery/spawnery/internal/testenv"
)

// recordingFleet is the Fleet double these tests need: it remembers the last
// readiness asserted for each pod UID, which is exactly what
// ProxyReadinessSetter's one method commits to. No pre-existing double did
// this — Task 2's Fleet tests exercise the real proxyreg.Fleet against a live
// stream, which these envtest-only pod-state tests have no need for.
type recordingFleet struct {
	mu   sync.Mutex
	last map[string]bool
}

// SetReady implements ProxyReadinessSetter.
func (f *recordingFleet) SetReady(_ context.Context, podUID string, ready bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.last = map[string]bool{}
	}
	f.last[podUID] = ready
	return nil
}

// lastReady returns the last readiness asserted for podUID, or nil if none
// was ever asserted.
func (f *recordingFleet) lastReady(podUID string) *bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.last[podUID]
	if !ok {
		return nil
	}
	return &v
}

// proxyGroupConfigMap re-reads the ConfigMap a ProxyGroupReconciler renders
// for the named group.
func (f *fixture) proxyGroupConfigMap(t *testing.T, group string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: podspec.GroupConfigMapName(group, podspec.RoleProxy), Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, cm); err != nil {
		t.Fatalf("get ConfigMap for group %s: %v", group, err)
	}
	return cm
}

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
		Proxies:       f.proxies,
		Clock:         f.clock.Now,
		Expectations:  newExpectations(f.clock.Now),
		Divergence:    newReadinessDivergence(f.clock.Now),
		// Every reconciler helper in this package wires a recorder whether or
		// not the test under it looks at one, so that a path which only fires
		// under an unusual condition — here, a drain that ran out of time —
		// cannot nil-dereference in a test that stumbled into it. A test that
		// wants to read the events replaces this with its own handle.
		Recorder: events.NewFakeRecorder(100),
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

// sortPodsOldestFirst orders pods the same way ProxyGroupReconciler.pods
// orders its own live list — oldest first, ties broken by name — so "the
// surplus pod" in a test is determined the way the reconciler determines it,
// not guessed.
func sortPodsOldestFirst(pods []corev1.Pod) {
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].CreationTimestamp.Equal(&pods[j].CreationTimestamp) {
			return pods[i].Name < pods[j].Name
		}
		return pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})
}

// setProxyReplicas updates spec.replicas on an already-created ProxyGroup.
func (f *fixture) setProxyReplicas(name string, n int32) {
	f.t.Helper()
	group := f.proxyGroup(name)
	group.Spec.Replicas = n
	if err := f.c.Update(f.ctx, group); err != nil {
		f.t.Fatalf("set replicas of %s to %d: %v", name, n, err)
	}
}

func (f *fixture) proxyGroup(name string) *spawneryv1alpha1.ProxyGroup {
	f.t.Helper()
	group := &spawneryv1alpha1.ProxyGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, group); err != nil {
		f.t.Fatalf("get ProxyGroup %s: %v", name, err)
	}
	return group
}

// watchThroughGrace runs the passes a real operator would run across one
// readinessDivergenceGrace: a reconcile every resyncInterval, which is what
// ProxyGroupReconciler requeues at on its successful path.
//
// It exists because readinessDivergence measures *watched* time. A single
// clock.Advance past the grace followed by one reconcile is a minute during
// which nothing observed the group, and the tracker declines to count that --
// deliberately, since the alternative is a Warning about a stretch nobody saw.
// Before it did, this shortcut passed, which means these tests would also have
// passed had the divergence gone entirely unobserved for the whole grace.
func (f *fixture) watchThroughGrace(t *testing.T, r *ProxyGroupReconciler, name string) {
	t.Helper()
	for elapsed := time.Duration(0); elapsed <= readinessDivergenceGrace; elapsed += resyncInterval {
		f.clock.Advance(resyncInterval)
		f.reconcileProxyGroup(r, name)
	}
}

// proxyPodHostIP is the node address markProxyPodReady puts on a ready proxy
// pod, named so the one test that reads it back out of status.address does not
// have to repeat the literal.
const proxyPodHostIP = "192.168.1.10"

// markProxyPodReady does to a proxy pod what a kubelet would once its probe
// passed: Running, on a node, and Ready. f.markReady is not this — it is for
// Servers, and goes through bringUpNamed.
//
// The hostIP is here because one caller needs it:
// TestProxyGroupAddressComesFromAReadyPodsHostIP, which is where this block was
// written inline before the rollout tests needed the same thing for every pod
// in a group. Its address assertion reads that value back out of
// status.address, so the constant is load-bearing there and inert everywhere
// else — the rollout tests never look at the address.
func (f *fixture) markProxyPodReady(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	pod.Status.Phase = corev1.PodRunning
	pod.Status.HostIP = proxyPodHostIP
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(f.clock.Now()),
	}}
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		t.Fatalf("mark proxy pod %s ready: %v", pod.Name, err)
	}
}

// setProxyPodReadyCondition flips just the PodReady condition, the way a
// kubelet would once a probe's answer changed -- unlike markProxyPodReady it
// does not also stamp Phase or HostIP, because the callers that need this
// (toggling a pod between Ready and NotReady more than once, to move it in
// and out of agreement with what the operator asserted) already ran
// markProxyPodReady once and have no reason to touch either again.
func (f *fixture) setProxyPodReadyCondition(t *testing.T, pod *corev1.Pod, ready bool) {
	t.Helper()
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: status,
		LastTransitionTime: metav1.NewTime(f.clock.Now()),
	}}
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		t.Fatalf("set proxy pod %s ready=%v: %v", pod.Name, ready, err)
	}
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
	f.markProxyPodReady(t, &pods[0])

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
	// The host half is the hostIP markProxyPodReady puts on the pod; the port
	// half is the node port the API server allocated, read off the Service
	// because that is now where proxyAddress reads it. Both sides are read
	// back from where they were written, not hardcoded twice.
	svc := &corev1.Service{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway", Namespace: f.ns}, svc); err != nil {
		t.Fatalf("get the group's Service: %v", err)
	}
	want := fmt.Sprintf("%s:%d", proxyPodHostIP, allocatedNodePort(svc))
	if group.Status.Address != want {
		t.Errorf("status.address = %q, want %s", group.Status.Address, want)
	}
	if group.Status.ReadyReplicas != 1 {
		t.Errorf("status.readyReplicas = %d, want 1", group.Status.ReadyReplicas)
	}
}

// TestProxyGroupCreateCountIsCutByAReservationTheCacheHasNotShown pins the
// correction reconcileReplicas applies on top of DecideRollout's answer: a
// create already reserved and not yet visible in the list must not be created
// again.
//
// The reservation here is a phantom -- a name no pod will ever carry --
// rather than a real generated one, and that is deliberate: a full Reconcile
// calls pods() twice (once before reconcileReplicas, once after for status),
// and testenv's client talks straight to envtest's API server with no
// informer cache in front of it, so a real reservation made during
// reconcileReplicas would already be observed and cleared by that second
// pods() call before this function returns. A phantom name is what stays
// pending across both listings, which is what this test needs to observe the
// cap actually subtract. TestProxyGroupReconcileReplicasReservesTheRealPodItCreated
// below is the complementary test: it stops between the two pods() calls and
// checks the real name, real key chain this one does not exercise.
func TestProxyGroupCreateCountIsCutByAReservationTheCacheHasNotShown(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway") // replicas: 2

	r.Expectations.expectCreated(f.ns+"/gateway", "gateway-phantom")

	f.reconcileProxyGroup(r, "gateway")

	// DecideRollout sees 0 live pods against a target of 2 and answers
	// Create: 2. Without the cap this pass creates both, landing on 2; the
	// phantom reservation cuts it to one real pod.
	if n := len(f.proxyPods("gateway")); n != 1 {
		t.Errorf("proxy pods = %d, want 1: the phantom reservation should have "+
			"cut DecideRollout's Create: 2 down by the one already pending", n)
	}
}

// TestProxyGroupReconcileReplicasReservesTheRealPodItCreated exercises the
// reservation's whole lifecycle with a name the API server actually assigned:
// expectCreated fires with the pod's real generated name under the real
// composite key, that reservation is still pending immediately afterward,
// and a second pods() call -- the same one Reconcile's status pass makes --
// clears it. Clearing is already covered structurally: the cap test's real
// pod has to clear through the second pods() for its arithmetic to hold, and
// the nine-victim mutation on observePods's create arm shows the rollout
// suite cannot pass without real-name clearing working. This test names that
// property directly, so a regression here is diagnosed as a clearing failure
// rather than rediscovered through unrelated rollout test failures.
//
// Not driven through f.reconcileProxyGroup / a full Reconcile: Reconcile
// calls pods() twice (once before reconcileReplicas, once after for status),
// and both calls run observePods. With testenv's non-cached client and
// reconcileReplicas completing without error -- the path both this test and
// the one above take -- the second pods() call sees the very pods
// reconcileReplicas just created and clears their reservations before
// Reconcile returns, so a full Reconcile on that path cannot catch a
// reservation mid-flight, real name or not. That is why
// TestProxyGroupCreateCountIsCutByAReservationTheCacheHasNotShown above has
// to use a name that can never appear in the API server: the real window this
// test targets sits strictly between the two pods() calls, and on the
// no-error path is only reachable by calling pods() and reconcileReplicas()
// directly and reading pending() before anything else runs.
func TestProxyGroupReconcileReplicasReservesTheRealPodItCreated(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway") // replicas: 2

	pods, err := r.pods(f.ctx, group)
	if err != nil {
		t.Fatalf("pods: %v", err)
	}
	if len(pods) != 0 {
		t.Fatalf("pods before any reconcile = %d, want 0", len(pods))
	}
	if err := r.reconcileReplicas(f.ctx, f.network, group, pods); err != nil {
		t.Fatalf("reconcileReplicas: %v", err)
	}

	created := f.proxyPods("gateway")
	if len(created) != 2 {
		t.Fatalf("proxy pods created = %d, want the group's 2 replicas", len(created))
	}

	key := f.ns + "/gateway"
	pendingCreates, _, _ := r.Expectations.pending(key)
	if len(pendingCreates) != len(created) {
		t.Errorf("pending creates = %v, want %d: reconcileReplicas must reserve "+
			"every pod it actually created, under that pod's real generated name "+
			"and the real composite key -- not a name or key this test made up",
			pendingCreates, len(created))
	}

	// The other half of the lifecycle: the status pass's pods() call is what
	// actually clears a reservation once the cache -- real or, as here,
	// immediate -- shows the pod. Without this second call the assertion
	// above only proves expectCreated fires; it says nothing about
	// observePods ever being satisfied by what expectCreated recorded.
	if _, err := r.pods(f.ctx, group); err != nil {
		t.Fatalf("pods (second call): %v", err)
	}
	if pendingCreates, _, _ := r.Expectations.pending(key); len(pendingCreates) != 0 {
		t.Errorf("pending creates = %v after the second pods() call, want empty: "+
			"observePods must clear a reservation once its pod is listed", pendingCreates)
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

// Scale-down has to remove the emptiest proxies, not a slice of the list they
// happen to sit in.
//
// This test's expectation has now moved twice, and both moves are the point.
// It first expected a single survivor: it connected no agent, every pod
// therefore reported zero players, and the old bare `players == 0` deleted both
// surplus pods on the spot. Task 5 made a count believable only while it is
// fresh and its stream is still up — an agent that has reported nothing and an
// agent that died with players on it are the same zero — so the pod with no
// agent had to be kept, and there were two survivors.
//
// The second move is this milestone's: which pods are surplus is no longer the
// tail of a list ordered by age but DecideRollout's answer, and it picks by
// occupancy. Scaling three down to one now marks the two pods that are known
// empty — the oldest and the newest here, position notwithstanding — and keeps
// the one nobody can vouch for. Both marked pods are empty, so both go on the
// same pass and the group reaches the size that was asked for, having
// disconnected nobody.
//
// The pod with no agent is what keeps the grip: it is named as the survivor
// rather than counted, so a selection that stopped distinguishing a fresh
// count from an untrusted one leaves a different pod standing and fails here —
// checked by running that mutation, which also takes eight other tests with it.
//
// It does not pin pick's player comparison, and the first draft of this comment
// claimed it did. Every count here is zero or unknown, so freshness alone
// decides the order and dropping the comparison changes nothing. What holds
// that term is TestSurplusProxyIsToldToStopTakingConnections, where the two
// counts differ.
func TestProxyGroupScalesDown(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
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
	sortPodsOldestFirst(before)
	oldest, unreported, newest := before[0].Name, before[1].Name, before[2].Name
	f.reportProxyPlayers(t, before[2], 0)
	// The oldest pod gets a fresh zero too, and it is what makes this test
	// about occupancy rather than about position: it sits at the head of the
	// list the reconciler walks, where a rule that took its surplus off the
	// tail would leave it, and the report is what puts it among the two that
	// go.
	f.reportProxyPlayers(t, before[0], 0)

	group := f.proxyGroup("gateway")
	group.Spec.Replicas = 1
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("scale down: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")

	after := f.proxyPods("gateway")
	if len(after) != 1 {
		t.Fatalf("proxy pods = %d, want 1 after scaling down — both known-empty proxies must go and the pod with "+
			"no player count must be kept", len(after))
	}
	if _, ok := f.pod(newest); ok {
		t.Errorf("the newest pod %s survived; it reported a fresh zero, so it is one of the two emptiest and must go",
			newest)
	}
	if _, ok := f.pod(oldest); ok {
		t.Errorf("the oldest pod %s survived; it reported a fresh zero, and being first in the list is not a reason "+
			"to keep a proxy nobody is on", oldest)
	}
	if _, ok := f.pod(unreported); !ok {
		t.Errorf("the pod %s was removed on an unknown player count; an agent that has reported nothing "+
			"is indistinguishable from one that died with players on it, and both must be treated as occupied",
			unreported)
	}
}

// TestAProxyWithAStalePlayerCountWaitsForTheDeadline is the other half of the
// occupancy rule: the wait it buys is bounded.
//
// A proxy whose agent never appears, or whose stream broke and never came
// back, would otherwise be kept forever by "unknown counts as occupied" —
// nothing would ever arrive to say it was empty, and a scale-down would
// silently never complete. The deadline is what makes treating unknown as
// occupied safe to do rather than merely cautious.
func TestAProxyWithAStalePlayerCountWaitsForTheDeadline(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	surplus := before[1]

	// No agent is connected for this pod at all, so the registry reports it
	// exactly as it reports one whose stream died with players on it.
	if snap := f.agents.Lookup(string(surplus.UID)); !snap.PlayersStale {
		t.Fatalf("registry reports a fresh count for a proxy with no agent: %+v", snap)
	}

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	if _, ok := f.pod(surplus.Name); !ok {
		t.Fatal("the surplus proxy was deleted on an unknown player count")
	}

	f.clock.Advance(f.proxyGroup("gateway").DrainTimeout() + time.Second)
	f.reconcileProxyGroup(r, "gateway")

	if got := len(f.proxyPods("gateway")); got != 1 {
		t.Errorf("proxy pods = %d after the deadline, want 1 — an unknown count must not hold a pod forever", got)
	}
}

// The bootstrap has to run before the first pod, or the pod would mount a
// ConfigMap that does not exist and never start.
func TestProxyGroupBootstrapsTheNamespace(t *testing.T) {
	f := newFixture(t)

	// newFixture's own Network reconcile already bootstrapped this namespace,
	// so the CA ConfigMap and both ServiceAccounts are here before this test
	// body runs. Remove them, or the assertion below reads the fixture's setup
	// and stays green with this reconciler's own Ensure call deleted.
	if err := f.c.Delete(f.ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: podspec.CAConfigMapName, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("delete the fixture's CA ConfigMap: %v", err)
	}
	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		if err := f.c.Delete(f.ctx, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		}); err != nil {
			t.Fatalf("delete the fixture's %s ServiceAccount: %v", name, err)
		}
	}

	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	sa := &corev1.ServiceAccount{}
	key := types.NamespacedName{Name: podspec.ProxyServiceAccountName, Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, sa); err != nil {
		t.Fatalf("the proxy ServiceAccount was not bootstrapped: %v", err)
	}
	cm := &corev1.ConfigMap{}
	if err := f.c.Get(f.ctx, types.NamespacedName{
		Name: podspec.CAConfigMapName, Namespace: f.ns,
	}, cm); err != nil {
		t.Fatalf("the CA ConfigMap was not bootstrapped: %v", err)
	}
}

// TestProxyGroupRendersConfigMap covers design section 5.4's promise for the
// proxy side: one ConfigMap per group, owned by it, carrying the label the
// manager's restricted cache requires, and holding exactly what spec.config
// says — not merely a ConfigMap that exists under the right name.
func TestProxyGroupRendersConfigMap(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Config = &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 500, Motd: "Welcome to Spawnery"}
	})

	f.reconcileProxyGroup(r, "gateway")

	cm := f.proxyGroupConfigMap(t, "gateway")
	if cm.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Errorf("labels = %+v, want %s=%s so the restricted cache can see this ConfigMap",
			cm.Labels, podspec.LabelManagedBy, podspec.ManagedByValue)
	}
	if len(cm.OwnerReferences) != 1 ||
		cm.OwnerReferences[0].Kind != "ProxyGroup" ||
		cm.OwnerReferences[0].Controller == nil || !*cm.OwnerReferences[0].Controller {
		t.Errorf("owner references = %+v, want a ProxyGroup controller ref", cm.OwnerReferences)
	}

	raw, ok := cm.Data[podspec.ConfigValuesKey]
	if !ok {
		t.Fatalf("data = %+v, want a %s key", cm.Data, podspec.ConfigValuesKey)
	}
	var values render.Values
	if err := yaml.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatalf("%s does not parse as render.Values: %v", podspec.ConfigValuesKey, err)
	}
	if values.PlayerLimit == nil || *values.PlayerLimit != 500 {
		t.Errorf("playerLimit = %v, want 500", values.PlayerLimit)
	}
	if values.Motd == nil || *values.Motd != "Welcome to Spawnery" {
		t.Errorf("motd = %v, want %q", values.Motd, "Welcome to Spawnery")
	}
	// Nothing in ProxyGroupSpec could produce maxPlayers, but a future change
	// reaching for a critical field directly on Values would slip past a
	// test that only checked playerLimit and motd.
	if values.MaxPlayers != nil {
		t.Errorf("values = %+v, want maxPlayers unset — a ProxyGroup has no maxPlayers", values)
	}
}

// TestProxyGroupWithNoConfigStillStartsAProxy is the regression test for the
// gap a per-task review could not see: spec.config is +optional, and
// spec.config.playerLimit is +optional inside it, so createProxyGroup's own
// default shape — no mutate function, exactly what "gateway" is above —
// used to leave the rendered ConfigMap with playerLimit unset. podspec always
// defaulted the pod's own SPAWNERY_PLAYER_LIMIT env var, so a real cluster
// showed a ProxyGroup that was Accepted, had its Service, and had pods stuck
// in CrashLoopBackOff reading "config.yaml: playerLimit is not set" — nothing
// on the CR said why. This carries the ConfigMap this reconciler actually
// writes all the way into render.Velocity, the way spawnery-config itself
// would read it, rather than stopping at "the ConfigMap exists".
func TestProxyGroupWithNoConfigStillStartsAProxy(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	cm := f.proxyGroupConfigMap(t, "gateway")
	var values render.Values
	if err := yaml.Unmarshal([]byte(cm.Data[podspec.ConfigValuesKey]), &values); err != nil {
		t.Fatalf("%s does not parse as render.Values: %v", podspec.ConfigValuesKey, err)
	}
	if values.PlayerLimit == nil {
		t.Fatal("playerLimit is nil: a ProxyGroup that sets no spec.config must still get the default, not silence")
	}
	if *values.PlayerLimit != podspec.DefaultPlayerLimit {
		t.Errorf("playerLimit = %d, want the default %d", *values.PlayerLimit, podspec.DefaultPlayerLimit)
	}

	// The same gap on the field where landing in it would be a security
	// failure rather than a crash loop. spec.config.onlineMode carries a CRD
	// default of true, but a nil spec.config never reaches that default at
	// all, so the only thing standing between "a user wrote no config block"
	// and "the proxy authenticates nobody" is proxyConfigValues writing the
	// key itself. render.Velocity refuses a nil rather than guessing, so a
	// regression here is at least loud — but loud in a CrashLoopBackOff, which
	// is exactly the failure the playerLimit half of this test already
	// documents as invisible from the CR.
	if values.OnlineMode == nil {
		t.Fatal("onlineMode is nil: a ProxyGroup that sets no spec.config must still say whether its proxy authenticates players")
	}
	if !*values.OnlineMode {
		t.Error("onlineMode = false for a ProxyGroup that never asked for it; the proxy authenticates nobody and anyone may connect under any name")
	}

	rendered, err := render.Velocity(values, "/etc/spawnery/forwarding.secret", nil)
	if err != nil {
		t.Fatalf("render.Velocity refused the ConfigMap a no-config ProxyGroup renders: %v", err)
	}
	if !strings.Contains(string(rendered["velocity.toml"]), "online-mode = true") {
		t.Errorf("velocity.toml does not authenticate players:\n%s", rendered["velocity.toml"])
	}
}

// The CRD default, exercised through a real API server rather than read off
// the marker. spec.config exists but names only playerLimit, which is the
// shape every sample and every pre-existing ProxyGroup has: the +kubebuilder
// default is what has to put onlineMode: true in it, and a marker that was
// dropped or written on the wrong field would leave this nil.
func TestProxyGroupDefaultsOnlineModeOnAPartialConfig(t *testing.T) {
	f := newFixture(t)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Config = &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 500}
	})

	group := f.proxyGroup("gateway")
	if group.Spec.Config.OnlineMode == nil {
		t.Fatal("spec.config.onlineMode is nil after a round trip through the API server; the CRD default did not apply")
	}
	if !*group.Spec.Config.OnlineMode {
		t.Error("spec.config.onlineMode defaulted to false; a ProxyGroup that never mentioned it must authenticate players")
	}
}

// And the field is a switch, not decoration: what a user sets has to reach
// velocity.toml. A ProxyGroup that asks for an offline-mode proxy and gets an
// authenticating one is the failure that makes the milestone's automated join
// proof impossible, because a Go client cannot authenticate against Microsoft.
func TestProxyGroupCarriesOnlineModeFalseIntoTheRenderedProxy(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	off := false
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Config = &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 500, OnlineMode: &off}
	})

	f.reconcileProxyGroup(r, "gateway")

	cm := f.proxyGroupConfigMap(t, "gateway")
	var values render.Values
	if err := yaml.Unmarshal([]byte(cm.Data[podspec.ConfigValuesKey]), &values); err != nil {
		t.Fatalf("%s does not parse as render.Values: %v", podspec.ConfigValuesKey, err)
	}
	if values.OnlineMode == nil || *values.OnlineMode {
		t.Fatalf("onlineMode = %v, want false: spec.config.onlineMode did not reach the ConfigMap", values.OnlineMode)
	}

	rendered, err := render.Velocity(values, "/etc/spawnery/forwarding.secret", nil)
	if err != nil {
		t.Fatalf("render.Velocity: %v", err)
	}
	if !strings.Contains(string(rendered["velocity.toml"]), "online-mode = false") {
		t.Errorf("velocity.toml still authenticates players:\n%s", rendered["velocity.toml"])
	}
}

// TestProxyGroupConfigMapUpdatesOnSpecChange guards against a renderer that
// only runs once: correct at creation but never revisited would look
// identical until the day an operator actually edits spec.config.
func TestProxyGroupConfigMapUpdatesOnSpecChange(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Config = &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 500, Motd: "Welcome"}
	})
	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	group.Spec.Config = &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 750, Motd: "Now hiring"}
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("update ProxyGroup: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")

	cm := f.proxyGroupConfigMap(t, "gateway")
	var values render.Values
	if err := yaml.Unmarshal([]byte(cm.Data[podspec.ConfigValuesKey]), &values); err != nil {
		t.Fatalf("unmarshal after update: %v", err)
	}
	if values.PlayerLimit == nil || *values.PlayerLimit != 750 {
		t.Errorf("playerLimit after the edit = %v, want 750", values.PlayerLimit)
	}
	if values.Motd == nil || *values.Motd != "Now hiring" {
		t.Errorf("motd after the edit = %v, want %q", values.Motd, "Now hiring")
	}
}

// TestProxyGroupConfigMapWrittenBeforeThePods proves the ordering the design
// depends on: a proxy pod's projected volume names this ConfigMap by group,
// so the ConfigMap must exist before the first proxy pod. Reading back the
// final state after a reconcile cannot distinguish "written first" from
// "written at some point" — both leave the same objects sitting there.
// Recording the actual Create calls can.
func TestProxyGroupConfigMapWrittenBeforeThePods(t *testing.T) {
	f := newFixture(t)
	recorder := &createOrderRecorder{Client: f.c}
	r := &ProxyGroupReconciler{
		Client: recorder,
		Scheme: testenv.Scheme(t),
		Agents: f.agents,
		Bootstrap: &Bootstrapper{
			Client: recorder, Reader: f.c,
			CA: func() []byte { return []byte("test-ca") },
		},
		AgentEndpoint: "spawnery-operator.spawnery-system.svc:9443",
		Proxies:       f.proxies,
		Clock:         f.clock.Now,
		Expectations:  newExpectations(f.clock.Now),
		Divergence:    newReadinessDivergence(f.clock.Now),
		// This literal builds its own reconciler rather than taking the
		// fixture's, so it has to repeat what newFixture's helper does — see
		// the comment there. Without it this test kills a mutation on the
		// divergence event by nil-dereferencing, which is a coincidence of
		// this literal's shape rather than anything the test asserts.
		Recorder: events.NewFakeRecorder(100),
	}
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	cmIdx := recorder.indexOf(fmt.Sprintf("%T/%s", &corev1.ConfigMap{}, podspec.GroupConfigMapName("gateway", podspec.RoleProxy)))
	podIdx := recorder.indexOf(fmt.Sprintf("%T/%s-", &corev1.Pod{}, "gateway"))
	if cmIdx == -1 {
		t.Fatalf("no ConfigMap create was recorded")
	}
	if podIdx == -1 {
		t.Fatalf("no pod create was recorded")
	}
	if cmIdx >= podIdx {
		t.Errorf("ConfigMap created at position %d, pod at %d — want the ConfigMap first: "+
			"a pod's projected volume names it, and does not start if it is missing", cmIdx, podIdx)
	}
}

// TestSurplusProxyIsToldToStopTakingConnections proves the operator asserts
// readiness for every proxy pod, not just the ones being removed: a surplus
// pod is told ready=false and every survivor is told ready=true, both on the
// same pass that discovers the surplus.
//
// The two player counts are what decide which pod is which, and they replaced
// the pods' positions when this milestone made the surplus DecideRollout's
// answer rather than the tail of a list. They are the honest version of the
// setup either way: the pod the group keeps is the one with somebody on it,
// and the test no longer turns on how ties between two indistinguishable pods
// happen to break.
func TestSurplusProxyIsToldToStopTakingConnections(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	survivor, surplus := before[0], before[1]
	f.reportProxyPlayers(t, survivor, 1)
	f.reportProxyPlayers(t, surplus, 0)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	if got := f.proxies.lastReady(string(surplus.UID)); got == nil || *got {
		t.Errorf("the surplus proxy was told ready=%v, want false", got)
	}
	if got := f.proxies.lastReady(string(survivor.UID)); got == nil || !*got {
		t.Errorf("the surviving proxy was told ready=%v, want true", got)
	}
}

// TestSurplusProxyIsMarkedWithWhenTheDrainStarted proves the deadline is
// written down: without it, nothing bounds how long the drain may run.
//
// The surplus proxy is given a player first, and that is load-bearing rather
// than colour. Until Task 5 these tests wrapped the client in a fake that
// swallowed Delete, because reconcileReplicas removed the surplus pod outright
// in the very Reconcile that stamped it — a pod here never leaves Pending,
// envtest runs no kubelet to move it further, and envtest's apiserver does not
// hold an unscheduled pod back for a grace period the way it would a running
// one, so there was no object left to read the annotation off. Now the wait is
// real, and the thing that makes a draining pod linger is the thing that makes
// it linger in a cluster: somebody is still on it. Reporting a player is what
// retires the fake, and it is strictly the stronger test — with Delete
// suppressed, this test could not have told a working wait from no wait at
// all.
func TestSurplusProxyIsMarkedWithWhenTheDrainStarted(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	sortPodsOldestFirst(before)
	surplusName := before[1].Name
	f.reportProxyPlayers(t, before[1], 1)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	surplus, ok := f.pod(surplusName)
	if !ok {
		t.Fatalf("surplus pod %s not found", surplusName)
	}
	at, ok := surplus.Annotations[ProxyDrainingSinceAnnotation]
	if !ok {
		t.Fatal("the surplus proxy carries no draining-since annotation; the deadline has nothing to run from")
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("draining-since = %q, want RFC 3339: %v", at, err)
	}
}

// TestMarkDrainingDoesNotMoveAnExistingMark pins markDraining's own guard: the
// deadline runs from the first assertion, and re-stamping it on a later call
// would push the deadline forever and the drain would never end.
//
// This calls markDraining directly rather than driving two full Reconcile
// passes over a scaled-down group, and the reason is narrowness, not
// impossibility: two passes would work now that Task 5's wait exists, given a
// reported player to hold the pod open across them. That is the point — the
// rationale that used to stand here said two passes would prove nothing
// because the surplus pod was deleted in the same reconcile that marked it,
// which was true when it was written and which this task falsified.
//
// What is left is a straightforward trade. The direct call is the exact call
// reconcileReplicas makes once per live pod per pass, it exercises the guard
// without a scale-down, a player report and two full reconciles standing
// between the test and the thing it is testing, and it does not silently
// depend on the deletion rule staying as it is today. The pod is re-read from
// the API server between the two calls, so the guard still reacts to what was
// persisted rather than to what this test holds in memory.
func TestMarkDrainingDoesNotMoveAnExistingMark(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("no proxy pods to drain")
	}
	name := pods[0].Name

	pod, ok := f.pod(name)
	if !ok {
		t.Fatalf("pod %s not found", name)
	}
	if err := r.markDraining(f.ctx, pod, true); err != nil {
		t.Fatalf("markDraining: %v", err)
	}
	first := pod.Annotations[ProxyDrainingSinceAnnotation]
	if first == "" {
		t.Fatal("markDraining did not stamp the annotation")
	}

	f.clock.Advance(time.Minute)

	pod, ok = f.pod(name)
	if !ok {
		t.Fatalf("pod %s not found on the later pass", name)
	}
	if err := r.markDraining(f.ctx, pod, true); err != nil {
		t.Fatalf("markDraining on the later pass: %v", err)
	}
	if got := pod.Annotations[ProxyDrainingSinceAnnotation]; got != first {
		t.Errorf("draining-since moved from %q to %q", first, got)
	}
}

// setDrainingSince writes the draining-since annotation straight onto a pod,
// bypassing markDraining, the way anything else with pod write access could.
func (f *fixture) setDrainingSince(name, value string) {
	f.t.Helper()
	pod, ok := f.pod(name)
	if !ok {
		f.t.Fatalf("pod %s not found", name)
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[ProxyDrainingSinceAnnotation] = value
	if err := f.c.Update(f.ctx, pod); err != nil {
		f.t.Fatalf("set %s on %s: %v", ProxyDrainingSinceAnnotation, name, err)
	}
}

// TestAnUnparsableDrainingSinceDoesNotWedgeTheGroup covers the annotation's
// one user-writable failure: it lives on a pod, so anybody who can write a pod
// annotation can put something in it that is not RFC 3339.
//
// Returning an error for that aborted the entire Reconcile — no Service, no
// status, no other pod's readiness assertion, and the corrupt pod never
// deleted — on every pass, permanently, because nothing downstream of the
// error ever rewrote the value and controller-runtime simply retried forever.
// One pod's bad annotation stopped a whole group converging.
//
// The recovery is a re-stamp rather than tolerance: the value must end up
// readable, or the deadline still has nothing to run from. The pod pays a
// restarted clock for it, which is asserted below as the deletion happening
// after a full drain timeout measured from the repair.
func TestAnUnparsableDrainingSinceDoesNotWedgeTheGroup(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	survivor, surplus := before[0], before[1]
	// A player on the surplus pod so the wait holds it long enough to be
	// looked at, and so the corrupt annotation is on a pod the deletion loop
	// actually reaches rather than one it skips.
	f.reportProxyPlayers(t, surplus, 1)
	f.setDrainingSince(surplus.Name, "yesterday afternoon")

	f.setProxyReplicas("gateway", 1)
	// f.reconcileProxyGroup fails the test on a returned error, which is the
	// wedge itself: before the fix this line never got past the pod carrying
	// the bad stamp.
	f.reconcileProxyGroup(r, "gateway")

	// The rest of the group converged rather than being taken down with the
	// one bad pod.
	if got := f.proxies.lastReady(string(survivor.UID)); got == nil || !*got {
		t.Errorf("the surviving proxy was told ready=%v, want true — one pod's bad annotation stopped the whole group",
			got)
	}
	if got := f.proxyGroup("gateway").Status.ObservedGeneration; got != f.proxyGroup("gateway").Generation {
		t.Errorf("status.observedGeneration = %d, want the current generation — the status write is downstream of the wedge", got)
	}

	drained, ok := f.pod(surplus.Name)
	if !ok {
		t.Fatal("the draining proxy was deleted with a player on it")
	}
	at, ok := drained.Annotations[ProxyDrainingSinceAnnotation]
	if !ok {
		t.Fatal("the unparsable draining-since was removed rather than replaced; the deadline has nothing to run from")
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("draining-since = %q, want a readable stamp: a value nothing rewrites is a value that stays wrong forever", at)
	}

	// And the replacement is a working deadline, not just a readable string.
	f.clock.Advance(f.proxyGroup("gateway").DrainTimeout() + time.Second)
	f.reconcileProxyGroup(r, "gateway")
	if _, ok := f.pod(surplus.Name); ok {
		t.Error("the repaired pod outlived its drain timeout; the re-stamped deadline never fired")
	}
}

// TestACancelledScaleDownPutsTheProxyBack proves readiness is derived, not
// remembered: an operator restart, or simply a later pass, recomputes which
// pods are surplus from scratch, so a scale-down that is reversed before the
// surplus pod is actually gone corrects itself with nothing left to clean
// up.
//
// The player on the surplus proxy is what keeps the same pod in play across
// both passes, which is what makes this test about "derived, not remembered"
// rather than about a fresh replacement pod happening to start out ready. It
// replaces the Delete-swallowing fake this test used before Task 5, for the
// reason TestSurplusProxyIsMarkedWithWhenTheDrainStarted spells out: the pod
// now survives because the operator waits for it, not because a double hid
// the deletion.
func TestACancelledScaleDownPutsTheProxyBack(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	sortPodsOldestFirst(before)
	surplus := before[1]
	f.reportProxyPlayers(t, surplus, 1)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	// The mark must actually be there to begin with: otherwise the absence
	// asserted below would pass just as well against a markDraining whose
	// write branch had been deleted entirely.
	drained, ok := f.pod(surplus.Name)
	if !ok {
		t.Fatalf("pod %s not found after the scale-down", surplus.Name)
	}
	if _, ok := drained.Annotations[ProxyDrainingSinceAnnotation]; !ok {
		t.Fatal("the surplus proxy carries no draining-since annotation after the scale-down; nothing for the cancel to remove")
	}

	f.setProxyReplicas("gateway", 2)
	f.reconcileProxyGroup(r, "gateway")

	pod, ok := f.pod(surplus.Name)
	if !ok {
		t.Fatalf("pod %s not found after the scale-down was cancelled", surplus.Name)
	}
	if got := f.proxies.lastReady(string(pod.UID)); got == nil || !*got {
		t.Errorf("the proxy was told ready=%v after the scale-down was cancelled, want true", got)
	}
	if _, ok := pod.Annotations[ProxyDrainingSinceAnnotation]; ok {
		t.Error("the draining-since annotation outlived the drain")
	}
}

// racingPodClient simulates a pod that was evicted or deleted between the
// informer's list and markDraining's patch: Patch on the named pod returns
// NotFound, exactly as the real client would if the object were already gone
// server-side.
//
// It overrides nothing else. It used to swallow Delete as well, so that the
// pods reconcileReplicas walked would survive to be inspected. Delete now
// reaches the real client, and neither surplus pod is removed on this pass —
// each for its own reason, and both of them the operator's real behaviour
// rather than a fake's:
//
//   - the inspected pod has a player reported on it, so the wait holds it;
//   - the racing pod has no player count at all, because nothing reports one
//     for it, and an unknown count is treated as occupied. Its deadline has
//     not started either: markDraining's patch is the call this fake fails, so
//     no draining-since was ever persisted and nothing can expire.
//
// The second bullet is the whole point of the rule this milestone added, so it
// is asserted at the end of the test rather than left as a claim here. A
// comment is not a test, and this one described the deletion rule backwards
// until the reviewer traced it.
// fired records whether the fake ever got to do its job. Without it this test
// cannot tell the tolerance working from the racing pod never being patched at
// all, and it has already been both: the pod it names went from marked to
// unmarked when the drain stopped being chosen by position, and every assertion
// went on passing. A fake that is never reached is a test asserting nothing, so
// this one says so out loud.
type racingPodClient struct {
	client.Client
	racingPod string
	fired     *bool
}

// Patch implements client.Client.
func (c racingPodClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if obj.GetName() == c.racingPod {
		*c.fired = true
		return apierrors.NewNotFound(corev1.Resource("pods"), obj.GetName())
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// TestAPodVanishingBetweenListAndPatchDoesNotFailTheReconcile proves
// markDraining tolerates the same race every other API call in
// reconcileReplicas already tolerates: Create ignores AlreadyExists, Delete
// ignores NotFound, and Fleet.SetReady is a no-op for a pod it has no
// session for. A pod evicted or deleted between the informer's list and this
// patch must not abort the whole reconcile — the Service, the status, and
// the other pods' assertions along with it — over a stamp that no longer
// matters.
//
// Three replicas scaled down to one puts two pods in the drain. racingPodClient
// fails the patch for the first of them in the assertion loop's iteration
// order, which is the order the pods were listed in, and this test's real
// assertion is that the second — later in the same loop — still gets marked and
// told ready=false. Without client.IgnoreNotFound around markDraining's Patch,
// the racing pod's NotFound error propagates out of reconcileReplicas and
// f.reconcileProxyGroup's own t.Fatalf fires before either check below ever
// runs.
//
// Which pod is the racing one moved with this milestone, and the test went
// quietly vacuous until a mutation run said so: with the drain chosen by
// occupancy rather than by position, the two pods marked here are the one with
// a player reported on it and one of the two whose counts are unknown.
// markDraining runs for every pod on every pass, but for one that is not going
// and carries no stamp it takes neither branch of its switch and never reaches
// the Patch, so a racing pod that is not marked leaves the tolerance this test
// is named for unexercised — deleting IgnoreNotFound left the suite green.
//
// Naming the pod the rule currently picks would repair that only until the rule
// moves again, and the ordering between the two unknown-count pods is not even
// stable in the meantime: they tie on every clause including age, but only
// while they share a creation timestamp, and three sequential creations that
// straddle a second do not. So the fake reports whether it fired, and that is
// asserted first. The test now fails loudly when it stops testing anything,
// which is the property it was missing both times.
func TestAPodVanishingBetweenListAndPatchDoesNotFailTheReconcile(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 3
	})
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 3 {
		t.Fatalf("proxy pods = %d, want 3", len(before))
	}
	sortPodsOldestFirst(before)
	racing, other := before[0], before[2]
	// The pod this test inspects has to still be there to inspect. A player on
	// it is what does that now, in place of the Delete-swallowing override
	// racingPodClient used to carry.
	f.reportProxyPlayers(t, other, 1)

	fired := false
	r.Client = racingPodClient{Client: r.Client, racingPod: racing.Name, fired: &fired}
	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	if !fired {
		t.Fatalf("markDraining never patched the racing pod %s, so nothing here exercises the NotFound it returns; "+
			"the drain no longer includes that pod and this test has stopped testing its own subject", racing.Name)
	}

	otherPod, ok := f.pod(other.Name)
	if !ok {
		t.Fatalf("pod %s not found", other.Name)
	}
	if _, ok := otherPod.Annotations[ProxyDrainingSinceAnnotation]; !ok {
		t.Error("the pod after the racing one in iteration order was never marked; " +
			"the racing pod's NotFound aborted the rest of the assertion loop")
	}
	if got := f.proxies.lastReady(string(otherPod.UID)); got == nil || *got {
		t.Errorf("the pod after the racing one was told ready=%v, want false", got)
	}
	// The racing pod survives too, and for the reason racingPodClient's comment
	// gives: nothing ever reported a count for it, and an unknown count is
	// treated as occupied. Asserted rather than described, so that a future
	// change to the deletion rule cannot make that comment false in silence —
	// which is exactly what it did before.
	if _, ok := f.pod(racing.Name); !ok {
		t.Error("the racing pod was deleted; its player count is unknown, not zero, " +
			"and an unknown count must be treated as occupied")
	}
}

// TestAProxyOnACordonedNodeIsReplaced is the proxy half of the milestone. It
// asserts the order, not just the outcome: the replacement is ready before
// anything is withdrawn, which is what keeps the group's ready capacity from
// dipping while a player is connected.
func TestAProxyOnACordonedNodeIsReplaced(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(pods))
	}
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}

	going := f.ensureNode(t, "node-going-"+f.ns, false)
	staying := f.ensureNode(t, "node-staying-"+f.ns, false)
	f.bindPodToNode(t, &pods[0], going.Name)
	f.bindPodToNode(t, &pods[1], staying.Name)
	f.ensureNode(t, going.Name, true)

	// The surge pod comes first, and nothing is marked while it is unready.
	f.reconcileProxyGroup(r, "gateway")
	after := f.proxyPods("gateway")
	if len(after) != 3 {
		t.Fatalf("proxy pods = %d after the cordon, want 3 — the replacement must exist before anything is marked", len(after))
	}
	for i := range after {
		if _, dated := drainingSince(&after[i]); dated {
			t.Fatalf("pod %s was marked while the replacement is still unready", after[i].Name)
		}
	}

	// Once it is ready, the pod on the departing node is the one that goes.
	for i := range after {
		f.markProxyPodReady(t, &after[i])
	}
	f.reconcileProxyGroup(r, "gateway")

	var marked []string
	for _, p := range f.proxyPods("gateway") {
		if _, dated := drainingSince(&p); dated {
			marked = append(marked, p.Name)
		}
	}
	if len(marked) != 1 || marked[0] != pods[0].Name {
		t.Fatalf("marked = %v, want exactly [%s]: the pod on the departing node and no other", marked, pods[0].Name)
	}
}

// TestAnUncordonedNodeKeepsTheMarkAlreadyMade is spec §3.6's promise, driven
// end to end: releasing a node mid-drain does not undo the drain already
// begun.
//
// TestDecideRolloutCountsDrainingIndependentlyOfStale in rollout_test.go
// pins the one clause of DecideRollout the one-at-a-time guard depends on,
// but DecideRollout has no concept of a mark already made -- it only reads
// this pass's Draining flag. What actually keeps the mark here is one layer
// up: reconcileReplicas derives Stale fresh on every pass, so once the node
// is uncordoned the marked pod reads Stale: false again, and it is the
// surplus-marks budget documented above reconcileReplicas's staleMarks loop
// -- the same mechanism TestARevertedSpecChangeKeepsTheMarkItAlreadyMade
// exercises for a reverted spec.image -- that re-nominates the same pod
// rather than releasing it. A node drain reverted mid-flight is the same
// shape of state, and this test is what proves the shape holds for it too
// rather than assuming the spec-image case generalizes.
func TestAnUncordonedNodeKeepsTheMarkAlreadyMade(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(pods))
	}
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}

	going := f.ensureNode(t, "node-going-"+f.ns, false)
	staying := f.ensureNode(t, "node-staying-"+f.ns, false)
	f.bindPodToNode(t, &pods[0], going.Name)
	f.bindPodToNode(t, &pods[1], staying.Name)
	f.ensureNode(t, going.Name, true)

	f.reconcileProxyGroup(r, "gateway") // surges the replacement
	after := f.proxyPods("gateway")
	if len(after) != 3 {
		t.Fatalf("proxy pods = %d after the cordon, want 3", len(after))
	}
	for i := range after {
		f.markProxyPodReady(t, &after[i])
	}
	// A player on the departing pod, so occupancy holds it open across the
	// assertions below rather than it being removed the instant it is marked.
	f.reportProxyPlayers(t, pods[0], 1)

	f.reconcileProxyGroup(r, "gateway") // marks the pod on the departing node

	marked, ok := f.pod(pods[0].Name)
	if !ok {
		t.Fatalf("pod %s not found after being marked", pods[0].Name)
	}
	if _, dated := drainingSince(marked); !dated {
		t.Fatalf("pod %s was not marked draining; nothing here would be released by the step below", pods[0].Name)
	}

	// Release the node mid-drain.
	f.ensureNode(t, going.Name, false)
	f.reconcileProxyGroup(r, "gateway")

	stillMarked, ok := f.pod(pods[0].Name)
	if !ok {
		t.Fatal("the marked pod was removed after the node was released; " +
			"the drain should have held rather than running to completion faster than it would have on the departing node")
	}
	if _, dated := drainingSince(stillMarked); !dated {
		t.Error("the draining-since annotation was removed after the node was uncordoned; the mark must be kept, not released")
	}
	if got := f.proxies.lastReady(string(stillMarked.UID)); got == nil || *got {
		t.Errorf("the marked pod was told ready=%v after the node was released, want false: "+
			"releasing the node must not resume the withdrawal it was already mid-way through", got)
	}

	// The replacement still completes: once the marked pod is empty, it is
	// removed like any other drain reaching zero players, and the group
	// settles at its replica count with the mark gone from the object it was
	// on -- not cancelled the instant the node was released, and not stuck
	// forever either.
	f.reportProxyPlayers(t, *stillMarked, 0)
	f.reconcileProxyGroup(r, "gateway")

	final := f.proxyPods("gateway")
	if len(final) != 2 {
		t.Fatalf("proxy pods = %d once the marked pod emptied, want 2", len(final))
	}
	if _, ok := f.pod(pods[0].Name); ok {
		t.Error("the marked pod survived becoming empty; the drain the uncordon was supposed to only pause never completed")
	}
}

// reportProxyPlayers puts a player count on a proxy pod the way that pod's own
// agent would, and proves the registry took it.
//
// Connect comes first because Registry.ReportPlayers refuses a count for a key
// with no live stream. The read-back afterwards is not ceremony: Lookup on a
// key it does not know returns a zero Players, which is indistinguishable from
// a genuinely empty proxy and is exactly the case the deletion loop acts on. A
// test whose setup silently failed here would go on believing it had put three
// players on a proxy while asserting against an empty one, and would pass for
// the wrong reason.
func (f *fixture) reportProxyPlayers(t *testing.T, pod corev1.Pod, players int32) {
	t.Helper()
	uid := string(pod.UID)
	f.agents.Connect(uid, agent.RoleProxy)
	if err := f.agents.ReportPlayers(uid, players, 100); err != nil {
		t.Fatalf("report %d players for proxy %s: %v", players, pod.Name, err)
	}
	switch snap := f.agents.Lookup(uid); {
	case snap.Players != players:
		t.Fatalf("registry holds %d players for proxy %s, want %d", snap.Players, pod.Name, players)
	case snap.PlayersStale:
		// The count check above cannot catch a failed report when players is
		// zero — an unknown key reports zero too, which is the whole reason
		// this read-back exists. Freshness is what tells the two apart, and a
		// fresh zero is precisely what TestProxyGroupScalesDown turns on.
		t.Fatalf("registry holds a stale count for proxy %s; the report did not land", pod.Name)
	}
}

// drainEvents empties a FakeRecorder and returns what it held, so a test can
// make several assertions about the same batch. Reading the channel twice
// would not work: each event is delivered once.
func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// containsEvent reports whether any event carries the given reason. The field
// comparison, and why it is not a substring match, is eventHasReason's.
func containsEvent(events []string, reason string) bool {
	for _, e := range events {
		if eventHasReason(e, reason) {
			return true
		}
	}
	return false
}

// containsEventType reports whether any event carries the given type, matched
// as FakeRecorder's first field for the reason containsEvent matches the
// second: "Warning" is an ordinary English word that a message body could
// easily contain, and a substring match over the whole line would accept one
// that did while the event itself was Normal.
func containsEventType(events []string, eventType string) bool {
	for _, e := range events {
		if fields := strings.SplitN(e, " ", 2); len(fields) >= 1 && fields[0] == eventType {
			return true
		}
	}
	return false
}

// containsSubstring reports whether any event's rendered text contains want.
// Unlike containsEvent and containsEventType this looks at the message body,
// which is where the cost of a timed-out drain is named and where there is no
// field to match on.
func containsSubstring(events []string, want string) bool {
	for _, e := range events {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}

// TestADrainingProxyWithPlayersIsNotDeleted is the promise of the milestone.
// Readiness stops the inflow — the Service drops the endpoint, so no new
// connection arrives — and says nothing whatever about the three people
// already connected, whose TCP sessions Kubernetes does not close. The wait is
// therefore for empty, not for NotReady.
//
// The second pass, a resync interval later, is the part that matters: one pass
// could be explained by a deletion that simply had not landed yet.
func TestADrainingProxyWithPlayersIsNotDeleted(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	surplus := before[1]
	f.reportProxyPlayers(t, surplus, 3)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")
	f.clock.Advance(resyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want the draining proxy still there with its three players", len(pods))
	}
	if _, ok := f.pod(surplus.Name); !ok {
		t.Error("the draining proxy was deleted with three players on it")
	}
	// It must be draining, not merely surviving: a reconciler that had stopped
	// asserting readiness altogether would also leave two pods standing.
	if got := f.proxies.lastReady(string(surplus.UID)); got == nil || *got {
		t.Errorf("the draining proxy was told ready=%v, want false", got)
	}
}

// TestStatusCountsPlayersOnADrainingProxy pins the field this milestone
// changed the meaning of without editing: status.connectedPlayers, the CRD's
// "sum of players across all proxies" and the PLAYERS column.
//
// A draining proxy is NotReady on purpose — that is the readiness contract —
// and it still has people on it. Summing only ready pods therefore printed
// PLAYERS 0 with a person visibly in the game, during the one operation where
// this number is the only thing an operator can watch: nothing logs a
// readiness withdrawal anywhere.
//
// The survivor carries a player too, so the sum is 1 + 3 rather than 0 + 3 and
// a fix that swapped the guard for "only count draining pods" fails here as
// well. readyReplicas is asserted in the same breath because it is the count
// that must keep the guard: the two numbers answer different questions about
// the same pods, and moving ready++ out with the sum would make this group
// claim two ready replicas while one of its pods is out of the Service.
func TestStatusCountsPlayersOnADrainingProxy(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	survivor, surplus := before[0], before[1]
	f.reportProxyPlayers(t, survivor, 1)
	f.reportProxyPlayers(t, surplus, 3)

	// What a kubelet would do on either side of the drain: the survivor's
	// probe passes, and the draining proxy's agent has closed the port the
	// probe reads, so its pod is NotReady while its three players play on.
	f.setPodRunning(survivor.Name, true)
	f.setPodRunning(surplus.Name, false)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	if _, ok := f.pod(surplus.Name); !ok {
		t.Fatal("the draining proxy was deleted with three players on it; there is nothing left to count")
	}
	group := f.proxyGroup("gateway")
	if group.Status.ConnectedPlayers != 4 {
		t.Errorf("status.connectedPlayers = %d, want 4 — the three players on the draining proxy are still playing, "+
			"and this field is the only place a drain is visible", group.Status.ConnectedPlayers)
	}
	if group.Status.ReadyReplicas != 1 {
		t.Errorf("status.readyReplicas = %d, want 1 — the draining proxy is out of the Service and must not be counted ready",
			group.Status.ReadyReplicas)
	}
}

// TestADrainingProxyIsDeletedOnceEmpty is the other half: the wait ends by
// itself when the last player leaves, with no deadline involved and nothing
// for an operator to do.
func TestADrainingProxyIsDeletedOnceEmpty(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	surplus := before[1]
	f.reportProxyPlayers(t, surplus, 3)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got != 2 {
		t.Fatalf("proxy pods = %d before the proxy emptied, want 2 — the wait never started", got)
	}

	f.reportProxyPlayers(t, surplus, 0)
	f.reconcileProxyGroup(r, "gateway")

	if got := len(f.proxyPods("gateway")); got != 1 {
		t.Errorf("got %d pods after the proxy emptied, want 1", got)
	}
}

// TestAZeroFromADeadStreamDoesNotDeleteTheProxy closes the gap between the
// two halves of "known empty": the count was fresh, and the stream that
// produced it was already gone.
//
// Registry.Disconnect keeps the last count and leaves lastReportAt untouched
// on purpose, so a zero stays inside the staleness window for twice the report
// interval after the stream died — ten seconds at the operator's configured
// five. The pod is still Ready for that whole window, because readiness is the
// agent's own gate and nothing closed it, so the Service still routes to it
// and a player can join with no live stream left to report them. Believing the
// zero there deletes a populated proxy, which is the exact failure the wait
// exists to prevent.
//
// The freshness check before the scale-down is what makes this test about the
// stream rather than about staleness: without it, the same two survivors would
// be produced by a clock that had simply moved on, and the test would pass
// against code that never looked at the stream at all.
func TestAZeroFromADeadStreamDoesNotDeleteTheProxy(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	surplus := before[1]
	f.reportProxyPlayers(t, surplus, 0)
	f.agents.Disconnect(string(surplus.UID))

	switch snap := f.agents.Lookup(string(surplus.UID)); {
	case snap.PlayersStale:
		t.Fatalf("the count went stale before the scale-down: this test would then pass for the wrong reason (%+v)", snap)
	case snap.Connected:
		t.Fatalf("the stream is still up after Disconnect; there is no dead stream to test (%+v)", snap)
	}

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	if _, ok := f.pod(surplus.Name); !ok {
		t.Error("the surplus proxy was deleted on a zero from a stream that had already died; " +
			"Velocity goes on serving, and anyone who joined after the last report is invisible to the registry")
	}

	// Bounded, like every other reason this loop keeps a pod: the deadline
	// still ends it.
	f.clock.Advance(f.proxyGroup("gateway").DrainTimeout() + time.Second)
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got != 1 {
		t.Errorf("proxy pods = %d after the deadline, want 1 — a dead stream must not hold a pod forever", got)
	}
}

// TestTheDeadlineDeletesLoudly covers the one path in this milestone that
// disconnects anybody.
//
// Nobody is moved, and that is not an omission: a draining server can hand its
// players to another backend because the connection terminates at the proxy,
// which stays, while a draining proxy has no such option because the
// connection terminates at the proxy being removed. So the deadline is a real
// cost, it is configured rather than accidental, and the event has to name how
// many were lost — a Warning that only said "timed out" would leave nobody
// able to tell a drain of three players from a drain of three hundred.
func TestTheDeadlineDeletesLoudly(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := events.NewFakeRecorder(100)
	r.Recorder = rec
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	surplus := before[1]
	f.reportProxyPlayers(t, surplus, 3)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	f.clock.Advance(f.proxyGroup("gateway").DrainTimeout() + time.Second)
	f.reconcileProxyGroup(r, "gateway")

	if got := len(f.proxyPods("gateway")); got != 1 {
		t.Fatalf("got %d pods after the deadline, want 1", got)
	}
	ev := drainEvents(rec)
	if !containsEvent(ev, "ProxyDrainTimeout") {
		t.Fatalf("events = %v, want a ProxyDrainTimeout", ev)
	}
	if !containsSubstring(ev, "3 player") {
		t.Errorf("events = %v, want the number of players lost named", ev)
	}
	if !containsEventType(ev, "Warning") {
		t.Errorf("events = %v, want the deadline recorded as a Warning — it cost somebody their session", ev)
	}
}

// TestAProxyThatIgnoresItsWithdrawalIsReported is what Fleet.Resync's own
// self-healing does not cover. Resync re-sends the last asserted readiness
// every 30 seconds, so a divergence caused by a lost SetReady call heals on
// its own by the next pass. What does not heal is an agent that received
// the instruction and did not act on it -- this test's pod stays Ready
// through every reconcile because nothing in envtest ever changes it, the
// same as a proxy whose agent heard the withdrawal and did not honor it.
// Nothing here repairs that; the assertion is only that it gets reported,
// and only once the grace period has actually run out.
func TestAProxyThatIgnoresItsWithdrawalIsReported(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := events.NewFakeRecorder(10)
	r.Recorder = rec
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}
	f.reportProxyPlayers(t, pods[1], 1)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	// The pod stays Ready even though its readiness was withdrawn: an agent
	// that heard the instruction and did not act. Nothing is reported yet.
	f.reconcileProxyGroup(r, "gateway")
	g := f.proxyGroup("gateway")
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Fatal("reported before the grace period elapsed")
	}

	f.watchThroughGrace(t, r, "gateway")

	g = f.proxyGroup("gateway")
	if !meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Error("a proxy that stayed Ready after its readiness was withdrawn was not reported")
	}
	ev := drainEvents(rec)
	if !containsEvent(ev, spawneryv1alpha1.ReasonReadinessDiverged) {
		t.Errorf("events = %v, want a ReadinessDiverged event naming the pod", ev)
	}
	if !containsEventType(ev, "Warning") {
		t.Errorf("events = %v, want it recorded as a Warning", ev)
	}

	// The condition stays true and the mismatch is still there, but the
	// event must not fire again: Resync repeats the same verdict every 30
	// seconds for the life of the mismatch, and re-announcing it on every
	// pass would drown the one flank worth telling an operator about in
	// noise. Nothing here advances the clock or changes the pod, so the
	// only thing this pass can prove is whether the event is gated on the
	// transition or fired unconditionally.
	f.reconcileProxyGroup(r, "gateway")
	if ev := drainEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v, want none: a resync of an already-reported divergence must stay silent", ev)
	}
}

// TestAProxyThatAgreesAgainClearsItsDivergenceEntry proves the half of the
// grace-period map that no assertion on the group's condition can prove by
// itself: that a pod's tracked start time is actually deleted once its
// readiness agrees again, not merely made irrelevant for as long as it stays
// agreeing. Without a real deletion, a pod that flickered once, agreed
// again, and then disagreed a second time would resume its grace clock from
// the *original* mismatch instead of restarting it -- reporting a span of
// divergence nobody actually watched, in violation of the same principle
// that makes readinessDivergence.forget correct on the early-return paths
// above: an entry measures divergence while something was watching, and a
// gap voids the measurement.
//
// The proof has to re-diverge the pod, or it proves nothing: a pod that
// simply stays in agreement forever never reaches the branch that reads a
// tracked entry back out of the map, so a correct delete and a delete that
// silently no-ops would be indistinguishable to a test that only ever
// checks agreement. Re-diverging past the point where the *original*
// mismatch would already have crossed the grace period, and finding the
// condition still false, is what tells a restarted clock from a merely
// resumed one -- and the final advance-and-check below confirms it is a
// restart and not just a clock stuck off, by showing the report does
// eventually arrive once a fresh grace period has actually elapsed from
// the second mismatch.
func TestAProxyThatAgreesAgainClearsItsDivergenceEntry(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}
	f.reportProxyPlayers(t, pods[1], 1)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")
	// The first mismatch begins here (call it T0): the withdrawn pod is
	// still Ready.

	// Half the grace period: not enough to report on its own, and here only
	// to prove the eventual all-clear below is not just "never got far
	// enough."
	f.clock.Advance(readinessDivergenceGrace/2 + time.Second)

	// The withdrawal is honored before the deadline: the surplus pod goes
	// NotReady, same as a real agent that closed its listener. This is
	// meant to clear its entry.
	pods = f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	surplus := &pods[1]
	f.setProxyPodReadyCondition(t, surplus, false)
	f.reconcileProxyGroup(r, "gateway")

	g := f.proxyGroup("gateway")
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Fatal("reported even though the pod agreed again before the grace period elapsed")
	}

	// Past the point (T0 + readinessDivergenceGrace) where the original
	// mismatch would already have crossed the grace period, if its
	// timestamp had survived instead of being cleared.
	f.clock.Advance(readinessDivergenceGrace/2 + time.Second)

	// Diverge the same pod again: still marked to leave, but Ready again, as
	// if the agent had reopened its listener. This is the only way to reach
	// the code path a leaked entry would betray.
	f.setProxyPodReadyCondition(t, surplus, true)
	f.reconcileProxyGroup(r, "gateway")

	g = f.proxyGroup("gateway")
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Fatal("reported the instant it re-diverged: the entry's original start time survived instead of " +
			"being cleared, so the grace period was measured from the first mismatch rather than restarting")
	}

	// The clock did restart, not just fail to fire early: a full grace
	// period measured from the *second* mismatch does report it.
	f.watchThroughGrace(t, r, "gateway")

	g = f.proxyGroup("gateway")
	if !meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Error("re-diverging was never reported at all: something other than a restarted clock is wrong")
	}
}

// TestASlowStartingProxyIsNotReportedAsDiverged pins the scope decision
// ReadinessDiverged narrowed to: only a withdrawal the operator asserted and
// the pod ignored is reported, never a pod that simply has not gotten
// around to its first successful probe yet.
//
// Every pod is asserted ready from the moment it exists — SetReady(true) for
// a non-draining pod runs before any kubelet has had a chance to probe it at
// all — so a bidirectional comparison would start this pod's grace clock at
// creation, and a proxy pulling a large image on a cold node can easily run
// past readinessDivergenceGrace without ever having disobeyed anything. This
// test's pods are never marked ready at all — envtest runs no kubelet — so
// they stand in for exactly that: a proxy still starting up, not one that
// heard an instruction and ignored it. Reporting it would misname the two as
// identical, which is the false diagnosis this test exists to rule out.
func TestASlowStartingProxyIsNotReportedAsDiverged(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := events.NewFakeRecorder(10)
	r.Recorder = rec
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")
	// A second pass is required before the pods this one created reach the
	// divergence check at all -- see the comment on Reconcile's pods, err :=
	// r.pods(ctx, group) call. Without it, the clock advance below would land
	// before the pods' first sight rather than after, and the test would
	// pass whether or not the scope decision held.
	f.reconcileProxyGroup(r, "gateway")

	// Neither pod is ever marked ready, and neither is asked to drain: both
	// replicas are still wanted, so this is purely "still starting up," with
	// no withdrawal anywhere in the picture.
	// Every pass a real operator would make, not one: a pod that never diverges
	// must stay unreported however many times it is looked at.
	f.watchThroughGrace(t, r, "gateway")

	g := f.proxyGroup("gateway")
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Error("a pod that has simply never passed its first probe was reported as ReadinessDiverged")
	}
	if ev := drainEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v, want none: a slow-starting pod is not a withdrawal that was ignored", ev)
	}
}

// TestASpecChangeSurgesBeforeItMarksAnything is the first move of the
// milestone's subject: the replacement proxy is created before any proxy is
// asked to stop taking connections. That ordering is what leaves room for the
// ready count to hold at replicas; whether it actually holds is the readiness
// gate's business, and that is the next test's.
//
// It measures the surge and nothing else. The rollout as a whole — one proxy at
// a time, every one of them, ending on the new shape — is
// TestTheRolloutFinishesWithEveryProxyOnTheNewShape, and the readiness gate in
// front of the first mark is TestTheSurgePodMustBeReadyBeforeAnyPodIsMarked.
// This test was called TestAStaleProxyIsReplacedOneAtATime, which named all
// three and asserted the first.
func TestASpecChangeSurgesBeforeItMarksAnything(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	for i := range before {
		f.markProxyPodReady(t, &before[i])
	}

	// A spec change every pod's digest disagrees with.
	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")
	after := f.proxyPods("gateway")
	if len(after) != 3 {
		t.Fatalf("proxy pods = %d after the spec change, want 3 — the surge pod must exist before anything is marked", len(after))
	}
	for i := range after {
		if _, dated := drainingSince(&after[i]); dated {
			t.Errorf("pod %s was marked while the surge pod is still unready; ready capacity would dip below replicas", after[i].Name)
		}
	}
}

// TestTheSurgePodMustBeReadyBeforeAnyPodIsMarked states the property the
// surge exists for, and it is the one a pod count alone cannot show.
//
// The second reconcile with the surge pod still unready is what gives this
// test its grip, and it was added after the mutation run said so. Without it
// this test's one pass under an unready surge pod is the pass that creates it,
// where the group is below its target and the decision is a create with no
// drain in it whatever the readiness gate says: the gate could be deleted
// outright and the test would not notice — which is what running that mutation
// against the version without this pass actually did. On the second pass the
// group is at its target, so the gate is the last thing standing between a
// stale pod and the mark.
func TestTheSurgePodMustBeReadyBeforeAnyPodIsMarked(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	for i := range before {
		f.markProxyPodReady(t, &before[i])
	}
	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")
	// A resync with the surge pod still unready: the group is at its target
	// size now, so nothing is created and nothing may be marked either.
	f.clock.Advance(resyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	// Make the surge pod ready and reconcile again.
	pods := f.proxyPods("gateway")
	if len(pods) != 3 {
		t.Fatalf("proxy pods = %d, want the group's 2 replicas plus one surge", len(pods))
	}
	for i := range pods {
		if _, dated := drainingSince(&pods[i]); dated {
			t.Fatalf("pod %s was marked before the surge pod turned ready; the group has 2 replicas and would "+
				"have been left with 1 ready proxy", pods[i].Name)
		}
		f.markProxyPodReady(t, &pods[i])
	}
	f.reconcileProxyGroup(r, "gateway")

	marked := 0
	for _, p := range f.proxyPods("gateway") {
		if _, dated := drainingSince(&p); dated {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("marked = %d, want exactly 1 — a rolling update replaces one proxy at a time", marked)
	}
}

// TestChangingReplicasAloneRollsNothing is the reason staleness is a digest of
// the rendered pod rather than metadata.generation.
func TestChangingReplicasAloneRollsNothing(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}

	f.setProxyReplicas("gateway", 3)
	f.reconcileProxyGroup(r, "gateway")

	pods = f.proxyPods("gateway")
	if len(pods) != 3 {
		t.Fatalf("proxy pods = %d, want 3", len(pods))
	}
	for _, p := range pods {
		if _, dated := drainingSince(&p); dated {
			t.Errorf("pod %s was marked draining after a replicas change; scaling must not roll the group", p.Name)
		}
	}
}

// TestADrainingProxyKeepsItsMarkWhileTheRolloutWaits pins the one piece of
// state a rollout cannot re-derive on the pass after it decides.
//
// DecideRollout deliberately names nobody while another pod is draining — that
// is what makes the update one proxy at a time — so a caller that rebuilt
// `leaving` from the decision alone would cancel its own drain on the very next
// pass: readiness restored, annotation deleted, and then the same choice made
// again five seconds later with the deadline running from zero. A proxy with a
// player on it would be told to stop taking connections and to start again,
// forever, and the drain would never end.
//
// Both proxies carry a player so that the one chosen is held open across the
// passes rather than deleted the moment it is marked, which is what makes the
// second pass observable at all.
func TestADrainingProxyKeepsItsMarkWhileTheRolloutWaits(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	for i := range before {
		f.markProxyPodReady(t, &before[i])
		f.reportProxyPlayers(t, before[i], 1)
	}

	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Pass one surges, pass two marks — once the surge pod is ready.
	f.reconcileProxyGroup(r, "gateway")
	surged := f.proxyPods("gateway")
	for i := range surged {
		f.markProxyPodReady(t, &surged[i])
	}
	f.reconcileProxyGroup(r, "gateway")

	marked, at := "", ""
	for _, p := range f.proxyPods("gateway") {
		if _, dated := drainingSince(&p); dated {
			marked, at = p.Name, p.Annotations[ProxyDrainingSinceAnnotation]
		}
	}
	if marked == "" {
		t.Fatal("no proxy was marked once the surge pod was ready; there is no drain to keep")
	}

	f.clock.Advance(resyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	pod, ok := f.pod(marked)
	if !ok {
		t.Fatalf("the marked proxy %s was deleted with a player on it", marked)
	}
	if got := pod.Annotations[ProxyDrainingSinceAnnotation]; got != at {
		t.Errorf("draining-since on %s is now %q, want the original %q — a mark that is dropped and rewritten "+
			"is a deadline that starts again every pass",
			marked, got, at)
	}
	if got := f.proxies.lastReady(string(pod.UID)); got == nil || *got {
		t.Errorf("the draining proxy was told ready=%v on the pass after it was marked, want false", got)
	}
}

// TestTheRolloutFinishesWithEveryProxyOnTheNewShape drives the whole thing:
// one spec change, and the operator converges on a group of the new shape
// without ever running more than the surge over its replica count.
//
// Every pod is reported empty, which is what lets a marked proxy go on the pass
// it is marked; the point here is the sequence, not the wait, which
// TestADrainingProxyWithPlayersIsNotDeleted already owns.
func TestTheRolloutFinishesWithEveryProxyOnTheNewShape(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	network := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.network.Name, Namespace: f.ns}, network); err != nil {
		t.Fatalf("get Network: %v", err)
	}
	configValues, err := yaml.Marshal(proxyConfigValues(f.proxyGroup("gateway")))
	if err != nil {
		t.Fatalf("marshal config values: %v", err)
	}
	want, err := podspec.DesiredProxyHash(network, f.proxyGroup("gateway"), r.AgentEndpoint, configValues)
	if err != nil {
		t.Fatalf("desired hash: %v", err)
	}

	// Ten passes is well over the four this rollout takes — a create and then a
	// mark for each of the two proxies, the count checked by running the loop
	// short — so it leaves room for the operator to be slower than expected and
	// none for it to never finish. The passes after the fourth are not padding
	// either: they are what says a settled group stays settled instead of
	// rolling itself forever.
	for pass := 0; pass < 10; pass++ {
		pods := f.proxyPods("gateway")
		for i := range pods {
			f.markProxyPodReady(t, &pods[i])
			f.reportProxyPlayers(t, pods[i], 0)
		}
		f.reconcileProxyGroup(r, "gateway")
		if n := len(f.proxyPods("gateway")); n > 3 {
			t.Fatalf("proxy pods = %d on pass %d, want at most replicas + the surge of 1", n, pass)
		}
	}

	done := f.proxyPods("gateway")
	if len(done) != 2 {
		t.Fatalf("proxy pods = %d once the rollout settled, want the group's 2 replicas", len(done))
	}
	for _, p := range done {
		if got := p.Labels[podspec.LabelPodHash]; got != want {
			t.Errorf("pod %s carries hash %q, want the current %q — the rollout left a proxy of the old shape behind",
				p.Name, got, want)
		}
		if _, dated := drainingSince(&p); dated {
			t.Errorf("pod %s is still marked draining once the rollout settled", p.Name)
		}
	}
}

// TestAMarkedProxyKeepsItsMarkWhenTheSurgePodIsLost covers the other half of
// why a mark persists: the pod is stale, so it has to go whatever else happens
// to the group around it.
//
// Losing the surge pod — evicted, or its node drained — puts the group back
// below its target with a marked pod still in it. Nothing in the replica count
// then argues for the mark, and dropping it would tell a proxy that has already
// stopped taking connections to start again, only to mark it once more when the
// replacement surge pod turns ready, with the drain deadline running from zero
// each time. The mark stays; the operator brings up another surge pod
// underneath it, which is what the create on this same pass is.
func TestAMarkedProxyKeepsItsMarkWhenTheSurgePodIsLost(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	for i := range before {
		f.markProxyPodReady(t, &before[i])
		// A player each, so whichever proxy is chosen is held open long enough
		// for the passes below to look at it.
		f.reportProxyPlayers(t, before[i], 1)
	}

	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")
	surged := f.proxyPods("gateway")
	for i := range surged {
		f.markProxyPodReady(t, &surged[i])
	}
	f.reconcileProxyGroup(r, "gateway")

	network := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.network.Name, Namespace: f.ns}, network); err != nil {
		t.Fatalf("get Network: %v", err)
	}
	configValues, err := yaml.Marshal(proxyConfigValues(f.proxyGroup("gateway")))
	if err != nil {
		t.Fatalf("marshal config values: %v", err)
	}
	current, err := podspec.DesiredProxyHash(network, f.proxyGroup("gateway"), r.AgentEndpoint, configValues)
	if err != nil {
		t.Fatalf("desired hash: %v", err)
	}

	var marked, at, surge string
	for _, p := range f.proxyPods("gateway") {
		if p.Labels[podspec.LabelPodHash] == current {
			surge = p.Name
		}
		if _, dated := drainingSince(&p); dated {
			marked, at = p.Name, p.Annotations[ProxyDrainingSinceAnnotation]
		}
	}
	if marked == "" || surge == "" {
		t.Fatalf("marked = %q, surge = %q; want both — there is nothing to lose otherwise", marked, surge)
	}

	lost, ok := f.pod(surge)
	if !ok {
		t.Fatalf("surge pod %s not found", surge)
	}
	if err := f.c.Delete(f.ctx, lost); err != nil {
		t.Fatalf("delete the surge pod: %v", err)
	}
	f.clock.Advance(resyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	pod, ok := f.pod(marked)
	if !ok {
		t.Fatalf("the marked proxy %s was deleted with a player on it", marked)
	}
	if got := pod.Annotations[ProxyDrainingSinceAnnotation]; got != at {
		t.Errorf("draining-since on the stale proxy %s is now %q, want the original %q — losing the surge pod "+
			"does not make a proxy of the old shape wanted again", marked, got, at)
	}
	if got := f.proxies.lastReady(string(pod.UID)); got == nil || *got {
		t.Errorf("the stale draining proxy was told ready=%v after the surge pod was lost, want false", got)
	}
	if n := len(f.proxyPods("gateway")); n != 3 {
		t.Errorf("proxy pods = %d, want 3 — a replacement surge pod must come up under the one that is going", n)
	}
}

// TestAPartlyCancelledScaleDownReleasesOnlyTheMarksItHasTo is the case that
// tells "is this pod surplus?" from "how many of these pods are surplus?".
//
// Four replicas taken to two marks two proxies, which is right. Raising the
// count to three afterwards makes exactly one of those two wanted again — and
// asking a group-level question once per marked pod cannot express that, because
// it returns the same answer for both and keeps both marks. The cost is not
// cosmetic: the group is not short of pods, only of ready ones, so nothing
// creates a replacement — it serves two proxies against a requested three until
// one of the drains finishes on its own, which the players on it can hold open
// to the full drain timeout.
//
// Every proxy carries a player so the marked ones stay to be looked at, and the
// counts are equal so that which two are marked is decided by the same rule on
// both passes rather than by the players moving underneath the test.
func TestAPartlyCancelledScaleDownReleasesOnlyTheMarksItHasTo(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 4
	})
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 4 {
		t.Fatalf("proxy pods = %d, want 4", len(before))
	}
	for i := range before {
		f.markProxyPodReady(t, &before[i])
		f.reportProxyPlayers(t, before[i], 1)
	}

	f.setProxyReplicas("gateway", 2)
	f.reconcileProxyGroup(r, "gateway")
	if got := markedProxies(f.proxyPods("gateway")); len(got) != 2 {
		t.Fatalf("marked = %v, want 2 after four replicas were taken to two", got)
	}

	f.setProxyReplicas("gateway", 3)
	f.clock.Advance(resyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 4 {
		t.Fatalf("proxy pods = %d, want the four still there — each has a player on it", len(pods))
	}
	marked := markedProxies(pods)
	if len(marked) != 1 {
		t.Errorf("marked = %v, want 1 — three replicas out of four pods leaves one surplus, not two", marked)
	}
	// The released proxy has to be back in the Service, not merely un-annotated:
	// the annotation dates the deadline, and readiness is what carries players.
	for i := range pods {
		going := false
		for _, name := range marked {
			if pods[i].Name == name {
				going = true
			}
		}
		if going {
			continue
		}
		if got := f.proxies.lastReady(string(pods[i].UID)); got == nil || !*got {
			t.Errorf("proxy %s carries no mark but was told ready=%v, want true", pods[i].Name, got)
		}
	}
}

// markedProxies names the pods carrying a readable draining-since, so a test can
// say how many are going and which, rather than counting annotations by hand in
// three places.
func markedProxies(pods []corev1.Pod) []string {
	var out []string
	for i := range pods {
		if _, dated := drainingSince(&pods[i]); dated {
			out = append(out, pods[i].Name)
		}
	}
	sort.Strings(out)
	return out
}

// TestAStaleMarkDoesNotSpendTheSurplusBudget is the case where the two reasons
// a proxy carries a mark meet on the same group.
//
// A pod marked for being stale is already leaving. Counting it as one of the
// surplus the group is allowed to keep counts the same departure twice: the
// group then holds a mark for a proxy it needs back, and gets no replacement
// for it either: it has pods enough, and only DecideRollout's create branch
// makes more. So the budget is len(views) minus the stale marks minus
// replicas, and here that
// is zero — one stale mark and one surplus mark against four pods and three
// replicas, where the stale pod's own departure is the whole of the surplus.
//
// The fixture is the shortest route to one stale pod among three current ones
// with nothing draining: a single proxy, then an image change and a scale-up in
// one update, so the three pods that come up are of the new shape and the
// original is not. Every pod carries a player, so a marked one stays to be
// looked at instead of going the moment it is marked.
func TestAStaleMarkDoesNotSpendTheSurplusBudget(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 1
	})
	f.reconcileProxyGroup(r, "gateway")

	first := f.proxyPods("gateway")
	if len(first) != 1 {
		t.Fatalf("proxy pods = %d, want 1", len(first))
	}
	f.markProxyPodReady(t, &first[0])
	f.reportProxyPlayers(t, first[0], 1)
	stale := first[0].Name

	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	g.Spec.Replicas = 3
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 4 {
		t.Fatalf("proxy pods = %d, want 4 — three replicas of the new shape plus the one of the old", len(pods))
	}
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
		f.reportProxyPlayers(t, pods[i], 1)
	}

	// Down to one: two pods must go, and the stale one goes first because it
	// has to go regardless. The second is a current pod, marked for surplus
	// alone — which is the pairing this test exists for.
	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")
	marked := markedProxies(f.proxyPods("gateway"))
	if len(marked) != 2 {
		t.Fatalf("marked = %v, want 2 — four pods down to one replica, with a surge of 1 for the stale pod", marked)
	}
	if !slices.Contains(marked, stale) {
		t.Fatalf("marked = %v, want the stale proxy %s among them", marked, stale)
	}

	// And back up to three. The stale proxy is still going, and its departure
	// is the only one the group has room for.
	f.setProxyReplicas("gateway", 3)
	f.clock.Advance(resyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	after := f.proxyPods("gateway")
	if len(after) != 4 {
		t.Fatalf("proxy pods = %d, want the four still there — each has a player on it", len(after))
	}
	if got := markedProxies(after); len(got) != 1 || got[0] != stale {
		t.Errorf("marked = %v, want just the stale proxy %s — a pod already leaving for its shape cannot also "+
			"be the surplus the group is short of", got, stale)
	}
	serving := 0
	for i := range after {
		if _, dated := drainingSince(&after[i]); dated {
			continue
		}
		serving++
		if got := f.proxies.lastReady(string(after[i].UID)); got == nil || !*got {
			t.Errorf("proxy %s carries no mark but was told ready=%v, want true", after[i].Name, got)
		}
	}
	if serving != 3 {
		t.Errorf("proxies still taking connections = %d, want the 3 replicas asked for", serving)
	}
}

// TestARevertedSpecChangeKeepsTheMarkItAlreadyMade is the one state where a
// pod's mark outlives the reason it was made, and it is the state a rollback
// puts a group into.
//
// A spec change is not one-way. Change the image, wait for the surge pod, let a
// proxy be marked for being stale — then put the image back, and that proxy
// matches the spec again while the surge pod raised to replace it does not. The
// mark that was a stale mark is now a surplus mark, and the pod nobody has
// marked is the stale one.
//
// The group holds the mark, which is a choice and not an accident. Releasing it
// would spend the surplus budget on the surge pod's departure before that
// departure has started — the surge pod is stale but unmarked, and gets marked
// only on a later pass, because a rolling update takes one proxy at a time. A
// spec that flaps would then cancel and remake drains, restarting a deadline
// that is supposed to bound them. The cost of holding is one proxy more than
// the minimum leaving, one at a time and bounded by the same deadline as every
// other wait here.
func TestARevertedSpecChangeKeepsTheMarkItAlreadyMade(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	// One proxy has a player and the other has no agent at all, so which of the
	// two is marked is decided by the occupancy rule rather than by a tie: a
	// reported count outranks an unknown one, and the player keeps that pod in
	// the group long enough for the revert below to land on it.
	held := before[0]
	f.reportProxyPlayers(t, held, 1)
	for i := range before {
		f.markProxyPodReady(t, &before[i])
	}

	g := f.proxyGroup("gateway")
	// Kept so the revert below is a revert rather than a second literal that
	// has to be checked against the fixture's default by eye.
	shipped := g.Spec.Image
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")
	surged := f.proxyPods("gateway")
	if len(surged) != 3 {
		t.Fatalf("proxy pods = %d after the spec change, want 3", len(surged))
	}
	for i := range surged {
		f.markProxyPodReady(t, &surged[i])
	}
	f.reconcileProxyGroup(r, "gateway")

	if got := markedProxies(f.proxyPods("gateway")); len(got) != 1 || got[0] != held.Name {
		t.Fatalf("marked = %v, want just %s — the proxy with a reported count is the one the rule takes", got, held.Name)
	}
	marked, ok := f.pod(held.Name)
	if !ok {
		t.Fatalf("pod %s not found", held.Name)
	}
	at := marked.Annotations[ProxyDrainingSinceAnnotation]

	// The rollback: the marked proxy matches the spec again, and the surge pod
	// that was brought up to replace it does not.
	g = f.proxyGroup("gateway")
	g.Spec.Image = shipped
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("revert: %v", err)
	}
	f.clock.Advance(resyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	network := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.network.Name, Namespace: f.ns}, network); err != nil {
		t.Fatalf("get Network: %v", err)
	}
	configValues, err := yaml.Marshal(proxyConfigValues(f.proxyGroup("gateway")))
	if err != nil {
		t.Fatalf("marshal config values: %v", err)
	}
	current, err := podspec.DesiredProxyHash(network, f.proxyGroup("gateway"), r.AgentEndpoint, configValues)
	if err != nil {
		t.Fatalf("desired hash: %v", err)
	}
	after, ok := f.pod(held.Name)
	if !ok {
		t.Fatalf("the marked proxy %s was deleted with a player on it", held.Name)
	}
	if after.Labels[podspec.LabelPodHash] != current {
		t.Fatalf("the marked proxy %s does not match the reverted spec, so this test is not in the state it "+
			"describes", held.Name)
	}

	if got := markedProxies(f.proxyPods("gateway")); len(got) != 1 || got[0] != held.Name {
		t.Errorf("marked = %v, want just %s still — the budget is spent on departures under way, and the surge "+
			"pod's has not started", got, held.Name)
	}
	if got := after.Annotations[ProxyDrainingSinceAnnotation]; got != at {
		t.Errorf("draining-since on %s is now %q, want the original %q — a rollback must not restart a deadline "+
			"that is already running", held.Name, got, at)
	}
	if got := f.proxies.lastReady(string(after.UID)); got == nil || *got {
		t.Errorf("the draining proxy was told ready=%v after the rollback, want false", got)
	}
}

func TestProxyOccupied(t *testing.T) {
	tests := []struct {
		name string
		snap agent.Snapshot
		want bool
	}{
		{"known empty on a live stream", agent.Snapshot{Players: 0, Connected: true}, false},
		{"players on a live stream", agent.Snapshot{Players: 3, Connected: true}, true},
		{"a stale count counts as occupied", agent.Snapshot{Players: 0, PlayersStale: true, Connected: true}, true},
		{"a dead stream counts as occupied", agent.Snapshot{Players: 0, Connected: false}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyOccupied(tc.snap); got != tc.want {
				t.Fatalf("proxyOccupied() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestABrokenNetworkDoesNotStopTheProxyBudget is the proxy half of the same
// ruling, and the half that is a disconnect rather than a hang.
//
// syncOccupiedLabels, reconcileProxyPDB and reportNodeDraining all sat below
// the Network and Expose early returns, so a ProxyGroup whose Network broke
// stopped maintaining its budget entirely. The dangerous shape is the one
// reproduced here: the proxy was empty at the last successful pass, so the
// budget froze at minAvailable: 0 with no label on the pod, and every player
// who joined afterwards was on a pod the eviction API could take with nothing
// standing in the way.
func TestABrokenNetworkDoesNotStopTheProxyBudget(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(pods))
	}
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}
	// Empty at the last good pass: this is what freezes the budget at 0.
	f.reportProxyPlayers(t, pods[0], 0)
	f.reportProxyPlayers(t, pods[1], 0)
	f.reconcileProxyGroup(r, "gateway")

	pdbKey := types.NamespacedName{Name: podspec.GroupPDBName("gateway", podspec.RoleProxy), Namespace: f.ns}
	pdb := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, pdbKey, pdb); err != nil {
		t.Fatalf("get proxy PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 0 {
		t.Fatalf("minAvailable = %v with both proxies empty, want 0: without that starting point "+
			"the assertion below could hold on a budget nobody updated", pdb.Spec.MinAvailable)
	}

	// The Network goes, a node is cordoned under one proxy, and a player joins
	// the other. All three things this group owes its players happen on a pass
	// that can do nothing else.
	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	node := f.ensureNode(t, "node-going-"+f.ns, false)
	f.bindPodToNode(t, &pods[1], node.Name)
	f.ensureNode(t, node.Name, true)
	f.reportProxyPlayers(t, pods[0], 5)

	f.reconcileProxyGroup(r, "gateway")

	// The pass really took the missing-Network route, or nothing below is a
	// statement about the gate.
	group := f.proxyGroup("gateway")
	accepted := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason != spawneryv1alpha1.ReasonNetworkNotFound {
		t.Fatalf("Accepted = %v, want False/%s", accepted, spawneryv1alpha1.ReasonNetworkNotFound)
	}

	if err := f.c.Get(f.ctx, pdbKey, pdb); err != nil {
		t.Fatalf("get proxy PDB after the Network went: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("minAvailable = %v, want 1: a player joined a proxy whose group's Network is broken, "+
			"and a frozen budget leaves exactly that pod evictable", pdb.Spec.MinAvailable)
	}
	byName := map[string]corev1.Pod{}
	for _, p := range f.proxyPods("gateway") {
		byName[p.Name] = p
	}
	if _, ok := byName[pods[0].Name].Labels[podspec.LabelOccupied]; !ok {
		t.Errorf("pod %s has players and carries no occupied label, so the budget's selector "+
			"matches nothing it counted", pods[0].Name)
	}
	f.assertBudgetSelectsExactlyWhatItCounts(t, pdbKey.Name)

	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("NodeDraining = %v, want True: a broken Network does not make the node stay", cond)
	} else if !strings.Contains(cond.Message, node.Name) {
		t.Errorf("message %q does not name the node", cond.Message)
	}
}

// TestProxyOccupiedForBudget is the deadline-free consumers' rule, and the
// whole of the difference from proxyOccupied above is the unknown-pod row.
//
// Registry.Lookup answers for a pod it has never seen with {Known: false,
// Connected: false, PlayersStale: true}, which proxyOccupied reads as
// occupied. The deletion wait can afford that because
// spec.drain.timeoutSeconds ends it; a budget cannot, so a surge pod would
// hold minAvailable above an achievable currentHealthy until its agent
// reported, and a crash-looping proxy would hold it there for good.
//
// The two unknown rows are the pair that matters, and they differ only in how
// long the registry itself has been up: Snapshot.StreamDownFor for an unknown
// pod is the time since the operator started, which is what keeps a fleet
// that is Ready and full of players from reading as empty in the seconds
// after an operator restart.
func TestProxyOccupiedForBudget(t *testing.T) {
	tests := []struct {
		name string
		snap agent.Snapshot
		want bool
	}{
		{"known empty on a live stream", agent.Snapshot{Known: true, Players: 0, Connected: true}, false},
		{"players on a live stream", agent.Snapshot{Known: true, Players: 3, Connected: true}, true},
		{
			"a stale count counts as occupied",
			agent.Snapshot{Known: true, Players: 0, PlayersStale: true, Connected: true},
			true,
		},
		{
			"an agent that connected and then died still counts as occupied",
			agent.Snapshot{Known: true, Players: 0, Connected: false, StreamDownFor: time.Hour},
			true,
		},
		{
			"a pod the registry has never seen does not count, once the grace has passed",
			agent.Snapshot{Players: 0, PlayersStale: true, StreamDownFor: phase.StreamDownGrace},
			false,
		},
		{
			"a pod the registry has never seen counts while the operator is still coming up",
			agent.Snapshot{Players: 0, PlayersStale: true, StreamDownFor: phase.StreamDownGrace - time.Second},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyOccupiedForBudget(tc.snap); got != tc.want {
				t.Fatalf("proxyOccupiedForBudget() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAProxyTheRegistryHasNeverSeenDoesNotWedgeTheBudget is the same rule
// through the reconciler, on the state that made it a defect: a proxy pod
// whose agent never connects at all. Before the qualifier it was labelled
// occupied and counted into minAvailable from the moment it appeared, so a
// group with one crash-looping proxy refused every eviction of the occupied
// proxies beside it, with no deadline anywhere to end that.
//
// The clock has to move past phase.StreamDownGrace, and that is the point
// rather than a fixture detail: newFixture starts the registry at the same
// instant it starts the clock, so at the top of any test built on newFixture
// every unknown pod is inside the post-restart grace and reads as occupied on
// purpose.
func TestAProxyTheRegistryHasNeverSeenDoesNotWedgeTheBudget(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(pods))
	}
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}
	// One proxy reports; the other's agent never connects.
	f.reportProxyPlayers(t, pods[0], 4)

	// Inside the grace both count, which is the operator-restart case and is
	// asserted here so that the assertion after the advance is about the
	// grace expiring rather than about anything else in the fixture.
	f.reconcileProxyGroup(r, "gateway")
	pdbKey := types.NamespacedName{Name: podspec.GroupPDBName("gateway", podspec.RoleProxy), Namespace: f.ns}
	pdb := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, pdbKey, pdb); err != nil {
		t.Fatalf("get proxy PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 2 {
		t.Fatalf("minAvailable = %v inside the post-restart grace, want 2: both the reporting proxy "+
			"and the silent one count while the registry itself is younger than the grace",
			pdb.Spec.MinAvailable)
	}

	// Past the grace the silent proxy stops being paid for. The reporting one
	// has to be kept fresh, or it would go stale on the same advance and the
	// count would stay at 2 for a reason that has nothing to do with this.
	f.clock.Advance(phase.StreamDownGrace + time.Second)
	f.reportProxyPlayers(t, pods[0], 4)
	f.reconcileProxyGroup(r, "gateway")

	if err := f.c.Get(f.ctx, pdbKey, pdb); err != nil {
		t.Fatalf("get proxy PDB after the grace: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("minAvailable = %v, want 1: a proxy whose agent has never appeared holds no players, "+
			"and counting it pins minAvailable above a currentHealthy the group can reach",
			pdb.Spec.MinAvailable)
	}
	byName := map[string]corev1.Pod{}
	for _, p := range f.proxyPods("gateway") {
		byName[p.Name] = p
	}
	if _, ok := byName[pods[1].Name].Labels[podspec.LabelOccupied]; ok {
		t.Errorf("pod %s has never been heard from and still carries the occupied label; "+
			"the label and the budget have to agree pod for pod", pods[1].Name)
	}
	f.assertBudgetSelectsExactlyWhatItCounts(t, pdbKey.Name)
}

// TestTheOccupiedLabelFollowsTheProxyPlayerCount is what makes the budget in
// Task 7 mean anything: the selector matches occupied pods and stops matching
// when they empty.
func TestTheOccupiedLabelFollowsTheProxyPlayerCount(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}
	f.reportProxyPlayers(t, pods[0], 4)
	f.reportProxyPlayers(t, pods[1], 0)
	f.reconcileProxyGroup(r, "gateway")

	byName := map[string]corev1.Pod{}
	for _, p := range f.proxyPods("gateway") {
		byName[p.Name] = p
	}
	if _, ok := byName[pods[0].Name].Labels[podspec.LabelOccupied]; !ok {
		t.Errorf("pod %s has players and no occupied label; the budget would let it be evicted", pods[0].Name)
	}
	if _, ok := byName[pods[1].Name].Labels[podspec.LabelOccupied]; ok {
		t.Errorf("pod %s is known empty and still labelled occupied; the budget would block a drain for nobody", pods[1].Name)
	}
}

// TestTheProxyGroupBudgetSizesToOccupiedPods is the object the eviction API
// consults. Every assertion here is a way the budget can be present and still
// protect nobody.
func TestTheProxyGroupBudgetSizesToOccupiedPods(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}
	f.reportProxyPlayers(t, pods[0], 4)
	f.reportProxyPlayers(t, pods[1], 0)
	f.reconcileProxyGroup(r, "gateway")

	pdb := &policyv1.PodDisruptionBudget{}
	pdbKey := types.NamespacedName{Name: podspec.GroupPDBName("gateway", podspec.RoleProxy), Namespace: f.ns}
	if err := f.c.Get(f.ctx, pdbKey, pdb); err != nil {
		t.Fatalf("get proxy PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("minAvailable = %v, want 1 — exactly one proxy is occupied", pdb.Spec.MinAvailable)
	}
	// The number above is only protection if the selector matches the same
	// pods it was counted from; the assertions here and there are separate
	// questions. See the helper.
	f.assertBudgetSelectsExactlyWhatItCounts(t, pdbKey.Name)
	if pdb.Spec.MaxUnavailable != nil {
		t.Error("maxUnavailable is set; Kubernetes rejects it for pods with no controller carrying a scale subresource")
	}
	if got := pdb.Spec.Selector.MatchLabels[podspec.LabelOccupied]; got != "true" {
		t.Errorf("selector occupied = %q, want \"true\": a selector that matches empty pods blocks drains for nobody", got)
	}
	if len(pdb.OwnerReferences) == 0 {
		t.Error("the PDB carries no owner reference; it would outlive the group and block evictions forever")
	}

	// Both proxies now report a fresh zero: minAvailable has to drop back to
	// 0, or a drained group would still block its own node.
	f.reportProxyPlayers(t, pods[0], 0)
	f.reportProxyPlayers(t, pods[1], 0)
	f.reconcileProxyGroup(r, "gateway")

	if err := f.c.Get(f.ctx, pdbKey, pdb); err != nil {
		t.Fatalf("get proxy PDB after both proxies emptied: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 0 {
		t.Errorf("minAvailable = %v after both proxies emptied, want 0", pdb.Spec.MinAvailable)
	}
}

// TestServerAndProxyGroupsSharingANameGetDistinctBudgets pins the fix for
// task 7's review Critical 1: a ServerGroup and a ProxyGroup are different
// Kinds, so Kubernetes lets a namespace hold one of each under the same
// name -- podspec.GroupConfigMapName's doc comment narrates the incident this
// repository already had from naming a per-group object after the bare group
// name alone. Before the fix, reconcileProxyPDB named its object group.Name,
// exactly like reconcilePDB, so the second of the two controllers to
// reconcile here would have its SetControllerReference call fail with
// AlreadyOwnedError, and its Reconcile would return before setStatus. The
// error itself would have named both the object and the owning controller —
// confirmed by mutation-testing this exact scenario, which reproduced
// "Object .../lobby is already owned by another ServerGroup controller
// lobby" verbatim — but the losing group's own status would carry no sign of
// it: no condition is written for this failure, so a stale Accepted=True (or
// whatever was last there) stays, invisible to anyone reading the CR alone.
func TestServerAndProxyGroupsSharingANameGetDistinctBudgets(t *testing.T) {
	f := newFixture(t)
	// f.group is already a ServerGroup named "lobby" (see newFixture);
	// giving the ProxyGroup the same name is the whole point of this test.
	f.createProxyGroup(f.group.Name)

	sr := groupReconciler(f)
	pr := proxyGroupReconciler(f)
	f.reconcileGroup(t, sr)
	f.reconcileProxyGroup(pr, f.group.Name)
	// Reconcile the ServerGroup again: had the ProxyGroup pass above already
	// stolen the object's controller reference, this call would either error
	// outright or silently leave the ServerGroup without a budget of its own.
	f.reconcileGroup(t, sr)

	serverPDB := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, types.NamespacedName{
		Name: podspec.GroupPDBName(f.group.Name, podspec.RoleServer), Namespace: f.ns,
	}, serverPDB); err != nil {
		t.Fatalf("get ServerGroup PDB: %v", err)
	}
	proxyPDB := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, types.NamespacedName{
		Name: podspec.GroupPDBName(f.group.Name, podspec.RoleProxy), Namespace: f.ns,
	}, proxyPDB); err != nil {
		t.Fatalf("get ProxyGroup PDB: %v", err)
	}

	if serverPDB.Name == proxyPDB.Name {
		t.Fatalf("both budgets are named %q; a same-named ServerGroup and ProxyGroup collide", serverPDB.Name)
	}
	if len(serverPDB.OwnerReferences) == 0 || serverPDB.OwnerReferences[0].Kind != "ServerGroup" {
		t.Errorf("ServerGroup PDB owner references = %+v, want it controlled by the ServerGroup", serverPDB.OwnerReferences)
	}
	if len(proxyPDB.OwnerReferences) == 0 || proxyPDB.OwnerReferences[0].Kind != "ProxyGroup" {
		t.Errorf("ProxyGroup PDB owner references = %+v, want it controlled by the ProxyGroup", proxyPDB.OwnerReferences)
	}
}

// TestABudgetSelectsExactlyThePodsItCounts is the pairing the milestone's
// final review found unasserted for both group kinds, in the configuration
// that made it a disconnect: a ServerGroup and a ProxyGroup sharing one name
// in one namespace, each holding occupied pods.
//
// Every earlier budget assertion checked minAvailable against a number, or
// the occupied label against the pods carrying it, and never the two against
// each other. The ServerGroup's selector was {managed-by, group, occupied}
// with no role term, which was harmless while only server pods carried
// spawnery.cloud/occupied; this milestone put that label on proxy pods too.
// From that point the server budget matched the proxies of the same-named
// ProxyGroup while counting only its own servers, disruptionsAllowed went
// positive, and kubectl drain could evict an occupied server pod.
//
// Both budgets are checked, through one helper, because the property belongs
// to neither kind in particular: whichever selector loses a term next, the
// same assertion is the one that catches it.
func TestABudgetSelectsExactlyThePodsItCounts(t *testing.T) {
	f := newFixture(t)
	sr := groupReconciler(f)
	pr := proxyGroupReconciler(f)
	// f.group is a ServerGroup named "lobby" (see newFixture). A ProxyGroup of
	// the same name in the same namespace is the configuration under test, and
	// Kubernetes permits it -- see podspec.GroupPDBName.
	f.createProxyGroup(f.group.Name)

	// One occupied server. f.reconcile runs the Server controller, which is
	// what writes the occupied label onto the server pod.
	f.reconcileGroup(t, sr)
	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	if err := f.agents.ReportPlayers(uid, 4, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)

	// Two occupied proxies under the same name.
	f.reconcileProxyGroup(pr, f.group.Name)
	proxies := f.proxyPods(f.group.Name)
	if len(proxies) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(proxies))
	}
	for i := range proxies {
		f.markProxyPodReady(t, &proxies[i])
	}
	f.reportProxyPlayers(t, proxies[0], 2)
	f.reportProxyPlayers(t, proxies[1], 3)
	f.reconcileProxyGroup(pr, f.group.Name)
	f.reconcileGroup(t, sr)

	// The configuration has to exist before the assertions mean anything. A
	// run where no proxy ended up labelled occupied would satisfy the helper
	// below for the one reason that says nothing.
	occupiedServers, occupiedProxies := 0, 0
	pods := &corev1.PodList{}
	if err := f.c.List(f.ctx, pods, client.InNamespace(f.ns), client.MatchingLabels{
		podspec.LabelGroup:    f.group.Name,
		podspec.LabelOccupied: "true",
	}); err != nil {
		t.Fatalf("list occupied pods: %v", err)
	}
	for i := range pods.Items {
		switch pods.Items[i].Labels[podspec.LabelRole] {
		case podspec.RoleServer:
			occupiedServers++
		case podspec.RoleProxy:
			occupiedProxies++
		}
	}
	if occupiedServers == 0 || occupiedProxies == 0 {
		t.Fatalf("occupied server pods = %d, occupied proxy pods = %d, want both non-zero: "+
			"without one of each under the same group name this test cannot tell a role-blind "+
			"selector from a correct one", occupiedServers, occupiedProxies)
	}

	f.assertBudgetSelectsExactlyWhatItCounts(t, podspec.GroupPDBName(f.group.Name, podspec.RoleServer))
	f.assertBudgetSelectsExactlyWhatItCounts(t, podspec.GroupPDBName(f.group.Name, podspec.RoleProxy))
}

// stepAfterPriming is the clock TestTheProxyGroupBudgetReadsTheRegistryOnce
// uses to reproduce task 7 review's Critical 2 deterministically, with no
// goroutines and no real concurrency.
//
// ProxyGroupReconciler.Agents is a concrete *agent.Registry, not an
// interface, so there is no seam to intercept Lookup itself or tell one call
// site's reads from another's. The one hook agent.New exposes is the clock
// function -- its own doc comment says "the clock is injectable so the
// staleness rules are testable" -- and that is enough: Registry.Lookup calls
// it fresh on every invocation and derives PlayersStale from the delta, so a
// clock that changes its answer partway through a single Reconcile pass
// changes what Lookup says without anything running concurrently at all.
//
// While priming, Now always returns the fixed instant fresh -- this is for
// setup (Connect, ReportPlayers), which must not be counted against the
// pass under test. Once priming stops, Now counts its own calls: the first
// freshCalls of them still return fresh, and every one after that returns
// fresh stepped forward by step. Choosing freshCalls precisely -- see the
// test for the count and why -- places the step exactly between
// syncOccupiedLabels's read and whatever runs after it.
type stepAfterPriming struct {
	fresh      time.Time
	step       time.Duration
	priming    bool
	freshCalls int
	calls      int
}

func (c *stepAfterPriming) Now() time.Time {
	if c.priming {
		return c.fresh
	}
	c.calls++
	if c.calls > c.freshCalls {
		return c.fresh.Add(c.step)
	}
	return c.fresh
}

// TestTheProxyGroupBudgetReadsTheRegistryOnce is the regression test for
// task 7 review's Critical 2: reconcileProxyPDB used to call r.Agents.Lookup
// a second time for the same pod, independently of syncOccupiedLabels's own
// call, and the registry's answer can change between two calls a few lines
// apart with no concurrency at all -- Lookup re-derives PlayersStale from
// the clock on every call.
//
// One Reconcile pass, in the shape both before and after the fix, makes
// exactly two registry reads before whatever runs immediately after
// syncOccupiedLabels: reconcileReplicas builds its rollout view (one Lookup
// per pod; this fixture uses one pod so this is one call), and
// syncOccupiedLabels itself makes the other. freshCalls: 2 lets both see a
// fresh count; the clock steps forward by twice the report interval starting
// on the call after that. Before the fix, that next call is
// reconcileProxyPDB's own second read of the same pod, and it lands on the
// stepped, now-stale side -- producing minAvailable=1 while the pod carries
// no occupied label at all, confirmed by mutation-testing this exact
// scenario against the pre-fix shape in a throwaway worktree. After the fix,
// that next call is setStatus's unrelated read of Players (which does not
// care about staleness), and minAvailable is sized from the one evaluation
// syncOccupiedLabels already made -- 0, agreeing with the unlabelled pod.
func TestTheProxyGroupBudgetReadsTheRegistryOnce(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)

	const reportInterval = 5 * time.Second
	clk := &stepAfterPriming{
		fresh:      f.clock.Now(),
		step:       2 * reportInterval,
		priming:    true,
		freshCalls: 2,
	}
	customAgents := agent.New(clk.Now, reportInterval, f.clock.Now())
	r.Agents = customAgents

	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 1
	})
	// Creates the one pod. The clock is still priming, so this pass cannot
	// affect the step count the pass under test relies on.
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 1 {
		t.Fatalf("proxy pods = %d, want 1 -- the step count below assumes exactly one", len(pods))
	}
	uid := string(pods[0].UID)
	customAgents.Connect(uid, agent.RoleProxy)
	if err := customAgents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	// The pass under test: the clock steps from fresh to stale partway
	// through it.
	clk.priming = false
	f.reconcileProxyGroup(r, "gateway")

	got := f.proxyPods("gateway")[0]
	if _, labelled := got.Labels[podspec.LabelOccupied]; labelled {
		t.Errorf("pod carries the occupied label, but the count read at the same fresh moment was not stale")
	}

	pdb := &policyv1.PodDisruptionBudget{}
	key := types.NamespacedName{Name: podspec.GroupPDBName("gateway", podspec.RoleProxy), Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, pdb); err != nil {
		t.Fatalf("get proxy PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 0 {
		t.Errorf("minAvailable = %v, want 0: the pod carries no occupied label, and the budget must be sized "+
			"from the same evaluation the label came from, not a second, later read of the registry",
			pdb.Spec.MinAvailable)
	}
}

// TestNodeDrainingConditionNamesTheNodeOnAProxyGroup is the proxy half of
// TestNodeDrainingConditionNamesTheNode: the same condition, built from the
// same fact -- a live pod on a departing node -- reported the same way
// regardless of which group kind noticed it.
func TestNodeDrainingConditionNamesTheNodeOnAProxyGroup(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(pods))
	}
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}

	node := f.ensureNode(t, "node-going-"+f.ns, false)
	f.bindPodToNode(t, &pods[0], node.Name)
	f.ensureNode(t, node.Name, true)
	f.reconcileProxyGroup(r, "gateway")

	cond := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions,
		spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("NodeDraining = %v, want True", cond)
	}
	if !strings.Contains(cond.Message, node.Name) {
		t.Errorf("message %q does not name the node", cond.Message)
	}

	// Release it. The condition reports where pods are, not what has been
	// decided about them, so it drops to False on the fact alone -- nothing
	// here waits for the mark this pass never got around to making.
	f.ensureNode(t, node.Name, false)
	f.reconcileProxyGroup(r, "gateway")
	cond = meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions,
		spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("NodeDraining after uncordon = %v, want False", cond)
	}
}

// TestANodeDrainMarkFiresANodeDrainingEvent is the event half of §3.7 for the
// ProxyGroup: Task 5 marked a proxy for its departing node without telling
// the operator why. It has to reach the actual mark, not just the surge, so
// this mirrors TestAProxyOnACordonedNodeIsReplaced's two-reconcile shape
// rather than stopping at the first pass, where nothing is marked yet and the
// event assertion below would pass for no reason.
func TestANodeDrainMarkFiresANodeDrainingEvent(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*events.FakeRecorder)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(pods))
	}
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}

	going := f.ensureNode(t, "node-going-"+f.ns, false)
	f.bindPodToNode(t, &pods[0], going.Name)
	f.ensureNode(t, going.Name, true)

	f.reconcileProxyGroup(r, "gateway") // surges the replacement; nothing marked yet
	if containsEvent(drainEvents(rec), spawneryv1alpha1.ReasonNodeDraining) {
		t.Fatal("NodeDraining event fired before any proxy was actually marked")
	}

	after := f.proxyPods("gateway")
	for i := range after {
		f.markProxyPodReady(t, &after[i])
	}
	f.reconcileProxyGroup(r, "gateway") // marks the pod on the departing node

	marked := 0
	for _, p := range f.proxyPods("gateway") {
		if _, dated := drainingSince(&p); dated {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1; nothing below tests the event without a mark to attach it to", marked)
	}

	events := drainEvents(rec)
	count := 0
	for _, e := range events {
		if strings.Contains(e, spawneryv1alpha1.ReasonNodeDraining) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("NodeDraining events = %d, want exactly 1: %v", count, events)
	}

	// A resync with the mark and the departing node both unchanged must not
	// re-fire: the event is for the moment of decision, not a status the
	// group repeats every pass. Without the guard against re-stamping an
	// existing mark, this would announce the same drain once per resync for
	// as long as the pod takes to empty.
	f.clock.Advance(resyncInterval)
	f.reconcileProxyGroup(r, "gateway")
	if containsEvent(drainEvents(rec), spawneryv1alpha1.ReasonNodeDraining) {
		t.Error("NodeDraining event fired again on a resync that changed nothing about the mark")
	}
}

// TestAHashMismatchMarkDoesNotFireANodeDrainingEvent is the negative half of
// the same gate: a rolling update marks a proxy too, and §3.7 reserves
// NodeDraining for the occasion a departing node causes, not the occasion a
// spec change does -- that one is already a rolling update, not a drain.
func TestAHashMismatchMarkDoesNotFireANodeDrainingEvent(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*events.FakeRecorder)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(pods))
	}
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}
	drainEvents(rec) // discard anything from setup

	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway") // surges the replacement; nothing marked yet

	after := f.proxyPods("gateway")
	if len(after) != 3 {
		t.Fatalf("proxy pods = %d after the spec change, want 3", len(after))
	}
	for i := range after {
		f.markProxyPodReady(t, &after[i])
	}
	f.reconcileProxyGroup(r, "gateway") // marks exactly one pod, for the hash mismatch

	marked := 0
	for _, p := range f.proxyPods("gateway") {
		if _, dated := drainingSince(&p); dated {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1; nothing below tests the gate without a mark to test it against", marked)
	}

	if containsEvent(drainEvents(rec), spawneryv1alpha1.ReasonNodeDraining) {
		t.Error("NodeDraining event fired for a mark caused by a hash mismatch, not a departing node")
	}
}

// TestGroupsOfNetworkWakesOnlyTheGroupsThatNameIt covers the mapper behind the
// Watches(&Network{}) both group reconcilers now carry. A group refused because
// its Network is missing or unaccepted has no way of hearing that the Network
// came back — it is not an owner, and nothing about the group itself changes —
// so it waited out the resync. docs/known-issues.md measures the pair of waits
// at roughly ninety seconds.
//
// The filter on NetworkRef is not a formality even under one-network-per-
// namespace: a namespace holding a loser as well as a winner is exactly the
// state this operator's own duplicate rule creates, and a group pointed at the
// loser has no business being woken by the winner.
func TestGroupsOfNetworkWakesOnlyTheGroupsThatNameIt(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)

	f.createProxyGroup("gateway")
	f.createProxyGroup("elsewhere", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.NetworkRef = spawneryv1alpha1.ObjectRef{Name: "some-other-network"}
	})

	got := r.groupsOfNetwork(f.ctx, f.network)
	names := make([]string, 0, len(got))
	for _, req := range got {
		names = append(names, req.Name)
	}
	if !slices.Equal(names, []string{"gateway"}) {
		t.Errorf("groupsOfNetwork(%s) = %v, want [gateway] only: a group naming a different "+
			"Network must not be woken by this one", f.network.Name, names)
	}
}
