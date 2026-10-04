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
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/render"
)

func (f *fixture) groupConfigMap(t *testing.T, group string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: podspec.GroupConfigMapName(group, podspec.RoleServer), Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, cm); err != nil {
		t.Fatalf("get ConfigMap for group %s: %v", group, err)
	}
	return cm
}

func groupReconciler(f *fixture) *ServerGroupReconciler {
	return &ServerGroupReconciler{
		Client:       f.rc,
		Scheme:       f.reconc.Scheme,
		Recorder:     newRecorder(),
		Agents:       f.agents,
		Clock:        f.clock.Now,
		Expectations: newExpectations(f.clock.Now),
		// Production must pass an uncached reader; Reconcile panics without one.
		ClaimReader: f.c,
	}
}

func scalingEvents(rec *nonBlockingRecorder, reason string) int {
	n := 0
	for _, e := range drainEvents(rec) {
		if eventHasReason(e, reason) {
			n++
		}
	}
	return n
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

func (f *fixture) groupPDB(t *testing.T) *policyv1.PodDisruptionBudget {
	t.Helper()
	pdb := &policyv1.PodDisruptionBudget{}
	key := types.NamespacedName{Name: podspec.GroupPDBName(f.group.Name, podspec.RoleServer), Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, pdb); err != nil {
		t.Fatalf("get PDB: %v", err)
	}
	return pdb
}

func (f *fixture) assertBudgetSelectsExactlyWhatItCounts(t *testing.T, name string) {
	t.Helper()
	pdb := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, pdb); err != nil {
		t.Fatalf("get PDB %s: %v", name, err)
	}
	if pdb.Spec.MinAvailable == nil {
		t.Fatalf("PDB %s has no minAvailable; there is nothing to compare a selector against", name)
	}
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		t.Fatalf("PDB %s has an unusable selector %+v: %v", name, pdb.Spec.Selector, err)
	}

	pods := &corev1.PodList{}
	if err := f.c.List(f.ctx, pods, ctrlclientInNamespace(f.ns),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		t.Fatalf("list the pods PDB %s selects: %v", name, err)
	}
	matched := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		matched = append(matched, pods.Items[i].Name)
	}
	sort.Strings(matched)

	if want := pdb.Spec.MinAvailable.IntValue(); len(matched) != want {
		t.Errorf("PDB %s selects %d pod(s) %v but its minAvailable is %d.\n"+
			"selector: %v\n"+
			"More selected than counted hands the eviction API a disruption to spend on an occupied pod; "+
			"fewer pins minAvailable above a number the group can reach and wedges kubectl drain on nobody.",
			name, len(matched), matched, want, selector)
	}
}

// envtest runs no disruption controller, and the eviction handler refuses a
// budget whose status lags its generation, so this writes the status it would.
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

// A rule that only breaks on repetition is invisible to a single reconcile.
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

	// 60 passes end five seconds short of the 300 s stabilization default; the
	// rest cover the drain.
	for i := 0; i < 65; i++ {
		// Live agents keep reporting, so no count goes stale by accident: the
		// test must exercise the occupied rule, not the staleness rule.
		for name, uid := range uids {
			players := int32(0)
			if name == busy {
				players = 7
			}
			// A server already gone has no stream left.
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
		f.clock.Advance(ResyncInterval)
	}

	final := f.listServers(t)
	var live, failed []spawneryv1alpha1.Server
	for _, s := range final {
		if s.Status.Phase == string(phase.Failed) {
			failed = append(failed, s)
			continue
		}
		live = append(live, s)
	}

	if len(live) != 1 || live[0].Name != busy {
		names := make([]string, 0, len(live))
		for _, s := range live {
			names = append(names, s.Name)
		}
		t.Fatalf("live servers settled on %v, want only the occupied server %q: "+
			"the idle one is shed and a capacity edit replaces nothing", names, busy)
	}
	if got := live[0].Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase of the surviving server = %q, want Ready", got)
	}
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 1 {
		t.Errorf("minAvailable = %d, want 1 — the surviving pod still carries players", got)
	}

	if len(failed) != 0 {
		names := make([]string, 0, len(failed))
		for _, s := range failed {
			names = append(names, s.Name)
		}
		t.Fatalf("failed servers = %v, want none — no changeover was begun", names)
	}
}

// A sizing bug that creates one server per reconcile is invisible to a single reconcile.
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
		f.clock.Advance(ResyncInterval)
	}
}

// Counting a Failed server, kept an hour for diagnosis, toward the floor would
// leave the group unplayable for that hour.
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

	f.clock.Advance(backoffBase + time.Second)
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
		f.clock.Advance(ResyncInterval)
	}

	if got := f.server(failed).Status.Phase; got != string(phase.Failed) {
		t.Errorf("phase of the failed server = %q, want it kept in Failed for diagnosis", got)
	}
	if got := f.server(replacement).Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase of the replacement = %q, want Ready", got)
	}
}

func TestAFinishedRoundIsReplacedWithoutCountingAFailure(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want minReplicas = 1", len(servers))
	}
	finished := servers[0].Name

	uid := bringUpNamed(t, f, finished)
	if _, err := f.agents.ReportAcceptJoins(uid, f.ns, false, true); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}
	f.reconcile(finished)
	if f.server(finished).Status.RoundEndedAt == nil {
		t.Fatalf("roundEndedAt was not stamped while the server still ran")
	}
	if got := f.server(finished).Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase right after the round-end word = %q, want it still Ready -- the pod has not stopped yet", got)
	}

	f.setPodFailed(finished)
	f.reconcile(finished)
	if got := f.server(finished).Status.Phase; got != string(phase.Finished) {
		t.Fatalf("phase after the pod stopped = %q, want Finished", got)
	}

	// No clock advance: a finished round must not wait out the backoff window.
	f.reconcileGroup(t, r)

	var replacement string
	for _, s := range f.listServers(t) {
		if s.Name != finished {
			replacement = s.Name
		}
	}
	if replacement == "" {
		t.Fatalf("the group did not replace the finished server; servers = %d", len(f.listServers(t)))
	}

	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got != 0 {
		t.Errorf("consecutiveFailures = %d, want 0 -- a finished round is not a fault", got)
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
	key := types.NamespacedName{Name: podspec.GroupPDBName("lobby", podspec.RoleServer), Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, pdb); err != nil {
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
	if pdb.Spec.Selector.MatchLabels[podspec.LabelRole] != podspec.RoleServer {
		t.Errorf("selector = %v, want it scoped to server pods: without the role term it also "+
			"matches the occupied proxies of a same-named ProxyGroup, which its minAvailable "+
			"never counted", pdb.Spec.Selector.MatchLabels)
	}
	f.assertBudgetSelectsExactlyWhatItCounts(t, key.Name)
}

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

	// A minute without a report makes the count stale, which counts as occupied.
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

	// ScalingLimited is guarded on IsEphemeral alone, so it is published without a Network too.
	cond := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionScalingLimited)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("ScalingLimited = %+v on a group without its Network, want False", cond)
	}
	if cond != nil && cond.Message != "scaling is not being decided: the group's network is not usable" {
		t.Errorf("message = %q, want the not-decided message, not the all-clear", cond.Message)
	}
}

// A missing Network blocks only server creation; the budget and status must keep
// tracking players.
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
	// The players arrive after the network is gone, so the budget must be written
	// after the guard.
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

// A retained dead pod whose count went stale must not be labelled occupied, or
// kubectl drain wedges on it. Staleness takes two report intervals, hence the loop.
func TestRetainedFailedPodDoesNotWedgeTheBudget(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// No floor, so no replacement blurs the picture.
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
		f.clock.Advance(ResyncInterval)
	}
}

// A server that crashed with players keeps its last count of seven, so the count
// branch of the occupancy rule, not the stale one, has to let it go.
func TestPodThatCrashedWithPlayersOnItDoesNotWedgeTheBudget(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// No floor, so no replacement blurs the picture.
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
		f.clock.Advance(ResyncInterval)
	}
}

// The pod crash-loops rather than failing: the eviction handler skips every
// budget for a pod in phase Failed or Succeeded.
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

	// The sessions died with the first crash; the registry has not been told.
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

// A server that lost its probe falls back to Starting but keeps its players;
// status.wasRegistered must keep it from being nominated over an empty peer.
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

	// As after an operator restart: the registry forgets the pod, so its count
	// reads zero and stale.
	f.agents.Forget(uids[victim])

	// Lowering the ceiling, not the floor, takes the surplus branch, which skips the
	// demand rule's Stale/Stabilization filters and leaves mayHavePlayers to decide.
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.MinReplicas = 1
	f.group.Spec.Scaling.MaxReplicas = 1
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}

	// The surplus branch has no stabilization wait; ten passes cover the drain.
	for i := 0; i < 10; i++ {
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
		f.clock.Advance(ResyncInterval)
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

// A Failed server holds its full resource request for the retention window,
// and a broken image fails every minute or two.
func TestGroupKeepsOnlyOneRetainedFailure(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// No floor, so no replacement blurs what is counted.
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

	// A failed server that still has players drains for a drain timeout first.
	for i := 0; i < 40; i++ {
		for _, s := range f.listServers(t) {
			f.reconcile(s.Name)
		}
		f.reconcileGroup(t, r)
		f.clock.Advance(ResyncInterval)
	}

	// spareSlots still demands capacity the Failed server cannot give, so one
	// replacement stands beside the retained failure.
	final := f.listServers(t)
	if len(final) != 2 {
		remaining := make([]string, 0, len(final))
		for _, s := range final {
			remaining = append(remaining, fmt.Sprintf("%s(%s)", s.Name, s.Status.Phase))
		}
		t.Fatalf("group retained %v, want exactly one failure kept for diagnosis "+
			"plus the spare-slot replacement", remaining)
	}

	var failed []*spawneryv1alpha1.Server
	for i := range final {
		if final[i].Status.Phase == string(phase.Failed) {
			failed = append(failed, &final[i])
		}
	}
	if len(failed) != 1 {
		t.Fatalf("%d servers in %v are Failed, want exactly one kept for diagnosis", len(failed), final)
	}
	retained := failed[0]
	if !retained.DeletionTimestamp.IsZero() {
		t.Errorf("the retained failure %q is being removed; one must be kept", retained.Name)
	}
}

func TestGroupPointingAtARejectedNetworkCreatesNoServers(t *testing.T) {
	f := newFixture(t)
	nr := networkReconciler(f)

	// "staging" loses by creation order, or by the name tie-break when envtest's
	// second-granularity timestamps coincide.
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

// The players arrive after the rejection, so the budget's rise from 0 to 1 proves
// it was computed on this pass.
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

// Looped at the network-retry cadence: a fix that works only once passes a single
// reconcile. "production" is created inside newFixture, before anything here can
// be older, so a dedicated "staging-net" takes the losing seat.
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

	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete production network: %v", err)
	}

	// Six passes (180 s) stay short of the five-minute startup deadline the
	// never-Ready server would otherwise fail.
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
	if values.PlayerLimit != nil || values.Motd != nil {
		t.Errorf("values = %+v, want only maxPlayers set — a ServerGroup has no playerLimit or motd", values)
	}
}

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

