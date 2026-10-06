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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// Every sizing rule computes zero for a group with no sizing fields, and that
// zero must never be acted on.
func TestOnDemandGroupCreatesAndDeletesNothing(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	// The persistent rule only sees members with an ordinal; at spec.replicas
	// nil this one is surplus.
	stray := f.createOnDemandMemberWithOrdinal(t, group, "decaf", 0)

	for i := 0; i < 3; i++ {
		f.reconcileNamedGroup(t, r, group.Name)
	}

	var servers spawneryv1alpha1.ServerList
	if err := f.c.List(f.ctx, &servers, client.InNamespace(f.ns)); err != nil {
		t.Fatalf("list servers: %v", err)
	}
	if got, want := f.serverNamesOfGroup(t, group.Name), []string{member.Name, stray.Name}; !slices.Equal(got, want) {
		t.Fatalf("servers = %v, want %v: the group built or removed a member nobody asked it about", got, want)
	}
	for _, srv := range servers.Items {
		if !srv.DeletionTimestamp.IsZero() {
			t.Fatalf("the group condemned %s, a member nobody asked it to remove", srv.Name)
		}
	}
}

// The world stays, so the key is free and its owner can start it again.
func TestOnDemandMemberOnADepartingNodeGoesWithoutItsWorld(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.reconcile(member.Name)
	claim := podspec.DataClaimName(member.Name)
	if f.claim(claim) == nil {
		t.Fatalf("the member has no claim %s to keep its world on", claim)
	}
	pod, ok := f.pod(f.server(member.Name).Status.PodName)
	if !ok {
		t.Fatalf("pod of %s not found", member.Name)
	}
	node := f.ensureNode(t, "node-going-"+f.ns, false)
	f.bindPodToNode(t, pod, node.Name)
	f.ensureNode(t, node.Name, true)

	f.reconcileNamedGroup(t, r, group.Name)

	if got, ok := f.serverIfPresent(member.Name); ok && got.DeletionTimestamp.IsZero() {
		t.Error("the member on the cordoned node was kept; its players will be evicted instead of moved")
	}
	if f.claim(claim) == nil {
		t.Fatal("the world went with the member, so its owner has nothing to start again")
	}
}

// The world is on the claim; deleting the object loses nothing.
func TestOnDemandFinishedMemberIsSwept(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.setPhase(t, member, phase.Finished)

	f.reconcileNamedGroup(t, r, group.Name)

	var got spawneryv1alpha1.Server
	err := f.c.Get(f.ctx, client.ObjectKeyFromObject(member), &got)
	if err == nil && got.DeletionTimestamp.IsZero() {
		t.Fatal("a finished member was kept, so its key cannot be started again")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get member: %v", err)
	}
}

// An image bump reaches a world the next time its owner starts it.
func TestOnDemandMemberIsNotRolledBySpecChange(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.setPhase(t, member, phase.Ready)
	f.reconcileNamedGroup(t, r, group.Name)

	f.bumpOnDemandImage(t, group)
	f.reconcileNamedGroup(t, r, group.Name)

	var got spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(member), &got); err != nil {
		t.Fatalf("the running member was removed by a spec change: %v", err)
	}
	if got.Spec.Retire {
		t.Fatal("a spec change retired a running private server")
	}
	if !got.DeletionTimestamp.IsZero() {
		t.Fatal("a spec change condemned a running private server")
	}
}

func TestOnDemandFailedMemberIsKeptForDiagnosis(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.setPhase(t, member, phase.Failed)

	f.reconcileNamedGroup(t, r, group.Name)

	var got spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(member), &got); err != nil {
		t.Fatalf("a failed member was swept away with nothing left to read: %v", err)
	}
}

// The cap is the group's, not each key's.
func TestOnDemandFailedMembersArePrunedPastTheCap(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	// Created and failed first, so the pruning rule and the alphabetical last
	// resort disagree: spelling alone would keep c0ffee.
	kept := f.createOnDemandMember(t, group, "decaf")
	pruned := f.createOnDemandMember(t, group, "c0ffee")
	f.failMember(t, kept, f.clock.Now())
	f.failMember(t, pruned, f.clock.Now().Add(time.Minute))

	f.reconcileNamedGroup(t, r, group.Name)

	names := f.serverNamesOfGroup(t, group.Name)
	if len(names) != 1 || names[0] != kept.Name {
		t.Fatalf("servers = %v, want [%s]: the earliest failure is the one kept, and %s failed a minute later",
			names, kept.Name, pruned.Name)
	}
}

