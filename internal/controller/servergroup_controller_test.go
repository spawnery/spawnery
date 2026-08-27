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
		Client:       f.rc,
		Scheme:       f.reconc.Scheme,
		Recorder:     newRecorder(),
		Agents:       f.agents,
		Clock:        f.clock.Now,
		Expectations: newExpectations(f.clock.Now),
	}
}

// scalingEvents drains the recorder and counts the events carrying a given
// reason. It matched the reason as a substring of the whole rendered line until
// milestone 6e's final review, which is a match a mutated, longer reason walks
// straight through -- see eventHasReason, which is what it compares with now.
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

// groupPDB re-reads the fixture's ServerGroup's PodDisruptionBudget.
func (f *fixture) groupPDB(t *testing.T) *policyv1.PodDisruptionBudget {
	t.Helper()
	pdb := &policyv1.PodDisruptionBudget{}
	key := types.NamespacedName{Name: podspec.GroupPDBName(f.group.Name, podspec.RoleServer), Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, pdb); err != nil {
		t.Fatalf("get PDB: %v", err)
	}
	return pdb
}

// assertBudgetSelectsExactlyWhatItCounts is the pairing neither group kind
// asserted before: that the pods a PodDisruptionBudget's selector actually
// matches are the pods its minAvailable was counted from.
//
// Both halves were already covered separately -- minAvailable against a
// number the test computed, and the occupied label against the pods carrying
// it -- and neither catches a selector that matches the wrong population.
// That is exactly the shape of the milestone's own Critical: the ServerGroup
// budget selected on {managed-by, group, occupied} with no role term, so in a
// namespace holding a same-named ProxyGroup it matched occupied proxies while
// counting only occupied servers, and each occupied proxy it picked up bought
// the eviction API another disruption to spend on a server pod full of players.
//
// It reads the selector off the object rather than rebuilding it, so it is a
// statement about what Kubernetes will match and not about what the test
// thinks the controller wrote.
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

	// 60 passes would only reach 295 seconds of emptiness at the last check —
	// five short of the CRD's 300-second stabilization default — so the idle
	// server would never clear DecideSize's stabilization gate and the loop
	// would end with both servers still standing. The extra passes cover that
	// window and the few more resyncs the drain itself needs once the removal
	// is ordered: Draining, then Terminating, then the object actually gone.
	for i := 0; i < 65; i++ {
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
		f.clock.Advance(ResyncInterval)
	}

	// setMinReplicas performs a real spec update, so the two servers already
	// up go stale and the cold start orders one replacement. This fixture
	// never reports for it, so it runs out its startup deadline and fails,
	// and the corpse is kept: Failed servers are retained for
	// failedRetentionSeconds (an hour by default), and the 65 passes above,
	// at one resync each, do not run long enough to clear it.
	//
	// The group then builds one more replacement before the loop ends, and
	// that second attempt is the backoff working rather than a leak. One
	// failure buys ten seconds; the loop advances five seconds a pass, so
	// the window closes two passes after the deadline expires and the cold
	// start is permitted again. Nothing ever reports for that one either, so
	// it is still starting when the loop stops — live, but not Ready.
	//
	// Until milestone 4d this settled on one live server, because 4b's
	// cold-start suppression held every further attempt off for the whole
	// retention hour after a single failure. Retiring that suppression is
	// what makes the second attempt appear, and the wait that replaces it is
	// ten seconds rather than an hour. Both live servers and the corpse are
	// asserted rather than filtered away, so a missing or a doubled one
	// still fails this test.
	final := f.listServers(t)
	var live, failed []spawneryv1alpha1.Server
	for _, s := range final {
		if s.Status.Phase == string(phase.Failed) {
			failed = append(failed, s)
			continue
		}
		live = append(live, s)
	}

	var survivor, attempt *spawneryv1alpha1.Server
	for i := range live {
		if live[i].Name == busy {
			survivor = &live[i]
			continue
		}
		attempt = &live[i]
	}
	if len(live) != 2 || survivor == nil || attempt == nil {
		names := make([]string, 0, len(live))
		for _, s := range live {
			names = append(names, s.Name)
		}
		t.Fatalf("live servers settled on %v, want the occupied server %q and one further "+
			"cold-start attempt: the backoff permits a second try ten seconds after the "+
			"first replacement fails", names, busy)
	}
	if got := survivor.Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase of the surviving server = %q, want Ready", got)
	}
	// The second live server is the attempt the closed window permitted, not
	// a second survivor: the current generation's, and still starting.
	if got := attempt.Spec.GroupGeneration; got != f.group.Generation {
		t.Errorf("second live server's generation = %d, want %d: it is the current "+
			"generation's next cold start, not a leftover", got, f.group.Generation)
	}
	if got := attempt.Status.Phase; got == string(phase.Ready) {
		t.Errorf("second live server is %q; nothing reports for it in this fixture, so a "+
			"Ready one means it is not the attempt this assertion is about", got)
	}
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 1 {
		t.Errorf("minAvailable = %d, want 1 — the surviving pod still carries players", got)
	}

	if len(failed) != 1 {
		names := make([]string, 0, len(failed))
		for _, s := range failed {
			names = append(names, s.Name)
		}
		t.Fatalf("failed servers = %v, want exactly one — the cold start's doomed replacement", names)
	}
	if got := failed[0].Spec.GroupGeneration; got != f.group.Generation {
		t.Errorf("failed server's generation = %d, want %d: the current generation's cold start, not a leftover", got, f.group.Generation)
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
		f.clock.Advance(ResyncInterval)
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

	// Milestone 4d: the first failure buys a ten-second window, so the
	// replacement this test is about arrives after it rather than on the very
	// next pass. What is asserted below is unchanged — a Failed server does not
	// hold the group at its floor, and a replacement is created while it is
	// kept for diagnosis — only when the group is allowed to act on it.
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
		f.clock.Advance(ResyncInterval)
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
	// The number and the selector are separate questions, and a budget is only
	// protection when they answer the same one. See the helper.
	f.assertBudgetSelectsExactlyWhatItCounts(t, key.Name)
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

	// An ephemeral group whose Network is missing is not limited by
	// maxReplicas — its Accepted condition already says what is wrong with it —
	// so ScalingLimited is published as False rather than left absent. The
	// condition is guarded on IsEphemeral alone, not on the Network being
	// usable, and this is what says so.
	cond := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionScalingLimited)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("ScalingLimited = %+v on a group without its Network, want False", cond)
	}
	// size() never ran, so the message must say nothing was decided rather
	// than assert the all-clear a sized pass would have checked.
	if cond != nil && cond.Message != "scaling is not being decided: the group's network is not usable" {
		t.Errorf("message = %q, want the not-decided message, not the all-clear", cond.Message)
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
		f.clock.Advance(ResyncInterval)
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
		f.clock.Advance(ResyncInterval)
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
// Driven through the ceiling, not the spare-slot demand rule. The demand
// branch filters a candidate out before SelectDeletionCandidates ever sees
// it — a forgotten agent is both Stale and, since it was never given time to
// wait, short of Stabilization — so mayHavePlayers, the rule this test is
// actually about, would never get a chance to decide anything there. The
// surplus branch has neither gate: it calls SelectDeletionCandidates
// directly on the raw views, so lowering maxReplicas below the current count
// puts mayHavePlayers back in sole charge of the choice, with the fixture's
// default spareSlots and stabilization window left untouched — and with no
// stabilization wait, there is no race against the victim's own
// StartupDeadline either.
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

	// Lowering the ceiling below the current count, rather than raising the
	// floor, is what routes DecideSize through the surplus branch and
	// SelectDeletionCandidates directly, instead of through the demand rule's
	// own Stale/Stabilization filtering.
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.MinReplicas = 1
	f.group.Spec.Scaling.MaxReplicas = 1
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}

	// The surplus branch has no stabilization wait, so a handful of passes is
	// enough to cover the drain that follows once the peer is nominated:
	// Ready -> Draining -> Terminating -> the object actually gone.
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
		f.clock.Advance(ResyncInterval)
	}

	// spareSlots keeps its fixture default of 40 free player slots, and
	// countsTowardSize excludes Phase == Failed from what counts toward it —
	// so once pruning is down to the one failure it keeps for diagnosis,
	// DecideSize still sees zero verified free capacity and orders one
	// replacement to hold those slots. That replacement sits alongside the
	// retained failure, not in place of it: two servers, not one, is the
	// correct count once the group is actually sizing itself.
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

// TestGroupCreatesTheShortfallOnceWhileTheNewServersStart is the test that
// carries this milestone.
//
// A group short of spare slots orders replacements. Those replacements are not
// Ready for tens of seconds, and a scaler reading status.freeSlots would see the
// same shortfall on every five-second pass and order the same replacement again,
// until maxReplicas stopped it. An assertion on a single decision cannot see
// that; only one that keeps reconciling can.
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

	// Ten more passes while the new server has no pod and no agent. Its
	// capacity is ordered, so nothing more may be ordered on top of it.
	//
	// The first server keeps reporting throughout, as a real agent does every
	// five seconds. Without that its count would go stale after two intervals
	// and the test would pass for the wrong reason — a stale server contributes
	// nothing either.
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

	// Four, not three: setMinReplicas performs a real spec update, so the
	// group's generation moves and the three Ready servers created under the
	// previous one are stale. With nothing of the current generation up, the
	// cold start orders exactly one replacement — the changeover beginning,
	// not the floor being missed. The shrink this test is about happens
	// below, once the stabilization window elapses.
	//
	// All three original servers are empty, but none has waited out the
	// window yet.
	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != 4 {
		t.Fatalf("got %d servers before the window elapsed, want 4", got)
	}

	// The window is the CRD default, 300 seconds. The agents keep reporting
	// across it, as real ones do: a count older than twice the report interval
	// is stale, and stale counts as occupied. A repeated zero must not restart
	// the emptiness window, which is what makes this assertion meaningful.
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
	// The ephemeral side of size()'s DeleteReason fallback. DecideSize leaves
	// the field empty -- only the persistent classes fill it, and those are
	// tabled in persistent_test.go -- so this removal must carry the reason
	// the ephemeral rule has always used.
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

// TestGroupSaysWhenItsCeilingHoldsCapacityBack closes a gap this repository
// already has once: a proxy that cannot bind its ready port says so only in a
// container log. A group that cannot serve its spareSlots because maxReplicas
// stops it is the same kind of silence, and this is the milestone that would
// otherwise add a second one next to the first.
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
	// Pinned down exactly, not just "contains maxReplicas": this is the
	// ordinary-shortfall message, and it must stay distinct from the
	// cold-start-refused message in TestGroupSaysColdStartIsBlockedByTheCeiling
	// so the two can never silently converge again.
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

	// Nothing changed. The group is still at its ceiling and still short, so
	// the condition stays True — and an event on every resync would be one
	// every five seconds for as long as the group is popular.
	f.reconcileGroup(t, r)
	if got := scalingEvents(rec, spawneryv1alpha1.ReasonMaxReplicasReached); got != 0 {
		t.Errorf("%d further MaxReplicasReached events on an unchanged resync, want none", got)
	}

	// Room again: the condition has to come back down.
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

