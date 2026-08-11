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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/render"
)

// groupConfigMap re-reads the ConfigMap a ServerGroupReconciler renders for
// the fixture's group.
func (f *fixture) groupConfigMap(t *testing.T, group string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: podspec.GroupConfigMapName(group, podspec.RoleServer), Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, cm); err != nil {
		t.Fatalf("get ConfigMap for group %s: %v", group, err)
	}
	return cm
}

// groupReconciler wires a ServerGroup reconciler onto an existing fixture.
func groupReconciler(f *fixture) *ServerGroupReconciler {
	return &ServerGroupReconciler{
		Client:   f.c,
		Scheme:   f.reconc.Scheme,
		Recorder: record.NewFakeRecorder(100),
		Agents:   f.agents,
		Clock:    f.clock.Now,
	}
}

func (f *fixture) reconcileGroup(t *testing.T, r *ServerGroupReconciler) {
	t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.group.Name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile group: %v", err)
	}
}

func (f *fixture) listServers(t *testing.T) []spawneryv1alpha1.Server {
	t.Helper()
	list := &spawneryv1alpha1.ServerList{}
	if err := f.c.List(f.ctx, list, ctrlclientInNamespace(f.ns)); err != nil {
		t.Fatalf("list servers: %v", err)
	}
	return list.Items
}

// setMinReplicas re-reads the group, moves its floor and writes it back, so
// the fixture's copy stays in step with the persisted generation.
func (f *fixture) setMinReplicas(t *testing.T, n int32) {
	t.Helper()
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.MinReplicas = n
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
}

// groupPDB re-reads the group's PodDisruptionBudget.
func (f *fixture) groupPDB(t *testing.T) *policyv1.PodDisruptionBudget {
	t.Helper()
	pdb := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, pdb); err != nil {
		t.Fatalf("get PDB: %v", err)
	}
	return pdb
}

// publishPDBStatus computes and writes the PodDisruptionBudget status that
// kube-controller-manager's disruption controller would produce. envtest runs
// no controller manager, and the API server's eviction handler reads only that
// status: a budget whose observedGeneration lags its generation is refused
// outright, so without this every eviction below would be refused for a reason
// that has nothing to do with our labels.
//
// The arithmetic is the disruption controller's. Healthy means selected by the
// budget and Ready; the allowed disruptions are the surplus over minAvailable,
// floored at zero. Nothing here is hand-picked — it is all derived from what
// the controllers actually put in the cluster, so the numbers move when the
// occupancy rule moves.
func (f *fixture) publishPDBStatus(t *testing.T) {
	t.Helper()
	pdb := f.groupPDB(t)

	pods := &corev1.PodList{}
	if err := f.c.List(f.ctx, pods, ctrlclientInNamespace(f.ns),
		client.MatchingLabels(pdb.Spec.Selector.MatchLabels)); err != nil {
		t.Fatalf("list the pods the budget selects: %v", err)
	}
	var expected, healthy int32
	for i := range pods.Items {
		expected++
		for _, c := range pods.Items[i].Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				healthy++
			}
		}
	}

	desired := int32(pdb.Spec.MinAvailable.IntValue())
	allowed := healthy - desired
	if allowed < 0 {
		allowed = 0
	}
	pdb.Status = policyv1.PodDisruptionBudgetStatus{
		ObservedGeneration: pdb.Generation,
		CurrentHealthy:     healthy,
		DesiredHealthy:     desired,
		ExpectedPods:       expected,
		DisruptionsAllowed: allowed,
	}
	if err := f.c.Status().Update(f.ctx, pdb); err != nil {
		t.Fatalf("publish PDB status: %v", err)
	}
}

// evict makes the call kubectl drain makes: create an Eviction against the
// pod's eviction subresource and let the API server decide.
func (f *fixture) evict(t *testing.T, name string) error {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns}}
	return f.c.SubResource("eviction").Create(f.ctx, pod, &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
	})
}

func TestGroupCreatesItsFloor(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want minReplicas = 1", len(servers))
	}
	srv := servers[0]
	if !strings.HasPrefix(srv.Name, "lobby-") {
		t.Errorf("server name = %q, want the group prefix", srv.Name)
	}
	if srv.Spec.GroupRef.Name != "lobby" {
		t.Errorf("groupRef = %q, want lobby", srv.Spec.GroupRef.Name)
	}
	if srv.Spec.GroupGeneration != f.group.Generation {
		t.Errorf("groupGeneration = %d, want %d", srv.Spec.GroupGeneration, f.group.Generation)
	}
	if len(srv.OwnerReferences) != 1 ||
		srv.OwnerReferences[0].Kind != "ServerGroup" ||
		srv.OwnerReferences[0].Controller == nil || !*srv.OwnerReferences[0].Controller {
		t.Errorf("owner references = %+v, want a ServerGroup controller ref", srv.OwnerReferences)
	}
}

func TestGroupScalesUpToTheFloor(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.group.Spec.Scaling.MinReplicas = 3
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	if got := len(f.listServers(t)); got != 3 {
		t.Fatalf("got %d servers, want 3", got)
	}

	// Names must be unique, or the pods would collide.
	names := map[string]bool{}
	for _, s := range f.listServers(t) {
		if names[s.Name] {
			t.Fatalf("duplicate server name %q", s.Name)
		}
		names[s.Name] = true
	}
}