// The final state cannot tell "written first" from "written at some point";
// the recorded Create calls can.
func TestServerGroupConfigMapWrittenBeforeTheServer(t *testing.T) {
	f := newFixture(t)
	recorder := &createOrderRecorder{Client: f.rc}
	r := &ServerGroupReconciler{
		Client:       recorder,
		Scheme:       f.reconc.Scheme,
		Recorder:     newRecorder(),
		Agents:       f.agents,
		Clock:        f.clock.Now,
		Expectations: newExpectations(f.clock.Now),
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

// Replacements are not Ready for tens of seconds, and a scaler reading
// status.freeSlots would order the same one again on every pass.
func TestGroupCreatesTheShortfallOnceWhileTheNewServersStart(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// minReplicas 1, maxReplicas 5, spareSlots 40, maxPlayers 100.
	f.group.Spec.Scaling.MaxReplicas = 5
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want the floor of 1", len(servers))
	}
	uid := bringUpNamed(t, f, servers[0].Name)
	if err := f.agents.ReportPlayers(uid, 70, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	// 30 free slots against 40 spare: exactly one server short.
	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != 2 {
		t.Fatalf("got %d servers after the shortfall, want 2", got)
	}

	// The first server keeps reporting, or its count would go stale and the test
	// would pass for the wrong reason.
	for i := 0; i < 10; i++ {
		f.clock.Advance(ResyncInterval)
		if err := f.agents.ReportPlayers(uid, 70, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcileGroup(t, r)
	}
	if got := len(f.listServers(t)); got != 2 {
		t.Fatalf("got %d servers after ten further passes, want 2 — the scaler "+
			"ordered the same replacement again while it was still starting", got)
	}
}

func TestGroupShrinksOnceTheStabilizationWindowElapses(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.setMinReplicas(t, 3)
	f.reconcileGroup(t, r)
	uids := map[string]string{}
	for _, s := range f.listServers(t) {
		uids[s.Name] = bringUpNamed(t, f, s.Name)
	}
	f.setMinReplicas(t, 1)

	// Three: spec.scaling never reaches podspec.DesiredServerHash, so lowering the
	// floor starts no changeover, and no empty server has waited out the window yet.
	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != 3 {
		t.Fatalf("got %d servers before the window elapsed, want 3 — a capacity "+
			"edit must start no changeover", got)
	}

	// The window is the CRD default, 300 s. The agents keep reporting, since a stale
	// count counts as occupied; a repeated zero must not restart the window.
	f.clock.Advance(301 * time.Second)
	for _, uid := range uids {
		if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
	}
	rec := newRecorder()
	r.Recorder = rec
	f.reconcileGroup(t, r)

	var leaving int
	for _, s := range f.listServers(t) {
		if !s.DeletionTimestamp.IsZero() {
			leaving++
		}
	}
	if leaving != 1 {
		t.Fatalf("%d servers marked for deletion, want exactly one per pass", leaving)
	}
	// DecideSize leaves DeleteReason empty for ephemeral groups; size() falls back
	// to ServerRemoved.
	if got := scalingEvents(rec, "ServerRemoved"); got != 1 {
		t.Errorf("ServerRemoved events = %d, want 1", got)
	}
}

func TestGroupRecordsWhatItIssued(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)

	creates, _, _ := r.Expectations.pending(f.ns + "/lobby")
	if len(creates) != 1 {
		t.Errorf("pending creates = %v right after the create, want 1: size() "+
			"did not record what it issued", creates)
	}
}

func TestGroupSaysWhenItsCeilingHoldsCapacityBack(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec

	f.group.Spec.Scaling.MaxReplicas = 1
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	uid := bringUpNamed(t, f, servers[0].Name)
	if err := f.agents.ReportPlayers(uid, 100, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionScalingLimited)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ScalingLimited = %+v, want True with the group full at its ceiling", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonMaxReplicasReached {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonMaxReplicasReached)
	}
	// Pinned exactly so it stays distinct from the message in
	// TestGroupSaysColdStartIsBlockedByTheCeiling.
	wantMsg := "1 more server(s) needed to cover spareSlots 40; maxReplicas 1 allows 0 now"
	if cond.Message != wantMsg {
		t.Errorf("message = %q, want %q", cond.Message, wantMsg)
	}
	if group.Status.Phase == "" {
		t.Error("phase went empty")
	}
	if meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Error("a group working exactly as configured must not be Degraded")
	}

	if got := scalingEvents(rec, spawneryv1alpha1.ReasonMaxReplicasReached); got != 1 {
		t.Errorf("%d MaxReplicasReached events on the flank, want exactly 1", got)
	}

	// Unchanged: an event on every resync would be one every five seconds.
	f.reconcileGroup(t, r)
	if got := scalingEvents(rec, spawneryv1alpha1.ReasonMaxReplicasReached); got != 0 {
		t.Errorf("%d further MaxReplicasReached events on an unchanged resync, want none", got)
	}

	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.MaxReplicas = 5
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	if meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionScalingLimited) {
		t.Error("ScalingLimited still True after maxReplicas was raised")
	}
	if got := scalingEvents(rec, spawneryv1alpha1.ReasonWithinLimits); got != 1 {
		t.Errorf("%d WithinLimits events when the ceiling was raised, want exactly 1", got)
	}
}

// Wanted and Create are both 0 here, as when nothing is limited, so the message
// must name the stalled changeover instead.
func TestGroupSaysColdStartIsBlockedByTheCeiling(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec

	f.group.Spec.Scaling.MaxReplicas = 1
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	bringUpNamed(t, f, servers[0].Name)
	f.reconcileGroup(t, r)

	// The new image makes the empty server stale: only the cold start wants a
	// server, and the ceiling has no room for it.
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Image = "ghcr.io/spawnery/paper:1.21.4-0.2.0"
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionScalingLimited)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ScalingLimited = %+v, want True with the ceiling refusing the cold start", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonMaxReplicasReached {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonMaxReplicasReached)
	}
	wantMsg := "changeover cannot begin: the group is already at maxReplicas 1; raise it by at least 1 to start the new generation"
	if cond.Message != wantMsg {
		t.Errorf("message = %q, want %q", cond.Message, wantMsg)
	}
	if strings.Contains(cond.Message, "0 more server") {
		t.Errorf("message = %q, must never claim nothing is needed while the changeover is refused", cond.Message)
	}
}

func TestGroupPatchesRetireOntoTheNominatedServer(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want minReplicas = 1", len(servers))
	}
	old := servers[0].Name
	bringUpNamed(t, f, old)

	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Image = "ghcr.io/spawnery/paper:1.21.4-0.2.0"
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	var newSrv string
	for _, s := range f.listServers(t) {
		if s.Name != old {
			newSrv = s.Name
		}
	}
	if newSrv == "" {
		t.Fatalf("the cold start did not create the changeover's replacement")
	}
	bringUpNamed(t, f, newSrv)

	f.reconcileGroup(t, r)

	got := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: old, Namespace: f.ns}, got); err != nil {
		t.Fatalf("get %s: %v", old, err)
	}
	if !got.Spec.Retire {
		t.Error("the stale server was not asked to retire")
	}
}

// selectRetirement never nominates a retiring server, so the guard is called
// directly.
func TestGroupRetireServerGuardsAgainstARepeatCall(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec

	f.reconcileGroup(t, r)
	list := f.listServers(t)
	if len(list) != 1 {
		t.Fatalf("got %d servers, want minReplicas = 1", len(list))
	}
	srv := &list[0]
	servers := map[string]*spawneryv1alpha1.Server{srv.Name: srv}

	if err := r.retireServer(f.ctx, f.group, servers, srv.Name); err != nil {
		t.Fatalf("first retireServer: %v", err)
	}
	first := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: srv.Name, Namespace: f.ns}, first); err != nil {
		t.Fatalf("get %s after first call: %v", srv.Name, err)
	}
	if !first.Spec.Retire {
		t.Fatalf("first retireServer did not patch spec.retire")
	}

	// retireServer already set Retire on the in-memory server the map holds.
	if err := r.retireServer(f.ctx, f.group, servers, srv.Name); err != nil {
		t.Fatalf("second retireServer: %v", err)
	}

	second := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: srv.Name, Namespace: f.ns}, second); err != nil {
		t.Fatalf("get %s after second call: %v", srv.Name, err)
	}
	if second.ResourceVersion != first.ResourceVersion {
		t.Errorf("second retireServer issued a patch: resourceVersion moved from %s to %s",
			first.ResourceVersion, second.ResourceVersion)
	}

	if got := scalingEvents(rec, "ServerRetiring"); got != 1 {
		t.Errorf("got %d ServerRetiring events, want 1", got)
	}
}

// reconcilePass reconciles every Server before the group, so a phase the
// group ordered lands before it looks.
func reconcilePass(t *testing.T, f *fixture, r *ServerGroupReconciler) {
	t.Helper()
	for _, s := range f.listServers(t) {
		f.reconcile(s.Name)
	}
	f.reconcileGroup(t, r)
}

func (f *fixture) markReady(t *testing.T, name string) {
	t.Helper()
	bringUpNamed(t, f, name)
}

func (f *fixture) markReadyWithPlayers(t *testing.T, name string, players int32) {
	t.Helper()
	uid := bringUpNamed(t, f, name)
	if err := f.agents.ReportPlayers(uid, players, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
}

// bumpPodSpec edits spec.image because a capacity edit makes nothing stale.
// spec.update is written out rather than relying on the floor of an unset
// maxUnavailable. spareSlots 150 exceeds one fresh server's 100, so a retiring
// server's capacity has to be backfilled, and stays inside (80, 180], where
// assertion 1 still sees exactly one create.
func (f *fixture) bumpPodSpec(t *testing.T) {
	t.Helper()
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Image = "ghcr.io/spawnery/paper:1.21.4-0.2.0"
	f.group.Spec.Update = &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1, MaxStaleSeconds: 0}
	f.group.Spec.Scaling.SpareSlots = 150
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
}

// serversOfGeneration stands in for "of the current pod hash", which holds
// only because bumpPodSpec moves the hash and the generation together.
func (f *fixture) serversOfGeneration(t *testing.T, generation int64) []spawneryv1alpha1.Server {
	t.Helper()
	var out []spawneryv1alpha1.Server
	for _, s := range f.listServers(t) {
		if s.Spec.GroupGeneration == generation {
			out = append(out, s)
		}
	}
	return out
}

func (f *fixture) retiringCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, s := range f.listServers(t) {
		if s.Status.Phase == string(phase.Retiring) {
			n++
		}
	}
	return n
}

func (f *fixture) firstRetiring(t *testing.T) *spawneryv1alpha1.Server {
	t.Helper()
	for _, s := range f.listServers(t) {
		if s.Status.Phase == string(phase.Retiring) {
			srv := s
			return &srv
		}
	}
	t.Fatal("no server in phase Retiring")
	return nil
}

// Each numbered step asserts a distinct thing that can break, so this runs a
// fixed number of passes rather than a loop.
func TestARollingUpdateReplacesAnOccupiedGroupWithoutKickingAnyone(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// Created directly: with minReplicas 1, scaling would create only one.
	a, b := "lobby-a", "lobby-b"
	f.createServer(a)
	f.markReadyWithPlayers(t, a, 60)
	f.createServer(b)
	f.markReadyWithPlayers(t, b, 60)

	f.bumpPodSpec(t)

	// 1. Exactly one replacement, not one per pass while it boots. Spare-slot
	// demand alone explains it (gap 70); TestARollingUpdateColdStartCreatesExactlyOneServer
	// pins coldStart itself.
	reconcilePass(t, f, r)
	fresh := f.serversOfGeneration(t, f.group.Generation)
	if len(fresh) != 1 {
		t.Fatalf("cold start created %d servers, want exactly 1", len(fresh))
	}

	// 2. Nothing retires before the replacement is Ready.
	reconcilePass(t, f, r)
	if n := f.retiringCount(t); n != 0 {
		t.Fatalf("%d servers retiring before a replacement was Ready", n)
	}

	// 3. Exactly one stale server retires (maxUnavailable 1); the second pass lands
	// the phase the first ordered.
	f.markReady(t, fresh[0].Name)
	reconcilePass(t, f, r)
	reconcilePass(t, f, r)
	if n := f.retiringCount(t); n != 1 {
		t.Fatalf("%d servers retiring, want exactly 1 (maxUnavailable)", n)
	}

	// 4. The retiring server keeps its players and takes no new joins.
	retiring := f.firstRetiring(t)
	if !retiring.DeletionTimestamp.IsZero() {
		t.Error("a retiring server with players was deleted")
	}
	if retiring.Status.Registered {
		t.Error("a retiring server is still registered")
	}

	// 5. The retiring server leaves the group's size, so the capacity it carried is
	// backfilled in the same pass.
	if got := len(f.serversOfGeneration(t, f.group.Generation)); got != 2 {
		t.Fatalf("%d current-generation servers after the retirement, want 2: "+
			"the cold-start replacement plus the backfill for the capacity the retiring server took with it", got)
	}
}

// 80 free slots on the stale servers already cover spareSlots 40, so nothing but
// coldStart can explain the create.
func TestARollingUpdateColdStartCreatesExactlyOneServer(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	a, b := "lobby-a", "lobby-b"
	f.createServer(a)
	f.markReadyWithPlayers(t, a, 60)
	f.createServer(b)
	f.markReadyWithPlayers(t, b, 60)

	// bumpPodSpec's image edit, without its spareSlots change.
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Image = "ghcr.io/spawnery/paper:1.21.4-0.2.0"
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}

	reconcilePass(t, f, r)
	fresh := f.serversOfGeneration(t, f.group.Generation)
	if len(fresh) != 1 {
		t.Fatalf("cold start created %d servers, want exactly 1", len(fresh))
	}

	// A booting server counts its full maxPlayers, so no second one may follow.
	for i := 0; i < 2; i++ {
		reconcilePass(t, f, r)
		if got := len(f.serversOfGeneration(t, f.group.Generation)); got != 1 {
			t.Fatalf("%d servers of the new generation after pass %d, want exactly 1: "+
				"cold start must create one server, not one per five-second pass while it boots", got, i+2)
		}
	}
}

func TestGroupBackoffFieldsRoundTripThroughTheAPIServer(t *testing.T) {
	f := newFixture(t)
	g := f.group

	now := metav1.Now()
	g.Status.ConsecutiveFailures = 3
	g.Status.LastFailureAt = &now
	if err := f.c.Status().Update(f.ctx, g); err != nil {
		t.Fatalf("status update: %v", err)
	}

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(g), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.ConsecutiveFailures != 3 {
		t.Error("status.consecutiveFailures did not survive the API server; run make manifests")
	}
	if got.Status.LastFailureAt == nil {
		t.Error("status.lastFailureAt did not survive the API server; run make manifests")
	}
}