// TestGroupSaysColdStartIsBlockedByTheCeiling covers the other way Limited
// gets set: a group pinned at maxReplicas with no shortfall of its own, whose
// one running server has just gone stale. Wanted and Create are both 0 here
// — exactly as they are when nothing is limited at all — so the message must
// name the real cause (the changeover is stalled at the ceiling) rather than
// the ordinary-shortfall wording, which would tell the operator that nothing
// is needed. This is the case the Important review finding on Task 5 was
// about: it must stay distinct from TestGroupSaysWhenItsCeilingHoldsCapacityBack's
// message so the two can never silently converge again.
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

	// A real spec update bumps the group's generation, so the server just
	// brought up goes stale. It is empty (100 free of 100 slots, well past
	// spareSlots 40), so nothing about capacity is short — the only reason
	// to create anything now is the cold start, and the ceiling (maxReplicas
	// still 1, one server already alive) has no room left to grant it.
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

// TestGroupPatchesRetireOntoTheNominatedServer proves the group actually
// carries out the changeover it decides on: spec.retire is the whole channel
// between the group's decision and the Server controller that executes it.
// If this patch does not land, the changeover is a rule nobody carries out.
//
// The file has no helper that builds two servers of different generations
// directly, so this drives the real path instead: bring the floor's one
// server up, bump the group's generation the way an operator would, and let
// the cold start create the replacement — exactly what
// TestOccupiedServerSurvivesAContinuousScaleDown and the ScalingLimited tests
// above already rely on to get a stale server and a current one onto the
// board.
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

	// A real spec update bumps the group's generation, so the server already
	// up goes stale and the cold start orders its replacement.
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

	// Both servers are Ready now: old at the previous generation, new at the
	// current one. This is the one pass that must nominate old to retire.
	f.reconcileGroup(t, r)

	got := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: old, Namespace: f.ns}, got); err != nil {
		t.Fatalf("get %s: %v", old, err)
	}
	if !got.Spec.Retire {
		t.Error("the stale server was not asked to retire")
	}
}

// TestGroupRetireServerGuardsAgainstARepeatCall exercises retireServer's
// idempotence guard directly. selectRetirement never nominates a server
// already showing Retire: true, so the only call site today never reaches
// the guard; this calls retireServer twice against the same servers map to
// stand in for a future call site that does not pre-filter, and confirms the
// second call is a true no-op: no second event, no second patch.
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

	// The in-memory server the map points at already reflects the patch —
	// retireServer sets srv.Spec.Retire = true on it before issuing the
	// patch — so this second call sees exactly what a future call site that
	// forgets to pre-filter would see.
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

// reconcilePass drives one resync the way the real system does: every Server
// first, so a phase transition the group ordered on a previous pass (spec.retire,
// most of all) actually lands before the group looks at the result, and then the
// group itself. Every loop-driven test above this one already repeats this exact
// shape inline (TestOccupiedServerSurvivesAContinuousScaleDown is the clearest
// example); this names it once for a test that needs it standalone.
func reconcilePass(t *testing.T, f *fixture, r *ServerGroupReconciler) {
	t.Helper()
	for _, s := range f.listServers(t) {
		f.reconcile(s.Name)
	}
	f.reconcileGroup(t, r)
}

// markReady brings an already-created server all the way to phase Ready with
// no players — the state a changeover's replacement is in the moment it
// becomes eligible to receive a retirement. A thin name for bringUpNamed, so
// the test below reads at the same level as the milestone's promise rather
// than the machinery underneath it.
func (f *fixture) markReady(t *testing.T, name string) {
	t.Helper()
	bringUpNamed(t, f, name)
}

// markReadyWithPlayers is markReady plus a live player count, for a server
// that must already be occupied by the time the changeover looks at it.
func (f *fixture) markReadyWithPlayers(t *testing.T, name string, players int32) {
	t.Helper()
	uid := bringUpNamed(t, f, name)
	if err := f.agents.ReportPlayers(uid, players, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
}

// bumpGeneration performs a real spec update, the way an operator rolling a
// new image would: it moves group.Generation forward, so every server created
// under the previous value reads as stale to the scaling rules.
//
// It also pins spec.update explicitly rather than leaving it unset.
// spec.update is optional, and an unset one arrives at MaxUnavailable: 0 —
// which ServerGroup.UpdateMaxUnavailable and selectRetirement both floor to 1
// already (see that accessor's doc comment) — but a prior finding on this
// branch showed relying on that floor silently is exactly how a group can
// appear to work while never actually rolling. Writing the policy out here
// removes that question from this test.
//
// It also raises spareSlots to 150, above what a single fresh replacement
// (100 free slots once it is Ready and empty) can cover on its own. The
// fixture's default of 40 never needs a retiring server's capacity backfilled
// — one fresh replacement already clears it before anything has even
// retired — so a test that left it there could pass even if a retiring
// server never dropped out of the group's size at all. 150 is what makes
// leaving()'s own promise ("dropping out of the group's size is exactly what
// makes the spare-slot rule order a replacement for a server a rolling
// update has retired") into something this test actually exercises.
//
// 150 also has to stay inside the window that keeps assertion 1 below
// reading "exactly one": roughly (80, 180]. At or below 80 the pre-bump gap
// (spareSlots minus the two occupied stale servers' 80 free) goes to zero or
// negative and the cold start alone would already have to explain the
// create; above 180 the post-bump gap exceeds what a single fresh server's
// 100 slots can cover and wanted climbs to 2 at pass 1. 150 sits comfortably
// mid-window, ~70 clear on either side, so this is not fragile today — but a
// future spareSlots change made for an unrelated reason could silently push
// assertion 1 (or 5) outside it.
func (f *fixture) bumpGeneration(t *testing.T) {
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

// serversOfGeneration filters the group's servers down to one generation.
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

// retiringCount counts the servers currently in phase Retiring.
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

// firstRetiring returns the (assumed unique) server in phase Retiring.
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

// TestARollingUpdateReplacesAnOccupiedGroupWithoutKickingAnyone is the
// milestone's acceptance criterion, end to end, in one test: two occupied
// stale servers, a spec change, and a group that ends up entirely on the new
// generation with nobody having been moved. Tasks 1 through 8 all have to be
// correct for this to pass — a regression in any one of them fails a specific
// numbered assertion below, not the test as an undifferentiated whole.
//
// Driven with the suite's own direct-Reconcile idiom (reconcilePass), not the
// brief's bare reconcileGroup calls: this repository runs the Server and
// ServerGroup reconcilers as two separate direct calls rather than through a
// running manager, and a phase the group orders (spec.retire, above all) only
// lands once the Server reconciler for that object runs. Every loop-driven
// test in this file already reconciles every server before the group on each
// pass; this test does the same, just for a fixed, small number of passes
// instead of a loop, because each step here asserts a distinct thing that can
// break rather than an invariant that must hold across many passes.
func TestARollingUpdateReplacesAnOccupiedGroupWithoutKickingAnyone(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// Two occupied servers of the group's starting generation, created
	// directly rather than through the reconciler's floor: minReplicas stays
	// at the fixture's default of 1, so scaling itself would only ever create
	// one of them.
	a, b := "lobby-a", "lobby-b"
	f.createServer(a)
	f.markReadyWithPlayers(t, a, 60)
	f.createServer(b)
	f.markReadyWithPlayers(t, b, 60)

	f.bumpGeneration(t)

	// 1. Exactly one replacement is created — not zero, and not one per
	// five-second pass while it boots. At this fixture's spareSlots (150,
	// see bumpGeneration), this create is explained by ordinary spare-slot
	// demand alone: pass 1 sees provisional=80 (two occupied stale servers,
	// 40 free each) against spareSlots=150, gap=70, wanted=1 — already 1
	// before coldStart's own "if cold && create<1" branch is even consulted.
	// So this assertion does not, on its own, exercise coldStart's
	// deadlock-breaker (Task 5); TestARollingUpdateColdStartCreatesExactlyOneServer
	// below pins that mechanism specifically, with a fixture tuned so the
	// spare-slot rule wants nothing and the create can only be explained by
	// coldStart.
	reconcilePass(t, f, r)
	fresh := f.serversOfGeneration(t, f.group.Generation)
	if len(fresh) != 1 {
		t.Fatalf("cold start created %d servers, want exactly 1", len(fresh))
	}

	// 2. Nothing retires before the replacement is Ready. This is the
	// guarantee that stops a group emptying itself: were it not enforced, the
	// two occupied servers below would already be candidates the instant they
	// went stale.
	reconcilePass(t, f, r)
	if n := f.retiringCount(t); n != 0 {
		t.Fatalf("%d servers retiring before a replacement was Ready", n)
	}

	// 3. Once the replacement is Ready, exactly one stale server retires —
	// one, because maxUnavailable defaults to (and here is pinned at) 1. The
	// first pass below is what nominates it (patches spec.retire); the second
	// is what lands the phase transition that patch orders, exactly as a real
	// resync would need two five-second passes to do the same.
	f.markReady(t, fresh[0].Name)
	reconcilePass(t, f, r)
	reconcilePass(t, f, r)
	if n := f.retiringCount(t); n != 1 {
		t.Fatalf("%d servers retiring, want exactly 1 (maxUnavailable)", n)
	}

	// 4. The retiring server keeps its players — it is not deleted — and it
	// is deregistered so it takes no new joins.
	retiring := f.firstRetiring(t)
	if !retiring.DeletionTimestamp.IsZero() {
		t.Error("a retiring server with players was deleted")
	}
	if retiring.Status.Registered {
		t.Error("a retiring server is still registered")
	}

	// 5. The retirement above is what leaving() (Task 3) exists to make
	// possible: the retiring server dropping out of the group's size is what
	// lets DecideSize notice the capacity it was carrying is gone and order a
	// replacement for it, in the very same pass — bumpGeneration's spareSlots
	// of 150 is chosen so fresh alone cannot cover that gap on its own. If
	// leaving() stopped including phase.Retiring, the retiring server would
	// keep holding the group's size, the shortfall would never become
	// visible, and this is the assertion that would catch it: exactly two
	// servers of the current generation, the cold start's replacement plus
	// the backfill for what the retirement just gave up.
	if got := len(f.serversOfGeneration(t, f.group.Generation)); got != 2 {
		t.Fatalf("%d current-generation servers after the retirement, want 2: "+
			"the cold-start replacement plus the backfill for the capacity the retiring server took with it", got)
	}
}

// TestARollingUpdateColdStartCreatesExactlyOneServer pins coldStart's (Task
// 5) deadlock-breaker in isolation, apart from ordinary spare-slot demand.
// TestARollingUpdateReplacesAnOccupiedGroupWithoutKickingAnyone's assertion 1
// no longer can: at that test's spareSlots of 150, ordinary demand alone
// already wants a create at pass 1, so hard-coding coldStart to always
// return false there still yields one create and that assertion would not
// notice. This test's fixture is tuned the other way, so nothing but
// coldStart can explain the create.
//
// Arithmetic: two stale (old-generation) occupied servers, 60 players each
// out of maxPlayers 100, for 40 free slots apiece — sum(stale free) = 80.
// spareSlots is left at the fixture's default of 40 (bumpGeneration is not
// used here precisely because it raises spareSlots to 150; this test needs
// it left alone). At pass 1: provisional = 80 >= spareSlots = 40, so gap <=
// 0 and wanted = 0. MinReplicas (1) does not raise create either — alive is
// already 2. So create is 0 before coldStart is consulted at all; coldStart
// sees stale=2, current=0, PendingCreates=0, reports true, and DecideSize's
// "if cold && create<1 { create = 1 }" is the only line that can produce the
// server this test asserts on. The margin is comfortable — sum(stale free)
// 80 is double spareSlots 40 — so a modest future change to either number
// will not flip this by accident, but a change that closes the gap (raises
// spareSlots toward 80, or lowers the two servers' free capacity) would.
func TestARollingUpdateColdStartCreatesExactlyOneServer(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	a, b := "lobby-a", "lobby-b"
	f.createServer(a)
	f.markReadyWithPlayers(t, a, 60)
	f.createServer(b)
	f.markReadyWithPlayers(t, b, 60)

	// A real spec update, bumping the generation exactly as bumpGeneration
	// does, but deliberately not touching spareSlots — it must stay at the
	// fixture's default of 40 for the arithmetic above to hold.
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

	// The replacement is still booting (Pending/Starting, never marked
	// Ready). Once it exists it counts toward coldStart's "current" tally
	// and its own provisional capacity (a not-yet-reporting server counts as
	// its full maxPlayers, per provisionalCapacity), so nothing should order
	// a second one on the next two five-second passes either — the cold
	// start must produce one server, not one per pass.
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
	// Both fields exist on the Server status and were simply never lifted into
	// the view. The backoff reads them, and a view that leaves them zero makes
	// every failure look like it happened at the epoch — which counts once and
	// then never again.
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

// oneServerName is the name of the group's only server. It fails the test on
// any other number, because every backoff test below reasons about one
// specific corpse and a second server would change what the count means.
func (f *fixture) oneServerName(t *testing.T) string {
	t.Helper()
	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want exactly 1", len(servers))
	}
	return servers[0].Name
}

// newestServerName returns the name of the group's one server not yet in
// phase Failed — the replacement a closed backoff window just allowed — and
// false once every server the group has ever created has failed and nothing
// new was created this pass (a give-up, or a window still open).
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

// reloadGroup re-reads the group from the API server. The count lives on the
// status precisely so that it survives a restart, so a test that asserts on it
// has to read what was persisted rather than the fixture's own copy.
func (f *fixture) reloadGroup(t *testing.T) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, g); err != nil {
		t.Fatalf("get group: %v", err)
	}
	return g
}

