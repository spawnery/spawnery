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
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// stranger builds a ConfigMap at a group's rendered name that the group does
// not own. labelled only matters to a filtered cache; this package's direct
// client lets both shapes reach the ownership check.
func (f *fixture) stranger(t *testing.T, name string, labelled bool, owner *metav1.OwnerReference) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Data:       map[string]string{podspec.ConfigValuesKey: "someone-elses: document\n"},
	}
	if labelled {
		cm.Labels = map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue}
	}
	if owner != nil {
		cm.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	if err := f.c.Create(f.ctx, cm); err != nil {
		t.Fatalf("create the colliding ConfigMap: %v", err)
	}
	return cm
}

func (f *fixture) reread(t *testing.T, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, cm); err != nil {
		t.Fatalf("re-read %s: %v", name, err)
	}
	return cm
}

// assertUntouched relies on ResourceVersion: unchanged means the operator
// issued no write. Data and owner references are compared too, so a failure
// names which write happened.
func assertUntouched(t *testing.T, before, after *corev1.ConfigMap) {
	t.Helper()
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("resourceVersion %s -> %s: the operator wrote to a ConfigMap it does not own",
			before.ResourceVersion, after.ResourceVersion)
	}
	if got, want := after.Data[podspec.ConfigValuesKey], before.Data[podspec.ConfigValuesKey]; got != want {
		t.Errorf("%s = %q, want %q -- the operator rewrote somebody else's document",
			podspec.ConfigValuesKey, got, want)
	}
	if len(after.OwnerReferences) != len(before.OwnerReferences) {
		t.Errorf("owner references = %+v, want %+v -- an adopted object is deleted with the group",
			after.OwnerReferences, before.OwnerReferences)
	}
}

func assertRefusedOnStatus(t *testing.T, conditions []metav1.Condition, phase string) {
	t.Helper()
	degraded := meta.FindStatusCondition(conditions, spawneryv1alpha1.ConditionDegraded)
	if degraded == nil {
		t.Fatalf("conditions = %+v, want a %s condition", conditions, spawneryv1alpha1.ConditionDegraded)
	}
	if degraded.Status != metav1.ConditionTrue {
		t.Errorf("Degraded = %s, want True", degraded.Status)
	}
	if degraded.Reason != spawneryv1alpha1.ReasonConfigMapNotOurs {
		t.Errorf("reason = %q, want %q", degraded.Reason, spawneryv1alpha1.ReasonConfigMapNotOurs)
	}
	// The operator cannot resolve this itself, so the message must name it.
	if degraded.Message == "" {
		t.Error("the condition carries no message")
	}
	if phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", phase)
	}
}

// SetControllerReference silently adopts an ownerless object, which would
// hand somebody else's ConfigMap to the garbage collector with the group.
func TestServerGroupRefusesAConfigMapItDoesNotOwn(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	name := podspec.GroupConfigMapName(f.group.Name, podspec.RoleServer)
	before := f.stranger(t, name, true, nil)

	// Not reconcileGroup: the refusal returns no error, it writes a status and
	// requeues.
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.group.Name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertUntouched(t, before, f.reread(t, name))

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	assertRefusedOnStatus(t, group.Status.Conditions, group.Status.Phase)

	// Pods would start against somebody else's configuration.
	if servers := f.listServers(t); len(servers) != 0 {
		t.Errorf("servers = %d, want none created while the group cannot write its own configuration", len(servers))
	}
}

func TestProxyGroupRefusesAConfigMapItDoesNotOwn(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	name := podspec.GroupConfigMapName("gateway", podspec.RoleProxy)
	before := f.stranger(t, name, true, nil)

	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gateway", Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertUntouched(t, before, f.reread(t, name))

	group := &spawneryv1alpha1.ProxyGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	assertRefusedOnStatus(t, group.Status.Conditions, group.Status.Phase)

	if pods := f.proxyPods("gateway"); len(pods) != 0 {
		t.Errorf("proxy pods = %d, want none started against a configuration the operator could not write", len(pods))
	}
}