func TestGroupDeletesOnlyEmptySurplus(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.group.Spec.Scaling.MinReplicas = 2
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(servers))
	}

	// Give both a pod and make one of them busy.
	busy := servers[0].Name
	for _, s := range servers {
		f.reconcile(s.Name)
	}
	for _, s := range servers {
		pod, ok := f.pod(s.Name)
		if !ok {
			t.Fatalf("no pod for %s", s.Name)
		}
		f.setPodRunning(s.Name, true)
		f.agents.Connect(string(pod.UID), agentRoleServer())
		f.agents.MarkReady(string(pod.UID))
		players := int32(0)
		if s.Name == busy {
			players = 5
		}
		if err := f.agents.ReportPlayers(string(pod.UID), players, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile(s.Name)
	}

	// Shrink the floor to 1 — exactly one server must go, and it must be the
	// empty one.
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.MinReplicas = 1
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	for _, s := range f.listServers(t) {
		if s.Name == busy && !s.DeletionTimestamp.IsZero() {
			t.Fatal("the occupied server was marked for deletion — core invariant broken")
		}
	}
}

// TestOccupiedServerSurvivesAContinuousScaleDown drives the core invariant the
// way the operator really runs it: a scale-down under a reconcile loop at the
// resync cadence, with live agents reporting throughout. A single reconcile
// cannot see a rule that only breaks on repetition — a group that re-nominates
// the occupied server once the empty one is gone, or one that keeps deleting
// past its floor, looks perfectly healthy after one pass.
func TestOccupiedServerSurvivesAContinuousScaleDown(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.setMinReplicas(t, 2)
	f.reconcileGroup(t, r)

	uids := map[string]string{}
	for _, s := range f.listServers(t) {
		uids[s.Name] = bringUpNamed(t, f, s.Name)
	}
	if len(uids) != 2 {
		t.Fatalf("got %d servers, want 2", len(uids))
	}

	var busy string
	for _, s := range f.listServers(t) {
		if busy == "" || s.Name < busy {
			busy = s.Name
		}
	}
	if err := f.agents.ReportPlayers(uids[busy], 7, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	f.setMinReplicas(t, 1)

	for i := 0; i < 60; i++ {
		// Live agents keep reporting, so no count goes stale by accident: the
		// test must exercise the occupied rule, not the staleness rule.
		for name, uid := range uids {
			players := int32(0)
			if name == busy {
				players = 7
			}
			// A server that has already gone away has no stream left; that is
			// not a failure of this test.
			_ = f.agents.ReportPlayers(uid, players, 100)
		}
		for _, s := range f.listServers(t) {
			f.reconcile(s.Name)
		}
		f.reconcileGroup(t, r)

		found := false
		for _, s := range f.listServers(t) {
			if s.Name != busy {
				continue
			}
			found = true
			if !s.DeletionTimestamp.IsZero() {
				t.Fatalf("pass %d marked the occupied server %q for deletion — core invariant broken", i, s.Name)
			}
		}
		if !found {
			t.Fatalf("pass %d removed the occupied server %q outright", i, busy)
		}
		f.clock.Advance(resyncInterval)
	}

	final := f.listServers(t)
	if len(final) != 1 || final[0].Name != busy {
		names := make([]string, 0, len(final))
		for _, s := range final {
			names = append(names, s.Name)
		}
		t.Fatalf("group settled on %v, want only the occupied server %q", names, busy)
	}
	if got := final[0].Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase of the surviving server = %q, want Ready", got)
	}
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 1 {
		t.Errorf("minAvailable = %d, want 1 — the surviving pod still carries players", got)
	}
}

// TestGroupHoldsItsFloorWithoutChurn is the other half of the loop: a healthy
// group must reach its floor and then do nothing at all, pass after pass. A
// sizing bug that creates one server per reconcile is invisible in a test that
// reconciles once.
func TestGroupHoldsItsFloorWithoutChurn(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.setMinReplicas(t, 2)
	f.reconcileGroup(t, r)

	uids := map[string]string{}
	for _, s := range f.listServers(t) {
		uids[s.Name] = bringUpNamed(t, f, s.Name)
	}

	for i := 0; i < 60; i++ {
		for _, uid := range uids {
			_ = f.agents.ReportPlayers(uid, 0, 100)
		}
		for _, s := range f.listServers(t) {
			f.reconcile(s.Name)
		}
		f.reconcileGroup(t, r)

		servers := f.listServers(t)
		if len(servers) != 2 {
			t.Fatalf("pass %d: got %d servers, want the floor of 2", i, len(servers))
		}
		for _, s := range servers {
			if !s.DeletionTimestamp.IsZero() {
				t.Fatalf("pass %d marked %q for deletion although the group sits exactly on its floor", i, s.Name)
			}
			if _, known := uids[s.Name]; !known {
				t.Fatalf("pass %d created the extra server %q", i, s.Name)
			}
		}
		f.clock.Advance(resyncInterval)
	}
}