// setMaxReplicas moves the group's ceiling, the mirror of setMinReplicas.
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

// failServer walks an existing server up to Ready and then past its readiness
// losses into Failed — bringUpNamed and driveToFailed, named once for the
// tests below. driveToFailed is what makes the Server controller stamp
// status.failedAt, and that timestamp, not the moment the group observes it,
// is what the count and the window are measured from.
func (f *fixture) failServer(t *testing.T, name string) {
	t.Helper()
	bringUpNamed(t, f, name)
	driveToFailed(t, f, name)
	if f.server(name).Status.FailedAt == nil {
		t.Fatalf("server %s reached Failed with no status.failedAt; that is the field the count reads", name)
	}
}

// failServerNeverReady is failServer's other half, and the one the milestone is
// actually named for: a server that never becomes playable at all, rather than
// one that was Ready and flapped.
//
// failServer goes bringUpNamed then driveToFailed, so every failure it produces
// belongs to a server that reached Ready first. That matters twice over. It is
// not the broken-image scenario the backoff exists for; and it walks through
// Starting, which clears status.readySince on its own, so it can never show
// whether the Failed arm's own clearing is doing anything (see
// TestServerFailedStraightFromReadyClearsReadySince).
//
// This walks the other path, with no new machinery: the pod runs, the server
// enters Starting, and status.startedAt then ages past the reconciler's
// StartupDeadline with the ready gate never satisfied — phase.Decide's
// `StartupDeadlineReached && current != Ready` branch. The clock move is what
// the failure is made of, so it is not a knob: it has to exceed the deadline.
//
// It does not, however, buy the caller anything against the backoff. The
// advance happens *before* the failure, so the failedAt it produces is stamped
// at the new time and the window opens from there — a caller driving a streak
// still has to move the clock again afterwards to earn its next attempt.
func (f *fixture) failServerNeverReady(t *testing.T, name string) {
	t.Helper()
	f.reconcile(name)
	if _, ok := f.pod(name); !ok {
		t.Fatalf("reconcile did not create the pod for %s", name)
	}
	// The kubelet's part, and only that part: the container is up, the probe
	// never goes green, and no agent ever connects.
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
	// This would fail regardless of whether the server was ever Ready: entering
	// Starting or Failed clears status.readySince unconditionally (the phase
	// switch in server_controller.go), so a failServer corpse — which did reach
	// Ready — passes this identically. What actually shows this server was
	// never Ready is the Starting assertion above and the fact that
	// phase.Decide's Ready case, the only place that sets readySince, is never
	// reached: the deadline advance runs out before the ready gate is ever
	// satisfied.
	if srv.Status.ReadySince != nil {
		t.Fatalf("server %s carries status.readySince", name)
	}
	if srv.Status.FailedAt == nil {
		t.Fatalf("server %s reached Failed with no status.failedAt; that is the field the count reads", name)
	}
}

// shedCount counts the servers the group has asked to remove, ignoring the
// named ones. envtest runs no kubelet and the Server controller holds a
// finalizer through the drain, so a deleted Server lingers carrying a deletion
// timestamp rather than disappearing.
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

// TestGroupStopsCreatingWhileItBacksOff is the point of the milestone: a group
// whose server failed does not build another one on the next five-second pass.
func TestGroupStopsCreatingWhileItBacksOff(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)

	name := f.oneServerName(t)
	f.failServer(t, name)

	// The pass that counts the failure is already inside the window it opens:
	// the clock has not moved since failedAt was stamped. So the replacement
	// the floor asks for must not be created on this pass either, and
	// asserting that here rather than only on the next pass is deliberate.
	// With the gate removed the replacement appears on exactly this pass, and
	// a test that took its baseline afterwards would take that replacement for
	// the baseline and then see nothing wrong on the next pass — where the
	// floor is satisfied and nothing more is created anyway.
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

// TestGroupCreatesAgainOnceTheWindowCloses is the other half: the backoff is a
// wait, not a stop. One failure buys ten seconds and no more.
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

// TestGroupStillShedsWhileItBacksOff pins that the backoff holds back
// building, not tidying up. A deletion path that waited on an unrelated
// failure would leave surplus servers standing for the whole window.
//
// The window is open by construction rather than by reading a condition:
// consecutiveFailures is 1 below and the clock has not moved past the failedAt
// the window runs from, which is DecideBackoff's "must wait" case exactly.
// Task 4 could only assert consecutiveFailures as a stand-in because the
// BackingOff condition did not exist yet; now that Task 5 has added it, the
// assertion below is strengthened to read the condition itself.
func TestGroupStillShedsWhileItBacksOff(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	// No floor and a ceiling of one, so a removal is the only thing this pass
	// can decide: nothing is short, and the surplus is unambiguous.
	f.setMinReplicas(t, 0)
	f.setMaxReplicas(t, 1)

	// Two idle servers against that ceiling, plus a third that fails and opens
	// the window. The failure does not count toward the group's size, so the
	// surplus is exactly one of the two idle servers.
	idle := []string{"lobby-idle-a", "lobby-idle-b"}
	for _, name := range idle {
		bringUpReady(t, f, name)
	}
	// The failure has to be newer than those two readySince stamps or it is
	// not a failure since the last success and the streak never starts. The
	// fixture's clock only moves when a test moves it, so without this the
	// whole scenario happens in one instant and CountFailures is right to
	// count nothing.
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

// TestGroupStillRetiresWhileItBacksOff pins the other half of the same
// promise as TestGroupStillSheds: the retire loop in size() must not wait on
// backoff.MayCreate either. The delete loop is covered above; this is the
// path a rolling update actually depends on, because a retirement is what
// starts a soft drain — the only thing in this diff that moves a player's
// session. A retirement stalled behind the window would be a changeover that
// stops mid-flight over a failure that has nothing to do with it.
//
// The fixture is a group mid-changeover: a stale (previous-generation) Ready
// server, which is the retirement candidate, and a Ready server of the
// current generation, which selectRetirement refuses to nominate anything
// without (design §3.4's "at least one ready server of the current
// generation"). Both are brought up with the fixture's default scaling
// (spareSlots 40, maxReplicas 10) so that each Ready server's 100 free slots
// already clears spare-slot demand and DecideSize reaches the retirement
// branch rather than a create or a ceiling-driven delete.
//
// As in TestGroupStillSheds, the window is proved open by construction:
// consecutiveFailures reads 1 with the clock still short of failedAt +
// backoffBase, DecideBackoff's must-wait case exactly.
func TestGroupStillRetiresWhileItBacksOff(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// The retirement candidate, created and readied under the group's
	// starting generation, before the bump below makes it stale.
	stale := "lobby-stale"
	bringUpReady(t, f, stale)

	// The operator's fix in flight: a real generation bump, so the group is
	// genuinely mid-changeover rather than merely holding one old server.
	f.bumpGeneration(t)

	// The current-generation Ready server selectRetirement requires before it
	// will nominate anything. Its readySince is the watermark the failure
	// below has to clear.
	current := "lobby-current"
	bringUpReady(t, f, current)

	// The failure has to be newer than lobby-current's readySince or it is
	// not a failure since the last success and the streak never starts — see
	// CountFailures and the identical comment on TestGroupStillSheds above.
	// ofGeneration excludes the stale server from the count entirely, so only
	// this watermark matters here.
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

// TestGenerationChangeClearsTheBackoff pins the way out. A spec change is the
// operator's answer to whatever broke, so the streak it caused is over and the
// next attempt is immediate.
func TestGenerationChangeClearsTheBackoff(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, r)
	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got != 1 {
		t.Fatalf("consecutiveFailures = %d before the spec change, want 1: there is no streak here to clear", got)
	}

	// The operator's answer to whatever failed.
	f.bumpGeneration(t)
	f.reconcileGroup(t, r)

	g := f.reloadGroup(t)
	if g.Status.ConsecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d after a spec change, want 0", g.Status.ConsecutiveFailures)
	}
	if g.Status.LastFailureAt != nil {
		t.Error("lastFailureAt survived a spec change")
	}
	// A cleared counter means no window, so the group builds at once rather
	// than serving out the wait the old spec earned.
	if len(f.serversOfGeneration(t, g.Generation)) == 0 {
		t.Error("the group created nothing on the pass that cleared the streak; after a spec change the next attempt is immediate")
	}
}

// TestBackingOffConditionNamesTheCountAndTheWait pins the waiting half of the
// two conditions Task 5 adds: after a single failure the group must say it is
// waiting, name the count, and say roughly how long — and it must not claim a
// fault. derivePhase turns a true Degraded into the group's phase, so
// conflating the two would make a ten-second wait look identical to a broken
// image.
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

