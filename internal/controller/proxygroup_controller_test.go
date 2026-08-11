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
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