// Nothing replaces an on-demand member, so an older render is no reason to
// stay Progressing.
func TestOnDemandProgressingIgnoresAnEarlierSpec(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.setPhase(t, member, phase.Ready)
	// Stamps the member with the render the bump below makes stale.
	f.reconcileNamedGroup(t, r, group.Name)
	f.bumpOnDemandImage(t, group)
	f.reconcileNamedGroup(t, r, group.Name)

	settled := f.progressing(t, group.Name)
	if settled.Status == metav1.ConditionTrue {
		t.Fatalf("Progressing = True (%s) for a group that replaces nothing: %s",
			settled.Reason, settled.Message)
	}
	// The message must report the phase that was checked, not the render the
	// loop declined to check.
	if strings.Contains(settled.Message, "current spec") {
		t.Fatalf("Progressing says every member carries the current spec, and c0ffee does not: %s",
			settled.Message)
	}
	if !strings.Contains(settled.Message, "the spec it started with") {
		t.Fatalf("Progressing does not say what it did check: %s", settled.Message)
	}

	second := f.createOnDemandMember(t, group, "decaf")
	f.setPhase(t, second, phase.Pending)
	f.reconcileNamedGroup(t, r, group.Name)

	cond := f.progressing(t, group.Name)
	if cond.Status != metav1.ConditionTrue || cond.Reason != spawneryv1alpha1.ReasonServersStarting {
		t.Fatalf("Progressing = %s/%s, want True/%s: %s",
			cond.Status, cond.Reason, spawneryv1alpha1.ReasonServersStarting, cond.Message)
	}
	if strings.Contains(cond.Message, "earlier spec") {
		t.Fatalf("Progressing reports a replacement that cannot happen: %s", cond.Message)
	}
}

func (f *fixture) createOnDemandGroup(t *testing.T, name string, maxInstances int32) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:   spawneryv1alpha1.ObjectRef{Name: f.network.Name},
			Type:         spawneryv1alpha1.ServerGroupOnDemand,
			Image:        "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:   10,
			MaxInstances: ptr.To(maxInstances),
			Storage:      &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("2Gi")},
		},
	}
	if err := f.c.Create(f.ctx, g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	return g
}

func (f *fixture) createOnDemandMember(t *testing.T, g *spawneryv1alpha1.ServerGroup, key string) *spawneryv1alpha1.Server {
	t.Helper()
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: g.Name + "-" + key, Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: g.Name},
			Key:      key,
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create member: %v", err)
	}
	return srv
}

// createOnDemandMemberWithOrdinal builds a member carrying spec.ordinal beside
// its key, which nothing forbids; it is the shape the persistent sizing rule
// sees.
func (f *fixture) createOnDemandMemberWithOrdinal(
	t *testing.T,
	g *spawneryv1alpha1.ServerGroup,
	key string,
	ordinal int32,
) *spawneryv1alpha1.Server {
	t.Helper()
	srv := f.createOnDemandMember(t, g, key)
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Ordinal = ptr.To(ordinal)
	if err := f.c.Patch(f.ctx, srv, patch); err != nil {
		t.Fatalf("give member %s an ordinal: %v", srv.Name, err)
	}
	return srv
}

// failMember's time matters: creationTimestamp has second resolution, so for
// members created together status.failedAt decides which one pruning keeps.
func (f *fixture) failMember(t *testing.T, srv *spawneryv1alpha1.Server, at time.Time) {
	t.Helper()
	srv.Status.Phase = string(phase.Failed)
	stamped := metav1.NewTime(at)
	srv.Status.FailedAt = &stamped
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("fail member %s: %v", srv.Name, err)
	}
}

func (f *fixture) setPhase(t *testing.T, srv *spawneryv1alpha1.Server, p phase.Phase) {
	t.Helper()
	srv.Status.Phase = string(p)
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("set phase %s: %v", p, err)
	}
}