// TestGroupGivesUpAndSaysSo drives the streak all the way to
// backoffGiveUpAt and checks the other half: once the group has given up,
// Degraded goes true (so the phase reflects the real fault) and BackingOff
// goes false — but with a reason and a message that say why, because
// NoRecentFailures there would be a lie. It then proves the give-up sticks:
// an hour later, with nothing about the spec changed, the group is carrying
// its six corpses and nothing live — an absolute count, for the reason the
// comment on that block gives.
func TestGroupGivesUpAndSaysSo(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setMinReplicas(t, 1)
	// Drive the streak to the threshold: fail, wait out the window, repeat.
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

	// The terminal proof, stated as an absolute count rather than a delta.
	//
	// A `before := len(f.listServers(t))` taken here would measure nothing: the
	// pass that established the give-up has already run, so a mutant that
	// creates anyway has already created its extra server and that server is
	// absorbed into the baseline — after which the pass an hour later builds
	// nothing either way, because the floor is by then satisfied. That is
	// exactly what the whole-branch review found by mutating DecideBackoff's
	// threshold branch to {GaveUp: true, MayCreate: true} and watching the
	// entire envtest suite stay green.
	//
	// So count the state itself. backoffGiveUpAt failures produce
	// backoffGiveUpAt corpses — pruneFailed asks for all but one to go, but the
	// Server controller holds a finalizer through the drain and this test never
	// reconciles a Server again, so every corpse lingers in phase Failed — and
	// **nothing live at all**. A group that gave up and created anyway shows up
	// here as a live server, whenever in the run it was built.
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

// TestGroupGivesUpOnServersThatNeverBecomeReady drives the milestone's headline
// case end to end, which nothing else in this package did: a group whose
// servers run out their startup deadline without ever becoming playable — a
// broken image, an unpullable tag, a config the server rejects on boot — makes
// its six attempts and then gives up.
//
// Every other backoff test here fails its servers with failServer, which is
// bringUpNamed plus driveToFailed: a server that reached Ready and then
// flapped. Those are valid assertions about the state at count 6, but they are
// a different scenario, and one that a production reconciler at a five-second
// resync would see differently — it would observe the Ready state in between
// and CountFailures would correctly reset the streak. Nothing was checking that
// a server which is *never* Ready climbs the same ladder. It does, and this is
// where that is written down.
//
// The round is the same shape as TestGroupGivesUpAndSaysSo's — fail, wait out
// the window, repeat — and the wait is backoffCap for the same reason: it
// exceeds every window the schedule can open before the threshold (160s at the
// most), so no round is held back by a wait this test is not about. Aging the
// server out of its startup deadline does not serve as that wait, because the
// advance precedes the failure it causes and the window runs from the failedAt
// it stamps.
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

	// The absolute state, for the reason TestGroupGivesUpAndSaysSo's tail gives:
	// six corpses, none of which was ever Ready, and nothing live.
	live, corpses := 0, 0
	for _, s := range f.listServers(t) {
		if s.Status.Phase != string(phase.Failed) {
			live++
			continue
		}
		corpses++
		// This would fail regardless of whether the server was ever Ready:
		// entering Starting or Failed clears status.readySince unconditionally,
		// so a failServer corpse — which did reach Ready — passes this
		// identically. What actually establishes that these six were never
		// Ready is failServerNeverReady's own Starting assertion and the ready
		// gate its deadline advance never satisfies.
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

// TestBackingOffEventFiresOnTheFlankOnly pins the same non-spam rule
// ScalingLimited already carries: SetStatusCondition moves lastTransitionTime
// only on a real change of status, so comparing across the call is what tells
// a transition apart from a five-second resync, and an event belongs only on
// the former.
func TestBackingOffEventFiresOnTheFlankOnly(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := newRecorder()
	r.Recorder = rec
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, r)

	// scalingEvents drains the recorder, so this first call also empties it of
	// whatever fired on the two passes above.
	if first := scalingEvents(rec, spawneryv1alpha1.ReasonCrashLoopBackoff); first != 1 {
		t.Fatalf("events = %d after the first failure, want 1", first)
	}
	f.clock.Advance(time.Second)
	f.reconcileGroup(t, r)
	if got := scalingEvents(rec, spawneryv1alpha1.ReasonCrashLoopBackoff); got != 0 {
		t.Errorf("events = %d after a resync inside the same window, want 0: the event goes on the flank", got)
	}
}

// TestBackingOffEventDoesNotFireWhenNetworkDiesMidBackoff pins the review's
// finding 1 on Task 5: the BackingOff/Degraded event guards must check sized
// the same way ScalingLimited's guard does (`if sized && decision.Limited !=
// was`). Without that check, a group whose Network dies while it is actively
// backing off flips BackingOff from True to the !sized case's False on the
// very next pass — a real transition by IsStatusConditionTrue's bookkeeping,
// even though nothing was decided — and fires an event carrying the
// condition's default NoRecentFailures reason next to a message that says
// the opposite: that backoff is not being decided, not that servers are
// healthy. A reassuring Reason paired with an unresolved-problem Message is
// exactly what the sized guard exists to prevent.
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
	// Drains the recorder of everything that fired getting here (this test is
	// about the next pass only); scalingEvents empties the whole channel
	// regardless of which reason it is asked to count.
	scalingEvents(rec, spawneryv1alpha1.ReasonCrashLoopBackoff)

	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}
	f.reconcileGroup(t, r)

	if got := scalingEvents(rec, spawneryv1alpha1.ReasonNoRecentFailures); got != 0 {
		t.Errorf("events = %d when the network died mid-backoff, want 0: nothing was decided this pass, so no event should fire", got)
	}
}

// TestDegradedEventDoesNotFireWhenNetworkDiesAfterAGiveUp is the mirror of
// TestBackingOffEventDoesNotFireWhenNetworkDiesMidBackoff, and it exists
// because the whole-branch review reproduced the asymmetry: dropping `sized &&`
// from the *Degraded* event guard alone left the entire suite green, while its
// byte-identical twin on the backingOff guard was already pinned by that test.
// An unpinned guard sitting beside a pinned identical one is a trap — the next
// person to touch the block has a test telling them one of the two matters and
// nothing telling them the other does.
//
// The mechanism is the same as the twin's, one condition over. A group that has
// given up carries Degraded: True. If its Network then dies, the !sized case
// forces Degraded back to the false-by-default condition, whose reason is
// NoRecentFailures — a real transition by IsStatusConditionTrue's bookkeeping,
// even though nothing was decided this pass. Without the sized check that fires
// an all-clear "servers are starting normally" reason next to a message saying
// the opposite, for a group that is six failures deep and creating nothing.
//
// NoRecentFailures is the right thing to count and it is unambiguous here:
// backingOff is False on both passes (CrashLoopBackoff at the give-up, then
// NoRecentFailures under !sized), so its own guard sees no transition and
// cannot contribute an event, and ScalingLimited's reason on the !sized pass is
// WithinLimits. Any NoRecentFailures event reaching the recorder came from the
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
		// The recorder's channel blocks its writer once full and nothing else
		// here drains it; six failures' worth of group events is close enough
		// to a hundred to be worth not finding out. See drainRecorder.
		drainRecorder(rec)
	}
	f.reconcileGroup(t, r)

	if !meta.IsStatusConditionTrue(f.reloadGroup(t).Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Fatalf("Degraded is not true after %d failures; the network-dies pass below would prove nothing", backoffGiveUpAt)
	}
	// Drains everything that fired getting here: this test is about the next
	// pass only.
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

// TestBackingOffMessageStaysSilentAboutAFailureThatNeverHappened pins the
// !newestFailure.IsZero() guard in Reconcile around the write to
// group.Status.LastFailureAt. No test that reloads the group can ever tell a
// guarded write apart from an unguarded one: a zero metav1.Time marshals to
// null and round-trips back to nil either way, so the field itself carries no
// evidence. What the guard actually protects is the in-memory value on this
// same pass, and the default BackingOff message is what reads it back — so a
// group that has never failed must render the plain, dateless message, not a
// zero time smuggled in by an unconditional write.
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

// TestGenerationChangeClearsAGaveUpGroupsConditions closes out the carried
// finding that the generation-change RemoveStatusCondition calls were
// completely untested: until this task nothing ever set either condition, so
// the removal had nothing to prove. Driving a real give-up first, then a real
// spec change, is what makes this a test of the clear rather than of two
// conditions that were already absent.
func TestGenerationChangeClearsAGaveUpGroupsConditions(t *testing.T) {
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
		t.Fatalf("Degraded is not true after %d failures; the generation change below would prove nothing", backoffGiveUpAt)
	}

	// The operator's answer to whatever failed.
	f.bumpGeneration(t)
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

// TestBackingOffIsNotDecidedWithoutAUsableNetwork closes out the carried
// finding that the counting block sits ahead of the networkUsable &&
// IsEphemeral guard: a group whose Network does not exist still counts and
// still computes a real backoff decision it cannot act on. Publishing that
// decision as BackingOff/Degraded would sit a second, differently-worded
// explanation for the same standstill next to Accepted: False. It has to say
// nothing was decided instead, mirroring ScalingLimited's own !sized case
// exactly.
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

// drainRecorder empties a FakeRecorder's buffer and throws the events away.
// It was written for a hazard that no longer exists: FakeRecorder's channel
// blocked its writer once full, so a test walking many servers through a whole
// lifecycle wedged the reconciler mid-event rather than reaching its assertion,
// and this drained between passes to stop that. nonBlockingRecorder has no
// buffer to overrun, so nothing wedges anywhere now.
//
// It stays because the second thing it did is still wanted: it resets the
// recorder between passes, so a count taken after one pass is about that pass
// rather than about every pass since the fixture was built.
func drainRecorder(rec *nonBlockingRecorder) { drainEvents(rec) }

