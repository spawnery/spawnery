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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
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
		// Every reconciler helper in this package wires a recorder whether or
		// not the test under it looks at one, so that a path which only fires
		// under an unusual condition — here, a drain that ran out of time —
		// cannot nil-dereference in a test that stumbled into it. A test that
		// wants to read the events replaces this with its own handle.
		Recorder: record.NewFakeRecorder(100),
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
	// half is the group's nodePort. Both sides of the address are read back
	// from where they were written, not hardcoded twice.
	want := fmt.Sprintf("%s:%d", proxyPodHostIP, f.proxyGroup("gateway").Spec.Expose.NodePort.Port)
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
// A real cache lag cannot be staged here: the fixture's client talks straight
// to envtest's API server (testenv.Client), so every List call this
// reconciler makes already reflects true state and DecideRollout would never
// see a short count on its own. Seeding the reservation directly is the same
// state a lagging informer cache would produce -- Expectations believes a pod
// exists under a name pods() cannot find, because nothing by that name was
// ever actually created -- without depending on real cache timing that this
// package's envtest suite cannot control.
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
	sortPodsOldestFirst(before)
	oldest, unreported, newest := before[0].Name, before[1].Name, before[2].Name
	f.reportProxyPlayers(t, before[2], 0)
	// The oldest pod gets a fresh zero too, and it is what makes this test
	// about occupancy rather than about position: it sits at the head of the
	// list the reconciler walks, where a rule that took its surplus off the
	// tail would leave it, and the report is what puts it among the two that
	// go.
	f.reportProxyPlayers(t, before[0], 0)

	group = f.proxyGroup("gateway")
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
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	sa := &corev1.ServiceAccount{}
	key := types.NamespacedName{Name: podspec.ProxyServiceAccountName, Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, sa); err != nil {
		t.Fatalf("the proxy ServiceAccount was not bootstrapped: %v", err)
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
func drainEvents(rec *record.FakeRecorder) []string {
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

// containsEvent reports whether any event carries the given reason.
//
// FakeRecorder renders an event as "<type> <reason> <message>", so the reason
// is the second space-separated field, and it is compared exactly rather than
// as a substring of the whole line: the reason also appears in prose in this
// file, and a message that merely mentioned it must not be able to stand in
// for an event that was actually recorded.
func containsEvent(events []string, reason string) bool {
	for _, e := range events {
		if fields := strings.SplitN(e, " ", 3); len(fields) >= 2 && fields[1] == reason {
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
	rec := record.NewFakeRecorder(100)
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
	want, err := podspec.DesiredProxyHash(network, f.proxyGroup("gateway"), r.AgentEndpoint)
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
	current, err := podspec.DesiredProxyHash(network, f.proxyGroup("gateway"), r.AgentEndpoint)
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
	current, err := podspec.DesiredProxyHash(network, f.proxyGroup("gateway"), r.AgentEndpoint)
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
