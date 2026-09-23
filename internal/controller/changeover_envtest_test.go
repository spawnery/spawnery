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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
)

const nextImage = "ghcr.io/spawnery/paper:1.21.4-0.2.0"

func TestChangeoverBudgetHoldsTheSecondGroup(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setChangeoverBudget(t, 1)
	f.createEphemeralGroupLike(t, "arena")
	for _, name := range []string{"arena", "lobby"} {
		f.reconcileNamedGroup(t, r, name)
		f.readyAllServersOf(t, name)
	}
	f.setImage(t, "arena", nextImage)
	f.setImage(t, "lobby", nextImage)

	f.reconcileNamedGroup(t, r, "arena")
	f.reconcileNamedGroup(t, r, "lobby")

	if n := len(f.serverNamesOfGroup(t, "arena")); n != 2 {
		t.Fatalf("arena has %d servers, want 2: first by name, it should hold the place", n)
	}
	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 1 {
		t.Fatalf("lobby has %d servers, want 1: the budget is spent", n)
	}
	if c := f.progressing(t, "lobby"); c.Reason != spawneryv1alpha1.ReasonWaitingForChangeoverBudget ||
		!strings.Contains(c.Message, "arena") {
		t.Fatalf("lobby Progressing = %s %q, want %s naming arena", c.Reason, c.Message,
			spawneryv1alpha1.ReasonWaitingForChangeoverBudget)
	}
	if got := f.serverGroup(t, "arena").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("arena status.changeover = %q, want Begun", got)
	}
	if got := f.serverGroup(t, "lobby").Status.Changeover; got != spawneryv1alpha1.ChangeoverWaiting {
		t.Fatalf("lobby status.changeover = %q, want Waiting", got)
	}

	f.finishChangeover(t, r, "arena")
	if got := f.serverGroup(t, "arena").Status.Changeover; got != spawneryv1alpha1.ChangeoverNone {
		t.Fatalf("arena status.changeover = %q after its changeover, want empty", got)
	}
	f.reconcileNamedGroup(t, r, "lobby")
	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 2 {
		t.Fatalf("lobby has %d servers, want 2: the place is free again", n)
	}
	if got := f.serverGroup(t, "lobby").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("lobby status.changeover = %q, want Begun", got)
	}
}

func TestChangeoverBudgetUnsetChangesNothing(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createEphemeralGroupLike(t, "arena")
	for _, name := range []string{"arena", "lobby"} {
		f.reconcileNamedGroup(t, r, name)
		f.readyAllServersOf(t, name)
	}
	f.setImage(t, "arena", nextImage)
	f.setImage(t, "lobby", nextImage)

	f.reconcileNamedGroup(t, r, "arena")
	f.reconcileNamedGroup(t, r, "lobby")

	for _, name := range []string{"arena", "lobby"} {
		if n := len(f.serverNamesOfGroup(t, name)); n != 2 {
			t.Fatalf("%s has %d servers, want 2: no budget, no wait", name, n)
		}
	}
}

// A group at its ceiling cannot begin; it must not keep a sibling waiting.
func TestChangeoverBudgetIgnoresAGroupAtItsCeiling(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setChangeoverBudget(t, 1)
	arena := f.createEphemeralGroupLike(t, "arena")
	arena.Spec.Scaling.MaxReplicas = 1
	if err := f.c.Update(f.ctx, arena); err != nil {
		t.Fatalf("update arena: %v", err)
	}
	for _, name := range []string{"arena", "lobby"} {
		f.reconcileNamedGroup(t, r, name)
		f.readyAllServersOf(t, name)
	}
	f.setImage(t, "arena", nextImage)
	f.setImage(t, "lobby", nextImage)

	f.reconcileNamedGroup(t, r, "arena")
	f.reconcileNamedGroup(t, r, "lobby")

	if n := len(f.serverNamesOfGroup(t, "arena")); n != 1 {
		t.Fatalf("arena has %d servers, want 1: maxReplicas holds it", n)
	}
	if got := f.serverGroup(t, "arena").Status.Changeover; got != spawneryv1alpha1.ChangeoverNone {
		t.Fatalf("arena status.changeover = %q, want empty: it cannot begin", got)
	}
	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 2 {
		t.Fatalf("lobby has %d servers, want 2: arena holds no place", n)
	}
}