// TestGroupWithABrokenNewImageDoesNotRebuildEveryPass is the loop milestone
// 4b's cold-start suppression was a stopgap for: a broken new image fails,
// stops counting toward the group's size, and is recreated on the next
// five-second pass, forever. It asserts the backoff bounds that loop, so the
// stopgap can be removed without reopening it.
//
// Three deliberate departures from the obvious way to write this:
//
// Every server of the new generation is failed on each pass, rather than "the
// one server that has not failed yet". The stale server the changeover is
// replacing stays Ready throughout, so that description matches two servers
// whenever a replacement exists and identifies neither — and a loop that
// failed nothing would pass this test with the backoff, the stopgap and
// everything else removed.
//
// The clock advances before each failure rather than after, so the first
// failedAt is strictly newer than the stale server's readySince.
// CountFailures restarts the streak on a success newer than the last counted
// failure and a same-instant stamp is not after it, so without this the run
// would count no failures at all.
//
// The bound is read off the backoff schedule rather than set at one more than
// the cold start built, and the arithmetic has two factors rather than one.
//
// Ten passes move the clock fifty seconds: passes * ResyncInterval = 10 * 5s.
// backoffDelay(n) = backoffBase * backoffFactor^(n-1) gives windows of 10s,
// 20s, 40s for n = 1, 2, 3 (backoffBase = 10s, backoffFactor = 2, both in
// backoff.go). Since 2026-08-24 the count is rounds rather than corpses, so it
// climbs 1, 2, 3 here: the first window opens at t=15s and the second at
// t=40s, both inside 50s, and the third would need t=85s. Two windows fire.
//
// **Each window builds two servers, not one**, and that is the factor the
// earlier version of this comment did not have. DecideSize runs the group
// above its floor to cover spareSlots, so even at minReplicas 1 a recovery
// builds two. The old comment reached its bound of 3 by a route that happened
// to give the right number for the wrong reason — it read two windows at one
// server each, where the run was one window at two servers. Measured, not
// reasoned: a probe over these ten passes shows the group going 1 -> 3 -> 5
// servers at passes 2 and 7.
//
// So bound = 1 (the cold start's own server) + 2 windows * 2 servers = 5. A
// group rebuilding every pass would have built about twenty, so the test still
// separates the two by a wide margin. Changing passes, backoffBase,
// backoffFactor, ResyncInterval or spareSlots moves this arithmetic and will
// turn this red — that is a fixture change, not a backoff regression.
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

	f.bumpGeneration(t) // the changeover begins; the cold start builds one
	f.reconcileGroup(t, r)
	generation := f.reloadGroup(t).Generation

	// Every replacement fails immediately, and the clock moves only by the
	// resync interval — far less than the backoff's first window.
	for i := 0; i < passes; i++ {
		// Both recorders, every pass: a group that rebuilt every pass — the
		// failure this test exists to catch — produces ten servers' worth of
		// lifecycle events, which is more than a FakeRecorder holds.
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

// TestACordonedNodeCondemnsTheServersOnIt is the server half of the milestone:
// a node on its way out empties itself of servers, and the group rebuilds them
// somewhere else without anybody being kicked.
func TestACordonedNodeCondemnsTheServersOnIt(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)

	// Two servers, both Ready, one occupied so the test also proves that
	// having players does not exempt a server from a node that is leaving.
	f.setMinReplicas(t, 2)
	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(servers))
	}
	f.markReadyWithPlayers(t, servers[0].Name, 3)
	f.markReady(t, servers[1].Name)

	// Place them on two nodes, then cordon the first.
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

	// The server on the cordoned node is going.
	condemned := f.server(servers[0].Name)
	if condemned.DeletionTimestamp.IsZero() {
		t.Error("the server on the cordoned node was not deleted; its players will be evicted instead of moved")
	}
	// The one beside it is not.
	if !f.server(servers[1].Name).DeletionTimestamp.IsZero() {
		t.Error("the server on the healthy node was deleted; only the departing node's servers may go")
	}
	// And the operator is told why. scalingEvents drains the recorder, so the
	// resync assertion below starts from an empty channel.
	if n := scalingEvents(rec, "NodeDraining"); n != 1 {
		t.Errorf("NodeDraining events = %d, want exactly 1", n)
	}

	// A resync that changes nothing must not announce the same drain again:
	// the event is the moment of decision, not a status repeated every five
	// seconds for as long as the server takes to drain. The proxy side learned
	// this mid-milestone (TestANodeDrainMarkFiresANodeDrainingEvent's third
	// pass); this is the same assertion for deleteServer's own guard.
	//
	// The clock jumps past expectationTTL rather than one resync, and that is
	// what makes this test the guard's rather than the reservation's. The
	// delete reserved above keeps condemned() from naming this server again
	// for as long as it stands, so a pass taken before it expires would emit
	// nothing whatever deleteServer did. Past the TTL the server is nominated
	// a second time, deleteServer is actually reached, and its
	// DeletionTimestamp check is the only thing between it and a second
	// event. Verified by removing that check: this then reports 1.
	f.clock.Advance(expectationTTL + time.Second)
	f.reconcileGroup(t, r)
	if n := scalingEvents(rec, "NodeDraining"); n != 0 {
		t.Errorf("NodeDraining events on an unchanged resync = %d, want 0: the drain is announced "+
			"once, not once per pass for its whole duration", n)
	}
}

// TestAFailedServerOnACordonedNodeGoesAwayOnce is the cost of the same
// omission, seen from outside: a Failed server on a departing node used to be
// both condemned by size() and collected by pruneFailed, which run over the
// same in-memory map in one pass. r.Delete stamps no deletion timestamp back
// onto the local object, so deleteServer's guard against repeating itself
// never sees the first removal, and one server going away once was deleted
// twice and announced twice under two different reasons.
//
// Both failures sit on the cordoned node, which is what makes the
// FailedServerPruned count below discriminating: with both condemned there is
// nothing left for the retention cap to prune, so any such event is the
// duplicate.
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
	// Two failures, one more than maxRetainedFailures, so the retention cap
	// really would nominate one of them if it were still looking at them.
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

	// Counted from one read of the channel: each event is delivered once, so
	// asking twice would empty it before the second question.
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

// TestABrokenNetworkDoesNotStopACordonedNodeFromEmptying pins the ruling that
// moved condemnation below the Network gate. Design §3.3 calls condemnation
// "unconditional. The node is leaving with or without our consent", and size()
// used to be called only when the Network was usable -- so a group whose
// Network had been deleted, or which had lost the one-per-namespace contest,
// published NodeDraining: True naming the node, condemned nothing, and left
// kubectl drain hanging on an occupied pod indefinitely. That is the failure
// §1 of the design opens by describing, reached through the operator that
// exists to prevent it.
//
// The server here is occupied, so the assertion is not merely about a
// bookkeeping deletion: this is the pod the eviction API would otherwise be
// left to take.
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

	// The Network goes after the group is already running, which is the state
	// an operator actually reaches: a deleted Network, or one that lost the
	// contest, leaves the groups that referenced it exactly here.
	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete network: %v", err)
	}

	f.reconcileGroup(t, r)

	// The pass really took the broken-Network route. Without this the
	// assertions below would hold just as well on a pass that found its
	// Network and sized normally, and would say nothing about the gate.
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

// TestCondemnedServersAreReplaced states the property that makes the drain
// finite: the pass that condemns is the pass that orders the replacement, so
// the group is never left short while it waits for a second reconcile.
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

	// The condemnation has to be checked before the replacement, or this test
	// says nothing. Counting live servers alone passes just as well against a
	// build that never condemns anything -- the original is still there and
	// still counts as one -- which is exactly how a test outlives the code it
	// was written for. Asserting the original is going is what makes the
	// count below a statement about a replacement.
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

// TestNodeDrainingConditionNamesTheNode is what an operator sees in
// kubectl describe. A bare True would tell them something is happening
// without telling them where.
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

	// Release it: with no pods left on a departing node the condition goes
	// False rather than staying True until something else clears it.
	f.ensureNode(t, node.Name, false)
	f.reconcileGroup(t, r)
	cond = meta.FindStatusCondition(f.reloadGroup(t).Status.Conditions,
		spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("NodeDraining after uncordon = %v, want False", cond)
	}
}

// createPersistentGroup adds a persistent ServerGroup beside the fixture's
// ephemeral one, in the same namespace and on the same Network.
//
// It is built from scratch rather than by editing the fixture's group,
// because spec.type is immutable and no edit can turn an ephemeral group into
// a persistent one. The field sets differ besides: the CRD requires
// spec.replicas and spec.storage here, and forbids spec.scaling.
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

// reconcilePersistentGroup runs the group reconciler once against a group
// named here, rather than against the fixture's own.
func (f *fixture) reconcilePersistentGroup(t *testing.T, r *ServerGroupReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile group %s: %v", name, err)
	}
}

// setPersistentReplicas moves spec.replicas of a persistent group.
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

// serverNamesOfGroup is the names of the servers a group owns that still
// exist, sorted, so an assertion can name what it expects to see.
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

// serverIfPresent reads a Server without failing the test when it is gone,
// which f.server cannot do and a removal has to be able to ask.
func (f *fixture) serverIfPresent(name string) (*spawneryv1alpha1.Server, bool) {
	srv := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, srv); err != nil {
		return nil, false
	}
	return srv, true
}

// TestAPersistentGroupBuildsItsOrdinals is the milestone's subject: a number
// in spec.replicas becomes servers with stable names.
func TestAPersistentGroupBuildsItsOrdinals(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)

	f.reconcilePersistentGroup(t, r, "survival")

	names := f.serverNamesOfGroup(t, "survival")
	if len(names) != 2 || names[0] != "survival-0" || names[1] != "survival-1" {
		t.Fatalf("servers = %v, want [survival-0 survival-1]", names)
	}
	// names is sorted and pinned above, so its index is the ordinal the name
	// carries. Deriving the expectation with OrdinalOf instead would only ask
	// the name what the name already says.
	for i, name := range names {
		srv := f.server(name)
		if srv.Spec.Ordinal == nil || *srv.Spec.Ordinal != int32(i) {
			t.Errorf("%s spec.ordinal = %v, want %d", name, srv.Spec.Ordinal, i)
		}
	}
}

// TestAPersistentGroupRemovesTheHighestOrdinal is the other direction. The
// group reconciler asks for the removal by deleting the Server object; the
// drain finalizer that would otherwise hold it in Terminating is the Server
// controller's, and this test drives only the group, so the surplus ordinal
// leaves the API server outright.
func TestAPersistentGroupRemovesTheHighestOrdinal(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 3)
	f.reconcilePersistentGroup(t, r, "survival")
	if got := len(f.serverNamesOfGroup(t, "survival")); got != 3 {
		t.Fatalf("servers = %d, want 3", got)
	}

	// SizeDecision.DeleteReason says which class nominated an ordinal, and the
	// event is where it reaches an operator -- nothing else reads the field.
	// The values are tabled in persistent_test.go; what this asserts is that
	// size() carries one through to deleteServer instead of falling back to
	// the ephemeral rule's ServerRemoved.
	rec := newRecorder()
	r.Recorder = rec

	f.setPersistentReplicas(t, "survival", 2)
	f.reconcilePersistentGroup(t, r, "survival")

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

// TestAPersistentGroupToleratesAnOrdinalNameAlreadyTaken pins the one thing a
// derived name can run into that a random one effectively cannot: the name it
// is about to create is already on an object.
//
// The server built here holds no spec.ordinal, which is how an object this
// rule did not create looks to it -- the rule reads spec.ordinal and never the
// name, so this one fills no ordinal and the create is asked for anyway. A
// lagging cache produces the same collision by a shorter route. Either way the
// object is already what the create wanted, and a reconcile that returned the
// error would come back to the same refusal on every pass for as long as the
// object exists, never reaching the status, the PDB or anything else below it.
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

	// The failure this pins is the reconcile erroring out, which
	// reconcilePersistentGroup reports.
	f.reconcilePersistentGroup(t, r, "survival")

	if names := f.serverNamesOfGroup(t, "survival"); len(names) != 1 || names[0] != "survival-0" {
		t.Errorf("servers = %v, want [survival-0]", names)
	}
	// And nothing announces a creation that did not happen. This was the
	// quietest part of the AlreadyExists branch and the part nothing held: the
	// branch's own comment says "No event: nothing was created here", and until
	// now that was prose. What makes it matter is that the collision is not
	// always a blip: a squatter holding the name without spec.ordinal is a
	// steady state, not a lagging cache — the rule reads spec.ordinal, so the
	// ordinal stays missing and the create is asked for again on every
	// five-second pass, for as long as the object sits there
	// (docs/known-issues.md). What fires per pass is the difference between a
	// quiet wait and an event stream nobody can read past.
	if n := scalingEvents(r.Recorder.(*nonBlockingRecorder), "ServerCreated"); n != 0 {
		t.Errorf("ServerCreated events = %d, want none: the object was already there, "+
			"and this collision repeats every pass for as long as it lasts", n)
	}
}