func TestCollectViewsCarriesTheFailureAndReadyTimestamps(t *testing.T) {
	// Zero timestamps would date every failure to the epoch, which the backoff
	// counts once and never again.
	f := newFixture(t)
	f.createServer("lobby-tsx1")
	r := groupReconciler(f)

	failed := metav1.NewTime(f.clock.Now().Add(-time.Minute))
	ready := metav1.NewTime(f.clock.Now().Add(-time.Hour))
	srv := f.server("lobby-tsx1")
	srv.Status.Phase = string(phase.Failed)
	srv.Status.FailedAt = &failed
	srv.Status.ReadySince = &ready
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("status update: %v", err)
	}

	views, _, err := r.collectViews(f.ctx, f.group)
	if err != nil {
		t.Fatalf("collectViews: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
	if !views[0].FailedAt.Equal(failed.Time) {
		t.Errorf("FailedAt = %v, want %v", views[0].FailedAt, failed.Time)
	}
	if !views[0].ReadySince.Equal(ready.Time) {
		t.Errorf("ReadySince = %v, want %v", views[0].ReadySince, ready.Time)
	}
}

func (f *fixture) oneServerName(t *testing.T) string {
	t.Helper()
	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want exactly 1", len(servers))
	}
	return servers[0].Name
}

func (f *fixture) newestServerName(t *testing.T) (string, bool) {
	t.Helper()
	var name string
	n := 0
	for _, s := range f.listServers(t) {
		if s.Status.Phase == string(phase.Failed) {
			continue
		}
		name = s.Name
		n++
	}
	if n != 1 {
		return "", false
	}
	return name, true
}

func (f *fixture) reloadGroup(t *testing.T) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, g); err != nil {
		t.Fatalf("get group: %v", err)
	}
	return g
}

func (f *fixture) setMaxReplicas(t *testing.T, n int32) {
	t.Helper()
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.MaxReplicas = n
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
}

func (f *fixture) failServer(t *testing.T, name string) {
	t.Helper()
	bringUpNamed(t, f, name)
	driveToFailed(t, f, name)
	if f.server(name).Status.FailedAt == nil {
		t.Fatalf("server %s reached Failed with no status.failedAt; that is the field the count reads", name)
	}
}

// failServerNeverReady fails a server on its startup deadline without it ever
// being Ready. failedAt is stamped after the clock advance, so a caller still has
// to wait out the window it opens.
func (f *fixture) failServerNeverReady(t *testing.T, name string) {
	t.Helper()
	f.reconcile(name)
	if _, ok := f.pod(name); !ok {
		t.Fatalf("reconcile did not create the pod for %s", name)
	}
	// The probe never goes green and no agent connects.
	f.setPodRunning(name, false)
	f.reconcile(name)
	if got := f.server(name).Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase of %s = %q, want Starting: the startup deadline is only armed on entry to Starting", name, got)
	}

	f.clock.Advance(f.reconc.StartupDeadline + time.Second)
	f.reconcile(name)

	srv := f.server(name)
	if got := srv.Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase of %s = %q after its startup deadline elapsed, want Failed", name, got)
	}
	// Not proof of never-Ready: entering Starting or Failed clears readySince anyway.
	if srv.Status.ReadySince != nil {
		t.Fatalf("server %s carries status.readySince", name)
	}
	if srv.Status.FailedAt == nil {
		t.Fatalf("server %s reached Failed with no status.failedAt; that is the field the count reads", name)
	}
}

// envtest runs no kubelet and the finalizer holds through the drain, so a
// deleted Server lingers with a deletion timestamp.
func (f *fixture) shedCount(t *testing.T, ignore ...string) int {
	t.Helper()
	n := 0
	for _, s := range f.listServers(t) {
		if containsString(ignore, s.Name) || s.DeletionTimestamp.IsZero() {
			continue
		}
		n++
	}
	return n
}

func TestGroupStopsCreatingWhileItBacksOff(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)

	name := f.oneServerName(t)
	f.failServer(t, name)

	// The pass that counts the failure is already inside the window it opens, so it
	// must not build the replacement either.
	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != 1 {
		t.Fatalf("%d servers on the pass that counted the failure, want 1: "+
			"the group built a replacement inside the backoff window it had just opened", got)
	}

	f.clock.Advance(ResyncInterval)
	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != 1 {
		t.Errorf("%d servers five seconds into a ten-second window, want 1", got)
	}

	g := f.reloadGroup(t)
	if g.Status.ConsecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1", g.Status.ConsecutiveFailures)
	}
	if g.Status.LastFailureAt == nil {
		t.Error("lastFailureAt was not stamped, so the count could not survive an operator restart")
	}
}

func TestGroupCreatesAgainOnceTheWindowCloses(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, r)

	before := len(f.listServers(t))
	f.clock.Advance(backoffBase + time.Second)
	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != before+1 {
		t.Errorf("servers = %d, want %d: the window closed and the group should build again", got, before+1)
	}
}

// A deletion path waiting on an unrelated failure would leave surplus servers
// standing for the whole window.
func TestGroupStillShedsWhileItBacksOff(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	// No floor and a ceiling of one: a removal is the only thing this pass can decide.
	f.setMinReplicas(t, 0)
	f.setMaxReplicas(t, 1)

	// The failure does not count toward size, so exactly one idle server is surplus.
	idle := []string{"lobby-idle-a", "lobby-idle-b"}
	for _, name := range idle {
		bringUpReady(t, f, name)
	}
	// The failure must be newer than the readySince stamps or the streak never
	// starts, and the fixture's clock moves only when told to.
	f.clock.Advance(time.Second)
	broken := "lobby-broken"
	f.createServer(broken)
	f.failServer(t, broken)

	f.reconcileGroup(t, r)

	c := meta.FindStatusCondition(f.reloadGroup(t).Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("BackingOff = %+v, want True: with no window open this proves nothing about shedding while one is", c)
	}
	if got := f.shedCount(t, broken); got != 1 {
		t.Errorf("%d of the two idle servers were shed while the group was backing off, want 1", got)
	}
}

// A retirement stalled behind the window would stop a changeover mid-flight over
// an unrelated failure.
func TestGroupStillRetiresWhileItBacksOff(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	stale := "lobby-stale"
	bringUpReady(t, f, stale)

	f.bumpPodSpec(t)

	// selectRetirement nominates nothing without a Ready current-generation server.
	current := "lobby-current"
	bringUpReady(t, f, current)

	// Newer than lobby-current's readySince, as in TestGroupStillShedsWhileItBacksOff.
	f.clock.Advance(time.Second)
	broken := "lobby-broken"
	f.createServer(broken)
	f.failServer(t, broken)

	f.reconcileGroup(t, r)

	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1: with no window open this proves nothing about retiring while one is", got)
	}
	if !f.server(stale).Spec.Retire {
		t.Errorf("the stale server was not asked to retire while the group was backing off")
	}
}

func TestAPodSpecChangeClearsTheBackoff(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, r)
	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got != 1 {
		t.Fatalf("consecutiveFailures = %d before the spec change, want 1: there is no streak here to clear", got)
	}

	f.bumpPodSpec(t)
	f.reconcileGroup(t, r)

	g := f.reloadGroup(t)
	if g.Status.ConsecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d after a spec change, want 0", g.Status.ConsecutiveFailures)
	}
	// Kept as the watermark that stops the old corpse counting into the new streak.
	if g.Status.LastFailureAt == nil {
		t.Error("lastFailureAt was cleared by the reset")
	}
	if len(f.serversOfGeneration(t, g.Generation)) == 0 {
		t.Error("the group created nothing on the pass that cleared the streak; after a spec change the next attempt is immediate")
	}
}

// derivePhase turns a true Degraded into the phase, so a ten-second wait must
// not claim it.
func TestBackingOffConditionNamesTheCountAndTheWait(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, r)

	g := f.reloadGroup(t)
	c := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("BackingOff = %+v, want True", c)
	}
	if c.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("reason = %q, want %q", c.Reason, spawneryv1alpha1.ReasonCrashLoopBackoff)
	}
	if !strings.Contains(c.Message, "1 round") || !strings.Contains(c.Message, "next attempt in") {
		t.Errorf("message = %q, want the round count and the remaining wait", c.Message)
	}
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Error("Degraded is true after a single failure")
	}
}

func TestGroupGivesUpAndSaysSo(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	for i := int32(0); i < backoffGiveUpAt; i++ {
		f.reconcileGroup(t, r)
		if name, ok := f.newestServerName(t); ok {
			f.failServer(t, name)
		}
		f.reconcileGroup(t, r)
		f.clock.Advance(backoffCap)
	}
	f.reconcileGroup(t, r)

	g := f.reloadGroup(t)
	if !meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Fatalf("Degraded is not true after %d failures", backoffGiveUpAt)
	}
	if got := meta.FindStatusCondition(g.Status.Conditions,
		spawneryv1alpha1.ConditionDegraded).Reason; got != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("Degraded reason = %q, want CrashLoopBackoff", got)
	}
	if g.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded: derivePhase already maps the condition", g.Status.Phase)
	}
	c := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("BackingOff = %+v, want False once the group gave up", c)
	}
	if c.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("reason = %q, want CrashLoopBackoff rather than an all-clear", c.Reason)
	}
	// The retry annotation is not discoverable from the field list.
	if !strings.Contains(c.Message, spawneryv1alpha1.AnnotationRetry) {
		t.Errorf("message = %q, want it to name the %s annotation", c.Message, spawneryv1alpha1.AnnotationRetry)
	}

	// An absolute count, not a delta: a group that created anyway has already
	// absorbed its extra server into any baseline taken here. No Server is reconciled
	// again, so the finalizer keeps every corpse in Failed.
	f.clock.Advance(time.Hour)
	f.reconcileGroup(t, r)
	live, corpses := 0, 0
	for _, s := range f.listServers(t) {
		if s.Status.Phase == string(phase.Failed) {
			corpses++
			continue
		}
		live++
	}
	if live != 0 {
		t.Errorf("%d live server(s) after the group gave up, want 0: a group past the threshold creates nothing", live)
	}
	if corpses != int(backoffGiveUpAt) {
		t.Errorf("%d corpses, want %d — one per counted failure", corpses, backoffGiveUpAt)
	}
}

// failServer's servers reached Ready, which at a real resync would reset the
// streak; this pins that a never-Ready server climbs the same ladder. backoffCap
// exceeds every window the schedule opens before the threshold.
func TestGroupGivesUpOnServersThatNeverBecomeReady(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)

	for i := int32(0); i < backoffGiveUpAt; i++ {
		f.reconcileGroup(t, r)
		name, ok := f.newestServerName(t)
		if !ok {
			t.Fatalf("round %d: the group has no live server to fail, so it stopped creating early", i+1)
		}
		f.failServerNeverReady(t, name)
		f.reconcileGroup(t, r)
		f.clock.Advance(backoffCap)
	}
	f.reconcileGroup(t, r)

	g := f.reloadGroup(t)
	if got := g.Status.ConsecutiveFailures; got != backoffGiveUpAt {
		t.Fatalf("consecutiveFailures = %d, want %d", got, backoffGiveUpAt)
	}
	if !meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Fatalf("Degraded is not true after %d servers failed to start at all", backoffGiveUpAt)
	}
	if got := meta.FindStatusCondition(g.Status.Conditions,
		spawneryv1alpha1.ConditionDegraded).Reason; got != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("Degraded reason = %q, want CrashLoopBackoff", got)
	}
	if g.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", g.Status.Phase)
	}

	// The absolute state, as in TestGroupGivesUpAndSaysSo.
	live, corpses := 0, 0
	for _, s := range f.listServers(t) {
		if s.Status.Phase != string(phase.Failed) {
			live++
			continue
		}
		corpses++
		// Not proof of never-Ready: entering Starting or Failed clears readySince anyway.
		if s.Status.ReadySince != nil {
			t.Errorf("corpse %s carries status.readySince", s.Name)
		}
	}
	if live != 0 {
		t.Errorf("%d live server(s) after the group gave up, want 0", live)
	}
	if corpses != int(backoffGiveUpAt) {
		t.Errorf("%d corpses, want %d — one per counted failure", corpses, backoffGiveUpAt)
	}
}

func TestBackingOffEventFiresOnTheFlankOnly(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, r)

	// scalingEvents drains the recorder, so this also empties it.
	if first := scalingEvents(rec, spawneryv1alpha1.ReasonCrashLoopBackoff); first != 1 {
		t.Fatalf("events = %d after the first failure, want 1", first)
	}
	f.clock.Advance(time.Second)
	f.reconcileGroup(t, r)
	if got := scalingEvents(rec, spawneryv1alpha1.ReasonCrashLoopBackoff); got != 0 {
		t.Errorf("events = %d after a resync inside the same window, want 0: the event goes on the flank", got)
	}
}

// Without the sized guard, the !sized pass flips BackingOff to False with the
// NoRecentFailures reason and fires an all-clear although nothing was decided.
func TestBackingOffEventDoesNotFireWhenNetworkDiesMidBackoff(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, r)

	if !meta.IsStatusConditionTrue(f.reloadGroup(t).Status.Conditions, spawneryv1alpha1.ConditionBackingOff) {
		t.Fatalf("BackingOff is not true after a failure; the network-dies-mid-backoff pass below would prove nothing")
	}
	// scalingEvents empties the recorder whichever reason it counts.
	scalingEvents(rec, spawneryv1alpha1.ReasonCrashLoopBackoff)

	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	f.reconcileGroup(t, r)

	if got := scalingEvents(rec, spawneryv1alpha1.ReasonNoRecentFailures); got != 0 {
		t.Errorf("events = %d when the network died mid-backoff, want 0: nothing was decided this pass, so no event should fire", got)
	}
}