// bumpOnDemandImage re-reads the group first: a reconcile pass has written its
// status since, so an update from the caller's copy is refused.
func (f *fixture) bumpOnDemandImage(t *testing.T, g *spawneryv1alpha1.ServerGroup) {
	t.Helper()
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(g), g); err != nil {
		t.Fatalf("re-read the group: %v", err)
	}
	g.Spec.Image = "ghcr.io/spawnery/paper:1.21.4-0.2.0"
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("bump the image: %v", err)
	}
}

func (f *fixture) progressing(t *testing.T, name string) *metav1.Condition {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, g); err != nil {
		t.Fatalf("get group %s: %v", name, err)
	}
	cond := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil {
		t.Fatalf("group %s publishes no %s condition", name, spawneryv1alpha1.ConditionProgressing)
	}
	return cond
}

// The Server controller keeps reconciling a Server that carries a deletion
// timestamp.
func TestADeletingOnDemandMemberDoesNotRecreateItsWorld(t *testing.T) {
	f := newFixture(t)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.reconcile(member.Name)
	claimName := podspec.DataClaimName(member.Name)
	claim := f.claim(claimName)
	if claim == nil {
		t.Fatalf("the member has no claim %s, so this test would assert nothing", claimName)
	}
	if len(f.server(member.Name).Finalizers) == 0 {
		t.Fatal("the member carries no finalizer, so its deletion would not leave it draining")
	}

	patch := client.MergeFrom(claim.DeepCopy())
	claim.Finalizers = nil
	if err := f.c.Patch(f.ctx, claim, patch); err != nil {
		t.Fatalf("release the claim: %v", err)
	}
	if err := f.c.Delete(f.ctx, claim); err != nil {
		t.Fatalf("delete the claim: %v", err)
	}
	if err := f.c.Delete(f.ctx, f.server(member.Name)); err != nil {
		t.Fatalf("delete the member: %v", err)
	}

	f.reconcile(member.Name)
	f.reconcile(member.Name)

	if f.claim(claimName) != nil {
		t.Fatal("the Server controller recreated the world of a member being deleted")
	}
}

// The chart's admission policy refuses the operator any label change, and
// adding the key would put a claim not created as a world in deletion's reach.
func TestAnOnDemandMembersOldWorldIsNotRelabelled(t *testing.T) {
	f := newFixture(t)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	old := podspec.BuildDataClaim(group, member)
	delete(old.Labels, podspec.LabelKey)
	if err := f.c.Create(f.ctx, old); err != nil {
		t.Fatalf("create the old world: %v", err)
	}

	f.reconcile(member.Name)

	claim := f.claim(podspec.DataClaimName(member.Name))
	if claim == nil {
		t.Fatal("the world is gone")
	}
	if got, ok := claim.Labels[podspec.LabelKey]; ok {
		t.Fatalf("key label = %q: the operator relabelled a claim it did not create keyed", got)
	}
}

func TestALoweredSizeReachesOnlyNewClaims(t *testing.T) {
	f := newFixture(t)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	a := f.createOnDemandMember(t, group, "c0ffee")
	f.reconcile(a.Name)
	before := f.claim(podspec.DataClaimName(a.Name))
	if before == nil {
		t.Fatal("member A has no claim")
	}

	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(group), group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	group.Spec.Storage.Size = resource.MustParse("1Gi")
	group.Spec.Storage.Annotations = map[string]string{"resize.topolvm.io/storage_limit": "20Gi"}
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("lower the size: %v", err)
	}
	f.reconcile(a.Name)

	after := f.claim(podspec.DataClaimName(a.Name))
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("claim of A was written: resourceVersion %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
	if got := f.server(a.Name).Status.StorageResizeError; got != "" {
		t.Errorf("storageResizeError = %q, want empty", got)
	}

	b := f.createOnDemandMember(t, group, "decaf")
	f.reconcile(b.Name)
	claimB := f.claim(podspec.DataClaimName(b.Name))
	if claimB == nil {
		t.Fatal("member B has no claim")
	}
	if got := claimB.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Errorf("claim of B requests %v, want 1Gi", got.String())
	}
	if got := claimB.Annotations["resize.topolvm.io/storage_limit"]; got != "20Gi" {
		t.Errorf("claim of B annotations = %v, want the group's", claimB.Annotations)
	}
}