// TestAPersistentServerIsCreatedCarryingItsRenderHash pins the stamp half of
// this task: a persistent server is created carrying the hash of what the
// operator would render for it, so a later pass can tell whether the spec has
// moved.
func TestAPersistentServerIsCreatedCarryingItsRenderHash(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createPersistentGroup(t, "survival", 1)

	f.reconcilePersistentGroup(t, r, "survival")

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

// TestAServerWithNoHashIsAdoptedRatherThanReplaced is the upgrade case, and it
// must assert both halves. Asserting only that the field gets filled would
// pass while every world restarted.
func TestAServerWithNoHashIsAdoptedRatherThanReplaced(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 1)
	f.reconcilePersistentGroup(t, r, "survival")

	// Blanked to simulate a server that predates spec.podHash.
	srv := f.server("survival-0")
	uidBefore := srv.UID
	srv.Spec.PodHash = ""
	if err := f.c.Update(f.ctx, srv); err != nil {
		t.Fatalf("blank the hash: %v", err)
	}

	f.reconcilePersistentGroup(t, r, "survival")

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

// TestAPersistentServerOnACordonedNodeIsCondemned pins acceptance criterion 5.
// Nothing in the production code is specific to persistent groups here -- that
// is the claim, and this is what holds it.
//
// The removal takes the shape a deletion takes once the Server controller has
// run: markReady drives that controller, so the drain finalizer is on the
// object and the delete leaves a deletion timestamp rather than removing the
// object outright. TestAPersistentGroupRemovesTheHighestOrdinal, which drives
// only the group, sees the other shape for exactly that reason.
func TestAPersistentServerOnACordonedNodeIsCondemned(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 1)
	f.reconcilePersistentGroup(t, r, "survival")

	srv := f.server("survival-0")
	f.markReady(t, srv.Name)
	node := f.ensureNode(t, "node-going-"+f.ns, false)
	pod, ok := f.pod(f.server(srv.Name).Status.PodName)
	if !ok {
		t.Fatal("pod not found")
	}
	f.bindPodToNode(t, pod, node.Name)
	f.ensureNode(t, node.Name, true)

	f.reconcilePersistentGroup(t, r, "survival")

	condemned, present := f.serverIfPresent("survival-0")
	if !present {
		t.Fatal("survival-0 is gone outright, which the drain finalizer should have prevented")
	}
	if condemned.DeletionTimestamp.IsZero() {
		t.Fatal("the persistent server on the cordoned node was not condemned; its players will be evicted instead of moved")
	}
}

// TestAPersistentGroupPublishesTheReadinessItsPodsSupport pins the removal of
// the group controller's own refusal. That refusal set Ready: False with
// reason NotImplementedInThisVersion on every persistent group unconditionally
// and slowed the group to a one-minute requeue; as of this task such a group
// builds ordinals that get pods, so the condition contradicted its own
// servers and the requeue delayed every removal decision behind it.
//
// Nothing replaced it, and the three assertions below are what say so rather
// than a fourth derivation nobody would maintain: no ServerGroup of either
// type publishes a Ready condition at all — readiness is status.phase, which
// derivePhase reads off ReadyReplicas against DesiredReplicas() for both kinds
// — and the requeue is the ordinary resync an ephemeral group gets.
func TestAPersistentGroupPublishesTheReadinessItsPodsSupport(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 1)
	f.reconcilePersistentGroup(t, r, "survival")
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

// clearFailedOrdinal takes away the corpse of a persistent ordinal down the
// one path that removes it for having failed: the Server controller's
// failed-retention path.
//
// It is a step of its own, and not folded into the failure that precedes it,
// because the two are on opposite sides of the state this test is about.
// Failing the server opens the backoff window; clearing the corpse costs an
// hour of the fixture clock and closes that window again. A helper that did
// both would make the waiting state unobservable -- which is exactly how the
// first draft of this test reported False and was right to.
//
// The clearing itself is why a persistent round is not shaped like the
// ephemeral ones above: pruneFailed runs only for an ephemeral group, and
// DecidePersistentSize holds an ordinal whatever phase its server is in, so
// nothing on the group's side removes a persistent server for having failed --
// its own removals answer to a lower spec.replicas or a departing node, and
// neither is happening here. Retention is what is left, which makes the
// advance below part of the mechanism rather than a convenience: without it
// the group has no second attempt to back off from.
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

// TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp is what the design owed
// and did not have. §3.5 of the persistent-groups design said of a claim that
// never binds that "the server goes Failed, the group reports Degraded, and
// 4d's per-group backoff stops it throwing the same ordinal at the same broken
// volume in a loop" -- and has since been rewritten, because the reporting
// half was false. The backoff half held: size() runs its CreateOrdinals loop
// under the same backoff.MayCreate as the ephemeral count. The reporting half
// did not: BackingOff and Degraded were published only inside
// an `if group.IsEphemeral()` block, so a persistent group stalled in silence
// and its phase read Pending, which is what a group that is merely still
// starting reads too.
//
// The round below is also the answer to a question the design got wrong in
// both directions, so it is worth naming here rather than leaving to whoever
// next reads §3.5: a persistent group does have a retry loop. Nothing on the
// group's side clears a persistent corpse for having failed, but the Server
// controller's failed-retention path does, and the ordinal is then created
// again -- which is why each round here ends by aging the corpse out. With
// survival-0 the only ordinal that ever fails, that loop is also the only way
// its count can reach the threshold at all, since CountFailures counts a
// corpse once; a group failing on six or more ordinals could reach it on six
// distinct first failures and never retry anything, so that half of the
// argument is this test's, not a general proof. The loop's period is
// spec.failedRetentionSeconds -- an hour at the CRD default, which is what
// this group carries -- so at that default the backoff's own windows (160s at
// the most) never delay an attempt; what the backoff does for this group is
// end the attempts, which is the last assertion below.
//
// survival-1 is the second ordinal spec §5.3 requires this test to carry, a
// healthy sibling standing the whole time survival-0 is not: with only one
// ordinal the minimum-over-required-ordinals rule and the maximum-over-all-
// views rule it replaced cannot disagree, so a group of one could reach
// Degraded under either and prove nothing about which is running.
//
// The failure driven here is a startup deadline rather than an unbound claim.
// envtest runs no provisioner and no scheduler, so a pod's volume never binds
// and never fails to bind -- nothing in this environment can make a claim
// stall a server. What the two have in common is the only thing this test is
// about: a server that never becomes playable, and a group that must say so.
// Whether an unbound claim really does end in Failed is a cluster question,
// and it belongs to the runbook.
func TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)

	f.reconcilePersistentGroup(t, r, "survival")
	// Brought up once and never touched again -- see the function doc for why.
	bringUpNamed(t, f, "survival-1")

	for i := int32(0); i < backoffGiveUpAt; i++ {
		f.reconcilePersistentGroup(t, r, "survival")
		if _, present := f.serverIfPresent("survival-0"); !present {
			t.Fatalf("round %d: the group did not rebuild its ordinal, so it stopped creating early", i+1)
		}
		f.failServerNeverReady(t, "survival-0")
		f.reconcilePersistentGroup(t, r, "survival")

		if i == 0 {
			// The waiting half, taken on the flank rather than after the loop:
			// once the count reaches the threshold BackingOff goes back to
			// False and this state is gone.
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

		// Last in the round, so the assertions above see the group while its
		// window is still open.
		f.clearFailedOrdinal(t, "survival-0")
	}
	f.reconcilePersistentGroup(t, r, "survival")

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
	// Given up, not waiting -- and the reason carries the real cause rather
	// than an all-clear.
	backingOff := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
	if backingOff == nil || backingOff.Status != metav1.ConditionFalse ||
		backingOff.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("BackingOff = %+v once the group gave up, want False/%s", backingOff,
			spawneryv1alpha1.ReasonCrashLoopBackoff)
	}
	// And the stall is real: the ordinal is not rebuilt, which is the
	// behaviour the condition exists to explain rather than to change.
	if _, present := f.serverIfPresent("survival-0"); present {
		t.Error("the group rebuilt its ordinal after giving up; the backoff gates persistent creates too")
	}
	// The healthy sibling was never touched again after coming up, and the
	// group reached Degraded anyway -- the assertions above would have
	// stopped short of the threshold under the old maximum-over-all-views
	// rule if survival-1 had so much as re-readied once per round.
	if got := f.server("survival-1").Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase of survival-1 = %q, want Ready: it was never failed", got)
	}
}

// TestAPersistentGroupCountsAFailureAfterItsGenerationMoves pins a defect
// found while verifying this milestone rather than one it set out to fix:
// ofGeneration -- built for the ephemeral count, where a generation change
// really does replace the population -- was the only filter CountFailures'
// call site applied, for a group of either type. spec.groupGeneration is
// stamped on a Server once at creation and never updated afterwards, so any
// edit to a persistent group's spec moves group.Generation out from under
// every ordinal it already has. Filtered by that, CountFailures would see an
// empty slice on every later pass and status.consecutiveFailures would freeze
// wherever it stood -- never reaching backoffGiveUpAt, so Degraded would
// never arrive, regardless of anything this milestone's own change does.
//
// The edit below touches spec.drain.timeoutSeconds, which reaches neither the
// pod hash (so the ordinal is not rebuilt into carrying the new generation
// itself) nor spec.replicas -- an edit an operator could make for a reason
// that has nothing to do with the ordinal's own health.
func TestAPersistentGroupCountsAFailureAfterItsGenerationMoves(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "outpost", 1)
	f.reconcilePersistentGroup(t, r, "outpost")
	if _, present := f.serverIfPresent("outpost-0"); !present {
		t.Fatalf("the group did not create its ordinal")
	}

	group := f.persistentGroup(t, "outpost")
	group.Spec.Drain.TimeoutSeconds++
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	// Observes the new generation with no failure yet on either side of it,
	// so the deliberate reset at a generation change (ConsecutiveFailures to
	// 0 -- a real feature, not what this test is about) has already happened
	// before the round below, and cannot be mistaken for what it is
	// checking.
	f.reconcilePersistentGroup(t, r, "outpost")

	f.failServerNeverReady(t, "outpost-0")
	f.reconcilePersistentGroup(t, r, "outpost")

	g := f.persistentGroup(t, "outpost")
	if got := g.Status.ConsecutiveFailures; got != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1: outpost-0 predates the group's current generation "+
			"and its failure must still be counted", got)
	}
}

// persistentGroup re-reads a ServerGroup named here, rather than the fixture's
// own, which is what reloadGroup does.
func (f *fixture) persistentGroup(t *testing.T, name string) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group %s: %v", name, err)
	}
	return group
}

// TestAPersistentGroupUpdatesOneOrdinalAtATime is the invariant, at the only
// layer that can show it: two ordinals, a spec change, and never two down at
// once across the whole sequence.
//
// The worst==0 assertion at the end is not decoration. Without it the test
// would pass just as well if the spec change never moved anything at all —
// exactly what a hash bug produces, and exactly the shape of non-discriminating
// assertion that this milestone's review found seven of.
func TestAPersistentGroupUpdatesOneOrdinalAtATime(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)
	f.reconcilePersistentGroup(t, r, "survival")
	f.markReady(t, "survival-0")
	f.markReady(t, "survival-1")

	group := f.persistentGroup(t, "survival")
	group.Spec.Image = "ghcr.io/spawnery/paper:1.21.4-0.2.0"
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("change the group's image: %v", err)
	}

	// reconcileGroupOnce runs the group reconciler, whose decision may create
	// a replacement ordinal or nominate a stale one for deletion, and then
	// reconciles every server that currently exists once -- the Server
	// reconciler is what actually creates a fresh ordinal's pod and what
	// carries a draining one's finalizer removal forward one step.
	reconcileGroupOnce := func() {
		f.reconcilePersistentGroup(t, r, "survival")
		for _, name := range f.serverNamesOfGroup(t, "survival") {
			f.reconcile(name)
		}
	}
	// driveNewPodsReady plays the kubelet and the in-game agent for every
	// server that is not on its way out: it never touches a server carrying a
	// deletion timestamp, because a draining server's pod is meant to go away,
	// not to be marked ready.
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