// The Degraded twin of the guard above. BackingOff is False on both passes and
// ScalingLimited reads WithinLimits, so any NoRecentFailures event came from the
// Degraded guard.
func TestDegradedEventDoesNotFireWhenNetworkDiesAfterAGiveUp(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec
	f.setMinReplicas(t, 1)
	for i := int32(0); i < backoffGiveUpAt; i++ {
		f.reconcileGroup(t, r)
		if name, ok := f.newestServerName(t); ok {
			f.failServer(t, name)
		}
		f.reconcileGroup(t, r)
		f.clock.Advance(backoffCap)
		drainRecorder(rec)
	}
	f.reconcileGroup(t, r)

	if !meta.IsStatusConditionTrue(f.reloadGroup(t).Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Fatalf("Degraded is not true after %d failures; the network-dies pass below would prove nothing", backoffGiveUpAt)
	}
	drainRecorder(rec)

	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	f.reconcileGroup(t, r)

	if got := scalingEvents(rec, spawneryv1alpha1.ReasonNoRecentFailures); got != 0 {
		t.Errorf("events = %d when the network died after a give-up, want 0: "+
			"nothing was decided this pass, so no all-clear event should fire", got)
	}
}

// A zero metav1.Time round-trips to nil, so only this pass's BackingOff message
// can show an unguarded LastFailureAt write.
func TestBackingOffMessageStaysSilentAboutAFailureThatNeverHappened(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)

	c := meta.FindStatusCondition(f.reloadGroup(t).Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
	if c == nil {
		t.Fatal("BackingOff condition was not published")
	}
	if c.Message != "no server has failed to start recently" {
		t.Errorf("message = %q, want the plain no-history message with no timestamp", c.Message)
	}
}

func TestAPodSpecChangeClearsAGaveUpGroupsConditions(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	for i := int32(0); i < backoffGiveUpAt; i++ {
		f.reconcileGroup(t, r)
		if name, ok := f.newestServerName(t); ok {
			f.failServer(t, name)
		}
		f.reconcileGroup(t, r)
		f.clock.Advance(backoffCap)
	}
	f.reconcileGroup(t, r)

	if !meta.IsStatusConditionTrue(f.reloadGroup(t).Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Fatalf("Degraded is not true after %d failures; the pod spec change below would prove nothing", backoffGiveUpAt)
	}

	f.bumpPodSpec(t)
	f.reconcileGroup(t, r)

	g := f.reloadGroup(t)
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Error("Degraded still true after the spec change that answers it")
	}
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionBackingOff) {
		t.Error("BackingOff still true after the spec change")
	}
	if got := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionBackingOff).Reason; got != spawneryv1alpha1.ReasonNoRecentFailures {
		t.Errorf("BackingOff reason = %q after the spec change, want %q: the give-up reason must not survive it", got, spawneryv1alpha1.ReasonNoRecentFailures)
	}
	if got := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionDegraded).Reason; got != spawneryv1alpha1.ReasonNoRecentFailures {
		t.Errorf("Degraded reason = %q after the spec change, want %q: the give-up reason must not survive it", got, spawneryv1alpha1.ReasonNoRecentFailures)
	}
}

// Publishing a backoff decision next to Accepted: False would explain the same
// standstill twice.
func TestBackingOffIsNotDecidedWithoutAUsableNetwork(t *testing.T) {
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
	backingOff := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
	if backingOff == nil || backingOff.Status != metav1.ConditionFalse {
		t.Fatalf("BackingOff = %+v on a group without its Network, want False", backingOff)
	}
	if backingOff.Message != "backoff is not being decided: the group's network is not usable" {
		t.Errorf("message = %q, want the not-decided message", backingOff.Message)
	}
	degraded := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionFalse {
		t.Fatalf("Degraded = %+v on a group without its Network, want False", degraded)
	}
	if degraded.Message != "backoff is not being decided: the group's network is not usable" {
		t.Errorf("message = %q, want the not-decided message", degraded.Message)
	}
}

func drainRecorder(rec *nonBlockingRecorder) { drainEvents(rec) }

// Every new-generation server is failed each pass because the stale one stays
// Ready, and the clock moves first so failedAt is newer than its readySince.
// bound = 1 cold-start server + 2 windows (opening at 15 s and 40 s of the 50 s
// run) * 2 servers each, since spareSlots makes a recovery build two; a group
// rebuilding every pass would build about twenty.
func TestGroupWithABrokenNewImageDoesNotRebuildEveryPass(t *testing.T) {
	const (
		passes = 10
		bound  = 5
	)
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	f.markReady(t, f.oneServerName(t))

	f.bumpPodSpec(t) // the changeover begins; the cold start builds one
	f.reconcileGroup(t, r)
	generation := f.reloadGroup(t).Generation

	for i := 0; i < passes; i++ {
		drainRecorder(f.reconc.Recorder.(*nonBlockingRecorder))
		drainRecorder(r.Recorder.(*nonBlockingRecorder))
		f.clock.Advance(ResyncInterval)
		for _, s := range f.serversOfGeneration(t, generation) {
			if s.Status.Phase != string(phase.Failed) {
				f.failServer(t, s.Name)
			}
		}
		f.reconcileGroup(t, r)
	}

	if got := len(f.serversOfGeneration(t, generation)); got > bound {
		t.Errorf("group built %d servers of the new generation across %d passes, want at most %d: "+
			"the backoff must bound the loop", got, passes, bound)
	}
}

func TestACordonedNodeCondemnsTheServersOnIt(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)

	// One occupied: players do not exempt a server from a leaving node.
	f.setMinReplicas(t, 2)
	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(servers))
	}
	f.markReadyWithPlayers(t, servers[0].Name, 3)
	f.markReady(t, servers[1].Name)

	going := f.ensureNode(t, "node-going-"+f.ns, false)
	f.ensureNode(t, "node-staying-"+f.ns, false)
	for i, srv := range f.listServers(t) {
		reloaded := f.server(srv.Name)
		pod, ok := f.pod(reloaded.Status.PodName)
		if !ok {
			t.Fatalf("pod of %s not found", srv.Name)
		}
		if i == 0 {
			f.bindPodToNode(t, pod, going.Name)
		} else {
			f.bindPodToNode(t, pod, "node-staying-"+f.ns)
		}
	}
	f.ensureNode(t, going.Name, true)

	f.reconcileGroup(t, r)

	condemned := f.server(servers[0].Name)
	if condemned.DeletionTimestamp.IsZero() {
		t.Error("the server on the cordoned node was not deleted; its players will be evicted instead of moved")
	}
	if !f.server(servers[1].Name).DeletionTimestamp.IsZero() {
		t.Error("the server on the healthy node was deleted; only the departing node's servers may go")
	}
	// scalingEvents drains the recorder, so the resync below starts empty.
	if n := scalingEvents(rec, "NodeDraining"); n != 1 {
		t.Errorf("NodeDraining events = %d, want exactly 1", n)
	}

	// Past expectationTTL condemned() names the server again, so only deleteServer's
	// DeletionTimestamp guard stands between it and a second event.
	f.clock.Advance(expectationTTL + time.Second)
	f.reconcileGroup(t, r)
	if n := scalingEvents(rec, "NodeDraining"); n != 0 {
		t.Errorf("NodeDraining events on an unchanged resync = %d, want 0: the drain is announced "+
			"once, not once per pass for its whole duration", n)
	}
}

// size() and pruneFailed both reach a Failed server on a departing node in one
// pass, and r.Delete stamps no deletion timestamp on the local object. Both
// failures are condemned, so any FailedServerPruned event is the duplicate.
func TestAFailedServerOnACordonedNodeGoesAwayOnce(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)

	f.setMinReplicas(t, 2)
	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(servers))
	}
	// One more than maxRetainedFailures, so the cap would prune one if it still looked.
	for _, s := range servers {
		f.failServer(t, s.Name)
	}

	node := f.ensureNode(t, "node-going-"+f.ns, false)
	for _, s := range servers {
		pod, ok := f.pod(f.server(s.Name).Status.PodName)
		if !ok {
			t.Fatalf("pod of %s not found", s.Name)
		}
		f.bindPodToNode(t, pod, node.Name)
	}
	f.ensureNode(t, node.Name, true)
	drainEvents(rec) // discard anything the setup recorded

	f.reconcileGroup(t, r)

	// Each event is delivered once, so the channel is read once.
	events := drainEvents(rec)
	count := func(reason string) int {
		n := 0
		for _, e := range events {
			if fields := strings.SplitN(e, " ", 3); len(fields) >= 2 && fields[1] == reason {
				n++
			}
		}
		return n
	}

	if got := count("NodeDraining"); got != 2 {
		t.Fatalf("NodeDraining events = %d, want 2: without both failures actually being condemned "+
			"the assertion below holds for a reason that has nothing to do with pruning: %v", got, events)
	}
	if got := count("FailedServerPruned"); got != 0 {
		t.Errorf("FailedServerPruned events = %d, want 0: both failures are already going as "+
			"condemned servers, and a second removal of the same server announces one departure "+
			"twice under two reasons: %v", got, events)
	}
}

// Condemnation must not wait on a usable Network: the node leaves regardless, and
// kubectl drain would hang on the occupied pod.
func TestABrokenNetworkDoesNotStopACordonedNodeFromEmptying(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(servers))
	}
	f.markReadyWithPlayers(t, servers[0].Name, 3)

	node := f.ensureNode(t, "node-going-"+f.ns, false)
	pod, ok := f.pod(f.server(servers[0].Name).Status.PodName)
	if !ok {
		t.Fatal("pod not found")
	}
	f.bindPodToNode(t, pod, node.Name)
	f.ensureNode(t, node.Name, true)

	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}

	f.reconcileGroup(t, r)

	group := f.reloadGroup(t)
	accepted := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason != spawneryv1alpha1.ReasonNetworkNotFound {
		t.Fatalf("Accepted = %v, want False/%s: this pass did not take the missing-Network route, "+
			"so nothing below is a statement about the gate",
			accepted, spawneryv1alpha1.ReasonNetworkNotFound)
	}

	if f.server(servers[0].Name).DeletionTimestamp.IsZero() {
		t.Error("the server on the cordoned node was not condemned because its group's Network is " +
			"broken; its players stay on a node that is leaving, and kubectl drain hangs on them")
	}
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("NodeDraining = %v, want True: the condition and the condemnation are computed from "+
			"the same views and must not be able to disagree", cond)
	}
}

func TestCondemnedServersAreReplaced(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	f.markReady(t, servers[0].Name)

	node := f.ensureNode(t, "node-going-"+f.ns, false)
	pod, ok := f.pod(f.server(servers[0].Name).Status.PodName)
	if !ok {
		t.Fatal("pod not found")
	}
	f.bindPodToNode(t, pod, node.Name)
	f.ensureNode(t, node.Name, true)

	f.reconcileGroup(t, r)

	if f.server(servers[0].Name).DeletionTimestamp.IsZero() {
		t.Fatal("the server on the cordoned node was not condemned; nothing below tests a replacement")
	}
	live := 0
	for _, s := range f.listServers(t) {
		if s.Name != servers[0].Name && s.DeletionTimestamp.IsZero() {
			live++
		}
	}
	if live < 1 {
		t.Fatal("no replacement was ordered in the pass that condemned; the group would sit empty until the next one")
	}
}

func TestNodeDrainingConditionNamesTheNode(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	f.markReady(t, servers[0].Name)

	node := f.ensureNode(t, "node-going-"+f.ns, false)
	pod, ok := f.pod(f.server(servers[0].Name).Status.PodName)
	if !ok {
		t.Fatal("pod not found")
	}
	f.bindPodToNode(t, pod, node.Name)
	f.ensureNode(t, node.Name, true)
	f.reconcileGroup(t, r)

	cond := meta.FindStatusCondition(f.reloadGroup(t).Status.Conditions,
		spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("NodeDraining = %v, want True", cond)
	}
	if !strings.Contains(cond.Message, node.Name) {
		t.Errorf("message %q does not name the node", cond.Message)
	}

	f.ensureNode(t, node.Name, false)
	f.reconcileGroup(t, r)
	cond = meta.FindStatusCondition(f.reloadGroup(t).Status.Conditions,
		spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("NodeDraining after uncordon = %v, want False", cond)
	}
}

// spec.type is immutable, so a persistent group is built from scratch rather
// than edited from the fixture's.
func (f *fixture) createPersistentGroup(t *testing.T, name string, replicas int32) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: f.network.Name},
			Type:                          spawneryv1alpha1.ServerGroupPersistent,
			Image:                         "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:                    100,
			Replicas:                      &replicas,
			TerminationGracePeriodSeconds: 60,
			FailedRetentionSeconds:        3600,
			Drain:                         &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 60},
			Storage:                       &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("10Gi")},
		},
	}
	if err := f.c.Create(f.ctx, group); err != nil {
		t.Fatalf("create persistent ServerGroup %s: %v", name, err)
	}
	return group
}