// TestGroupReplacesAFailedServer settles what a Failed server means for the
// size of a group. A Failed server is deregistered from the proxies and kept
// for spec.failedRetentionSeconds — an hour by default — purely so somebody can
// look at it. No player can join it. Counting it toward the floor would leave
// the group with nothing playable for that whole hour, which turns a diagnostic
// aid into an outage, so it does not count and a replacement is created at once.
// The failed server itself stays: its cleanup belongs to the Server controller.
func TestGroupReplacesAFailedServer(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want minReplicas = 1", len(servers))
	}
	failed := servers[0].Name

	bringUpNamed(t, f, failed)
	driveToFailed(t, f, failed)

	f.reconcileGroup(t, r)

	var replacement string
	for _, s := range f.listServers(t) {
		if s.Name != failed {
			replacement = s.Name
		}
	}
	if replacement == "" {
		t.Fatalf("the group stayed below its floor with its only server in Failed; servers = %d", len(f.listServers(t)))
	}
	uid := bringUpNamed(t, f, replacement)

	// And it stops there: one replacement, not one per pass, and the failed
	// server is left alone for its retention.
	for i := 0; i < 30; i++ {
		_ = f.agents.ReportPlayers(uid, 0, 100)
		f.reconcile(replacement)
		f.reconcileGroup(t, r)

		servers := f.listServers(t)
		if len(servers) != 2 {
			t.Fatalf("pass %d: got %d servers, want the failed one plus exactly one replacement", i, len(servers))
		}
		for _, s := range servers {
			if s.Name == failed && !s.DeletionTimestamp.IsZero() {
				t.Fatalf("pass %d deleted the failed server before its retention elapsed", i)
			}
		}
		f.clock.Advance(resyncInterval)
	}

	if got := f.server(failed).Status.Phase; got != string(phase.Failed) {
		t.Errorf("phase of the failed server = %q, want it kept in Failed for diagnosis", got)
	}
	if got := f.server(replacement).Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase of the replacement = %q, want Ready", got)
	}
}

func TestGroupAggregatesStatus(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)

	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	if err := f.agents.ReportPlayers(uid, 12, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	if group.Status.Replicas != 1 || group.Status.ReadyReplicas != 1 {
		t.Errorf("replicas = %d/%d, want 1/1", group.Status.ReadyReplicas, group.Status.Replicas)
	}
	if group.Status.OnlinePlayers != 12 {
		t.Errorf("onlinePlayers = %d, want 12", group.Status.OnlinePlayers)
	}
	if group.Status.FreeSlots != 88 {
		t.Errorf("freeSlots = %d, want 88", group.Status.FreeSlots)
	}
	if group.Status.Phase != string(phase.Ready) {
		t.Errorf("phase = %q, want Ready", group.Status.Phase)
	}
}

func TestGroupMaintainsAPodDisruptionBudget(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)

	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	if err := f.agents.ReportPlayers(uid, 4, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, r)

	pdb := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, pdb); err != nil {
		t.Fatalf("get PDB: %v", err)
	}
	if pdb.Spec.MaxUnavailable != nil {
		t.Error("maxUnavailable is not allowed for pods without a scale subresource")
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.Type != intstrInt {
		t.Fatalf("minAvailable = %+v, want an absolute integer", pdb.Spec.MinAvailable)
	}
	if pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("minAvailable = %d, want 1 occupied pod", pdb.Spec.MinAvailable.IntValue())
	}
	if pdb.Spec.Selector.MatchLabels[podspec.LabelOccupied] != "true" {
		t.Errorf("selector = %v, want it to match the occupied label", pdb.Spec.Selector.MatchLabels)
	}
	if pdb.Spec.Selector.MatchLabels[podspec.LabelGroup] != "lobby" {
		t.Errorf("selector = %v, want it scoped to the group", pdb.Spec.Selector.MatchLabels)
	}
}

// TestPodDisruptionBudgetTracksThePlayerCount pins that the budget follows
// reality in both directions. It has to rise before a pod can be evicted and
// fall again once the last player has left, otherwise a group would either
// leak protection forever or, worse, protect nobody.
func TestPodDisruptionBudgetTracksThePlayerCount(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)

	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	f.reconcileGroup(t, r)
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 0 {
		t.Errorf("minAvailable = %d on an empty group, want 0", got)
	}

	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, r)
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 1 {
		t.Errorf("minAvailable = %d with a player online, want 1", got)
	}

	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, r)
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 0 {
		t.Errorf("minAvailable = %d after the last player left, want 0", got)
	}

	// A count we can no longer trust protects the pod again — the Server
	// controller labels it occupied, and the budget has to match that label or
	// the eviction API gets a disruption to spend on it.
	f.clock.Advance(time.Minute)
	f.reconcile(srv.Name)
	f.reconcileGroup(t, r)
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 1 {
		t.Errorf("minAvailable = %d with a stale count, want 1", got)
	}
}