type rejectClaimCreates struct{ client.Client }

func (r rejectClaimCreates) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
		return apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim").GroupKind(), obj.GetName(), nil)
	}
	return r.Client.Create(ctx, obj, opts...)
}

// So nothing the API server would refuse on create can keep the pod away.
func TestAnExistingClaimIsNotCreatedAgain(t *testing.T) {
	f := newFixture(t)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	if err := f.c.Create(f.ctx, podspec.BuildDataClaim(group, member)); err != nil {
		t.Fatalf("create the existing claim: %v", err)
	}
	f.reconc.Client = rejectClaimCreates{f.reconc.Client}

	f.reconcile(member.Name)

	if _, ok := f.pod(member.Name); !ok {
		t.Fatal("no pod: the reconcile tried to create the existing claim")
	}
}

func TestARefusedClaimCreateIsReported(t *testing.T) {
	f := newFixture(t)
	rec := newRecorder()
	f.reconc.Recorder = rec
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.reconc.Client = rejectClaimCreates{f.reconc.Client}

	f.reconcile(member.Name)

	if _, ok := f.pod(member.Name); ok {
		t.Fatal("a pod exists although its claim was refused")
	}
	got := f.server(member.Name)
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, ReasonServerClaimRejected) {
		t.Errorf("conditions = %+v, want Accepted=False with reason %s",
			got.Status.Conditions, ReasonServerClaimRejected)
	}
	found := false
	for _, ev := range drainEvents(rec) {
		if strings.Contains(ev, ReasonServerClaimRejected) {
			found = true
		}
	}
	if !found {
		t.Errorf("no event names %s", ReasonServerClaimRejected)
	}
}

func (f *fixture) createObjectStoreGroup(t *testing.T, name string) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	g := f.createOnDemandGroup(t, name, 50)
	g.Spec.Storage = &spawneryv1alpha1.StorageSpec{
		Backend: spawneryv1alpha1.StorageBackendObjectStore,
		Keep:    []string{"world"},
	}
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("switch the group to ObjectStore: %v", err)
	}
	return g
}

func TestAnObjectStoreMemberGetsNoClaim(t *testing.T) {
	f := newFixture(t)
	f.reconc.WorldSync = true
	group := f.createObjectStoreGroup(t, "private-servers")
	member := f.createOnDemandMember(t, group, "c0ffee")

	f.reconcile(member.Name)

	if _, ok := f.pod(member.Name); !ok {
		t.Fatal("no pod for an ObjectStore member with world sync on")
	}
	if claim := f.claim(podspec.DataClaimName(member.Name)); claim != nil {
		t.Fatalf("an ObjectStore member got the claim %s", claim.Name)
	}
}

func TestAnObjectStoreMemberWaitsWhenWorldSyncIsOff(t *testing.T) {
	f := newFixture(t)
	group := f.createObjectStoreGroup(t, "private-servers")
	member := f.createOnDemandMember(t, group, "c0ffee")

	f.reconcile(member.Name)

	if _, ok := f.pod(member.Name); ok {
		t.Fatal("a pod exists although world sync is off")
	}
	got := f.server(member.Name)
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, ReasonWorldSyncOff) {
		t.Errorf("conditions = %+v, want Accepted=False with reason %s",
			got.Status.Conditions, ReasonWorldSyncOff)
	}
}

func TestAnObjectStoreMemberWithoutAKeyGetsNoPod(t *testing.T) {
	f := newFixture(t)
	f.reconc.WorldSync = true
	group := f.createObjectStoreGroup(t, "private-servers")
	member := f.createOnDemandMember(t, group, "c0ffee")
	patch := client.MergeFrom(member.DeepCopy())
	member.Spec.Key = ""
	if err := f.c.Patch(f.ctx, member, patch); err != nil {
		t.Skipf("the API server refuses an empty key already: %v", err)
	}

	f.reconcile(member.Name)

	if _, ok := f.pod(member.Name); ok {
		t.Fatal("a pod exists for a member without a key")
	}
	got := f.server(member.Name)
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, ReasonServerKeyMissing) {
		t.Errorf("conditions = %+v, want Accepted=False with reason %s",
			got.Status.Conditions, ReasonServerKeyMissing)
	}
}