func (f *fixture) reconcileNamedGroup(t *testing.T, r *ServerGroupReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile group %s: %v", name, err)
	}
}

func (f *fixture) setPersistentReplicas(t *testing.T, name string, n int32) {
	t.Helper()
	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group %s: %v", name, err)
	}
	group.Spec.Replicas = &n
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("update group %s: %v", name, err)
	}
}

func (f *fixture) serverNamesOfGroup(t *testing.T, group string) []string {
	t.Helper()
	list := &spawneryv1alpha1.ServerList{}
	if err := f.c.List(f.ctx, list, ctrlclientInNamespace(f.ns)); err != nil {
		t.Fatalf("list servers: %v", err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.GroupRef.Name == group {
			names = append(names, list.Items[i].Name)
		}
	}
	sort.Strings(names)
	return names
}

func (f *fixture) serverIfPresent(name string) (*spawneryv1alpha1.Server, bool) {
	srv := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, srv); err != nil {
		return nil, false
	}
	return srv, true
}

func TestAPersistentGroupBuildsItsOrdinals(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)

	f.reconcileNamedGroup(t, r, "survival")

	names := f.serverNamesOfGroup(t, "survival")
	if len(names) != 2 || names[0] != "survival-0" || names[1] != "survival-1" {
		t.Fatalf("servers = %v, want [survival-0 survival-1]", names)
	}
	// The sorted index is the ordinal; OrdinalOf would only ask the name what it says.
	for i, name := range names {
		srv := f.server(name)
		if srv.Spec.Ordinal == nil || *srv.Spec.Ordinal != int32(i) {
			t.Errorf("%s spec.ordinal = %v, want %d", name, srv.Spec.Ordinal, i)
		}
	}
}

// Only the group is driven, so no drain finalizer holds the surplus ordinal in
// Terminating.
func TestAPersistentGroupRemovesTheHighestOrdinal(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 3)
	f.reconcileNamedGroup(t, r, "survival")
	if got := len(f.serverNamesOfGroup(t, "survival")); got != 3 {
		t.Fatalf("servers = %d, want 3", got)
	}

	// The event is the only reader of SizeDecision.DeleteReason.
	rec := newRecorder()
	r.Recorder = rec

	f.setPersistentReplicas(t, "survival", 2)
	f.reconcileNamedGroup(t, r, "survival")

	if _, present := f.serverIfPresent("survival-2"); present {
		t.Error("survival-2 is still here; the highest ordinal is the one that goes")
	}
	if got := scalingEvents(rec, "SurplusOrdinal"); got != 1 {
		t.Errorf("SurplusOrdinal events = %d, want 1; the takedown must say which class named it", got)
	}
	if names := f.serverNamesOfGroup(t, "survival"); len(names) != 2 ||
		names[0] != "survival-0" || names[1] != "survival-1" {
		t.Errorf("surviving servers = %v, want [survival-0 survival-1]", names)
	}
}

// A server holding the name without spec.ordinal is asked for again on every
// pass; returning AlreadyExists would block the rest of the reconcile for good.
func TestAPersistentGroupToleratesAnOrdinalNameAlreadyTaken(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createPersistentGroup(t, "survival", 1)

	squatter := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PersistentServerName("survival", 0),
			Namespace: f.ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       group.Name,
				UID:        group.UID,
			}},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: group.Name},
		},
	}
	if err := f.c.Create(f.ctx, squatter); err != nil {
		t.Fatalf("create the server already holding the name: %v", err)
	}

	f.reconcileNamedGroup(t, r, "survival")

	if names := f.serverNamesOfGroup(t, "survival"); len(names) != 1 || names[0] != "survival-0" {
		t.Errorf("servers = %v, want [survival-0]", names)
	}
	if n := scalingEvents(r.Recorder.(*nonBlockingRecorder), "ServerCreated"); n != 0 {
		t.Errorf("ServerCreated events = %d, want none: the object was already there, "+
			"and this collision repeats every pass for as long as it lasts", n)
	}
}

func TestAPersistentServerIsCreatedCarryingItsRenderHash(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createPersistentGroup(t, "survival", 1)

	f.reconcileNamedGroup(t, r, "survival")

	srv := f.server("survival-0")
	if srv.Spec.PodHash == "" {
		t.Fatal("server was created with no pod hash, so it can never be found stale")
	}

	values, err := serverConfigValues(group)
	if err != nil {
		t.Fatalf("serverConfigValues: %v", err)
	}
	want, err := podspec.DesiredServerHash(f.network, group, values)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	if srv.Spec.PodHash != want {
		t.Fatalf("stamped %q, the group would render %q", srv.Spec.PodHash, want)
	}
}

// Asserting only that the hash gets filled would pass while every world restarted.
func TestAServerWithNoHashIsAdoptedRatherThanReplaced(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 1)
	f.reconcileNamedGroup(t, r, "survival")

	// Blanked to simulate a server that predates spec.podHash.
	srv := f.server("survival-0")
	uidBefore := srv.UID
	srv.Spec.PodHash = ""
	if err := f.c.Update(f.ctx, srv); err != nil {
		t.Fatalf("blank the hash: %v", err)
	}

	f.reconcileNamedGroup(t, r, "survival")

	after := f.server("survival-0")
	if after.UID != uidBefore {
		t.Fatal("the ordinal was replaced; an empty hash must be adopted, not treated as stale")
	}
	if after.Spec.PodHash == "" {
		t.Fatal("the hash was not stamped, so the server stays unadoptable forever")
	}
	if !after.DeletionTimestamp.IsZero() {
		t.Fatal("a takedown was ordered for an adopted server")
	}
}

// markReady runs the Server controller, so the drain finalizer leaves a deletion
// timestamp rather than removing the object.
func TestAPersistentServerOnACordonedNodeIsCondemned(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 1)
	f.reconcileNamedGroup(t, r, "survival")

	srv := f.server("survival-0")
	f.markReady(t, srv.Name)
	node := f.ensureNode(t, "node-going-"+f.ns, false)
	pod, ok := f.pod(f.server(srv.Name).Status.PodName)
	if !ok {
		t.Fatal("pod not found")
	}
	f.bindPodToNode(t, pod, node.Name)
	f.ensureNode(t, node.Name, true)

	f.reconcileNamedGroup(t, r, "survival")

	condemned, present := f.serverIfPresent("survival-0")
	if !present {
		t.Fatal("survival-0 is gone outright, which the drain finalizer should have prevented")
	}
	if condemned.DeletionTimestamp.IsZero() {
		t.Fatal("the persistent server on the cordoned node was not condemned; its players will be evicted instead of moved")
	}
}

// No ServerGroup publishes a Ready condition: readiness is status.phase, from
// ReadyReplicas against DesiredReplicas().
func TestAPersistentGroupPublishesTheReadinessItsPodsSupport(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 1)
	f.reconcileNamedGroup(t, r, "survival")
	f.markReady(t, "survival-0")

	res, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "survival", Namespace: f.ns},
	})
	if err != nil {
		t.Fatalf("reconcile group survival: %v", err)
	}

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "survival", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group survival: %v", err)
	}
	if cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionReady); cond != nil {
		t.Errorf("Ready condition = %+v; no ServerGroup publishes one, and a persistent group "+
			"that did would be contradicting the pods it now has", cond)
	}
	if got := group.Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase = %q with its one replica Ready, want %s", got, phase.Ready)
	}
	if res.RequeueAfter != ResyncInterval {
		t.Errorf("RequeueAfter = %s, want the ordinary resync %s: a persistent group decides "+
			"removals on the same clock as an ephemeral one", res.RequeueAfter, ResyncInterval)
	}
}

// Only the Server controller's failed retention removes a persistent corpse. Kept
// apart from the failure so the window it opens stays observable.
func (f *fixture) clearFailedOrdinal(t *testing.T, name string) {
	t.Helper()
	f.clock.Advance(time.Hour + time.Second)
	for i := 0; i < 4; i++ {
		if _, present := f.serverIfPresent(name); !present {
			return
		}
		f.reconcile(name)
	}
	t.Fatalf("the corpse of %s was not taken away by its failed retention", name)
}

// Each round ages the corpse out, since only failed retention clears a persistent
// corpse and lets the ordinal be rebuilt. survival-1 stays Ready throughout, so it
// is the minimum over required ordinals that reaches Degraded. The failure is a
// startup deadline because envtest never binds or fails to bind a claim.
func TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)

	f.reconcileNamedGroup(t, r, "survival")
	bringUpNamed(t, f, "survival-1")

	for i := int32(0); i < backoffGiveUpAt; i++ {
		f.reconcileNamedGroup(t, r, "survival")
		if _, present := f.serverIfPresent("survival-0"); !present {
			t.Fatalf("round %d: the group did not rebuild its ordinal, so it stopped creating early", i+1)
		}
		f.failServerNeverReady(t, "survival-0")
		f.reconcileNamedGroup(t, r, "survival")

		if i == 0 {
			// Taken on the flank: at the threshold BackingOff goes False again.
			g := f.persistentGroup(t, "survival")
			c := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
			if c == nil || c.Status != metav1.ConditionTrue {
				t.Fatalf("BackingOff = %+v after the first failure, want True: a persistent group "+
					"that is waiting before its next attempt has to say so", c)
			}
			if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
				t.Error("Degraded is true after a single failure; one hiccup is not a fault")
			}
		}

		// Last, so the assertions above still see the window open.
		f.clearFailedOrdinal(t, "survival-0")
	}
	f.reconcileNamedGroup(t, r, "survival")

	g := f.persistentGroup(t, "survival")
	if got := g.Status.ConsecutiveFailures; got != backoffGiveUpAt {
		t.Fatalf("consecutiveFailures = %d, want %d", got, backoffGiveUpAt)
	}
	degraded := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v after %d failures, want True", degraded, backoffGiveUpAt)
	}
	if degraded.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("Degraded reason = %q, want %s", degraded.Reason, spawneryv1alpha1.ReasonCrashLoopBackoff)
	}
	if g.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded: derivePhase maps the condition, and Pending here would "+
			"be indistinguishable from a group that is still starting", g.Status.Phase)
	}
	backingOff := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
	if backingOff == nil || backingOff.Status != metav1.ConditionFalse ||
		backingOff.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("BackingOff = %+v once the group gave up, want False/%s", backingOff,
			spawneryv1alpha1.ReasonCrashLoopBackoff)
	}
	if _, present := f.serverIfPresent("survival-0"); present {
		t.Error("the group rebuilt its ordinal after giving up; the backoff gates persistent creates too")
	}
	if got := f.server("survival-1").Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase of survival-1 = %q, want Ready: it was never failed", got)
	}
}

// spec.groupGeneration is stamped once at creation, so a generation-filtered
// count would freeze after any edit. spec.drain.timeoutSeconds reaches neither the
// pod hash nor spec.replicas.
func TestAPersistentGroupCountsAFailureAfterItsGenerationMoves(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "outpost", 1)
	f.reconcileNamedGroup(t, r, "outpost")
	if _, present := f.serverIfPresent("outpost-0"); !present {
		t.Fatalf("the group did not create its ordinal")
	}

	group := f.persistentGroup(t, "outpost")
	group.Spec.Drain.TimeoutSeconds++
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	// Observes the new generation with no failure yet on either side of it.
	f.reconcileNamedGroup(t, r, "outpost")

	f.failServerNeverReady(t, "outpost-0")
	f.reconcileNamedGroup(t, r, "outpost")

	g := f.persistentGroup(t, "outpost")
	if got := g.Status.ConsecutiveFailures; got != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1: outpost-0 predates the group's current generation "+
			"and its failure must still be counted", got)
	}
}

func (f *fixture) persistentGroup(t *testing.T, name string) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group %s: %v", name, err)
	}
	return group
}

// Without worst == 0 at the end, a spec change that moved nothing would pass.
func TestAPersistentGroupUpdatesOneOrdinalAtATime(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)
	f.reconcileNamedGroup(t, r, "survival")
	f.markReady(t, "survival-0")
	f.markReady(t, "survival-1")

	group := f.persistentGroup(t, "survival")
	group.Spec.Image = "ghcr.io/spawnery/paper:1.21.4-0.2.0"
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("change the group's image: %v", err)
	}

	// The Server reconciler creates a fresh ordinal's pod and moves a draining one
	// forward.
	reconcileGroupOnce := func() {
		f.reconcileNamedGroup(t, r, "survival")
		for _, name := range f.serverNamesOfGroup(t, "survival") {
			f.reconcile(name)
		}
	}
	driveNewPodsReady := func() {
		for _, name := range f.serverNamesOfGroup(t, "survival") {
			srv, ok := f.serverIfPresent(name)
			if !ok || !srv.DeletionTimestamp.IsZero() {
				continue // gone, or draining and not this helper's to touch
			}
			pod, ok := f.pod(name)
			if !ok {
				continue // no pod yet; the next reconcile creates one
			}
			f.setPodRunning(name, true)
			uid := string(pod.UID)
			f.agents.Connect(uid, agentRoleServer())
			f.agents.MarkReady(uid)
			if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
				t.Fatalf("ReportPlayers: %v", err)
			}
		}
	}

	worst := 0
	for pass := 0; pass < 40; pass++ {
		reconcileGroupOnce()
		driveNewPodsReady()

		down := 0
		for _, ordinal := range []int32{0, 1} {
			srv, ok := f.serverIfPresent(PersistentServerName("survival", ordinal))
			if !ok || srv.Status.Phase != string(phase.Ready) {
				down++
			}
		}
		if down > worst {
			worst = down
		}
	}
	if worst > 1 {
		t.Fatalf("%d ordinals were down at once; the invariant allows one", worst)
	}
	if worst == 0 {
		t.Fatal("no ordinal ever went down, so this test proves nothing about the update")
	}
}