func TestGroupWithoutItsNetworkIsNotAccepted(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	orphan := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "nowhere", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "missing"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
		},
	}
	if err := f.c.Create(f.ctx, orphan); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "nowhere", Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "nowhere", Namespace: f.ns}, got); err != nil {
		t.Fatalf("get group: %v", err)
	}
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted, metav1.ConditionFalse, spawneryv1alpha1.ReasonNetworkNotFound) {
		t.Errorf("conditions = %+v, want Accepted=False/NetworkNotFound", got.Status.Conditions)
	}
	if len(f.listServers(t)) != 0 {
		t.Error("a group without a network must not create servers")
	}
}

// TestGroupWithoutItsNetworkStillProtectsItsPlayers is the guard-scope rule: a
// missing Network blocks only what depends on it, which is creating servers
// that could never get a pod. The PodDisruptionBudget and the published status
// do not depend on the Network at all, and freezing them would leave the pods
// of a group whose Network was deleted open to the eviction API exactly when
// nobody is watching.
func TestGroupWithoutItsNetworkStillProtectsItsPlayers(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)

	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	f.reconcileGroup(t, r)

	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	// The players arrive only after the network is gone, so the budget has to
	// be written after the guard, not before it.
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted, metav1.ConditionFalse, spawneryv1alpha1.ReasonNetworkNotFound) {
		t.Errorf("conditions = %+v, want Accepted=False/NetworkNotFound", group.Status.Conditions)
	}
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 1 {
		t.Errorf("minAvailable = %d, want 1 — the pod still carries players", got)
	}
	if group.Status.OnlinePlayers != 6 {
		t.Errorf("onlinePlayers = %d, want 6 — the status must keep reporting", group.Status.OnlinePlayers)
	}
}

// TestRetainedFailedPodDoesNotWedgeTheBudget covers the pod of a server that
// failed with its pod already dead. The state machine calls such a pod terminal
// and refuses to drain it, precisely because the process is down and its
// sessions went with it — so there is nobody left to protect. The pod is still
// kept for the retention window, and across that window its player count goes
// stale. A rule that reads "stale means occupied" whatever the phase then
// labels a dead pod as occupied and counts it into minAvailable, and the
// eviction API answers "cannot evict pod as it would violate the pod's
// disruption budget" — with currentHealthy below desiredHealthy, the default
// IfHealthyBudget policy will not release it either. An operator's kubectl
// drain never finishes on that node and a cluster upgrade wedges.
//
// The staleness only shows up after two report intervals, so this has to run
// as a loop at the resync cadence; a single reconcile sees a count that is
// still fresh and proves nothing.
func TestRetainedFailedPodDoesNotWedgeTheBudget(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// No floor: this test is about the retained failure alone, not about the
	// replacement the group would otherwise create for it.
	f.setMinReplicas(t, 0)

	bringUpReady(t, f, "lobby-x7k2")

	pod, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("no pod for lobby-x7k2")
	}
	pod.Status.Phase = corev1.PodFailed
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q, want Failed", got)
	}

	for i := 0; i < 60; i++ {
		f.reconcile("lobby-x7k2")
		f.reconcileGroup(t, r)

		p, ok := f.pod("lobby-x7k2")
		if !ok {
			t.Fatalf("pass %d: the retained pod disappeared before its retention elapsed", i)
		}
		if v, set := p.Labels[podspec.LabelOccupied]; set {
			t.Fatalf("pass %d: the pod of a terminally failed server carries %s=%q, so the eviction API will refuse to release it",
				i, podspec.LabelOccupied, v)
		}
		if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 0 {
			t.Fatalf("pass %d: minAvailable = %d for a group with no live server, want 0", i, got)
		}
		f.clock.Advance(resyncInterval)
	}
}

// TestPodThatCrashedWithPlayersOnItDoesNotWedgeTheBudget is the budget half of
// the same regression as
// TestPodThatCrashedWithPlayersOnItLosesTheOccupiedLabel.
//
// TestRetainedFailedPodDoesNotWedgeTheBudget above builds a server whose last
// reported count was zero, so it only ever exercised the stale branch of the
// occupancy rule — which was the only branch the terminal-pod exemption sat in.
// A server that dies with players on it takes the other branch: the registry is
// never told to forget a pod, so its count stays at seven, seven > 0 wins
// before staleness is even looked at, and minAvailable stayed at 1 against a
// currentHealthy of 0 for the whole retention window.
func TestPodThatCrashedWithPlayersOnItDoesNotWedgeTheBudget(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// No floor: this is about the retained failure alone, not the replacement.
	f.setMinReplicas(t, 0)

	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 7, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	f.reconcileGroup(t, r)
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 1 {
		t.Fatalf("minAvailable = %d for a live server with 7 players, want 1", got)
	}

	f.setPodFailed("lobby-x7k2")
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q, want Failed", got)
	}

	for i := 0; i < 60; i++ {
		if err := f.agents.ReportPlayers(uid, 7, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")
		f.reconcileGroup(t, r)

		p, ok := f.pod("lobby-x7k2")
		if !ok {
			t.Fatalf("pass %d: the retained pod disappeared before its retention elapsed", i)
		}
		if v, set := p.Labels[podspec.LabelOccupied]; set {
			t.Fatalf("pass %d: the pod of a server that crashed with 7 players carries %s=%q",
				i, podspec.LabelOccupied, v)
		}
		if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 0 {
			t.Fatalf("pass %d: minAvailable = %d with no live server left, want 0", i, got)
		}
		f.clock.Advance(resyncInterval)
	}
}

