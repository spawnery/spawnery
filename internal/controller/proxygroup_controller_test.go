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
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/render"
	"github.com/spawnery/spawnery/internal/testenv"
)

type recordingFleet struct {
	mu   sync.Mutex
	last map[string]bool
}

func (f *recordingFleet) SetReady(_ context.Context, podUID string, ready bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.last = map[string]bool{}
	}
	f.last[podUID] = ready
	return nil
}

func (f *recordingFleet) lastReady(podUID string) *bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.last[podUID]
	if !ok {
		return nil
	}
	return &v
}

func (f *fixture) proxyGroupConfigMap(t *testing.T, group string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: podspec.GroupConfigMapName(group, podspec.RoleProxy), Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, cm); err != nil {
		t.Fatalf("get ConfigMap for group %s: %v", group, err)
	}
	return cm
}

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
		Client: f.rc,
		Scheme: testenv.Scheme(f.t),
		Agents: f.agents,
		Bootstrap: &Bootstrapper{
			Client: f.c, Reader: f.c,
			CA: func() []byte { return []byte("test-ca") },
		},
		AgentEndpoint:     "spawnery-operator.spawnery-system.svc:9443",
		OperatorNamespace: "spawnery-system",
		Proxies:           f.proxies,
		Clock:             f.clock.Now,
		Expectations:      newExpectations(f.clock.Now),
		Divergence:        newReadinessDivergence(f.clock.Now),
		// Wired whether or not the test reads events, so a rare path cannot nil-dereference.
		Recorder: newRecorder(),
		// Production must pass an uncached reader; a reconciler built without one panics.
		ClaimReader: f.c,
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

// sortPodsOldestFirst matches the order ProxyGroupReconciler.pods gives its live list.
func sortPodsOldestFirst(pods []corev1.Pod) {
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].CreationTimestamp.Equal(&pods[j].CreationTimestamp) {
			return pods[i].Name < pods[j].Name
		}
		return pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})
}

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

// readinessDivergence counts only watched time, so one Advance past the grace would never report.
func (f *fixture) watchThroughGrace(t *testing.T, r *ProxyGroupReconciler, name string) {
	t.Helper()
	for elapsed := time.Duration(0); elapsed <= readinessDivergenceGrace; elapsed += ResyncInterval {
		f.clock.Advance(ResyncInterval)
		f.reconcileProxyGroup(r, name)
	}
}

const proxyPodHostIP = "192.168.1.10"

// markProxyPodReady does what a kubelet would once the probe passed; f.markReady is for Servers.
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
	for k, v := range svc.Spec.Selector {
		if pods[0].Labels[k] != v {
			t.Errorf("Service selector %s=%q does not match the pods", k, v)
		}
	}
	if svc.Spec.Selector[podspec.LabelRole] != podspec.RoleProxy {
		t.Error("the Service selector must pin the proxy role, or it would also select server pods")
	}
}

// The operator may not read Nodes, so a running proxy pod's hostIP is the NodePort address.
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

	// ReadyReplicas alone would not catch a second pass creating two more pods.
	if n := len(f.proxyPods("gateway")); n != 2 {
		t.Errorf("proxy pods = %d, want the steady-state resync to leave the count at 2", n)
	}

	group := f.proxyGroup("gateway")
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

// A phantom name: the uncached client would clear a real reservation in Reconcile's second pods() call.
func TestProxyGroupCreateCountIsCutByAReservationTheCacheHasNotShown(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway") // replicas: 2

	r.Expectations.expectCreated(f.ns+"/gateway", "gateway-phantom", 0)

	f.reconcileProxyGroup(r, "gateway")

	if n := len(f.proxyPods("gateway")); n != 1 {
		t.Errorf("proxy pods = %d, want 1: the phantom reservation should have "+
			"cut DecideRollout's Create: 2 down by the one already pending", n)
	}
}

// Not a full Reconcile: its second pods() call clears the reservation before Reconcile returns.
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

	// The status pass's pods() call is what clears the reservation.
	if _, err := r.pods(f.ctx, group); err != nil {
		t.Fatalf("pods (second call): %v", err)
	}
	if pendingCreates, _, _ := r.Expectations.pending(key); len(pendingCreates) != 0 {
		t.Errorf("pending creates = %v after the second pods() call, want empty: "+
			"observePods must clear a reservation once its pod is listed", pendingCreates)
	}
}

// A node address for a proxy that is not serving would send players at a closed port.
func TestProxyGroupAddressIsEmptyWithNoReadyPod(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	if got := f.proxyGroup("gateway").Status.Address; got != "" {
		t.Errorf("status.address = %q, want empty while no proxy is ready", got)
	}
}