// The two views carry different messages, or a reversed ordinalBefore would pass
// as well.
func TestTheStorageResizeMessageComesFromTheLowestOrdinal(t *testing.T) {
	views := []ServerView{
		{Name: "survival-1", Ordinal: ptr.To(int32(1)), ResizeError: "ordinal 1's message"},
		{Name: "survival-0", Ordinal: ptr.To(int32(0)), ResizeError: "ordinal 0's message"},
	}
	// Listed highest-first; collectViews makes no ordering promise.
	got := storageResizeCondition(views)
	if got.Status != metav1.ConditionFalse {
		t.Fatalf("Status = %v, want False; two claims carry an error", got.Status)
	}
	if got.Message != "ordinal 0's message" {
		t.Errorf("Message = %q, want ordinal 0's; the lowest ordinal is the tie-break that stays put",
			got.Message)
	}

	nameless := ServerView{Name: "survival-a7kd", ResizeError: "a squatter's message"}
	if got := storageResizeCondition([]ServerView{nameless, views[1]}); got.Message != "ordinal 0's message" {
		t.Errorf("Message = %q, want ordinal 0's; a nil ordinal sorts after a numbered one", got.Message)
	}
	if got := storageResizeCondition([]ServerView{nameless}); got.Message != "a squatter's message" {
		t.Errorf("Message = %q, want the squatter's; it is the only candidate there is", got.Message)
	}
}

// envtest has no resizer, but its API server refuses the grow against
// allowVolumeExpansion: false as a real cluster would. Bound is faked because
// nothing binds a claim, and an unbound claim is refused for a reason of its own.
// The asynchronous ControllerResizeError/NodeResizeError path is not exercised.
func TestAGroupSaysWhenItsStorageClassCannotGrow(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	class := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "unexpandable-" + f.ns},
		Provisioner: "kubernetes.io/no-provisioner",
	}
	class.AllowVolumeExpansion = ptr.To(false)
	if err := f.c.Create(f.ctx, class); err != nil {
		t.Fatalf("create StorageClass: %v", err)
	}

	// storageClassName is immutable once set, so the group is built by hand with it.
	replicas := int32(1)
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: f.network.Name},
			Type:                          spawneryv1alpha1.ServerGroupPersistent,
			Image:                         "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:                    100,
			Replicas:                      &replicas,
			TerminationGracePeriodSeconds: 60,
			FailedRetentionSeconds:        3600,
			Drain:                         &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 60},
			Storage: &spawneryv1alpha1.StorageSpec{
				Size:             resource.MustParse("10Gi"),
				StorageClassName: &class.Name,
			},
		},
	}
	if err := f.c.Create(f.ctx, group); err != nil {
		t.Fatalf("create persistent ServerGroup: %v", err)
	}

	f.reconcileNamedGroup(t, r, "g")
	f.reconcile("g-0")

	claim := f.claim("g-0-data")
	if claim == nil {
		t.Fatal("no claim to grow")
	}
	claim.Status.Phase = corev1.ClaimBound
	claim.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}
	if err := f.c.Status().Update(f.ctx, claim); err != nil {
		t.Fatalf("fake-bind the claim: %v", err)
	}

	g := f.persistentGroup(t, "g")
	g.Spec.Storage.Size = resource.MustParse("20Gi")
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("grow the group: %v", err)
	}

	f.reconcile("g-0")
	f.reconcileNamedGroup(t, r, "g")

	got := f.persistentGroup(t, "g")
	cond := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionStorageResize)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected StorageResize=False, got %+v", cond)
	}
	// growClaim cannot tell this rejection from an unbound claim's, so the message
	// only points at the field to check first.
	if !strings.Contains(cond.Message, "allowVolumeExpansion") {
		t.Fatalf("the message does not point at the field to check: %q", cond.Message)
	}

	// Without this, folding the refusal into Degraded would pass as well.
	degraded := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if degraded != nil && degraded.Status == metav1.ConditionTrue {
		t.Fatal("a storage class that cannot expand is not a degraded group")
	}
}

// Giving up is terminal and needs a retry, while the Network's trouble is
// transient and already on Accepted, so Degraded names the give-up. The Network is
// deleted rather than re-pointed: editing the group would clear the streak.
func TestAGroupThatGaveUpSaysSoEvenWhileItsNetworkIsDead(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// The count is rounds, not corpses. failServerNeverReady, because a server that
	// reached Ready would end the streak.
	f.setMinReplicas(t, 1)
	for round := int32(0); round < backoffGiveUpAt; round++ {
		f.reconcileGroup(t, r)
		for _, name := range f.serverNamesOfGroup(t, "lobby") {
			if f.server(name).Status.Phase != string(phase.Failed) {
				f.failServerNeverReady(t, name)
			}
		}
		// Past the window this round earned, or the next pass creates nothing.
		f.clock.Advance(10 * time.Minute)
	}
	f.reconcileGroup(t, r)
	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got < backoffGiveUpAt {
		t.Fatalf("consecutiveFailures = %d after %d rounds, want at least %d; this test "+
			"needs a group that has actually given up", got, backoffGiveUpAt, backoffGiveUpAt)
	}

	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete the Network: %v", err)
	}

	f.reconcileGroup(t, r)

	got := f.reloadGroup(t)
	degraded := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Fatalf("Degraded = %+v, want reason %s. The give-up outlives the Network problem "+
			"and needs a retry; the Network's own trouble is on Accepted",
			degraded, spawneryv1alpha1.ReasonCrashLoopBackoff)
	}
	if !strings.Contains(degraded.Message, spawneryv1alpha1.AnnotationRetry) {
		t.Errorf("message = %q, want it to name the remedy the operator actually has to apply",
			degraded.Message)
	}
	accepted := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse {
		t.Errorf("Accepted = %+v, want False: moving the give-up first must not hide the "+
			"Network, only stop repeating it", accepted)
	}
}

// spec.replicas: 0 is how an operator parks a persistent group and keeps its claims.
func TestAParkedPersistentGroupIsReadyRatherThanPending(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "parked", 0)

	f.reconcileNamedGroup(t, r, "parked")

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "parked", Namespace: f.ns}, got); err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if got.Status.ReadyReplicas != 0 {
		t.Fatalf("readyReplicas = %d, want 0; this test is about a group that has been "+
			"asked for nothing", got.Status.ReadyReplicas)
	}
	if got.Status.Phase != string(phase.Ready) {
		t.Errorf("phase = %q, want %q. A group asked for nothing and running nothing has "+
			"exactly what it was asked for; Pending says something is still coming",
			got.Status.Phase, phase.Ready)
	}
}

func TestAGroupShortOfItsTargetIsStillPending(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "wanting", 2)

	f.reconcileNamedGroup(t, r, "wanting")

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "wanting", Namespace: f.ns}, got); err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if got.Status.ReadyReplicas != 0 {
		t.Fatalf("readyReplicas = %d, want 0 for this test's premise", got.Status.ReadyReplicas)
	}
	if got.Status.Phase != string(phase.Pending) {
		t.Errorf("phase = %q, want %q: two servers were asked for and none is ready",
			got.Status.Phase, phase.Pending)
	}
}

// DecidePersistentSize reads spec.ordinal, not names, so a squatter keeps the
// ordinal missing and the create collides on every pass.
func TestASquatterOnAnOrdinalNameSaysSoOnTheGroup(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	squatter := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "held-0", Namespace: f.ns},
		// No spec.ordinal: that absence is what makes it a squatter.
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "held"},
		},
	}
	if err := f.c.Create(f.ctx, squatter); err != nil {
		t.Fatalf("create the squatter: %v", err)
	}
	f.createPersistentGroup(t, "held", 1)

	f.reconcileNamedGroup(t, r, "held")

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "held", Namespace: f.ns}, got); err != nil {
		t.Fatalf("get the group: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionOrdinalBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("OrdinalBlocked = %+v, want True. The group will retry this ordinal every "+
			"five seconds until somebody removes that object, and used to say nothing", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonOrdinalNameTaken {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonOrdinalNameTaken)
	}
	for _, want := range []string{"held-0", "spec.ordinal"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message = %q, want it to contain %q", cond.Message, want)
		}
	}
}

// Set from each pass's own finding, not latched.
func TestAGroupWithNoSquatterSaysTheOrdinalsAreAvailable(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "clear", 1)

	f.reconcileNamedGroup(t, r, "clear")

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "clear", Namespace: f.ns}, got); err != nil {
		t.Fatalf("get the group: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionOrdinalBlocked)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("OrdinalBlocked = %+v on a group whose ordinals are free, want False", cond)
	}
}

// A cache one generation behind resolves itself and must not be reported.
// Called directly: with the fixture's uncached client a reconcile never gets here.
func TestACacheOneGenerationBehindIsNotReportedAsASquatter(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createPersistentGroup(t, "lagging", 1)

	// This reconciler's own object, as it would look once the cache catches up.
	ordinal := int32(0)
	mine := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lagging-0", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lagging"},
			Ordinal:  &ordinal,
		},
	}
	if err := f.c.Create(f.ctx, mine); err != nil {
		t.Fatalf("create the server: %v", err)
	}

	if err := r.reportSquatter(f.ctx, group, "lagging-0", ordinal); err != nil {
		t.Fatalf("reportSquatter: %v", err)
	}
	if c := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionOrdinalBlocked); c != nil {
		t.Errorf("OrdinalBlocked = %+v for an object carrying the very ordinal this pass "+
			"wanted, want no condition at all: that is this reconciler's own creation seen "+
			"through a cache one generation behind, and it resolves itself", c)
	}

	// A different ordinal is a squatter: the value decides, not the field's presence.
	other := int32(7)
	mine.Spec.Ordinal = &other
	if err := f.c.Update(f.ctx, mine); err != nil {
		t.Fatalf("move the ordinal: %v", err)
	}
	if err := r.reportSquatter(f.ctx, group, "lagging-0", ordinal); err != nil {
		t.Fatalf("reportSquatter: %v", err)
	}
	c := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionOrdinalBlocked)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("OrdinalBlocked = %+v for an object holding the name under a different "+
			"ordinal, want True", c)
	}
	if !strings.Contains(c.Message, "spec.ordinal 7") {
		t.Errorf("message = %q, want it to name the ordinal the squatter carries", c.Message)
	}
}

func TestProgressingSaysWhetherTheGroupHasArrived(t *testing.T) {
	const gen = "current"
	view := func(p phase.Phase, hash string) ServerView {
		return ServerView{Phase: p, PodHash: hash}
	}
	for _, tc := range []struct {
		name   string
		views  []ServerView
		status metav1.ConditionStatus
		reason string
		says   string
	}{
		{
			"nothing at all", nil,
			metav1.ConditionFalse, spawneryv1alpha1.ReasonAtDesiredState, "",
		},
		{
			"every server ready and current",
			[]ServerView{view(phase.Ready, gen), view(phase.Ready, gen)},
			metav1.ConditionFalse, spawneryv1alpha1.ReasonAtDesiredState, "",
		},
		{
			"one up, one still coming",
			[]ServerView{view(phase.Ready, gen), view(phase.Starting, gen)},
			metav1.ConditionTrue, spawneryv1alpha1.ReasonServersStarting, "1 server(s) of the group's current spec",
		},
		{
			"a pending server counts as coming",
			[]ServerView{view(phase.Ready, gen), view(phase.Pending, gen)},
			metav1.ConditionTrue, spawneryv1alpha1.ReasonServersStarting, "1 server(s) of the group's current spec",
		},
		{
			"the new spec is up but the old one is still here",
			[]ServerView{view(phase.Ready, gen), view(phase.Ready, "old")},
			metav1.ConditionTrue, spawneryv1alpha1.ReasonReplacingServers, "earlier spec",
		},
		{
			"both waits at once say both",
			[]ServerView{view(phase.Starting, gen), view(phase.Ready, "old")},
			metav1.ConditionTrue, spawneryv1alpha1.ReasonServersStarting, "still being replaced",
		},
		{
			// Being shed on purpose. A scale-down has arrived, it is tidying.
			"a draining server of the current spec",
			[]ServerView{view(phase.Ready, gen), view(phase.Draining, gen)},
			metav1.ConditionFalse, spawneryv1alpha1.ReasonAtDesiredState, "",
		},
		{
			// Degraded is what says this, and its replacement will be Pending
			// on the next pass, which this counts then.
			"a failed server of the current spec",
			[]ServerView{view(phase.Ready, gen), view(phase.Failed, gen)},
			metav1.ConditionFalse, spawneryv1alpha1.ReasonAtDesiredState, "",
		},
		{
			// Kept for diagnosis, not waiting to be replaced: its group
			// already replaced it when it failed.
			"a failed server of an earlier spec",
			[]ServerView{view(phase.Ready, gen), view(phase.Failed, "old")},
			metav1.ConditionFalse, spawneryv1alpha1.ReasonAtDesiredState, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &spawneryv1alpha1.ServerGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "lobby"},
			}
			reportProgressing(group, tc.views, gen, ChangeoverWait{}, FloorReport{})
			got := meta.FindStatusCondition(group.Status.Conditions,
				spawneryv1alpha1.ConditionProgressing)
			if got == nil {
				t.Fatal("no Progressing condition was set at all")
			}
			if got.Status != tc.status || got.Reason != tc.reason {
				t.Errorf("Progressing = %s/%s, want %s/%s: %s",
					got.Status, got.Reason, tc.status, tc.reason, got.Message)
			}
			if tc.says != "" && !strings.Contains(got.Message, tc.says) {
				t.Errorf("message = %q, want it to contain %q", got.Message, tc.says)
			}
		})
	}
}