// A delete-and-recreate leaves the old group's ConfigMap until the garbage
// collector catches up: same kind and name, a different owner UID.
func TestAConfigMapOwnedByAPredecessorIsRefused(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	name := podspec.GroupConfigMapName(f.group.Name, podspec.RoleServer)

	controller := true
	before := f.stranger(t, name, true, &metav1.OwnerReference{
		APIVersion: spawneryv1alpha1.GroupVersion.String(),
		Kind:       "ServerGroup",
		Name:       f.group.Name,
		UID:        f.group.UID + "-predecessor",
		Controller: &controller,
	})

	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.group.Name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertUntouched(t, before, f.reread(t, name))
}

// A refusal is a state, not a latch: deleting the object must be enough.
func TestTheGroupRecoversWhenTheCollisionGoesAway(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	name := podspec.GroupConfigMapName(f.group.Name, podspec.RoleServer)
	cm := f.stranger(t, name, true, nil)

	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.group.Name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := f.c.Delete(f.ctx, cm); err != nil {
		t.Fatalf("delete the collision: %v", err)
	}

	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	degraded := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if degraded != nil && degraded.Reason == spawneryv1alpha1.ReasonConfigMapNotOurs {
		t.Errorf("Degraded still reads %q after the collision was removed", degraded.Reason)
	}
	written := f.reread(t, name)
	if len(written.OwnerReferences) != 1 || written.OwnerReferences[0].UID != group.UID {
		t.Errorf("owner references = %+v, want this group's", written.OwnerReferences)
	}
}

// The manager's ConfigMap cache is narrowed to podspec.LabelManagedBy, so an
// unlabelled collision is invisible to Get and surfaces as AlreadyExists on
// Create. Needs a real filtered cache; the fixture's direct client sees it.
func TestAnInvisibleCollisionIsRefusedToo(t *testing.T) {
	f := newFixture(t)
	name := podspec.GroupConfigMapName(f.group.Name, podspec.RoleServer)
	before := f.stranger(t, name, false, nil)

	r := groupReconciler(f)
	r.Client = restrictedCacheClient(t, f.ctx)

	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.group.Name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	assertUntouched(t, before, f.reread(t, name))

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	assertRefusedOnStatus(t, group.Status.Conditions, group.Status.Phase)
}

// The budget does not depend on the group's ConfigMap.
func TestAForeignConfigMapDoesNotStopTheServerBudget(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)
	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	f.reconcileGroup(t, r)
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 0 {
		t.Fatalf("minAvailable = %d on an empty server before the collision, want 0", got)
	}

	// The collision arrives while the group is serving.
	name := podspec.GroupConfigMapName(f.group.Name, podspec.RoleServer)
	if err := f.c.Delete(f.ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns}}); err != nil {
		t.Fatalf("delete the group's ConfigMap: %v", err)
	}
	f.stranger(t, name, true, nil)
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.group.Name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	assertRefusedOnStatus(t, group.Status.Conditions, group.Status.Phase)
	if got := f.groupPDB(t).Spec.MinAvailable.IntValue(); got != 1 {
		t.Errorf("minAvailable = %d after the collision, want 1 -- the pod now carries a player", got)
	}
	for _, s := range f.listServers(t) {
		if !s.DeletionTimestamp.IsZero() {
			t.Errorf("server %s was marked for deletion over a ConfigMap collision", s.Name)
		}
	}
}

func TestAForeignConfigMapDoesNotStopTheProxyBudget(t *testing.T) {
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
	f.reportProxyPlayers(t, pods[0], 0)
	f.reportProxyPlayers(t, pods[1], 0)
	f.reconcileProxyGroup(r, "gateway")

	name := podspec.GroupConfigMapName("gateway", podspec.RoleProxy)
	if err := f.c.Delete(f.ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns}}); err != nil {
		t.Fatalf("delete the group's ConfigMap: %v", err)
	}
	f.stranger(t, name, true, nil)
	f.reportProxyPlayers(t, pods[0], 3)
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gateway", Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	pdb := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: podspec.GroupPDBName("gateway", podspec.RoleProxy), Namespace: f.ns}, pdb); err != nil {
		t.Fatalf("get proxy PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("minAvailable = %v after the collision, want 1 -- one proxy carries players", pdb.Spec.MinAvailable)
	}
	got := f.proxyGroup("gateway")
	assertRefusedOnStatus(t, got.Status.Conditions, got.Status.Phase)
}