// An unreported count is untrusted, so the pod with no agent survives and both known-empty pods go.
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
	sortPodsOldestFirst(before)
	oldest, unreported, newest := before[0].Name, before[1].Name, before[2].Name
	f.reportProxyPlayers(t, before[2], 0)
	// The oldest pod's fresh zero puts it at the list head, where a tail-based rule would keep it.
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

// Without the deadline, treating unknown counts as occupied would hold a pod forever.
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

	// An unconnected agent reads the same as one whose stream died with players on it.
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

// Without the bootstrap the pod would mount a ConfigMap that does not exist and never start.
func TestProxyGroupBootstrapsTheNamespace(t *testing.T) {
	f := newFixture(t)

	// newFixture's Network reconcile already bootstrapped this namespace.
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

// The label is what the manager's restricted cache requires.
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
	if values.MaxPlayers != nil {
		t.Errorf("values = %+v, want maxPlayers unset — a ProxyGroup has no maxPlayers", values)
	}
}

// Without a default, playerLimit stays unset while the pod's env defaults it, and the proxy crash-loops.
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

	// A nil spec.config never reaches the CRD default for onlineMode.
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

// The CRD default, exercised through a real API server rather than read off the marker.
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

// The join proof needs offline mode: a Go client cannot authenticate against Microsoft.
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

// The final state cannot tell "written first" from "written at some point"; recording the Creates can.
func TestProxyGroupConfigMapWrittenBeforeThePods(t *testing.T) {
	f := newFixture(t)
	recorder := &createOrderRecorder{Client: f.rc}
	r := &ProxyGroupReconciler{
		Client: recorder,
		Scheme: testenv.Scheme(t),
		Agents: f.agents,
		Bootstrap: &Bootstrapper{
			Client: recorder, Reader: f.rc,
			CA: func() []byte { return []byte("test-ca") },
		},
		AgentEndpoint:     "spawnery-operator.spawnery-system.svc:9443",
		OperatorNamespace: "spawnery-system",
		Proxies:           f.proxies,
		Clock:             f.clock.Now,
		Expectations:      newExpectations(f.clock.Now),
		Divergence:        newReadinessDivergence(f.clock.Now),
		// Repeats newFixture's recorder wiring; see the comment there.
		Recorder: newRecorder(),
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

// The player counts, not pod positions, decide which pod the group keeps.
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

// The surplus proxy's player is what keeps the draining pod around to be read.
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

// Re-stamping an existing mark would push the deadline forever.
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

// Anyone with pod write access can corrupt the annotation; the repair re-stamps it and restarts the clock.
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
	// A player keeps the surplus pod within the deletion loop's reach.
	f.reportProxyPlayers(t, surplus, 1)
	f.setDrainingSince(surplus.Name, "yesterday afternoon")

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

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

	f.clock.Advance(f.proxyGroup("gateway").DrainTimeout() + time.Second)
	f.reconcileProxyGroup(r, "gateway")
	if _, ok := f.pod(surplus.Name); ok {
		t.Error("the repaired pod outlived its drain timeout; the re-stamped deadline never fired")
	}
}

// Readiness is derived each pass, so a reversed scale-down needs no cleanup.
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

// racingPodClient fails Patch with NotFound for racingPod, as if it was evicted after the list.
// fired matters because a fake that is never reached makes the test assert nothing.
type racingPodClient struct {
	client.Client
	racingPod string
	fired     *bool
}

func (c racingPodClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if obj.GetName() == c.racingPod {
		*c.fired = true
		return apierrors.NewNotFound(corev1.Resource("pods"), obj.GetName())
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// Which pod races moves with the selection rule, so the fake reports whether it fired.
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
	// A player keeps the inspected pod around.
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
	// The racing pod survives too: an unknown count is treated as occupied.
	if _, ok := f.pod(racing.Name); !ok {
		t.Error("the racing pod was deleted; its player count is unknown, not zero, " +
			"and an unknown count must be treated as occupied")
	}
}

// The replacement must be ready before anything is withdrawn.
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

// Stale is re-derived each pass, so once uncordoned the pod keeps its mark only through the surplus budget.
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
	// A player holds the departing pod open across the assertions.
	f.reportProxyPlayers(t, pods[0], 1)

	f.reconcileProxyGroup(r, "gateway") // marks the pod on the departing node

	marked, ok := f.pod(pods[0].Name)
	if !ok {
		t.Fatalf("pod %s not found after being marked", pods[0].Name)
	}
	if _, dated := drainingSince(marked); !dated {
		t.Fatalf("pod %s was not marked draining; nothing here would be released by the step below", pods[0].Name)
	}

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

// ReportPlayers refuses a key with no live stream, and an unknown key also reads zero, hence the read-back.
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
		// An unknown key also reports zero; freshness tells the two apart.
		t.Fatalf("registry holds a stale count for proxy %s; the report did not land", pod.Name)
	}
}