// TestTheBudgetRefusesToEvictAPlayedOnPodAndReleasesADeadOne drives the promise
// through the API server itself rather than through our own arithmetic: it
// creates an Eviction, the same call kubectl drain makes.
//
// The dead pod here is crash-looping rather than PodFailed on purpose. The
// eviction handler skips every PodDisruptionBudget for a pod in phase Failed or
// Succeeded, so an eviction test built on those would succeed whatever we
// labelled the pod — passing for the wrong reason, which is the trap this whole
// review round is about. A crash-looping pod is still in phase Running, so its
// eviction really does depend on whether we released it from the budget.
func TestTheBudgetRefusesToEvictAPlayedOnPodAndReleasesADeadOne(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 0)

	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 7, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	f.reconcileGroup(t, r)
	f.publishPDBStatus(t)

	err := f.evict(t, "lobby-x7k2")
	if err == nil {
		t.Fatal("the API server evicted a pod with 7 players on it — the core promise is broken")
	}
	if !apierrors.IsTooManyRequests(err) {
		t.Fatalf("eviction of an occupied pod failed with %v, want a disruption-budget refusal", err)
	}

	// The Minecraft container now dies over and over. The seven sessions went
	// down with the first crash; the registry just has not been told.
	f.setPodCrashLooping("lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 7, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q for a crash-looping server, want Failed", got)
	}
	f.reconcileGroup(t, r)
	f.publishPDBStatus(t)

	if err := f.evict(t, "lobby-x7k2"); err != nil {
		t.Fatalf("the eviction of a crash-looping pod was refused: %v — "+
			"kubectl drain on that node would never finish and a cluster upgrade wedges", err)
	}
}

// TestServerThatKeptItsPlayersAfterAReadinessLossIsNotNominated covers the
// server the phase cannot describe. A server that loses its probe falls back to
// Starting, and deregistering it only stops new joins — nobody is moved off, so
// its players are still connected. If its player count then becomes
// unreadable, a rule that asks "is the phase Ready?" as its proxy for "was this
// registered?" reads Starting, decides the server is empty and nominates it,
// while its genuinely empty peer survives. Task 8 drains it so nobody is
// kicked, but the players get a visible move that the empty server should have
// absorbed. status.wasRegistered exists to answer that question properly.
//
// Loop-driven: the nomination is made afresh on every pass, so the invariant
// has to hold on every pass and the group still has to converge.
func TestServerThatKeptItsPlayersAfterAReadinessLossIsNotNominated(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.setMinReplicas(t, 2)
	f.reconcileGroup(t, r)

	uids := map[string]string{}
	for _, s := range f.listServers(t) {
		uids[s.Name] = bringUpNamed(t, f, s.Name)
	}
	if len(uids) != 2 {
		t.Fatalf("got %d servers, want 2", len(uids))
	}
	var victim, peer string
	for name := range uids {
		if victim == "" || name < victim {
			victim = name
		}
	}
	for name := range uids {
		if name != victim {
			peer = name
		}
	}

	// Seven players are on the victim when its probe goes red.
	if err := f.agents.ReportPlayers(uids[victim], 7, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(victim)
	f.setPodRunning(victim, false)
	f.reconcile(victim)
	if got := f.server(victim).Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase of %s = %q, want Starting after the readiness loss", victim, got)
	}
	if !f.server(victim).Status.WasRegistered {
		t.Fatal("wasRegistered must survive the readiness loss, or the fixture proves nothing")
	}

	// The operator restarts, or the pod UID can no longer be resolved: the
	// registry no longer knows this pod, so its count reads zero and stale.
	f.agents.Forget(uids[victim])

	f.setMinReplicas(t, 1)

	for i := 0; i < 30; i++ {
		_ = f.agents.ReportPlayers(uids[peer], 0, 100)
		for _, s := range f.listServers(t) {
			f.reconcile(s.Name)
		}
		f.reconcileGroup(t, r)

		found := false
		for _, s := range f.listServers(t) {
			if s.Name != victim {
				continue
			}
			found = true
			if !s.DeletionTimestamp.IsZero() {
				t.Fatalf("pass %d nominated %q, which lost its probe but kept its seven players, while the empty %q was available",
					i, victim, peer)
			}
		}
		if !found {
			t.Fatalf("pass %d removed %q outright", i, victim)
		}
		f.clock.Advance(resyncInterval)
	}

	final := f.listServers(t)
	if len(final) != 1 || final[0].Name != victim {
		names := make([]string, 0, len(final))
		for _, s := range final {
			names = append(names, s.Name)
		}
		t.Fatalf("group settled on %v, want only %q — the empty peer was the one to remove", names, victim)
	}
}

