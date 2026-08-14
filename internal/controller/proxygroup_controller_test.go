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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
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

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	if got := f.proxies.lastReady(string(surplus.UID)); got == nil || *got {
		t.Errorf("the surplus proxy was told ready=%v, want false", got)
	}
	if got := f.proxies.lastReady(string(survivor.UID)); got == nil || !*got {
		t.Errorf("the surviving proxy was told ready=%v, want true", got)
	}
}

// deleteSuppressingClient lets everything through except Delete. A pod these
// tests create never leaves Pending — envtest runs no kubelet to move it any
// further — and envtest's apiserver does not hold a Pending pod back for its
// grace period the way it would a Running one: r.Delete removes the object
// outright, in the same Reconcile call that just patched its draining
// annotation onto it, before a test could ever read the annotation back.
// Suppressing the delete is what lets these tests observe what the
// assertion loop actually wrote, and it is also an honest stand-in for
// Task 5's behaviour, which will stop deleting a draining pod immediately —
// exactly what this fake pretends is already true.
type deleteSuppressingClient struct {
	client.Client
}

// Delete implements client.Client by doing nothing.
func (deleteSuppressingClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return nil
}

// TestSurplusProxyIsMarkedWithWhenTheDrainStarted proves the deadline is
// written down: without it, nothing bounds how long the drain may run.
func TestSurplusProxyIsMarkedWithWhenTheDrainStarted(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	r.Client = deleteSuppressingClient{Client: r.Client}
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	sortPodsOldestFirst(before)
	surplusName := before[1].Name

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
// passes over a scaled-down group: reconcileReplicas' own delete loop is
// unchanged by this task, and a surplus pod here never leaves Pending —
// envtest runs no kubelet — so, exactly as deleteSuppressingClient's comment
// above explains, r.Delete removes it outright in the same reconcile that
// marked it. There is no deletion timestamp and no pod left for a second
// Reconcile to revisit, so driving two passes would prove nothing either
// way: the second pass would never call markDraining on that pod at all,
// guard or no guard, and this test would pass even after the mutation in
// Step 6. Calling markDraining directly — the exact call reconcileReplicas
// itself makes once per live pod per pass — is what actually exercises a
// second call to the guard, and the pod is re-read from the API server
// between the two calls so the guard reacts to what was actually persisted.
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

// TestACancelledScaleDownPutsTheProxyBack proves readiness is derived, not
// remembered: an operator restart, or simply a later pass, recomputes which
// pods are surplus from scratch, so a scale-down that is reversed before the
// surplus pod is actually gone corrects itself with nothing left to clean
// up.
//
// It uses deleteSuppressingClient for the same reason
// TestSurplusProxyIsMarkedWithWhenTheDrainStarted does: reconcileReplicas'
// delete loop is unchanged by this task and would otherwise remove the
// surplus pod outright in the very reconcile that marks it, leaving no
// object for the "scale back up" pass to correct. Suppressing the delete
// keeps the same pod in play, which is what actually exercises "derived,
// not remembered" rather than a coincidence of a fresh pod happening to
// start out ready.
func TestACancelledScaleDownPutsTheProxyBack(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	r.Client = deleteSuppressingClient{Client: r.Client}
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	sortPodsOldestFirst(before)
	surplus := before[1]

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
// NotFound, exactly as the real client would if the object were already
// gone server-side. Delete is also suppressed, the same way
// deleteSuppressingClient's is, so every pod in reconcileReplicas' assertion
// loop survives to be inspected afterward — including the pod after the
// racing one in iteration order, which is what
// TestAPodVanishingBetweenListAndPatchDoesNotFailTheReconcile actually
// checks.
type racingPodClient struct {
	client.Client
	racingPod string
}

// Patch implements client.Client.
func (c racingPodClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if obj.GetName() == c.racingPod {
		return apierrors.NewNotFound(corev1.Resource("pods"), obj.GetName())
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// Delete implements client.Client by doing nothing, for the same reason
// deleteSuppressingClient's does.
func (racingPodClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return nil
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
// Three replicas scaled down to one puts two pods in the assertion loop's
// surplus tail. racingPodClient fails the patch for the first of them
// (iteration order, not list order) and this test's real assertion is that
// the second surplus pod — later in the same loop — still gets marked and
// told ready=false. Without client.IgnoreNotFound around markDraining's
// Patch, the racing pod's NotFound error propagates out of reconcileReplicas
// and f.reconcileProxyGroup's own t.Fatalf fires before either check below
// ever runs.
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
	racing, other := before[1], before[2]

	r.Client = racingPodClient{Client: r.Client, racingPod: racing.Name}
	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

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
}