// Each event is delivered once, so tests drain the channel and assert on the batch.
func drainEvents(rec *nonBlockingRecorder) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := rec.events
	rec.events = nil
	return out
}

func containsEvent(events []string, reason string) bool {
	for _, e := range events {
		if eventHasReason(e, reason) {
			return true
		}
	}
	return false
}

// Matches the type field: "Warning" could appear in any message body.
func containsEventType(events []string, eventType string) bool {
	for _, e := range events {
		if fields := strings.SplitN(e, " ", 2); len(fields) >= 1 && fields[0] == eventType {
			return true
		}
	}
	return false
}

func containsSubstring(events []string, want string) bool {
	for _, e := range events {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}

// Kubernetes does not close connected sessions on NotReady, so the wait is for empty.
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
	f.clock.Advance(ResyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want the draining proxy still there with its three players", len(pods))
	}
	if _, ok := f.pod(surplus.Name); !ok {
		t.Error("the draining proxy was deleted with three players on it")
	}
	// A reconciler that stopped asserting readiness would also leave two pods standing.
	if got := f.proxies.lastReady(string(surplus.UID)); got == nil || *got {
		t.Errorf("the draining proxy was told ready=%v, want false", got)
	}
}

// A draining proxy is NotReady but still has players, and they must be counted.
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

	// The draining proxy's agent has closed the probe port, so it is NotReady while its players play on.
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

// Disconnect keeps the last count fresh for twice the report interval while the pod is still Ready.
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

	f.clock.Advance(f.proxyGroup("gateway").DrainTimeout() + time.Second)
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got != 1 {
		t.Errorf("proxy pods = %d after the deadline, want 1 — a dead stream must not hold a pod forever", got)
	}
}

// A draining proxy cannot hand players on, so the Warning must name how many were lost.
func TestTheDeadlineDeletesLoudly(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := newRecorder()
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

// Resync heals a lost SetReady; an agent that ignores the instruction must be reported.
func TestAProxyThatIgnoresItsWithdrawalIsReported(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := newRecorder()
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

	// Resync repeats the verdict every 30 seconds, so the event must fire on the transition only.
	f.reconcileProxyGroup(r, "gateway")
	if ev := drainEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v, want none: a resync of an already-reported divergence must stay silent", ev)
	}
}

// The pod must re-diverge, or a delete that no-ops is indistinguishable from a real one.
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

	f.clock.Advance(readinessDivergenceGrace/2 + time.Second)

	pods = f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	surplus := &pods[1]
	f.setProxyPodReadyCondition(t, surplus, false)
	f.reconcileProxyGroup(r, "gateway")

	g := f.proxyGroup("gateway")
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Fatal("reported even though the pod agreed again before the grace period elapsed")
	}

	// Past T0 + grace: a surviving original timestamp would report here.
	f.clock.Advance(readinessDivergenceGrace/2 + time.Second)

	f.setProxyPodReadyCondition(t, surplus, true)
	f.reconcileProxyGroup(r, "gateway")

	g = f.proxyGroup("gateway")
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Fatal("reported the instant it re-diverged: the entry's original start time survived instead of " +
			"being cleared, so the grace period was measured from the first mismatch rather than restarting")
	}

	f.watchThroughGrace(t, r, "gateway")

	g = f.proxyGroup("gateway")
	if !meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Error("re-diverging was never reported at all: something other than a restarted clock is wrong")
	}
}

// Every pod is asserted ready from creation, so a slow first probe must not count as divergence.
func TestASlowStartingProxyIsNotReportedAsDiverged(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")
	// New pods reach the divergence check only on the second pass; see Reconcile's r.pods call.
	f.reconcileProxyGroup(r, "gateway")

	f.watchThroughGrace(t, r, "gateway")

	g := f.proxyGroup("gateway")
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Error("a pod that has simply never passed its first probe was reported as ReadinessDiverged")
	}
	if ev := drainEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v, want none: a slow-starting pod is not a withdrawal that was ignored", ev)
	}
}

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

	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")
	after := f.proxyPods("gateway")
	if len(after) != 4 {
		t.Fatalf("proxy pods = %d after the spec change, want 4 — every replacement must exist before anything is marked", len(after))
	}
	for i := range after {
		if _, dated := drainingSince(&after[i]); dated {
			t.Errorf("pod %s was marked while the replacements are still unready; ready capacity would dip below replicas", after[i].Name)
		}
	}
}