// TestGroupKeepsOnlyOneRetainedFailure bounds what a broken image costs. A
// Failed server holds its pod and its full resource request for the whole
// retention window, and it does not take that window to fail — the restart cap
// plus kubelet backoff gets there in a minute or two, and the group replaces it
// on the next five-second pass. Uncapped, one floor replica piles up dozens of
// retained servers before the first one expires. One is enough to diagnose
// from.
func TestGroupKeepsOnlyOneRetainedFailure(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// No floor, so the replacements the group would otherwise create do not
	// blur what is being counted.
	f.setMinReplicas(t, 0)

	names := []string{"lobby-aaaa", "lobby-bbbb", "lobby-cccc"}
	for _, name := range names {
		bringUpReady(t, f, name)
		driveToFailed(t, f, name)
	}
	for _, name := range names {
		if got := f.server(name).Status.Phase; got != string(phase.Failed) {
			t.Fatalf("phase of %s = %q, want Failed", name, got)
		}
	}

	// Let the pruning run its course: a failed server that still has players is
	// drained before it goes, so it takes a drain timeout to disappear.
	for i := 0; i < 40; i++ {
		for _, s := range f.listServers(t) {
			f.reconcile(s.Name)
		}
		f.reconcileGroup(t, r)
		f.clock.Advance(resyncInterval)
	}

	final := f.listServers(t)
	if len(final) != 1 {
		remaining := make([]string, 0, len(final))
		for _, s := range final {
			remaining = append(remaining, s.Name)
		}
		t.Fatalf("group retained %v, want exactly one failure kept for diagnosis", remaining)
	}
	if !final[0].DeletionTimestamp.IsZero() {
		t.Errorf("the retained failure %q is being removed; one must be kept", final[0].Name)
	}
	if got := final[0].Status.Phase; got != string(phase.Failed) {
		t.Errorf("phase of the retained server = %q, want Failed", got)
	}
}

// TestGroupPointingAtARejectedNetworkCreatesNoServers closes the gap Task 10
// left open: a Network that loses the one-per-namespace contest only carries
// an Accepted=False/DuplicateNetwork condition, and until a group actually
// consults it, that condition is decoration — a ServerGroup pointing at the
// loser would run at full strength in the same namespace as the winner's
// groups, exactly the isolation failure the rule exists to prevent.
func TestGroupPointingAtARejectedNetworkCreatesNoServers(t *testing.T) {
	f := newFixture(t)
	nr := networkReconciler(f)

	// "staging" is created after the fixture's "production" and loses the
	// contest — by creation order if the two land in different seconds, or by
	// the name tie-break ("production" < "staging") if envtest's
	// second-granularity timestamps put them in the same one, exactly as
	// TestSecondNetworkInTheSameNamespaceIsRejected already relies on.
	staging := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "other-secret"},
		},
	}
	if err := f.c.Create(f.ctx, staging); err != nil {
		t.Fatalf("create staging network: %v", err)
	}
	f.reconcileNetwork(t, nr, "production")
	f.reconcileNetwork(t, nr, "staging")
	if !hasCondition(f.getNetwork(t, "staging").Status.Conditions,
		spawneryv1alpha1.ConditionAccepted, metav1.ConditionFalse, spawneryv1alpha1.ReasonDuplicateNetwork) {
		t.Fatalf("staging network = %+v, want it rejected — the rest of this test proves nothing otherwise",
			f.getNetwork(t, "staging").Status.Conditions)
	}

	arena := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "arena", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "staging"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
		},
	}
	if err := f.c.Create(f.ctx, arena); err != nil {
		t.Fatalf("create arena group: %v", err)
	}

	r := groupReconciler(f)
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "arena", Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile arena: %v", err)
	}

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "arena", Namespace: f.ns}, got); err != nil {
		t.Fatalf("get arena: %v", err)
	}
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonNetworkNotAccepted) {
		t.Errorf("conditions = %+v, want Accepted=False/NetworkNotAccepted", got.Status.Conditions)
	}
	for _, s := range f.listServers(t) {
		if s.Spec.GroupRef.Name == "arena" {
			t.Errorf("arena created a server (%s) although its network lost the one-per-namespace contest", s.Name)
		}
	}
}

// TestGroupWithARejectedNetworkStillProtectsItsPlayers is the guard-scope
// rule (Task 8 lesson 3) applied to the new rejection state: a group whose
// Network loses the one-per-namespace contest after it already has servers
// running must not delete anything and must not drop the PodDisruptionBudget
// that protects its occupied pods — a rejected group holding players is still
// holding players. Only creating new servers genuinely depends on the Network
// being usable; this mirrors TestGroupWithoutItsNetworkStillProtectsItsPlayers
// for rejection instead of deletion.
//
// The players arrive only after the rejection, exactly like that sibling
// test's own comment explains: if the PDB were computed before the guard (or
// skipped by it), it would already show minAvailable = 1 from a stale prior
// pass, and a test that only checks the value afterwards could not tell a
// live PodDisruptionBudget from a frozen one that happens to read the right
// number by coincidence. Starting from 0 and asserting the rise to 1 proves
// reconcilePDB actually ran on this pass, with this pass's views.
func TestGroupWithARejectedNetworkStillProtectsItsPlayers(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)
	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	f.reconcileGroup(t, r)
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 0 {
		t.Fatalf("minAvailable = %d on an empty server before the rejection, want 0", got)
	}

	rejectNetwork(t, f, "production")

	// The player joins only now, with the network already rejected.
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonNetworkNotAccepted) {
		t.Errorf("conditions = %+v, want Accepted=False/NetworkNotAccepted", group.Status.Conditions)
	}
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 1 {
		t.Errorf("minAvailable = %d after the rejection, want 1 — the pod now carries a player", got)
	}
	if group.Status.OnlinePlayers != 6 {
		t.Errorf("onlinePlayers = %d, want 6 — the status must keep reporting", group.Status.OnlinePlayers)
	}
	for _, s := range f.listServers(t) {
		if !s.DeletionTimestamp.IsZero() {
			t.Errorf("server %s was marked for deletion after its network was merely rejected, not removed", s.Name)
		}
	}
}

