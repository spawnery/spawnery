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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// stranger builds a ConfigMap at a group's rendered name that the group does
// not own, and returns it as it stands in the API server.
//
// labelled decides which of the two collisions this is. With the label the
// object is visible to a restricted cache, so CreateOrUpdate's Get finds it
// and the ownership check refuses it; without, a real operator's Get misses
// and the Create comes back AlreadyExists. This package's fixture reads
// through a direct client, so both land on the ownership check here -- which
// is why TestAnInvisibleCollisionIsRefusedToo below builds a real filtered
// cache instead of relying on this.
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

// reread returns the ConfigMap as the API server holds it now.
func (f *fixture) reread(t *testing.T, name string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, cm); err != nil {
		t.Fatalf("re-read %s: %v", name, err)
	}
	return cm
}

// assertUntouched compares the object against itself as it stood before the
// reconcile.
//
// ResourceVersion carries the whole claim, and it is a stronger one than
// checking the fields this operator would have changed: the API server moves
// it on any write at all, so an unchanged version means the operator issued
// none. The data and owner references are asserted beside it anyway, because
// a failure that says only "the version moved" sends whoever reads it looking
// for which write, and these two are the writes the defect made.
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
	// The message has to name the object, because "delete it" is useless
	// advice without a name and this is a collision the operator cannot
	// resolve on its own.
	if degraded.Message == "" {
		t.Error("the condition carries no message")
	}
	if phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", phase)
	}
}

// TestServerGroupRefusesAConfigMapItDoesNotOwn is the defect itself.
// SetControllerReference refuses an object owned by a *different* controller
// and silently adopts one owned by nobody, so an ownerless ConfigMap at the
// rendered name used to be rewritten and given an owner reference that hands
// it to the garbage collector when the group goes.
func TestServerGroupRefusesAConfigMapItDoesNotOwn(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	name := podspec.GroupConfigMapName(f.group.Name, podspec.RoleServer)
	before := f.stranger(t, name, true, nil)

	// Not reconcileGroup: that helper fails the test on any error, and the
	// refusal deliberately returns none -- it writes a status and requeues.
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

	// And no Server was created behind the refusal. The ConfigMap is what a
	// pod's projected volume names, so a group that started servers anyway
	// would be starting them against somebody else's configuration.
	if servers := f.listServers(t); len(servers) != 0 {
		t.Errorf("servers = %d, want none created while the group cannot write its own configuration", len(servers))
	}
}

// TestProxyGroupRefusesAConfigMapItDoesNotOwn is the same defect on the other
// controller. The two reconcilers had the same code and would have needed the
// same fix twice; they now share one, and this is what says so.
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

// TestAConfigMapOwnedByAPredecessorIsRefused is the case the UID half of the
// rule exists for, and the one a Kind-and-name check would walk straight
// past. A delete-and-recreate of a group leaves the old group's ConfigMap
// standing until the garbage collector catches up: same name, same kind, and
// an object already condemned.
func TestAConfigMapOwnedByAPredecessorIsRefused(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	name := podspec.GroupConfigMapName(f.group.Name, podspec.RoleServer)

	controller := true
	before := f.stranger(t, name, true, &metav1.OwnerReference{
		APIVersion: spawneryv1alpha1.GroupVersion.String(),
		Kind:       "ServerGroup",
		Name:       f.group.Name,
		// Everything matches except the identity, which is the whole point.
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

// TestTheGroupRecoversWhenTheCollisionGoesAway is the other half of a
// refusal: it has to be a state and not a latch. The remedy the message gives
// is "delete it", so deleting it has to be enough -- without a restart, and
// without the Degraded condition outliving its cause.
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
	// And the group's own ConfigMap is there now, owned by it.
	written := f.reread(t, name)
	if len(written.OwnerReferences) != 1 || written.OwnerReferences[0].UID != group.UID {
		t.Errorf("owner references = %+v, want this group's", written.OwnerReferences)
	}
}

// TestAnInvisibleCollisionIsRefusedToo covers the other shape, and the more
// likely one: a colliding ConfigMap that does *not* carry
// podspec.LabelManagedBy.
//
// cmd/spawnery-operator narrows the manager's ConfigMap cache to that label,
// so such an object is invisible to the reconciler's Get. CreateOrUpdate
// therefore never reaches its mutate closure -- the ownership check the tests
// above exercise is not consulted at all -- and goes on to a Create the API
// server rejects as AlreadyExists. Before this was mapped to the same
// sentinel, that was a bare error and an endless requeue with nothing on the
// group.
//
// It needs a real filtered cache, for the reason bootstrap_test.go's
// restrictedCacheClient gives: every other test in this package reads through
// a direct client, where an unlabelled ConfigMap is perfectly visible and this
// branch is unreachable.
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