// On the second pass the group is at its target, so only the readiness gate stops a mark.
func TestTheReplacementsMustBeReadyBeforeAnyPodIsMarked(t *testing.T) {
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
	f.clock.Advance(ResyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 4 {
		t.Fatalf("proxy pods = %d, want the group's 2 replicas plus a replacement for each", len(pods))
	}
	for i := range pods {
		if _, dated := drainingSince(&pods[i]); dated {
			t.Fatalf("pod %s was marked before the replacements turned ready; the group has 2 replicas and would "+
				"have been left without a ready proxy", pods[i].Name)
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
	if marked != 2 {
		t.Errorf("marked = %d, want 2 — once the replacements are Ready every old proxy is marked at once", marked)
	}
}

// Staleness is a digest of the rendered pod, not metadata.generation.
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

// DecideRollout never names an already-draining pod, so rebuilding leaving from it would cancel the drain.
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
		t.Fatal("no proxy was marked once the replacements were ready; there is no drain to keep")
	}

	f.clock.Advance(ResyncInterval)
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

	// The rollout takes two passes; the rest show a settled group stays settled.
	for pass := 0; pass < 10; pass++ {
		pods := f.proxyPods("gateway")
		for i := range pods {
			f.markProxyPodReady(t, &pods[i])
			f.reportProxyPlayers(t, pods[i], 0)
		}
		f.reconcileProxyGroup(r, "gateway")
		if n := len(f.proxyPods("gateway")); n > 4 {
			t.Fatalf("proxy pods = %d on pass %d, want at most replicas + one replacement per stale proxy", n, pass)
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

// A lost surge pod must not release the mark: the stale pod has to go regardless.
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
	f.clock.Advance(ResyncInterval)
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
	if n := len(f.proxyPods("gateway")); n != 4 {
		t.Errorf("proxy pods = %d, want 4 — the lost replacement must come up again under the ones that are going", n)
	}
}

// Raising four-to-two back to three must release exactly one of the two marks.
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
	f.clock.Advance(ResyncInterval)
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 4 {
		t.Fatalf("proxy pods = %d, want the four still there — each has a player on it", len(pods))
	}
	marked := markedProxies(pods)
	if len(marked) != 1 {
		t.Errorf("marked = %v, want 1 — three replicas out of four pods leaves one surplus, not two", marked)
	}
	// The annotation dates the deadline; readiness is what carries players.
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

// A stale mark is already leaving, so it must not also count against the surplus budget.
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

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")
	marked := markedProxies(f.proxyPods("gateway"))
	if len(marked) != 2 {
		t.Fatalf("marked = %v, want 2 — four pods down to one replica, with a surge of 1 for the stale pod", marked)
	}
	if !slices.Contains(marked, stale) {
		t.Fatalf("marked = %v, want the stale proxy %s among them", marked, stale)
	}

	f.setProxyReplicas("gateway", 3)
	f.clock.Advance(ResyncInterval)
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

// A rollback keeps the marks: releasing them would let a flapping spec restart drain deadlines.
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
	// A proxy with no agent counts as occupied, so both stay for the revert.
	held := before[0]
	f.reportProxyPlayers(t, held, 1)
	for i := range before {
		f.markProxyPodReady(t, &before[i])
	}

	g := f.proxyGroup("gateway")
	shipped := g.Spec.Image
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")
	surged := f.proxyPods("gateway")
	if len(surged) != 4 {
		t.Fatalf("proxy pods = %d after the spec change, want 4", len(surged))
	}
	for i := range surged {
		f.markProxyPodReady(t, &surged[i])
	}
	f.reconcileProxyGroup(r, "gateway")

	originals := []string{before[0].Name, before[1].Name}
	sort.Strings(originals)
	if got := markedProxies(f.proxyPods("gateway")); !slices.Equal(got, originals) {
		t.Fatalf("marked = %v, want both originals %v", got, originals)
	}
	marked, ok := f.pod(held.Name)
	if !ok {
		t.Fatalf("pod %s not found", held.Name)
	}
	at := marked.Annotations[ProxyDrainingSinceAnnotation]

	g = f.proxyGroup("gateway")
	g.Spec.Image = shipped
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("revert: %v", err)
	}
	f.clock.Advance(ResyncInterval)
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

	if got := markedProxies(f.proxyPods("gateway")); !slices.Equal(got, originals) {
		t.Errorf("marked = %v, want just the originals %v still — the budget is spent on departures under way, "+
			"and the replacements' have not started", got, originals)
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

// A broken Network must not freeze the proxy budget.
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

	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	node := f.ensureNode(t, "node-going-"+f.ns, false)
	f.bindPodToNode(t, &pods[1], node.Name)
	f.ensureNode(t, node.Name, true)
	f.reportProxyPlayers(t, pods[0], 5)

	f.reconcileProxyGroup(r, "gateway")

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

// Unlike proxyOccupied, an unknown pod counts only within the post-restart grace: no deadline ends a budget.
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
			agent.Snapshot{Players: 0, PlayersStale: true, StreamDownFor: budgetReconnectGrace},
			false,
		},
		{
			"a pod the registry has never seen counts while the operator is still coming up",
			agent.Snapshot{Players: 0, PlayersStale: true, StreamDownFor: budgetReconnectGrace - time.Second},
			true,
		},
		{
			"a pod still reconnecting at eighteen seconds is protected, which fifteen did not do",
			agent.Snapshot{Players: 0, PlayersStale: true, StreamDownFor: 18 * time.Second},
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

// newFixture starts the registry with the clock, so unknown pods begin inside budgetReconnectGrace.
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
	f.reportProxyPlayers(t, pods[0], 4)

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

	// Keep the reporting proxy fresh, or it goes stale on the same advance.
	f.clock.Advance(budgetReconnectGrace + time.Second)
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

	// A drained group must not block its own node.
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

// A ServerGroup and a ProxyGroup may share a name, so their budgets must not.
func TestServerAndProxyGroupsSharingANameGetDistinctBudgets(t *testing.T) {
	f := newFixture(t)
	// f.group is a ServerGroup named "lobby" (see newFixture).
	f.createProxyGroup(f.group.Name)

	sr := groupReconciler(f)
	pr := proxyGroupReconciler(f)
	f.reconcileGroup(t, sr)
	f.reconcileProxyGroup(pr, f.group.Name)
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

// Both kinds carry spawnery.cloud/occupied, so a selector without the role term matches the other kind's pods.
func TestABudgetSelectsExactlyThePodsItCounts(t *testing.T) {
	f := newFixture(t)
	sr := groupReconciler(f)
	pr := proxyGroupReconciler(f)
	// f.group is a ServerGroup named "lobby" (see newFixture).
	f.createProxyGroup(f.group.Name)

	// f.reconcile runs the Server controller, which writes the occupied label.
	f.reconcileGroup(t, sr)
	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	if err := f.agents.ReportPlayers(uid, 4, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)

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

// stepAfterPriming steps the clock after freshCalls reads, changing Lookup's answer mid-Reconcile without concurrency.
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

// One pass reads the registry twice before reconcileProxyPDB runs, hence freshCalls: 2.
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

	// The condition reports where pods are, not what was decided about them.
	f.ensureNode(t, node.Name, false)
	f.reconcileProxyGroup(r, "gateway")
	cond = meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions,
		spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("NodeDraining after uncordon = %v, want False", cond)
	}
}

// Two reconciles: the first pass only surges, and nothing is marked yet.
func TestANodeDrainMarkFiresANodeDrainingEvent(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)
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

	// The event is for the decision, not repeated on every resync.
	f.clock.Advance(ResyncInterval)
	f.reconcileProxyGroup(r, "gateway")
	if containsEvent(drainEvents(rec), spawneryv1alpha1.ReasonNodeDraining) {
		t.Error("NodeDraining event fired again on a resync that changed nothing about the mark")
	}
}

func TestAHashMismatchMarkDoesNotFireANodeDrainingEvent(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)
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
	f.reconcileProxyGroup(r, "gateway") // surges the replacements; nothing marked yet

	after := f.proxyPods("gateway")
	if len(after) != 4 {
		t.Fatalf("proxy pods = %d after the spec change, want 4", len(after))
	}
	for i := range after {
		f.markProxyPodReady(t, &after[i])
	}
	f.reconcileProxyGroup(r, "gateway") // marks both old pods, for the hash mismatch

	marked := 0
	for _, p := range f.proxyPods("gateway") {
		if _, dated := drainingSince(&p); dated {
			marked++
		}
	}
	if marked != 2 {
		t.Fatalf("marked = %d, want 2; nothing below tests the gate without a mark to test it against", marked)
	}

	if containsEvent(drainEvents(rec), spawneryv1alpha1.ReasonNodeDraining) {
		t.Error("NodeDraining event fired for a mark caused by a hash mismatch, not a departing node")
	}
}

// A namespace may hold a losing duplicate Network; groups pointed at it must not be woken by the winner.
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

// Asserts the outcome, so inlining either call site of ProxyPlayerLimit fails here.
func TestTheProxyPlayerLimitIsDecidedTheSameWayTwice(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *spawneryv1alpha1.ProxyConfigSpec
		want int32
	}{
		{"no config block at all", nil, podspec.DefaultPlayerLimit},
		{"a config block that sets nothing", &spawneryv1alpha1.ProxyConfigSpec{}, podspec.DefaultPlayerLimit},
		{"an explicit zero", &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 0}, podspec.DefaultPlayerLimit},
		{"a real limit", &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 40}, 40},
		{"a limit that happens to equal the default", &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: podspec.DefaultPlayerLimit}, podspec.DefaultPlayerLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &spawneryv1alpha1.ProxyGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "ns"},
				Spec: spawneryv1alpha1.ProxyGroupSpec{
					NetworkRef: spawneryv1alpha1.ObjectRef{Name: "net"},
					Replicas:   1,
					Image:      "ghcr.io/spawnery/velocity:test",
					Expose: spawneryv1alpha1.ExposeSpec{
						Type:     spawneryv1alpha1.ExposeNodePort,
						NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30000},
					},
					Routing: spawneryv1alpha1.RoutingSpec{FallbackGroups: []string{"lobby"}},
					Config:  tc.cfg,
				},
			}
			net := &spawneryv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Name: "net", Namespace: "ns"}}

			pod, err := podspec.BuildProxyPod(net, group, "gateway-aaaa", "spawnery.invalid:0", nil)
			if err != nil {
				t.Fatalf("BuildProxyPod: %v", err)
			}
			var fromPod string
			for _, e := range pod.Spec.Containers[0].Env {
				if e.Name == podspec.EnvPlayerLimit {
					fromPod = e.Value
				}
			}
			if fromPod == "" {
				t.Fatalf("the pod carries no %s at all", podspec.EnvPlayerLimit)
			}

			values := proxyConfigValues(group)
			if values.PlayerLimit == nil {
				t.Fatal("the ConfigMap's playerLimit is nil; the proxy refuses to start on that, " +
					"and this is exactly the shape that crash-looped every pod of a group with " +
					"no spec.config")
			}

			fromConfig := strconv.FormatInt(int64(*values.PlayerLimit), 10)
			if fromPod != fromConfig {
				t.Errorf("SPAWNERY_PLAYER_LIMIT = %s but the ConfigMap says playerLimit: %s. "+
					"The same proxy reads both", fromPod, fromConfig)
			}
			if fromConfig != strconv.FormatInt(int64(tc.want), 10) {
				t.Errorf("both say %s, want %d", fromConfig, tc.want)
			}
		})
	}
}