// TestTheStorageResizeMessageComesFromTheLowestOrdinal pins the tie-break
// storageResizeCondition argues for at length and nothing exercised: with two
// unhealthy claims, the message must come from the lower ordinal every time,
// so that an operator reads one steady signal rather than watching two
// messages alternate between reconciles. Reversing ordinalBefore to pick the
// highest is a live mutation only against a case where the two views carry
// *different* messages -- one where they agreed would pass either way.
//
// The views are built here rather than driven through a cluster because
// storageResizeCondition is a pure function of them, and the two distinct
// messages are the whole fixture.
func TestTheStorageResizeMessageComesFromTheLowestOrdinal(t *testing.T) {
	views := []ServerView{
		{Name: "survival-1", Ordinal: ptr.To(int32(1)), ResizeError: "ordinal 1's message"},
		{Name: "survival-0", Ordinal: ptr.To(int32(0)), ResizeError: "ordinal 0's message"},
	}
	// Listed highest-first above, so "the first one found" would answer with
	// ordinal 1; collectViews makes no ordering promise, which is the whole
	// reason the tie-break exists.
	got := storageResizeCondition(views)
	if got.Status != metav1.ConditionFalse {
		t.Fatalf("Status = %v, want False; two claims carry an error", got.Status)
	}
	if got.Message != "ordinal 0's message" {
		t.Errorf("Message = %q, want ordinal 0's; the lowest ordinal is the tie-break that stays put",
			got.Message)
	}

	// A nil ordinal sorts last, so it loses to any numbered view and wins only
	// when it is the sole candidate.
	nameless := ServerView{Name: "survival-a7kd", ResizeError: "a squatter's message"}
	if got := storageResizeCondition([]ServerView{nameless, views[1]}); got.Message != "ordinal 0's message" {
		t.Errorf("Message = %q, want ordinal 0's; a nil ordinal sorts after a numbered one", got.Message)
	}
	if got := storageResizeCondition([]ServerView{nameless}); got.Message != "a squatter's message" {
		t.Errorf("Message = %q, want the squatter's; it is the only candidate there is", got.Message)
	}
}

// TestAGroupSaysWhenItsStorageClassCannotGrow exercises growClaim's
// synchronous rejection through envtest's real admission -- the same
// mechanism TestGrowingStorageSizePatchesTheClaim (server_controller_test.go)
// confirmed by hand: envtest runs no CSI driver and no external-resizer, so
// a StorageClass with allowVolumeExpansion: false is real (creating one
// needs no controller behind it), and the API server's own resize admission
// refuses the grow patch against it exactly as a real cluster would. The
// claim's Bound status is faked the same way that test fakes it, for the
// same reason: nothing in envtest ever binds a claim to a volume, and an
// unbound claim is refused resize for an unrelated reason of its own.
//
// What this does not prove is the asynchronous shape of the same failure --
// a driver that accepts a resize and fails it only later, reported through
// the claim's own ControllerResizeError or NodeResizeError condition.
// resizeConditionError (server_controller.go) is what reads that, but
// nothing here hand-writes either condition onto a claim to exercise it.
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

	// storage.storageClassName is immutable once set (CEL rule on
	// ServerGroupSpec), so it has to be there at creation -- built by hand
	// rather than through createPersistentGroup for that one reason, the same
	// as TestGrowingStorageSizePatchesTheClaim.
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

	f.reconcilePersistentGroup(t, r, "g")
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

	// The Server reconciler is the one that runs growClaim and meets the
	// refusal; the group reconciler that follows is the one that reads
	// status.storageResizeError back into the condition under test.
	f.reconcile("g-0")
	f.reconcilePersistentGroup(t, r, "g")

	got := f.persistentGroup(t, "g")
	cond := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionStorageResize)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected StorageResize=False, got %+v", cond)
	}
	// growClaim cannot tell this rejection apart from Task 7's unbound-claim
	// one by the API error alone (see growClaim's own doc comment), so the
	// message does not claim allowVolumeExpansion caused it -- this only
	// checks that an operator reading it is pointed at the field to check
	// first, alongside the API's own error, which is what actually does.
	if !strings.Contains(cond.Message, "allowVolumeExpansion") {
		t.Fatalf("the message does not point at the field to check: %q", cond.Message)
	}

	// The assertion that matters: it pins the separation the condition
	// exists for. Without it, folding the refusal into Degraded as well
	// would satisfy the assertion above just as easily.
	degraded := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if degraded != nil && degraded.Status == metav1.ConditionTrue {
		t.Fatal("a storage class that cannot expand is not a degraded group")
	}
}

// TestAGroupThatGaveUpSaysSoEvenWhileItsNetworkIsDead pins which of two true
// messages an operator sees first when a group has both given up and lost its
// Network. The BackingOff/Degraded switch in Reconcile tests !sized before
// backoff.GaveUp, so without this ruling such a group reports "backoff is not
// being decided: the group's network is not usable" -- even though the failure
// count that produced GaveUp is computed from the views before sized is known
// and does not depend on the Network at all.
//
// Both are true. They are not equally useful. The Network's unusability is
// transient and already carried by Accepted: False, so repeating it here spends
// the only two conditions that can report the give-up on a fact reported
// elsewhere. Giving up is terminal — it takes a spec edit — so an operator who
// reads only "the group's network is not usable", fixes the Network and walks
// away has been told the truth and left with a group that still creates
// nothing. Restoring the old order fails this test, and it also asserts
// Accepted is still False, so the ruling cannot quietly become hiding the
// Network rather than declining to repeat it.
//
// How this state is reached is the part that cost the most to find. The
// obvious construction -- point the group's networkRef at something missing --
// cannot reach it at all, because editing the group is a spec change and a
// spec change deliberately clears the failure streak. The Network has to
// become unusable *without* the group being touched: deleted, or made
// unaccepted by a rival. Any future test about this pair has to break the
// Network, never the group.
func TestAGroupThatGaveUpSaysSoEvenWhileItsNetworkIsDead(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// backoffGiveUpAt rounds, each one failing whatever the group built.
	//
	// A whole floor failing at once used to be enough on its own: the count
	// was corpses, so one
	// round at minReplicas 6 spent the entire budget. Since the count became
	// rounds it takes six of them, and building the give-up that way is also
	// the honest construction — it is what a genuinely broken image does.
	// failServerNeverReady rather than failServer: the latter walks a server up
	// to Ready first, and a success ends the streak this test is building.
	// Never-ready is also the broken-image shape the backoff exists for.
	f.setMinReplicas(t, 1)
	for round := int32(0); round < backoffGiveUpAt; round++ {
		f.reconcileGroup(t, r)
		for _, name := range f.serverNamesOfGroup(t, "lobby") {
			if f.server(name).Status.Phase != string(phase.Failed) {
				f.failServerNeverReady(t, name)
			}
		}
		// Past the window this round earned, or the next pass creates nothing
		// and there is no next round to fail.
		f.clock.Advance(10 * time.Minute)
	}
	f.reconcileGroup(t, r)
	if got := f.reloadGroup(t).Status.ConsecutiveFailures; got < backoffGiveUpAt {
		t.Fatalf("consecutiveFailures = %d after %d rounds, want at least %d; this test "+
			"needs a group that has actually given up", got, backoffGiveUpAt, backoffGiveUpAt)
	}

	// And then the Network is taken away underneath it. Deleted rather than
	// re-pointed, and that is not a stylistic choice: editing the group's
	// NetworkRef is a spec change, and a spec change deliberately clears the
	// failure streak — "the operator's answer to whatever broke". A test that
	// broke the Network by editing the group would therefore destroy the very
	// state it is about, and did, on the first attempt at writing it.
	if err := f.c.Delete(f.ctx, f.network); err != nil {
		t.Fatalf("delete the Network: %v", err)
	}

	f.reconcileGroup(t, r)

	got := f.reloadGroup(t)
	degraded := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Fatalf("Degraded = %+v, want reason %s. The give-up outlives the Network problem "+
			"and needs a spec edit; the Network's own trouble is on Accepted",
			degraded, spawneryv1alpha1.ReasonCrashLoopBackoff)
	}
	if !strings.Contains(degraded.Message, "change the group's spec") {
		t.Errorf("message = %q, want it to name the remedy the operator actually has to apply",
			degraded.Message)
	}
	// And the Network's trouble is still reported, on the condition that owns it.
	accepted := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse {
		t.Errorf("Accepted = %+v, want False: moving the give-up first must not hide the "+
			"Network, only stop repeating it", accepted)
	}
}

// TestAParkedPersistentGroupIsReadyRatherThanPending closes the entry in
// docs/known-issues.md: "a Persistent group with replicas: 0 reports Pending
// forever."
//
// Zero is a deliberate operator action rather than an edge case. spec.replicas:
// 0 is the accepted way to park a persistent group and keep its claims, because
// deleting the group would leave the claims behind but take the ordinals'
// Server objects with it. Such a group published Accepted: True,
// Degraded: False, replicas: 0, readyReplicas: 0 — and phase: Pending, which
// was the only field that was not true.
func TestAParkedPersistentGroupIsReadyRatherThanPending(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "parked", 0)

	f.reconcilePersistentGroup(t, r, "parked")

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

// TestAGroupShortOfItsTargetIsStillPending is the other half, and it is what
// says the change above decided one pair rather than loosening the rule. The
// clause that was removed also read on a group with zero ready and several
// wanted, and that group must still be Pending.
func TestAGroupShortOfItsTargetIsStillPending(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "wanting", 2)

	f.reconcilePersistentGroup(t, r, "wanting")

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

// TestASquatterOnAnOrdinalNameSaysSoOnTheGroup closes the entry in
// docs/known-issues.md: "a squatter can stall an ordinal silently."
//
// A persistent ordinal's name is derived, <group>-<ordinal>, so anything
// created by hand under that name takes it. DecidePersistentSize reads
// spec.ordinal rather than parsing names, so the squatter never enters its held
// map and the group goes on believing the ordinal is missing; the create then
// returns AlreadyExists, which the reconciler treats as success because that is
// the right answer for the other cause of the same error — a cache one
// generation behind. So the group retried every five seconds forever, and
// nothing on its conditions, events or logs distinguished that from an
// ordinary transient collision. The tell was a kubectl jsonpath on the object.
func TestASquatterOnAnOrdinalNameSaysSoOnTheGroup(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// Something else holds the name a persistent ordinal will want, without
	// carrying the ordinal that would make it a member.
	squatter := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "held-0", Namespace: f.ns},
		// GroupRef and nothing else. spec.ordinal is what makes an object a
		// member of a persistent group, and its absence is the whole point.
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "held"},
		},
	}
	if err := f.c.Create(f.ctx, squatter); err != nil {
		t.Fatalf("create the squatter: %v", err)
	}
	f.createPersistentGroup(t, "held", 1)

	f.reconcilePersistentGroup(t, r, "held")

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
	// The message has to name the object and say what is wrong with it, or a
	// reader is back to the kubectl jsonpath this replaces.
	for _, want := range []string{"held-0", "spec.ordinal"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message = %q, want it to contain %q", cond.Message, want)
		}
	}
}