// DesiredReplicas() is only the floor, so a group sized above it reads Ready while
// a server is still starting; only Progressing can say so.
func TestAGroupAboveItsFloorIsReadyAndStillProgressing(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)
	first := f.listServers(t)
	if len(first) != 1 {
		t.Fatalf("got %d servers, want minReplicas = 1", len(first))
	}
	// 70 players leave fewer than the 40 spare slots free, so the next pass orders another.
	f.markReadyWithPlayers(t, first[0].Name, 70)

	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != 2 {
		t.Fatalf("got %d servers, want a second one ordered for the spare slots", got)
	}

	// Views are collected before the same pass's creates, so the new server shows up
	// on the next pass.
	f.reconcileGroup(t, r)

	group := f.reloadGroup(t)
	if got := group.Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase = %q, want %q. This is not the bug — the group is serving, and the "+
			"phase saying so is the useful thing for a printed column", got, phase.Ready)
	}
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil || cond.Status != metav1.ConditionTrue ||
		cond.Reason != spawneryv1alpha1.ReasonServersStarting {
		t.Errorf("Progressing = %+v, want True/%s. One ready server out of two decided upon "+
			"is serving and not arrived, and nothing on this object said the second half",
			cond, spawneryv1alpha1.ReasonServersStarting)
	}
}

// A selector matching no pods would report a group mid-rotation as in sync.
func TestAServerGroupReportsItsOwnRotationState(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	net := f.getNetwork(t, f.network.Name)
	net.Status.ForwardingSecretHash = "aaaaaaaaaaaaaaaa"
	if err := f.c.Status().Update(f.ctx, net); err != nil {
		t.Fatalf("publish a forwarding digest on the network: %v", err)
	}

	labels := podspec.ServerLabels(f.network.Name, "lobby", "lobby-x7k2")
	labels[podspec.LabelForwardingHash] = "0000000000000000"
	if err := f.c.Create(f.ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-x7k2", Namespace: f.ns, Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "paper", Image: "img:1"}}},
	}); err != nil {
		t.Fatalf("create a stale-stamped pod: %v", err)
	}

	f.reconcileGroup(t, r)

	group := f.reloadGroup(t)
	got := meta.FindStatusCondition(group.Status.Conditions,
		spawneryv1alpha1.ConditionForwardingSecretRotationPending)
	if got == nil {
		t.Fatal("the group carries no ForwardingSecretRotationPending condition at all")
	}
	if got.Status != metav1.ConditionTrue || got.Reason != spawneryv1alpha1.ReasonRotationPending {
		t.Errorf("condition = %s/%s, want True/%s — this group holds a pod on the previous "+
			"secret, and until now the only place that said so was a message on the Network",
			got.Status, got.Reason, spawneryv1alpha1.ReasonRotationPending)
	}

	pod := &corev1.Pod{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: f.ns}, pod); err != nil {
		t.Fatalf("get the pod: %v", err)
	}
	pod.Labels[podspec.LabelForwardingHash] = "aaaaaaaaaaaaaaaa"
	if err := f.c.Update(f.ctx, pod); err != nil {
		t.Fatalf("restamp the pod: %v", err)
	}

	f.reconcileGroup(t, r)
	got = meta.FindStatusCondition(f.reloadGroup(t).Status.Conditions,
		spawneryv1alpha1.ConditionForwardingSecretRotationPending)
	if got == nil || got.Status != metav1.ConditionFalse ||
		got.Reason != spawneryv1alpha1.ReasonForwardingSecretInSync {
		t.Errorf("condition = %+v once the pod carries the current stamp, want False/%s",
			got, spawneryv1alpha1.ReasonForwardingSecretInSync)
	}
}

func (f *fixture) taintNode(t *testing.T, name, key string) {
	t.Helper()
	node := &corev1.Node{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name}, node); err != nil {
		t.Fatalf("get node %s: %v", name, err)
	}
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key: key, Effect: corev1.TaintEffectNoSchedule,
	})
	if err := f.c.Update(f.ctx, node); err != nil {
		t.Fatalf("taint node %s: %v", name, err)
	}
}

// cluster-autoscaler taints without cordoning unless
// --cordon-node-before-terminating is set, so a scale-in arrives as a taint.
func TestATaintedNodeCondemnsOnlyWhenItsKeyIsConfigured(t *testing.T) {
	const key = "ToBeDeletedByClusterAutoscaler"

	for _, tc := range []struct {
		name       string
		taintKeys  []string
		wantGoing  bool
		wantReason string
	}{
		{
			"the key the operator was told to watch",
			[]string{key},
			true,
			"a taint whose key is configured is a node on its way out",
		},
		{
			"no keys configured, which is the default",
			nil,
			false,
			"an empty -drain-taint list must see nothing: reacting to another " +
				"project's taint key by default would couple this operator to a " +
				"vocabulary that project is free to rename",
		},
		{
			"a key that is not the one taints carry",
			[]string{"example.com/going-away"},
			false,
			"only the configured keys count, or the list would be decoration",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := groupReconciler(f)
			r.DrainTaintKeys = tc.taintKeys

			f.reconcileGroup(t, r)
			servers := f.listServers(t)
			if len(servers) != 1 {
				t.Fatalf("servers = %d, want 1", len(servers))
			}
			f.markReady(t, servers[0].Name)

			node := f.ensureNode(t, "node-tainted-"+f.ns, false)
			reloaded := f.server(servers[0].Name)
			pod, ok := f.pod(reloaded.Status.PodName)
			if !ok {
				t.Fatalf("pod of %s not found", servers[0].Name)
			}
			f.bindPodToNode(t, pod, node.Name)
			f.taintNode(t, node.Name, key)

			f.reconcileGroup(t, r)

			going := !f.server(servers[0].Name).DeletionTimestamp.IsZero()
			if going != tc.wantGoing {
				t.Errorf("server condemned = %v, want %v: %s", going, tc.wantGoing, tc.wantReason)
			}
		})
	}
}

// A group that quietly stopped shrinking would look like one with nothing to shrink.
func TestADuplicatedOrdinalReachesTheGroupsConditions(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)
	f.reconcileNamedGroup(t, r, "survival")

	// A second server carrying ordinal 1 under another name, as after a restore; its
	// labels make it a member.
	copied := f.server("survival-1").DeepCopy()
	dup := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name: "survival-restored", Namespace: f.ns, Labels: copied.Labels,
		},
		Spec: copied.Spec,
	}
	if err := f.c.Create(f.ctx, dup); err != nil {
		t.Fatalf("create the duplicate: %v", err)
	}

	f.reconcileNamedGroup(t, r, "survival")

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "survival", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	blocked := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionOrdinalBlocked)
	if blocked == nil || blocked.Status != metav1.ConditionTrue {
		t.Fatalf("OrdinalBlocked = %+v, want True", blocked)
	}
	if blocked.Reason != spawneryv1alpha1.ReasonOrdinalDuplicated {
		t.Errorf("reason = %q, want %q", blocked.Reason, spawneryv1alpha1.ReasonOrdinalDuplicated)
	}
	// Both names, because the remedy is a choice between them.
	for _, name := range []string{"survival-1", "survival-restored"} {
		if !strings.Contains(blocked.Message, name) {
			t.Errorf("message %q does not name %s", blocked.Message, name)
		}
	}

	// Nothing is surplus at replicas 2, but the stale and resize paths would nominate
	// from the pair.
	names := f.serverNamesOfGroup(t, "survival")
	if len(names) != 3 {
		t.Errorf("servers = %v, want all three left standing", names)
	}
}

func TestTheOrdinalConditionClearsWhenTheDuplicateGoes(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)
	f.reconcileNamedGroup(t, r, "survival")

	copied := f.server("survival-1").DeepCopy()
	dup := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name: "survival-restored", Namespace: f.ns, Labels: copied.Labels,
		},
		Spec: copied.Spec,
	}
	if err := f.c.Create(f.ctx, dup); err != nil {
		t.Fatalf("create the duplicate: %v", err)
	}
	f.reconcileNamedGroup(t, r, "survival")
	if err := f.c.Delete(f.ctx, dup); err != nil {
		t.Fatalf("delete the duplicate: %v", err)
	}

	f.reconcileNamedGroup(t, r, "survival")

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "survival", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	blocked := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionOrdinalBlocked)
	if blocked == nil || blocked.Status != metav1.ConditionFalse {
		t.Errorf("OrdinalBlocked = %+v, want False once the duplicate is gone", blocked)
	}
}

// spec.retire survives the failure, so the retiree holds a maxUnavailable slot
// for its whole retention and the changeover stops.
func TestAFailedRetireeIsNamedOnProgressing(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Generation: 2},
		Spec:       spawneryv1alpha1.ServerGroupSpec{FailedRetentionSeconds: 3600},
	}
	views := []ServerView{
		{Name: "lobby-new", PodHash: "current", Phase: phase.Ready},
		// The stuck one: retiring, and dead.
		{Name: "lobby-old", PodHash: "old", Phase: phase.Failed, Retire: true},
	}

	reportProgressing(group, views, "current", ChangeoverWait{}, FloorReport{})

	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Progressing = %+v, want True", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonRetireeStuck {
		t.Fatalf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonRetireeStuck)
	}
	// The remedy is per server, so the message names it.
	for _, want := range []string{"lobby-old", "spec.retire", "3600", "Deleting it"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q does not mention %q", cond.Message, want)
		}
	}
}

func TestAnOrdinaryRetireeIsNotReportedAsStuck(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Generation: 2},
		Spec:       spawneryv1alpha1.ServerGroupSpec{FailedRetentionSeconds: 3600},
	}
	for _, p := range []phase.Phase{phase.Retiring, phase.Draining, phase.Ready} {
		t.Run(string(p), func(t *testing.T) {
			group.Status.Conditions = nil
			reportProgressing(group, []ServerView{
				{Name: "lobby-new", PodHash: "current", Phase: phase.Ready},
				{Name: "lobby-old", PodHash: "old", Phase: p, Retire: true},
			}, "current", ChangeoverWait{}, FloorReport{})
			cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
			if cond != nil && cond.Reason == spawneryv1alpha1.ReasonRetireeStuck {
				t.Errorf("a %s retiree was reported as stuck: %q", p, cond.Message)
			}
		})
	}
}

func (f *fixture) liveServers(t *testing.T) int {
	t.Helper()
	n := 0
	for _, s := range f.listServers(t) {
		if s.Status.Phase != string(phase.Failed) && s.DeletionTimestamp == nil {
			n++
		}
	}
	return n
}

// A Failed server does not count toward size and creates wait for the window, so
// at minReplicas 1 the group has no live server until the window closes.
func TestTheGroupHasNoLiveServerWhileItBacksOffAndRebuildsAfter(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)

	name := f.oneServerName(t)
	if got := f.liveServers(t); got != 1 {
		t.Fatalf("%d live servers before the failure, want 1", got)
	}

	f.failServer(t, name)
	f.reconcileGroup(t, r)

	// The corpse is kept, so status.replicas does not read zero; only this count does.
	if got := f.liveServers(t); got != 0 {
		t.Fatalf("%d live servers inside the backoff window, want 0: the window is the "+
			"whole reason a replacement is not ordered yet", got)
	}
	if got := len(f.listServers(t)); got != 1 {
		t.Errorf("%d servers exist, want the 1 corpse: a replacement was ordered inside "+
			"the window", got)
	}
	if c := meta.FindStatusCondition(f.reloadGroup(t).Status.Conditions,
		spawneryv1alpha1.ConditionBackingOff); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("BackingOff = %+v, want True: without it the zero above would be "+
			"unexplained on the object itself, which is what made it worth recording", c)
	}

	f.clock.Advance(backoffBase + time.Second)
	f.reconcileGroup(t, r)

	if got := f.liveServers(t); got != 1 {
		t.Fatalf("%d live servers after the window expired, want 1: the count recovers or "+
			"the observation was not the backoff after all", got)
	}
}