// Lookup returns the last report, so a dead or never-connected agent's count is unknown, not zero.
func TestTheDeadlineEventSaysWhatItActuallyKnows(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap agent.Snapshot
		want string
	}{
		{
			"an agent that never connected",
			agent.Snapshot{Known: false, PlayersStale: true},
			"unknown",
		},
		{
			"a report that has gone stale",
			agent.Snapshot{Known: true, Players: 7, PlayersStale: true},
			"already stale, said 7 player(s)",
		},
		{
			"a current report",
			agent.Snapshot{Known: true, Connected: true, Players: 7},
			"7 player(s) still connected",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyPlayerNote(tc.snap); !strings.Contains(got, tc.want) {
				t.Errorf("proxyPlayerNote = %q, want it to contain %q", got, tc.want)
			}
		})
	}

	// A phrase saying "unknown" and printing a zero would satisfy the table above.
	if got := proxyPlayerNote(agent.Snapshot{PlayersStale: true}); strings.Contains(got, "0 player") {
		t.Errorf("proxyPlayerNote = %q for a pod no agent ever reported from; it must not "+
			"name a count, least of all zero", got)
	}
}

type refusingPodDeleter struct {
	client.Client
	err error
}

func (r refusingPodDeleter) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		return r.err
	}
	return r.Client.Delete(ctx, obj, opts...)
}