// TestAGroupWithNoSquatterSaysTheOrdinalsAreAvailable is the False side. A
// condition that can only go True stops meaning anything the first time it
// fires, and this one is set from each pass's own finding rather than latched.
func TestAGroupWithNoSquatterSaysTheOrdinalsAreAvailable(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "clear", 1)

	f.reconcilePersistentGroup(t, r, "clear")

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "clear", Namespace: f.ns}, got); err != nil {
		t.Fatalf("get the group: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionOrdinalBlocked)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("OrdinalBlocked = %+v on a group whose ordinals are free, want False", cond)
	}
}

// TestACacheOneGenerationBehindIsNotReportedAsASquatter is the distinction the
// whole fix turns on, and it went unpinned until a mutation showed it: treating
// the transient cause as the permanent one broke no test.
//
// AlreadyExists on a derived ordinal name has two causes needing opposite
// answers. A squatter holds the name forever and must be reported. A cache that
// has not yet shown this reconciler its own creation resolves itself in a pass
// or two, and reporting it would put a condition on the group for something
// that is already fixed — the sort of alarm that teaches an operator to ignore
// the condition.
//
// Driven against reportSquatter directly. Through a reconcile it is not
// reachable at all: the fixture's client is direct, so DecidePersistentSize and
// the Create see the same objects and the group never tries to create an
// ordinal that already carries its own number.
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

	// And an object carrying a *different* ordinal is a squatter, not a lagging
	// cache — the field matching is what separates them, not its presence.
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

// reportProgressing is a pure function of the group and its views, so its
// whole vocabulary fits in a table. The states below are the ones an operator
// meets; the two that matter most are the last two, which the phase cannot
// tell apart from the first.
func TestProgressingSaysWhetherTheGroupHasArrived(t *testing.T) {
	const gen = 7
	view := func(p phase.Phase, generation int64) ServerView {
		return ServerView{Phase: p, Generation: generation}
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
			metav1.ConditionTrue, spawneryv1alpha1.ReasonServersStarting, "1 server(s) of generation 7",
		},
		{
			"a pending server counts as coming",
			[]ServerView{view(phase.Ready, gen), view(phase.Pending, gen)},
			metav1.ConditionTrue, spawneryv1alpha1.ReasonServersStarting, "1 server(s) of generation 7",
		},
		{
			"the new generation is up but the old one is still here",
			[]ServerView{view(phase.Ready, gen), view(phase.Ready, gen-1)},
			metav1.ConditionTrue, spawneryv1alpha1.ReasonReplacingServers, "earlier generation",
		},
		{
			"both waits at once say both",
			[]ServerView{view(phase.Starting, gen), view(phase.Ready, gen-1)},
			metav1.ConditionTrue, spawneryv1alpha1.ReasonServersStarting, "still being replaced",
		},
		{
			// Being shed on purpose. A scale-down has arrived, it is tidying.
			"a draining server of the current generation",
			[]ServerView{view(phase.Ready, gen), view(phase.Draining, gen)},
			metav1.ConditionFalse, spawneryv1alpha1.ReasonAtDesiredState, "",
		},
		{
			// Degraded is what says this, and its replacement will be Pending
			// on the next pass, which this counts then.
			"a failed server of the current generation",
			[]ServerView{view(phase.Ready, gen), view(phase.Failed, gen)},
			metav1.ConditionFalse, spawneryv1alpha1.ReasonAtDesiredState, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &spawneryv1alpha1.ServerGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "lobby", Generation: gen},
			}
			reportProgressing(group, tc.views)
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

// The lag provisionalCapacity's own comment describes, driven.
//
// An ephemeral group runs above spec.scaling.minReplicas to keep spareSlots
// free, and DesiredReplicas() is only that floor — so a group DecideSize has
// taken to two satisfies the phase with one server ready while the other is
// still starting. The phase is not wrong about what it says (the group is
// serving); it just cannot say this. Progressing can.
func TestAGroupAboveItsFloorIsReadyAndStillProgressing(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)
	first := f.listServers(t)
	if len(first) != 1 {
		t.Fatalf("got %d servers, want minReplicas = 1", len(first))
	}
	// Ready, and busy enough that the 40 spare slots this group wants are no
	// longer free — so the next pass orders another server it has not got.
	f.markReadyWithPlayers(t, first[0].Name, 70)

	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != 2 {
		t.Fatalf("got %d servers, want a second one ordered for the spare slots", got)
	}

	// A third pass, because the views this status is built from are collected
	// before the creates of the same pass — so the server just ordered is
	// first seen by the next one. reportProgressing says so in its own
	// comment; it is the property every other field here already has.
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

// The group finds its own pods and answers for them.
//
// The unit test beside groupRotationCondition covers the decision; this covers
// the parts it cannot reach — the selector matching real pods, the list, and
// the digest coming off the Network. A selector that matched nothing would
// pass every case of that unit test by having no stamps to judge, and report a
// group mid-rotation as being in sync.
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

	// Rolled: the stamp catches up and so does the group.
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

// taintNode puts a drain taint on a node the fixture already created.
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

// The taint half of IsDeparting, against a real Node object.
//
// Both halves were covered before this only where it costs least: IsDeparting
// itself takes a corev1.Node built in memory, and setup_test.go proves
// Options.DrainTaintKeys reaches the reconciler. What nothing drove was a taint
// on a Node the API server holds, read back through nodeDeparting on the path a
// condemnation actually takes — and that is the half an autoscaler exercises,
// because cluster-autoscaler taints and deletes without touching
// spec.unschedulable unless --cordon-node-before-terminating is on, which it is
// not by default.
//
// The second subtest is the configuration `paulwtf` runs: measured 2026-08-25,
// its Deployment passes no --drain-taint at all. Harmless there, because the
// cluster has three fixed nodes and no autoscaler — and exactly the state this
// entry warns would make a scale-in invisible.
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

// TestADuplicatedOrdinalReachesTheGroupsConditions is the half of the fix that
// a rule test cannot show: that the refusal is visible. The rule declining to
// nominate is only half an answer -- a group that quietly stopped shrinking
// would look exactly like one that had nothing to shrink.
func TestADuplicatedOrdinalReachesTheGroupsConditions(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)
	f.reconcilePersistentGroup(t, r, "survival")

	// A second server carrying ordinal 1, under a name this operator would
	// never choose -- restored from a backup, or copied. It is a member of the
	// group by label, which is what puts it in the views.
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

	f.reconcilePersistentGroup(t, r, "survival")

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

	// And neither server was removed. This is the part that would have cost a
	// world: with replicas at 2 nothing is surplus here, but the pair is
	// exactly what the stale and resize paths would also have nominated from.
	names := f.serverNamesOfGroup(t, "survival")
	if len(names) != 3 {
		t.Errorf("servers = %v, want all three left standing", names)
	}
}

// TestTheOrdinalConditionClearsWhenTheDuplicateGoes pins the other side. A
// condition that only goes True stops meaning anything the first time it
// fires, which is why clearOrdinalBlocked runs every pass.
func TestTheOrdinalConditionClearsWhenTheDuplicateGoes(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2)
	f.reconcilePersistentGroup(t, r, "survival")

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
	f.reconcilePersistentGroup(t, r, "survival")
	if err := f.c.Delete(f.ctx, dup); err != nil {
		t.Fatalf("delete the duplicate: %v", err)
	}

	f.reconcilePersistentGroup(t, r, "survival")

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "survival", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	blocked := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionOrdinalBlocked)
	if blocked == nil || blocked.Status != metav1.ConditionFalse {
		t.Errorf("OrdinalBlocked = %+v, want False once the duplicate is gone", blocked)
	}
}

// TestAFailedRetireeIsNamedOnProgressing closes the silent half of
// docs/known-issues.md's milestone 4b entry, since removed. spec.retire is the update
// budget's one signal and it survives the server failing, so a server the
// group patched it onto and that then failed holds a maxUnavailable slot for
// its whole failedRetentionSeconds -- an hour by default -- and the changeover
// simply stops. Before this there was no condition, no event and nothing else
// saying so: the group reported "still being replaced" for the whole window,
// which is true and useless.
func TestAFailedRetireeIsNamedOnProgressing(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Generation: 2},
		Spec:       spawneryv1alpha1.ServerGroupSpec{FailedRetentionSeconds: 3600},
	}
	views := []ServerView{
		{Name: "lobby-new", Generation: 2, Phase: phase.Ready},
		// The stuck one: retiring, and dead.
		{Name: "lobby-old", Generation: 1, Phase: phase.Failed, Retire: true},
	}

	reportProgressing(group, views)

	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Progressing = %+v, want True", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonRetireeStuck {
		t.Fatalf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonRetireeStuck)
	}
	// The name and the remedy, because the remedy is per server: deleting that
	// one server returns the slot at once, and a count would leave an operator
	// listing every server to find which.
	for _, want := range []string{"lobby-old", "spec.retire", "3600", "Deleting it"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message %q does not mention %q", cond.Message, want)
		}
	}
}

// TestAnOrdinaryRetireeIsNotReportedAsStuck keeps the check from firing on the
// normal path. A retiree that is draining is the update working, not the
// update stopped, and a condition that fired on it would name every changeover
// this operator ever performs.
func TestAnOrdinaryRetireeIsNotReportedAsStuck(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Generation: 2},
		Spec:       spawneryv1alpha1.ServerGroupSpec{FailedRetentionSeconds: 3600},
	}
	for _, p := range []phase.Phase{phase.Retiring, phase.Draining, phase.Ready} {
		t.Run(string(p), func(t *testing.T) {
			group.Status.Conditions = nil
			reportProgressing(group, []ServerView{
				{Name: "lobby-new", Generation: 2, Phase: phase.Ready},
				{Name: "lobby-old", Generation: 1, Phase: p, Retire: true},
			})
			cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
			if cond != nil && cond.Reason == spawneryv1alpha1.ReasonRetireeStuck {
				t.Errorf("a %s retiree was reported as stuck: %q", p, cond.Message)
			}
		})
	}
}

// liveServers is the count docs/known-issues.md recorded touching zero: the
// servers that count toward the group's size, which is every one that is
// neither leaving nor Failed.
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

// TestTheGroupHasNoLiveServerWhileItBacksOffAndRebuildsAfter is the
// explanation for an observation that sat in docs/known-issues.md as
// uninvestigated: a group's count of live servers briefly touched zero under
// sustained churn before recovering.
//
// It is the backoff, and it is not a defect. Creates are gated on
// backoff.MayCreate, and a Failed server does not count toward the group's
// size (ServerView.countsTowardSize), so between the failure of the last live
// server and the expiry of the window the group genuinely has none. That is
// the whole point of the window: a group whose servers are failing to start
// must stop rebuilding them for a while, and at minReplicas 1 "a while with
// none" is the only shape that can take.
//
// What makes it a state rather than a mystery is that the group says so while
// it lasts. TestBackingOffConditionNamesTheCountAndTheWait pins the condition;
// this pins the count beside it, and then the recovery, which is the half the
// original observation reported and nothing asserted.
//
// The churn that produced it was Task 5's 20-second startup deadline, which is
// short enough that every server fails it. Nothing here needs to be that
// realistic: one failure opens the window, and the window is what the count
// follows.
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

	// Zero live, and no replacement ordered. The corpse is still there --
	// it is kept for diagnosis -- so status.replicas does not read zero and
	// only this count does.
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

	// Past the first window, which is backoffBase.
	f.clock.Advance(backoffBase + time.Second)
	f.reconcileGroup(t, r)

	if got := f.liveServers(t); got != 1 {
		t.Fatalf("%d live servers after the window expired, want 1: the count recovers or "+
			"the observation was not the backoff after all", got)
	}
}