// TestGroupResumesOnceItsNetworkIsAccepted is the recovery half of the story:
// once whatever made the Network lose the contest goes away, a frozen group
// has to resume on its own, without an operator touching the group. Driven as
// a loop at the real network-retry cadence rather than a single jump — Task 8
// lesson 1 is explicit that time- and repetition-driven behaviour needs a
// loop, because a fix that only works when tried exactly once would sail
// through a single-reconcile test unnoticed.
//
// This uses a dedicated network+group pair ("staging-net"/"arena"), not the
// fixture's own "production"/"lobby". "staging-net" is created after
// "production" and so deterministically loses the one-per-namespace contest —
// chronologically if the two real timestamps differ, or by the name
// tie-break if envtest's second-granularity clock ties them, exactly the
// guarantee TestGroupPointingAtARejectedNetworkCreatesNoServers relies on.
// The fixture's own "production" cannot be put in the losing seat this way:
// it is created first, inside newFixture, before this test's code runs at
// all, so nothing this test creates can ever carry an earlier real
// timestamp. An earlier version of this test tried anyway, by racing a
// competitor created after several seconds of setup work (bringing a server
// up) against "production" — that gave "production" enough real elapsed time
// to win outright regardless of name, and the test failed intermittently on
// its own setup assertion, before ever reaching the behaviour under test.
func TestGroupResumesOnceItsNetworkIsAccepted(t *testing.T) {
	f := newFixture(t)
	nr := networkReconciler(f)

	arenaNet := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging-net", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "staging-net-secret"},
		},
	}
	if err := f.c.Create(f.ctx, arenaNet); err != nil {
		t.Fatalf("create staging-net: %v", err)
	}
	f.reconcileNetwork(t, nr, "production")
	f.reconcileNetwork(t, nr, "staging-net")
	if !hasCondition(f.getNetwork(t, "staging-net").Status.Conditions,
		spawneryv1alpha1.ConditionAccepted, metav1.ConditionFalse, spawneryv1alpha1.ReasonDuplicateNetwork) {
		t.Fatalf("staging-net = %+v, want it rejected by production — the rest of this test proves nothing otherwise",
			f.getNetwork(t, "staging-net").Status.Conditions)
	}

	arena := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "arena", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "staging-net"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
		},
	}
	if err := f.c.Create(f.ctx, arena); err != nil {
		t.Fatalf("create arena group: %v", err)
	}

	r := groupReconciler(f)
	reconcileArena := func() {
		t.Helper()
		if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
			NamespacedName: types.NamespacedName{Name: "arena", Namespace: f.ns},
		}); err != nil {
			t.Fatalf("reconcile arena: %v", err)
		}
	}
	arenaServers := func() []spawneryv1alpha1.Server {
		var out []spawneryv1alpha1.Server
		for _, s := range f.listServers(t) {
			if s.Spec.GroupRef.Name == "arena" {
				out = append(out, s)
			}
		}
		return out
	}

	reconcileArena()
	if got := len(arenaServers()); got != 0 {
		t.Fatalf("got %d servers while the network was rejected, want 0", got)
	}

	// The winner goes away — a namespace migration finishing, or an operator
	// cleaning up a mistake.
	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete production network: %v", err)
	}

	// Six passes at the network-retry cadence is 180s of simulated time — a
	// real loop, well short of the 5-minute startup deadline. This test never
	// drives the created server to Ready (bringUpNamed is a separate concern,
	// already covered elsewhere), so a longer loop would eventually fail it
	// for outliving its startup deadline and create a legitimate replacement,
	// which would be a false failure of this test, not a bug.
	for i := 0; i < 6; i++ {
		f.reconcileNetwork(t, nr, "staging-net")
		reconcileArena()
		for _, s := range arenaServers() {
			f.reconcile(s.Name)
		}

		if got := len(arenaServers()); got > 1 {
			t.Fatalf("pass %d: got %d servers, want at most the floor of 1 — the group over-created after recovering", i, got)
		}
		f.clock.Advance(networkRetryInterval)
	}

	servers := arenaServers()
	if len(servers) != 1 {
		t.Fatalf("group settled on %d servers after 6 passes past the recovery, want its floor of 1", len(servers))
	}
	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "arena", Namespace: f.ns}, got); err != nil {
		t.Fatalf("get arena: %v", err)
	}
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Errorf("conditions = %+v, want Accepted=True again after recovery", got.Status.Conditions)
	}
}