// A pass that did not delete the pod has nothing to announce.
func TestTheDeadlineIsAnnouncedOnlyOnceThePodIsGone(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	sortPodsOldestFirst(before)
	f.reportProxyPlayers(t, before[1], 3)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")
	drainEvents(rec)

	f.clock.Advance(f.proxyGroup("gateway").DrainTimeout() + time.Second)

	broken := *r
	broken.Client = refusingPodDeleter{Client: f.c, err: errors.New("no pod deletes today")}
	if _, err := broken.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gateway", Namespace: f.ns},
	}); err == nil {
		t.Fatal("the reconcile succeeded with a client that refuses pod deletes")
	}
	if ev := drainEvents(rec); containsEvent(ev, "ProxyDrainTimeout") {
		t.Errorf("events = %v after a refused delete, want no ProxyDrainTimeout. The pod is "+
			"still there and still serving; announcing the disconnection now means "+
			"announcing it again on every pass until the refusal is lifted", ev)
	}

	f.reconcileProxyGroup(r, "gateway")
	if ev := drainEvents(rec); !containsEvent(ev, "ProxyDrainTimeout") {
		t.Errorf("events = %v once the pod could be deleted, want the deadline announced", ev)
	}
}

// A digest from the wrong place would leave every group always in sync or always pending.
func TestAProxyGroupReportsItsOwnRotationState(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)

	// Without a digest first, the pods carry no stamp and the group reports PodsPredateTracking.
	publishDigest := func(hash string) {
		t.Helper()
		net := f.getNetwork(t, f.network.Name)
		net.Status.ForwardingSecretHash = hash
		if err := f.c.Status().Update(f.ctx, net); err != nil {
			t.Fatalf("publish a forwarding digest on the network: %v", err)
		}
	}
	publishDigest("aaaaaaaaaaaaaaaa")

	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got == 0 {
		t.Fatal("no proxy pods were created")
	}

	publishDigest("bbbbbbbbbbbbbbbb")

	f.reconcileProxyGroup(r, "gateway")
	got := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions,
		spawneryv1alpha1.ConditionForwardingSecretRotationPending)
	if got == nil {
		t.Fatal("the group carries no ForwardingSecretRotationPending condition at all")
	}
	// A digest from the wrong place reads as Unknown/SecretUnresolved, which is also not False.
	if got.Status != metav1.ConditionTrue || got.Reason != spawneryv1alpha1.ReasonRotationPending {
		t.Errorf("condition = %s/%s, want True/%s — every pod of this group was stamped before "+
			"the digest the network now publishes",
			got.Status, got.Reason, spawneryv1alpha1.ReasonRotationPending)
	}
}