// Without a budget a failing group is not refused either: the pass on which
// its backoff has just expired still reads BackingOff True from the last one.
func TestChangeoverBudgetUnsetDoesNotRefuseAFailingGroup(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileNamedGroup(t, r, "lobby")
	f.readyAllServersOf(t, "lobby")
	f.setImage(t, "lobby", nextImage)

	g := f.serverGroup(t, "lobby")
	meta.SetStatusCondition(&g.Status.Conditions, metav1.Condition{
		Type: spawneryv1alpha1.ConditionBackingOff, Status: metav1.ConditionTrue,
		Reason: "Test", Message: "left over from the last pass",
	})
	if err := f.c.Status().Update(f.ctx, g); err != nil {
		t.Fatalf("update lobby status: %v", err)
	}

	f.reconcileNamedGroup(t, r, "lobby")

	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 2 {
		t.Fatalf("lobby has %d servers, want 2: no budget refuses nothing", n)
	}
}

// A stale server that is leaving but not gone is still the group's extra
// server, so the group keeps its place until it is gone.
func TestChangeoverBudgetHeldUntilTheLastStaleServerIsGone(t *testing.T) {
	for _, p := range []phase.Phase{phase.Retiring, phase.Draining, phase.Terminating} {
		t.Run(string(p), func(t *testing.T) {
			f := newFixture(t)
			r := groupReconciler(f)
			f.setChangeoverBudget(t, 1)
			f.createEphemeralGroupLike(t, "arena")
			for _, name := range []string{"arena", "lobby"} {
				f.reconcileNamedGroup(t, r, name)
				f.readyAllServersOf(t, name)
			}
			stale := f.serverNamesOfGroup(t, "arena")
			f.setImage(t, "arena", nextImage)
			f.setImage(t, "lobby", nextImage)
			f.reconcileNamedGroup(t, r, "arena")

			g := f.serverGroup(t, "arena")
			for _, name := range f.serverNamesOfGroup(t, "arena") {
				if srv := f.server(name); srv.Spec.GroupGeneration == g.Generation {
					bringUpNamed(t, f, name)
				}
			}
			for _, name := range stale {
				f.setPhase(t, f.server(name), p)
			}
			f.reconcileNamedGroup(t, r, "arena")
			f.reconcileNamedGroup(t, r, "lobby")

			if got := f.serverGroup(t, "arena").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
				t.Fatalf("arena status.changeover = %q with its stale server %s, want Begun", got, p)
			}
			if n := len(f.serverNamesOfGroup(t, "lobby")); n != 1 {
				t.Fatalf("lobby has %d servers, want 1: arena still holds the place", n)
			}
		})
	}
}

func (f *fixture) setChangeoverBudget(t *testing.T, n int32) {
	t.Helper()
	net := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.network.Name, Namespace: f.ns}, net); err != nil {
		t.Fatalf("get network: %v", err)
	}
	net.Spec.Update = &spawneryv1alpha1.NetworkUpdateSpec{MaxConcurrentChangeovers: ptr.To(n)}
	if err := f.c.Update(f.ctx, net); err != nil {
		t.Fatalf("update network: %v", err)
	}
	f.network = net
}

func (f *fixture) createEphemeralGroupLike(t *testing.T, name string) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{}
	g.Name = name
	g.Namespace = f.ns
	g.Spec = *f.group.Spec.DeepCopy()
	if err := f.c.Create(f.ctx, g); err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	return g
}

func (f *fixture) readyAllServersOf(t *testing.T, group string) {
	t.Helper()
	for _, name := range f.serverNamesOfGroup(t, group) {
		bringUpNamed(t, f, name)
	}
}

func (f *fixture) setImage(t *testing.T, group, image string) {
	t.Helper()
	g := f.serverGroup(t, group)
	g.Spec.Image = image
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update group %s: %v", group, err)
	}
}

func (f *fixture) serverGroup(t *testing.T, name string) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, g); err != nil {
		t.Fatalf("get group %s: %v", name, err)
	}
	return g
}

// finishChangeover readies the group's current-generation servers, deletes its
// stale ones and reconciles the group twice.
func (f *fixture) finishChangeover(t *testing.T, r *ServerGroupReconciler, group string) {
	t.Helper()
	g := f.serverGroup(t, group)
	for _, name := range f.serverNamesOfGroup(t, group) {
		srv := f.server(name)
		if srv.Spec.GroupGeneration == g.Generation {
			bringUpNamed(t, f, name)
			continue
		}
		srv.Finalizers = nil
		if err := f.c.Update(f.ctx, srv); err != nil {
			t.Fatalf("drop finalizers of stale server %s: %v", name, err)
		}
		if err := f.c.Delete(f.ctx, srv); err != nil {
			t.Fatalf("delete stale server %s: %v", name, err)
		}
	}
	f.reconcileNamedGroup(t, r, group)
	f.reconcileNamedGroup(t, r, group)
}