// TestServerGroupRendersConfigMap covers design section 5.4's promise: one
// ConfigMap per group, owned by it, carrying the label the manager's
// restricted cache requires, and holding exactly what spec.maxPlayers says —
// not merely a ConfigMap that exists under the right name, which a renderer
// that wrote an empty document or the wrong key would also produce.
func TestServerGroupRendersConfigMap(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)

	cm := f.groupConfigMap(t, "lobby")
	if cm.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Errorf("labels = %+v, want %s=%s so the restricted cache can see this ConfigMap",
			cm.Labels, podspec.LabelManagedBy, podspec.ManagedByValue)
	}
	if len(cm.OwnerReferences) != 1 ||
		cm.OwnerReferences[0].Kind != "ServerGroup" ||
		cm.OwnerReferences[0].Controller == nil || !*cm.OwnerReferences[0].Controller {
		t.Errorf("owner references = %+v, want a ServerGroup controller ref", cm.OwnerReferences)
	}

	raw, ok := cm.Data[podspec.ConfigValuesKey]
	if !ok {
		t.Fatalf("data = %+v, want a %s key", cm.Data, podspec.ConfigValuesKey)
	}
	var values render.Values
	if err := yaml.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatalf("%s does not parse as render.Values: %v", podspec.ConfigValuesKey, err)
	}
	if values.MaxPlayers == nil || *values.MaxPlayers != f.group.Spec.MaxPlayers {
		t.Errorf("maxPlayers = %v, want %d", values.MaxPlayers, f.group.Spec.MaxPlayers)
	}
	// The critical fields never travel through this document — there is
	// nothing in ServerGroupSpec that could even populate them, but a future
	// change that reached for one directly on Values would slip past a test
	// that only checked maxPlayers.
	if values.PlayerLimit != nil || values.Motd != nil {
		t.Errorf("values = %+v, want only maxPlayers set — a ServerGroup has no playerLimit or motd", values)
	}
}

// TestServerGroupConfigMapUpdatesOnSpecChange guards against a renderer that
// only runs once: a ConfigMap that gets created correctly but never revisited
// would be indistinguishable from a working one until the day an operator
// actually edits maxPlayers.
func TestServerGroupConfigMapUpdatesOnSpecChange(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)
	before := f.groupConfigMap(t, "lobby")
	var beforeValues render.Values
	if err := yaml.Unmarshal([]byte(before.Data[podspec.ConfigValuesKey]), &beforeValues); err != nil {
		t.Fatalf("unmarshal before update: %v", err)
	}
	if beforeValues.MaxPlayers == nil || *beforeValues.MaxPlayers != 100 {
		t.Fatalf("maxPlayers before the edit = %v, want the fixture's 100", beforeValues.MaxPlayers)
	}

	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.MaxPlayers = 55
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}

	f.reconcileGroup(t, r)

	after := f.groupConfigMap(t, "lobby")
	var afterValues render.Values
	if err := yaml.Unmarshal([]byte(after.Data[podspec.ConfigValuesKey]), &afterValues); err != nil {
		t.Fatalf("unmarshal after update: %v", err)
	}
	if afterValues.MaxPlayers == nil || *afterValues.MaxPlayers != 55 {
		t.Errorf("maxPlayers after the edit = %v, want 55", afterValues.MaxPlayers)
	}
}

// TestServerGroupConfigMapWrittenBeforeTheServer proves the ordering the
// design depends on: a pod's projected volume names this ConfigMap by group,
// so the ConfigMap must exist before the Server that will eventually get a
// pod. Reading back the final state after a reconcile cannot tell "written
// first" apart from "written at some point" — both leave the same two objects
// sitting there. Recording the actual Create calls can.
func TestServerGroupConfigMapWrittenBeforeTheServer(t *testing.T) {
	f := newFixture(t)
	recorder := &createOrderRecorder{Client: f.c}
	r := &ServerGroupReconciler{
		Client:   recorder,
		Scheme:   f.reconc.Scheme,
		Recorder: record.NewFakeRecorder(100),
		Agents:   f.agents,
		Clock:    f.clock.Now,
	}

	f.reconcileGroup(t, r)

	cmIdx := recorder.indexOf(fmt.Sprintf("%T/%s", &corev1.ConfigMap{}, podspec.GroupConfigMapName(f.group.Name, podspec.RoleServer)))
	srvIdx := recorder.indexOf(fmt.Sprintf("%T/%s-", &spawneryv1alpha1.Server{}, f.group.Name))
	if cmIdx == -1 {
		t.Fatalf("no ConfigMap create was recorded")
	}
	if srvIdx == -1 {
		t.Fatalf("no Server create was recorded")
	}
	if cmIdx >= srvIdx {
		t.Errorf("ConfigMap created at position %d, Server at %d — want the ConfigMap first: "+
			"ServerReconciler can only build a pod for a Server that already exists, so a ConfigMap "+
			"written before the Server is written before any pod that could reference it", cmIdx, srvIdx)
	}
}