// refusedFixture returns pods in proxyPods order with pods[1] on the departing node, and deletes the Network.
func refusedFixture(t *testing.T) (*fixture, *ProxyGroupReconciler, []corev1.Pod, *corev1.Node) {
	t.Helper()
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
	f.bindPodToNode(t, &pods[1], node.Name)
	f.ensureNode(t, node.Name, true)
	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	return f, r, pods, node
}

func (f *fixture) proxyPod(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	for _, p := range f.proxyPods("gateway") {
		if p.Name == name {
			return &p
		}
	}
	return nil
}

func assertRefused(t *testing.T, f *fixture) {
	t.Helper()
	accepted := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions,
		spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason != spawneryv1alpha1.ReasonNetworkNotFound {
		t.Fatalf("Accepted = %v, want False/%s", accepted, spawneryv1alpha1.ReasonNetworkNotFound)
	}
}

// A refused group must still drain a proxy off a departing node, or kubectl drain cannot finish.
func TestARefusedGroupDrainsAProxyOffADepartingNode(t *testing.T) {
	f, r, pods, _ := refusedFixture(t)
	going := pods[1]

	f.reportProxyPlayers(t, going, 0)

	f.reconcileProxyGroup(r, "gateway")
	assertRefused(t, f)

	if got := f.proxies.lastReady(string(going.UID)); got == nil || *got {
		t.Errorf("lastReady(%s) = %v, want false: the proxy on the departing node is still taking players",
			going.Name, got)
	}
	if got := f.proxies.lastReady(string(pods[0].UID)); got == nil || !*got {
		t.Errorf("lastReady(%s) = %v, want true", pods[0].Name, got)
	}

	// A live-stream zero: a silent agent would read as occupied.
	live := f.proxyPods("gateway")
	for _, p := range live {
		if p.Name == going.Name {
			t.Fatalf("proxy %s is still there; a refused group still cannot drain a departing node", p.Name)
		}
	}
}

