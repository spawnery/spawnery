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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
)

// The group must not size its own members. Every other group type answers
// "how many", and the one thing that must never happen here is that an
// answer of zero -- which is what every sizing rule computes for a group
// with no sizing fields -- is acted on.
func TestOnDemandGroupCreatesAndDeletesNothing(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")

	for i := 0; i < 3; i++ {
		f.reconcileNamedGroup(t, r, group.Name)
	}

	var servers spawneryv1alpha1.ServerList
	if err := f.c.List(f.ctx, &servers, client.InNamespace(f.ns)); err != nil {
		t.Fatalf("list servers: %v", err)
	}
	if len(servers.Items) != 1 {
		t.Fatalf("after three passes the group has %d servers, want exactly the one that was asked for",
			len(servers.Items))
	}
	if servers.Items[0].Name != member.Name {
		t.Fatalf("server = %q, want %q", servers.Items[0].Name, member.Name)
	}
	if !servers.Items[0].DeletionTimestamp.IsZero() {
		t.Fatal("the group condemned a member nobody asked it to remove")
	}
}

// A member that said its round was over and stopped is gone: its world is on
// the claim, and the object holds the one name its owner needs to start again.
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

// Nothing rolls. An image bump reaches a world the next time its owner starts
// it, and throwing a player out of their own world to apply one is the
// opposite of what a private server is for.
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

// A broken world is kept, because somebody has to be able to look at it.
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

// The cap is the group's and not each key's: a second broken world prunes the
// first, so an operator reading a private-server group finds one corpse to
// look at rather than as many as the group has keys.
func TestOnDemandFailedMembersArePrunedPastTheCap(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	kept := f.createOnDemandMember(t, group, "c0ffee")
	pruned := f.createOnDemandMember(t, group, "decaf")
	f.setPhase(t, kept, phase.Failed)
	f.setPhase(t, pruned, phase.Failed)

	f.reconcileNamedGroup(t, r, group.Name)

	names := f.serverNamesOfGroup(t, group.Name)
	if len(names) != 1 || names[0] != kept.Name {
		t.Fatalf("servers = %v, want [%s]", names, kept.Name)
	}
}

// Progressing is about members coming up, not about the render they carry.
// Nothing replaces an on-demand member, so a group whose worlds predate an
// image bump has arrived where it decided to be, and a condition that said
// otherwise would say it for as long as somebody kept playing.
func TestOnDemandProgressingIgnoresAnEarlierSpec(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.setPhase(t, member, phase.Ready)
	// The pass that stamps the member with the group's current render, which is
	// what the bump below makes it older than.
	f.reconcileNamedGroup(t, r, group.Name)
	f.bumpOnDemandImage(t, group)
	f.reconcileNamedGroup(t, r, group.Name)

	if cond := f.progressing(t, group.Name); cond.Status == metav1.ConditionTrue {
		t.Fatalf("Progressing = True (%s) for a group that replaces nothing: %s",
			cond.Reason, cond.Message)
	}

	// And a member that is coming up is progress, whatever render it carries.
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

func (f *fixture) setPhase(t *testing.T, srv *spawneryv1alpha1.Server, p phase.Phase) {
	t.Helper()
	srv.Status.Phase = string(p)
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("set phase %s: %v", p, err)
	}
}

// bumpOnDemandImage moves the group's image, re-reading it first: a reconcile
// pass has written the group's status since it was created, so an update from
// the caller's copy is refused.
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

// progressing is the group's Progressing condition, which has to be published
// for an assertion to be about anything.
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