// staleSpec adopts a hashless view on every pass, so an unstamped server would
// never be stale.
func TestAdoptStampsEphemeralServers(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	srv := f.createServer("lobby-old")
	// The shape of a server from before the field had a reader here.
	srv.Spec.PodHash = ""
	if err := f.c.Update(f.ctx, srv); err != nil {
		t.Fatalf("clear podHash: %v", err)
	}

	f.reconcileGroup(t, r)

	if got := f.server("lobby-old").Spec.PodHash; got == "" {
		t.Fatal("an ephemeral server was left without a render hash: it can never be stale")
	}
}

// Reads spec.retire, not the phase: the nomination lands a pass before the
// transition.
func requireNoneRetiring(t *testing.T, f *fixture, when string) {
	t.Helper()
	for _, s := range f.listServers(t) {
		if s.Spec.Retire {
			t.Fatalf("%s: server %s was asked to retire and nothing should have", when, s.Name)
		}
	}
}

// Driven through real reconciles: the unit tests supply PodHash themselves and
// cannot catch a wrong value at the call site.
func TestCapacityEditDoesNotRollAnEphemeralGroup(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// Created directly; the floor is 1.
	a, b := "lobby-a", "lobby-b"
	f.createServer(a)
	f.markReadyWithPlayers(t, a, 60)
	f.createServer(b)
	f.markReadyWithPlayers(t, b, 60)

	reconcilePass(t, f, r)
	requireNoneRetiring(t, f, "before any edit")

	// A capacity edit moves metadata.generation but not the rendered pod; with two
	// servers up, a retirement is the only thing it could produce.
	f.setMinReplicas(t, 2)
	reconcilePass(t, f, r)
	requireNoneRetiring(t, f, "after raising minReplicas")

	// 80 slots are free across the two servers, so 60 keeps the spare-slot rule satisfied.
	if err := f.c.Get(f.ctx,
		types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.SpareSlots = 60
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("retune spareSlots: %v", err)
	}
	reconcilePass(t, f, r)
	requireNoneRetiring(t, f, "after retuning spareSlots")

	// An image edit changes the rendered pod: one replacement, then one retirement.
	f.bumpPodSpec(t)
	reconcilePass(t, f, r)

	fresh := f.serversOfGeneration(t, f.group.Generation)
	if len(fresh) != 1 {
		t.Fatalf("the image edit created %d replacements, want exactly 1", len(fresh))
	}
	f.markReady(t, fresh[0].Name)
	reconcilePass(t, f, r)
	reconcilePass(t, f, r)

	if n := f.retiringCount(t); n != 1 {
		t.Fatalf("%d servers retiring after an image edit, want exactly 1 (maxUnavailable)", n)
	}
}

// An unexplained floor is the likeliest failure of a boost.
func TestAGroupSaysHowMuchOfItsFloorIsABoost(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	expires := metav1.NewTime(f.clock.now.Add(time.Hour))
	if err := f.c.Create(f.ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-boost", Namespace: f.ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			Replicas:  2,
			ExpiresAt: &expires,
		},
	}); err != nil {
		t.Fatalf("create the boost: %v", err)
	}

	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx,
		types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if got := group.Status.BoostedReplicas; got != 2 {
		t.Errorf("status.boostedReplicas = %d, want 2", got)
	}
}

// Zero and present, so "no boost" is not mistaken for an operator too old to say.
func TestAGroupWithNoBoostReportsZero(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx,
		types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if got := group.Status.BoostedReplicas; got != 0 {
		t.Errorf("status.boostedReplicas = %d, want 0", got)
	}
}

// Through a real reconcile: the rule can be right while the value never reaches it.
func TestABoostActuallyCreatesAServer(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	expires := metav1.NewTime(f.clock.now.Add(time.Hour))
	if err := f.c.Create(f.ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-boost", Namespace: f.ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			Replicas:  2,
			ExpiresAt: &expires,
		},
	}); err != nil {
		t.Fatalf("create the boost: %v", err)
	}

	f.reconcileGroup(t, r)

	if got := len(f.listServers(t)); got != 3 {
		t.Fatalf("got %d servers, want 3: a floor of one plus a boost of two", got)
	}
}

func (f *fixture) createPin(t *testing.T, name string, replicas int32, owner *metav1.OwnerReference) {
	t.Helper()
	expires := metav1.NewTime(f.clock.now.Add(time.Hour))
	b := &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			Mode:      spawneryv1alpha1.ScaleBoostExact,
			Replicas:  replicas,
			ExpiresAt: &expires,
		},
	}
	if owner != nil {
		b.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	if err := f.c.Create(f.ctx, b); err != nil {
		t.Fatalf("create the pin: %v", err)
	}
}

func TestAPinOfZeroLeavesTheGroupEmptyAndSaysSo(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPin(t, "lobby-off", 0, nil)

	f.reconcileGroup(t, r)

	if got := len(f.listServers(t)); got != 0 {
		t.Fatalf("got %d servers, want 0 under a pin of 0", got)
	}
	group := f.serverGroup(t, f.group.Name)
	if group.Status.PinnedReplicas == nil || *group.Status.PinnedReplicas != 0 {
		t.Errorf("status.pinnedReplicas = %v, want a present 0", group.Status.PinnedReplicas)
	}
	if group.Status.PinnedUntil == nil {
		t.Error("status.pinnedUntil is empty for a pin that ends")
	}
}

func TestAPinBuildsExactlyItsNumber(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPin(t, "lobby-four", 4, nil)

	f.reconcileGroup(t, r)

	if got := len(f.listServers(t)); got != 4 {
		t.Fatalf("got %d servers, want the pinned 4", got)
	}
}

// envtest runs no garbage collector, so the stale pin stays as it would for a while in a cluster.
func TestAPredecessorsPinDoesNotHoldTheGroup(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPin(t, "lobby-stale", 0, &metav1.OwnerReference{
		APIVersion: spawneryv1alpha1.GroupVersion.String(), Kind: "ServerGroup",
		Name: f.group.Name, UID: "00000000-0000-0000-0000-00000000dead",
	})

	f.reconcileGroup(t, r)

	if got := len(f.listServers(t)); got != 1 {
		t.Fatalf("got %d servers, want the floor of 1: the pin belongs to a group that is gone", got)
	}
	if p := f.serverGroup(t, f.group.Name).Status.PinnedReplicas; p != nil {
		t.Errorf("status.pinnedReplicas = %d, want absent", *p)
	}
}

func (f *fixture) pluginPVC(t *testing.T, name string, modes ...corev1.PersistentVolumeAccessMode) {
	t.Helper()
	if err := f.c.Create(f.ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: modes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}); err != nil {
		t.Fatalf("create claim %s: %v", name, err)
	}
}

func (f *fixture) setExtraPlugins(t *testing.T, claim string) {
	t.Helper()
	f.group.Spec.ExtraPlugins = &spawneryv1alpha1.ExtraPlugins{ClaimName: claim}
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
}

func TestAGroupWithAReadWriteOnceClaimIsRefusedAndCreatesNothing(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	r.AllowPluginVolumes = true
	f.pluginPVC(t, "plugins", corev1.ReadWriteOnce)
	f.setExtraPlugins(t, "plugins")

	f.reconcileGroup(t, r)

	group := f.reloadGroup(t)
	accepted := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason != spawneryv1alpha1.ReasonPluginVolumeUnusable {
		t.Fatalf("Accepted = %+v, want False/%s", accepted, spawneryv1alpha1.ReasonPluginVolumeUnusable)
	}
	// Creating the servers anyway would leave every pod Pending on a volume that
	// never attaches.
	if servers := f.listServers(t); len(servers) != 0 {
		t.Errorf("the group created %d servers despite an unusable plugin claim", len(servers))
	}
}

func TestAGroupWithAReadWriteManyClaimIsAcceptedAndCreatesItsFloor(t *testing.T) {
	// Without this, a check that refused everything would pass the test above.
	f := newFixture(t)
	r := groupReconciler(f)
	r.AllowPluginVolumes = true
	f.pluginPVC(t, "plugins", corev1.ReadWriteMany)
	f.setExtraPlugins(t, "plugins")

	f.reconcileGroup(t, r)

	group := f.reloadGroup(t)
	if accepted := meta.FindStatusCondition(group.Status.Conditions,
		spawneryv1alpha1.ConditionAccepted); accepted == nil ||
		accepted.Status != metav1.ConditionTrue {
		t.Fatalf("Accepted = %+v, want True for a ReadWriteMany claim", accepted)
	}
	if servers := f.listServers(t); len(servers) != 1 {
		t.Errorf("got %d servers, want the group's floor", len(servers))
	}
}

func TestAGroupNamingAClaimOnADisabledInstallationIsRefused(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	// AllowPluginVolumes is false, which is the default an operator gets.
	f.pluginPVC(t, "plugins", corev1.ReadWriteMany)
	f.setExtraPlugins(t, "plugins")

	f.reconcileGroup(t, r)

	group := f.reloadGroup(t)
	accepted := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Reason != spawneryv1alpha1.ReasonPluginVolumesDisabled {
		t.Fatalf("Accepted = %+v, want False/%s", accepted, spawneryv1alpha1.ReasonPluginVolumesDisabled)
	}
	if !strings.Contains(accepted.Message, "--allow-plugin-volumes") {
		t.Errorf("message = %q, want it to name the flag", accepted.Message)
	}
}

func TestTheRefusalIsAnnouncedOnceAndNotEveryResync(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	r.AllowPluginVolumes = true
	f.pluginPVC(t, "plugins", corev1.ReadWriteOnce)
	f.setExtraPlugins(t, "plugins")

	f.reconcileGroup(t, r)
	f.reconcileGroup(t, r)
	f.reconcileGroup(t, r)

	rec, ok := r.Recorder.(*nonBlockingRecorder)
	if !ok {
		t.Fatalf("recorder is %T, want the test recorder", r.Recorder)
	}
	if got := scalingEvents(rec, spawneryv1alpha1.ReasonPluginVolumeUnusable); got != 1 {
		t.Errorf("recorded %d refusal events across three reconciles, want one on the transition", got)
	}
}

func TestProgressingNamesTheFloor(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{ObjectMeta: metav1.ObjectMeta{Name: "lobby", Generation: 2}}
	views := []ServerView{
		{Name: "lobby-new", PodHash: "current", Phase: phase.Ready},
		{Name: "lobby-old", PodHash: "old", Phase: phase.Ready},
	}
	reportProgressing(group, views, "current", ChangeoverWait{}, FloorReport{Joinable: 2, Min: 2})
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil || cond.Reason != spawneryv1alpha1.ReasonWaitingForMinAvailable {
		t.Fatalf("Progressing = %+v, want reason %s", cond, spawneryv1alpha1.ReasonWaitingForMinAvailable)
	}
	if !strings.Contains(cond.Message, "minAvailable 2") {
		t.Errorf("message %q does not name the floor", cond.Message)
	}

	group.Status.Conditions = nil
	views = append(views, ServerView{Name: "lobby-surge", PodHash: "current", Phase: phase.Starting})
	reportProgressing(group, views, "current", ChangeoverWait{}, FloorReport{Joinable: 2, Min: 2})
	cond = meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil || cond.Reason != spawneryv1alpha1.ReasonServersStarting {
		t.Errorf("Progressing = %+v, want %s while the extra server starts", cond, spawneryv1alpha1.ReasonServersStarting)
	}
}

func TestProgressingDoesNotCountAHeldServer(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{ObjectMeta: metav1.ObjectMeta{Name: "lobby", Generation: 2}}
	reportProgressing(group, []ServerView{
		{Name: "lobby-new", PodHash: "current", Phase: phase.Ready},
		{Name: "lobby-old", PodHash: "old", Phase: phase.Ready, Hold: true},
	}, "current", ChangeoverWait{}, FloorReport{})
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil || cond.Reason != spawneryv1alpha1.ReasonAtDesiredState {
		t.Errorf("Progressing = %+v, want AtDesiredState: a held server is not being replaced", cond)
	}
}

func TestCollectViewsResolvesThePlayableFigure(t *testing.T) {
	f := newFixture(t)
	f.group.Spec.PlayableSlots = ptr.To[int32](20)
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.createServer("lobby-play")
	f.reconcile("lobby-play")
	pod, ok := f.pod("lobby-play")
	if !ok {
		t.Fatal("no pod for lobby-play")
	}
	f.agents.Connect(string(pod.UID), agentRoleServer())
	if err := f.agents.ReportPlayers(string(pod.UID), 14, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	r := groupReconciler(f)

	views, _, err := r.collectViews(f.ctx, f.group)
	if err != nil || len(views) != 1 {
		t.Fatalf("collectViews = %v, %v; want one view", views, err)
	}
	if views[0].Playable != 20 {
		t.Errorf("Playable = %d, want the spec's 20", views[0].Playable)
	}

	if err := f.agents.ReportPlayableSlots(string(pod.UID), 12); err != nil {
		t.Fatalf("ReportPlayableSlots: %v", err)
	}
	views, _, _ = r.collectViews(f.ctx, f.group)
	if views[0].Playable != 12 {
		t.Errorf("Playable = %d, want the plugin's 12", views[0].Playable)
	}
}