func TestARefusedGroupWaitsOutTheDeadlineForAnOccupiedProxy(t *testing.T) {
	f, r, pods, _ := refusedFixture(t)
	going := pods[1]
	f.reportProxyPlayers(t, going, 3)

	f.reconcileProxyGroup(r, "gateway")
	assertRefused(t, f)

	marked := f.proxyPod(t, going.Name)
	if marked == nil {
		t.Fatal("the occupied proxy was deleted with players on it")
	}
	if _, dated := drainingSince(marked); !dated {
		t.Error("the occupied proxy carries no draining mark, so its deadline never starts")
	}
	if got := f.proxies.lastReady(string(going.UID)); got == nil || *got {
		t.Errorf("lastReady = %v, want false", got)
	}

	// DrainTimeout's default: the fixture sets no spec.drain.
	f.clock.Advance(6 * time.Minute)
	f.reconcileProxyGroup(r, "gateway")

	if f.proxyPod(t, going.Name) != nil {
		t.Error("the proxy outlived its drain deadline on a refused group")
	}
}

// Surplus and a stale hash can wait for the Network; only a departing node cannot.
func TestARefusedGroupLeavesASurplusProxyAlone(t *testing.T) {
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

	group := f.proxyGroup("gateway")
	group.Spec.Replicas = 1
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("lower replicas: %v", err)
	}
	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")
	assertRefused(t, f)

	if live := f.proxyPods("gateway"); len(live) != 2 {
		t.Errorf("proxy pods = %d, want both: a refused group drains for a departing node and nothing else",
			len(live))
	}
	for i := range pods {
		if got := f.proxies.lastReady(string(pods[i].UID)); got == nil || !*got {
			t.Errorf("lastReady(%s) = %v, want true", pods[i].Name, got)
		}
	}
}

// A refused group cannot create a replacement, so leaving must be recomputed from the node.
func TestUncordoningRestoresARefusedGroupsProxy(t *testing.T) {
	f, r, pods, node := refusedFixture(t)
	going := pods[1]
	f.reportProxyPlayers(t, going, 2)

	f.reconcileProxyGroup(r, "gateway")
	assertRefused(t, f)
	if marked := f.proxyPod(t, going.Name); marked == nil {
		t.Fatal("the proxy went before it could be restored")
	} else if _, dated := drainingSince(marked); !dated {
		t.Fatal("the proxy was never marked, so this test would pass for the wrong reason")
	}

	f.ensureNode(t, node.Name, false)
	f.reconcileProxyGroup(r, "gateway")

	restored := f.proxyPod(t, going.Name)
	if restored == nil {
		t.Fatal("the proxy was deleted after its node was released")
	}
	if _, dated := drainingSince(restored); dated {
		t.Error("the draining mark survived the uncordon, so the deadline is still running")
	}
	if got := f.proxies.lastReady(string(going.UID)); got == nil || !*got {
		t.Errorf("lastReady = %v, want true: the proxy is out of service with nothing able to bring it back", got)
	}
}

func TestACrashLoopingProxyIsReportedOnTheGroup(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("no proxy pods")
	}
	crashed := pods[0].DeepCopy()
	crashed.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "velocity",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  "CrashLoopBackOff",
			Message: "back-off 5m0s restarting failed container",
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1,
			Reason:   "Error",
		}},
	}}
	if err := f.c.Status().Update(f.ctx, crashed); err != nil {
		t.Fatalf("set the crash-loop status: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")

	cond := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions,
		spawneryv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonCrashLoopBackoff)
	}
	// The waiting message is generic; the previous exit code is what an operator can act on.
	for _, want := range []string{pods[0].Name, "exit 1", "Error"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q does not mention %q", cond.Message, want)
		}
	}
}

func TestAHealthyProxyGroupIsNotReportedAsCrashLooping(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")
	for _, p := range f.proxyPods("gateway") {
		pod := p
		f.markProxyPodReady(t, &pod)
	}

	f.reconcileProxyGroup(r, "gateway")

	cond := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions,
		spawneryv1alpha1.ConditionDegraded)
	if cond != nil && cond.Reason == spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("a healthy group reports a crash loop: %q", cond.Message)
	}
}

func TestProxyConfigValuesCarriesTheTransferOptIn(t *testing.T) {
	group := &spawneryv1alpha1.ProxyGroup{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "ns"}}

	off, err := yaml.Marshal(proxyConfigValues(group))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(off), "acceptsTransfers") {
		t.Errorf("config.yaml without transfer mentions acceptsTransfers, which moves every proxy's hash:\n%s", off)
	}

	group.Spec.Update = &spawneryv1alpha1.ProxyUpdateSpec{Transfer: &spawneryv1alpha1.ProxyTransferSpec{}}
	if !proxyConfigValues(group).AcceptsTransfers {
		t.Error("a group with transfer renders a proxy that refuses transferred players")
	}
}
