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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// A group whose stale servers are all leaving and whose current one is Ready holds no place.
func TestARollingUpdateGroupReleasesItsPlaceOnceDeferred(t *testing.T) {
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

			if got := f.serverGroup(t, "arena").Status.Changeover; got != spawneryv1alpha1.ChangeoverDeferred {
				t.Fatalf("arena status.changeover = %q with its stale server %s, want Deferred", got, p)
			}
			if n := len(f.serverNamesOfGroup(t, "lobby")); n != 2 {
				t.Fatalf("lobby has %d servers, want 2: the place is free, lobby's cold start comes", n)
			}
		})
	}
}

func TestALaterStageWaitsWithTheBudgetUnset(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createEphemeralGroupLike(t, "arena")
	for _, name := range []string{"arena", "lobby"} {
		f.reconcileNamedGroup(t, r, name)
		f.readyAllServersOf(t, name)
	}
	f.setStage(t, "arena", 10)
	f.setImage(t, "arena", nextImage)
	f.setImage(t, "lobby", nextImage)

	f.reconcileNamedGroup(t, r, "lobby")
	f.reconcileNamedGroup(t, r, "arena")

	if n := len(f.serverNamesOfGroup(t, "arena")); n != 1 {
		t.Fatalf("arena has %d servers, want 1: stage 10 waits for lobby", n)
	}
	if c := f.progressing(t, "arena"); c.Reason != spawneryv1alpha1.ReasonWaitingForEarlierStage ||
		c.Message != "waiting for stage 0: lobby" {
		t.Fatalf("arena Progressing = %s %q", c.Reason, c.Message)
	}

	f.finishChangeover(t, r, "lobby")
	f.reconcileNamedGroup(t, r, "arena")
	if n := len(f.serverNamesOfGroup(t, "arena")); n != 2 {
		t.Fatalf("arena has %d servers, want 2 once lobby is through", n)
	}
}

func TestARollingUpdateGroupReleasesTheNextStageOnceDeferred(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createEphemeralGroupLike(t, "arena")
	for _, name := range []string{"arena", "lobby"} {
		f.reconcileNamedGroup(t, r, name)
		f.readyAllServersOf(t, name)
	}
	stale := f.serverNamesOfGroup(t, "lobby")
	f.setStage(t, "arena", 10)
	f.setImage(t, "arena", nextImage)
	f.setImage(t, "lobby", nextImage)

	f.reconcileNamedGroup(t, r, "lobby")
	f.reconcileNamedGroup(t, r, "arena")
	if n := len(f.serverNamesOfGroup(t, "arena")); n != 1 {
		t.Fatalf("arena has %d servers, want 1: lobby is still changing over", n)
	}

	f.bringUpCurrent(t, "lobby")
	f.reconcileNamedGroup(t, r, "lobby")
	for _, name := range stale {
		srv := f.server(name)
		if !srv.Spec.Retire {
			t.Fatalf("stale %s not retired once its replacement is Ready", name)
		}
		f.setPhase(t, srv, phase.Retiring)
	}
	f.reconcileNamedGroup(t, r, "lobby")
	if got := f.serverGroup(t, "lobby").Status.Changeover; got != spawneryv1alpha1.ChangeoverDeferred {
		t.Fatalf("lobby status.changeover = %q with its stale server retiring, want Deferred", got)
	}

	f.reconcileNamedGroup(t, r, "arena")
	if n := len(f.serverNamesOfGroup(t, "arena")); n != 2 {
		t.Fatalf("arena has %d servers, want 2: lobby is Deferred and holds stage 0 no longer", n)
	}
}

func TestAPersistentGroupWaitsForItsStage(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "world", 1)
	for _, name := range []string{"lobby", "world"} {
		f.reconcileNamedGroup(t, r, name)
		f.readyAllServersOf(t, name)
	}
	f.setStage(t, "world", 10)
	f.setImage(t, "lobby", nextImage)
	f.setImage(t, "world", nextImage)

	f.reconcileNamedGroup(t, r, "lobby")
	f.reconcileNamedGroup(t, r, "world")

	if srv := f.server("world-0"); !srv.DeletionTimestamp.IsZero() || srv.Status.Phase == string(phase.Draining) {
		t.Fatalf("world-0 is being taken down while stage 0 is in flight")
	}
	if got := f.serverGroup(t, "world").Status.Changeover; got != spawneryv1alpha1.ChangeoverWaiting {
		t.Fatalf("world status.changeover = %q, want Waiting", got)
	}
	if c := f.progressing(t, "world"); c.Reason != spawneryv1alpha1.ReasonWaitingForEarlierStage ||
		c.Message != "waiting for stage 0: lobby" {
		t.Fatalf("world Progressing = %s %q", c.Reason, c.Message)
	}

	f.finishChangeover(t, r, "lobby")
	f.reconcileNamedGroup(t, r, "world")

	if srv, present := f.serverIfPresent("world-0"); present && srv.DeletionTimestamp.IsZero() &&
		srv.Status.Phase != string(phase.Draining) {
		t.Fatalf("world-0 phase %s is not being taken down once lobby is through", srv.Status.Phase)
	}
	if got := f.serverGroup(t, "world").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("world status.changeover = %q, want Begun", got)
	}
}

func TestABegunPersistentGroupStaysBegunWhileItsNetworkIsUnusable(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "world", 1)
	f.reconcileNamedGroup(t, r, "world")
	f.readyAllServersOf(t, "world")
	f.setImage(t, "world", nextImage)
	f.reconcileNamedGroup(t, r, "world")
	if got := f.serverGroup(t, "world").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("world status.changeover = %q, want Begun before the network goes", got)
	}

	net := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: f.network.Name, Namespace: f.ns}, net); err != nil {
		t.Fatalf("get network: %v", err)
	}
	meta.SetStatusCondition(&net.Status.Conditions, metav1.Condition{
		Type: spawneryv1alpha1.ConditionAccepted, Status: metav1.ConditionFalse,
		Reason: spawneryv1alpha1.ReasonDuplicateNetwork, Message: "test",
	})
	if err := f.c.Status().Update(f.ctx, net); err != nil {
		t.Fatalf("update network status: %v", err)
	}
	f.reconcileNamedGroup(t, r, "world")

	if got := f.serverGroup(t, "world").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("world status.changeover = %q with its network unusable, want Begun", got)
	}
}

func TestAWhenEmptyGroupStaysDeferredThroughAReadinessLoss(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setUpdatePolicy(t, 2, &spawneryv1alpha1.UpdateSpec{
		Strategy: spawneryv1alpha1.UpdateWhenEmpty, MaxUnavailable: 2,
	})
	f.reconcileNamedGroup(t, r, "lobby")
	f.readyAllServersOf(t, "lobby")
	f.reportPlayersOn(t, f.serverNamesOfGroup(t, "lobby")[0], 5)

	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, r, "lobby")
	f.bringUpCurrent(t, "lobby")
	f.reconcileNamedGroup(t, r, "lobby")
	if got := f.serverGroup(t, "lobby").Status.Changeover; got != spawneryv1alpha1.ChangeoverDeferred {
		t.Fatalf("status.changeover = %q, want Deferred before the readiness loss", got)
	}

	g := f.serverGroup(t, "lobby")
	for _, name := range f.serverNamesOfGroup(t, "lobby") {
		if srv := f.server(name); srv.Spec.GroupGeneration == g.Generation {
			f.setPhase(t, srv, phase.Starting)
		}
	}
	f.reconcileNamedGroup(t, r, "lobby")

	if got := f.serverGroup(t, "lobby").Status.Changeover; got != spawneryv1alpha1.ChangeoverDeferred {
		t.Fatalf("status.changeover = %q, want Deferred", got)
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

func (f *fixture) setStage(t *testing.T, group string, stage int32) {
	t.Helper()
	g := f.serverGroup(t, group)
	g.Spec.ChangeoverStage = stage
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update ServerGroup %s: %v", group, err)
	}
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

const nextProxyImage = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"

func TestChangeoverBudgetHoldsAProxyGroupBehindAServerGroup(t *testing.T) {
	f := newFixture(t)
	gr := groupReconciler(f)
	pr := proxyGroupReconciler(f)
	f.setChangeoverBudget(t, 1)
	f.reconcileNamedGroup(t, gr, "lobby")
	f.readyAllServersOf(t, "lobby")
	f.readyProxyGroup(t, pr, "gateway")

	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, gr, "lobby")
	f.setProxyImage(t, "gateway", nextProxyImage)
	f.reconcileProxyGroup(pr, "gateway")

	if n := len(f.proxyPods("gateway")); n != 2 {
		t.Fatalf("gateway has %d pods, want 2: lobby holds the place", n)
	}
	g := f.proxyGroup("gateway")
	if g.Status.Changeover != spawneryv1alpha1.ChangeoverWaiting {
		t.Fatalf("gateway status.changeover = %q, want Waiting", g.Status.Changeover)
	}
	if c := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionChangingOver); c == nil ||
		c.Status != metav1.ConditionTrue ||
		c.Message != "waiting for a changeover place; changing over: lobby" {
		t.Fatalf("gateway ChangingOver = %+v, want True naming lobby", c)
	}

	f.finishChangeover(t, gr, "lobby")
	f.reconcileProxyGroup(pr, "gateway")

	if n := len(f.proxyPods("gateway")); n != 4 {
		t.Fatalf("gateway has %d pods, want 4: the place is free, the replacements come", n)
	}
	if got := f.proxyGroup("gateway").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("gateway status.changeover = %q, want Begun", got)
	}
}

func TestChangeoverBudgetUnsetDoesNotRefuseADegradedProxyGroup(t *testing.T) {
	f := newFixture(t)
	pr := proxyGroupReconciler(f)
	f.readyProxyGroup(t, pr, "gateway")
	f.setProxyImage(t, "gateway", nextProxyImage)

	g := f.proxyGroup("gateway")
	meta.SetStatusCondition(&g.Status.Conditions, metav1.Condition{
		Type: spawneryv1alpha1.ConditionDegraded, Status: metav1.ConditionTrue,
		Reason: "Test", Message: "left over from the last pass",
	})
	if err := f.c.Status().Update(f.ctx, g); err != nil {
		t.Fatalf("update gateway status: %v", err)
	}

	f.reconcileProxyGroup(pr, "gateway")

	if n := len(f.proxyPods("gateway")); n != 4 {
		t.Fatalf("gateway has %d pods, want 4: no budget refuses nothing", n)
	}
}

// The freed place does not wait for the stale pod to actually be gone.
func TestChangeoverBudgetFreedOnceTheLastStaleProxyIsLeaving(t *testing.T) {
	for _, leaving := range []string{"draining", "terminating"} {
		t.Run(leaving, func(t *testing.T) {
			f := newFixture(t)
			gr := groupReconciler(f)
			pr := proxyGroupReconciler(f)
			f.setChangeoverBudget(t, 1)
			f.reconcileNamedGroup(t, gr, "lobby")
			f.readyAllServersOf(t, "lobby")
			stale := f.readyProxyGroup(t, pr, "gateway")

			f.setProxyImage(t, "gateway", nextProxyImage)
			f.reconcileProxyGroup(pr, "gateway")
			pods := f.proxyPods("gateway")
			if len(pods) != 4 {
				t.Fatalf("gateway has %d pods, want 4", len(pods))
			}
			for i := range pods {
				f.markProxyPodReady(t, &pods[i])
			}

			f.deletePod(t, stale[0], false)
			switch leaving {
			case "draining":
				f.setDrainingSince(stale[1], f.clock.Now().UTC().Format(time.RFC3339))
			case "terminating":
				f.deletePod(t, stale[1], true)
			}
			f.reconcileProxyGroup(pr, "gateway")
			f.setImage(t, "lobby", nextImage)
			f.reconcileNamedGroup(t, gr, "lobby")

			if got := f.proxyGroup("gateway").Status.Changeover; got != spawneryv1alpha1.ChangeoverDeferred {
				t.Fatalf("gateway status.changeover = %q with its last stale pod %s, want Deferred", got, leaving)
			}
			if n := len(f.serverNamesOfGroup(t, "lobby")); n != 2 {
				t.Fatalf("lobby has %d servers, want 2: gateway no longer holds the place", n)
			}
		})
	}
}

// Otherwise the refused group keeps winning AdmitChangeovers's name-ordered
// admission, and a waiting sibling named after it never gets in.
func TestARefusedWaitingProxyGroupGivesUpItsPlace(t *testing.T) {
	f := newFixture(t)
	gr := groupReconciler(f)
	pr := proxyGroupReconciler(f)
	f.setChangeoverBudget(t, 1)
	f.reconcileNamedGroup(t, gr, "lobby")
	f.readyAllServersOf(t, "lobby")
	f.readyProxyGroup(t, pr, "gateway")
	f.readyProxyGroup(t, pr, "zulu", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose.NodePort.Port = 30002
	})

	// lobby, reconciled first and alone, takes the network's one place.
	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, gr, "lobby")

	f.setProxyImage(t, "gateway", nextProxyImage)
	f.reconcileProxyGroup(pr, "gateway")
	f.setProxyImage(t, "zulu", nextProxyImage)
	f.reconcileProxyGroup(pr, "zulu")
	if got := f.proxyGroup("gateway").Status.Changeover; got != spawneryv1alpha1.ChangeoverWaiting {
		t.Fatalf("gateway status.changeover = %q, want Waiting", got)
	}
	if got := f.proxyGroup("zulu").Status.Changeover; got != spawneryv1alpha1.ChangeoverWaiting {
		t.Fatalf("zulu status.changeover = %q, want Waiting", got)
	}

	// gateway is refused on its own scheduling while still Waiting.
	gateway := f.proxyGroup("gateway")
	gateway.Spec.Scheduling = &spawneryv1alpha1.Scheduling{
		Tolerations: []corev1.Toleration{{Key: "spawnery.cloud/test-refusal", Operator: corev1.TolerationOpExists}},
	}
	if err := f.c.Update(f.ctx, gateway); err != nil {
		t.Fatalf("update gateway: %v", err)
	}
	f.reconcileProxyGroup(pr, "gateway")
	if c := meta.FindStatusCondition(f.proxyGroup("gateway").Status.Conditions, spawneryv1alpha1.ConditionAccepted); c == nil ||
		c.Reason != spawneryv1alpha1.ReasonSchedulingNotAllowed {
		t.Fatalf("gateway Accepted condition = %+v, want SchedulingNotAllowed", c)
	}
	if got := f.proxyGroup("gateway").Status.Changeover; got != spawneryv1alpha1.ChangeoverNone {
		t.Fatalf("gateway status.changeover = %q after its refusal, want empty: a Waiting group has no extra pod to hold a place with", got)
	}

	// zulu, waiting and named after gateway, must now be admitted.
	f.finishChangeover(t, gr, "lobby")
	f.reconcileProxyGroup(pr, "zulu")
	if n := len(f.proxyPods("zulu")); n != 4 {
		t.Fatalf("zulu has %d pods, want 4: the place is free, the replacements come", n)
	}
	if got := f.proxyGroup("zulu").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("zulu status.changeover = %q, want Begun", got)
	}
}

func TestAServerGroupWaitsForTheProxyStageUntilItsNewPodsStand(t *testing.T) {
	f := newFixture(t)
	gr := groupReconciler(f)
	pr := proxyGroupReconciler(f)
	f.reconcileNamedGroup(t, gr, "lobby")
	f.readyAllServersOf(t, "lobby")
	stale := f.readyProxyGroup(t, pr, "gateway", func(g *spawneryv1alpha1.ProxyGroup) { g.Spec.ChangeoverStage = -10 })

	f.setProxyImage(t, "gateway", nextProxyImage)
	f.setImage(t, "lobby", nextImage)
	f.reconcileProxyGroup(pr, "gateway")
	f.reconcileNamedGroup(t, gr, "lobby")

	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 1 {
		t.Fatalf("lobby has %d servers, want 1: the proxy stage is in flight", n)
	}
	if c := f.progressing(t, "lobby"); c.Reason != spawneryv1alpha1.ReasonWaitingForEarlierStage ||
		c.Message != "waiting for stage -10: gateway" {
		t.Fatalf("lobby Progressing = %s %q", c.Reason, c.Message)
	}

	pods := f.proxyPods("gateway")
	if len(pods) != 4 {
		t.Fatalf("gateway has %d pods, want 4", len(pods))
	}
	for i := range pods {
		if slices.Contains(stale, pods[i].Name) {
			f.reportProxyPlayers(t, pods[i], 1)
		} else {
			f.markProxyPodReady(t, &pods[i])
		}
	}
	f.reconcileProxyGroup(pr, "gateway")
	f.reconcileProxyGroup(pr, "gateway")
	for _, name := range stale {
		pod, ok := f.pod(name)
		if !ok {
			t.Fatalf("stale proxy %s is gone; it should still be draining its player", name)
		}
		if _, marked := drainingSince(pod); !marked {
			t.Fatalf("stale proxy %s is not draining", name)
		}
	}
	if got := f.proxyGroup("gateway").Status.Changeover; got != spawneryv1alpha1.ChangeoverDeferred {
		t.Fatalf("gateway status.changeover = %q, want Deferred while its old pods drain", got)
	}

	f.reconcileNamedGroup(t, gr, "lobby")
	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 2 {
		t.Fatalf("lobby has %d servers, want 2: the proxy stage stands", n)
	}
}

func TestAnUnreconciledProxyStageGatesAPersistentGroup(t *testing.T) {
	f := newFixture(t)
	gr := groupReconciler(f)
	pr := proxyGroupReconciler(f)
	f.createPersistentGroup(t, "world", 1)
	for _, name := range []string{"lobby", "world"} {
		f.reconcileNamedGroup(t, gr, name)
		f.readyAllServersOf(t, name)
	}
	stale := f.readyProxyGroup(t, pr, "gateway", func(g *spawneryv1alpha1.ProxyGroup) { g.Spec.ChangeoverStage = -10 })

	f.setProxyImage(t, "gateway", nextProxyImage)
	f.setImage(t, "world", nextImage)
	f.reconcileNamedGroup(t, gr, "world")

	if g := f.proxyGroup("gateway"); g.Status.Changeover != spawneryv1alpha1.ChangeoverNone ||
		g.Generation == g.Status.ObservedGeneration {
		t.Fatalf("gateway changeover %q, generation %d, observed %d: the race needs a stale status",
			g.Status.Changeover, g.Generation, g.Status.ObservedGeneration)
	}
	if srv := f.server("world-0"); !srv.DeletionTimestamp.IsZero() || srv.Status.Phase == string(phase.Draining) {
		t.Fatalf("world-0 is being taken down before the proxy stage has been reconciled")
	}
	if c := f.progressing(t, "world"); c.Reason != spawneryv1alpha1.ReasonWaitingForEarlierStage ||
		c.Message != "waiting for stage -10: gateway (not yet reconciled)" {
		t.Fatalf("world Progressing = %s %q", c.Reason, c.Message)
	}

	f.reconcileProxyGroup(pr, "gateway")
	if got := f.proxyGroup("gateway").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("gateway status.changeover = %q, want Begun", got)
	}
	f.reconcileNamedGroup(t, gr, "world")
	if srv := f.server("world-0"); !srv.DeletionTimestamp.IsZero() || srv.Status.Phase == string(phase.Draining) {
		t.Fatalf("world-0 is being taken down while the proxy stage is in flight")
	}

	pods := f.proxyPods("gateway")
	for i := range pods {
		if slices.Contains(stale, pods[i].Name) {
			f.reportProxyPlayers(t, pods[i], 1)
		} else {
			f.markProxyPodReady(t, &pods[i])
		}
	}
	f.reconcileProxyGroup(pr, "gateway")
	f.reconcileProxyGroup(pr, "gateway")
	if got := f.proxyGroup("gateway").Status.Changeover; got != spawneryv1alpha1.ChangeoverDeferred {
		t.Fatalf("gateway status.changeover = %q, want Deferred", got)
	}

	f.reconcileNamedGroup(t, gr, "world")
	if srv, present := f.serverIfPresent("world-0"); present && srv.DeletionTimestamp.IsZero() &&
		srv.Status.Phase != string(phase.Draining) {
		t.Fatalf("world-0 phase %s is not being taken down once the proxy stage stands", srv.Status.Phase)
	}
}

func TestChangeoverSiblingsMarkAnUnobservedSpec(t *testing.T) {
	f := newFixture(t)
	pr := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) { g.Spec.ChangeoverStage = -10 })
	unobserved := func() (bool, bool) {
		t.Helper()
		views, err := changeoverSiblings(f.ctx, f.c, f.ns, f.network.Name, "ServerGroup", "lobby")
		if err != nil {
			t.Fatalf("changeoverSiblings: %v", err)
		}
		for _, v := range views {
			if v.Kind == "ProxyGroup" && v.Name == "gateway" {
				return v.Unobserved, true
			}
		}
		return false, false
	}

	if u, _ := unobserved(); !u {
		t.Fatalf("a proxy group never reconciled is not unobserved")
	}
	f.reconcileProxyGroup(pr, "gateway")
	if u, _ := unobserved(); u {
		t.Fatalf("a reconciled proxy group is still unobserved")
	}
	f.setProxyImage(t, "gateway", nextProxyImage)
	if u, _ := unobserved(); !u {
		t.Fatalf("a spec change not yet reconciled is not unobserved")
	}

	g := f.proxyGroup("gateway")
	g.Finalizers = append(g.Finalizers, "spawnery.cloud/test-hold")
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("hold gateway: %v", err)
	}
	t.Cleanup(func() {
		g := &spawneryv1alpha1.ProxyGroup{}
		if err := f.c.Get(context.Background(), types.NamespacedName{Name: "gateway", Namespace: f.ns}, g); err == nil {
			g.Finalizers = nil
			_ = f.c.Update(context.Background(), g)
		}
	})
	if err := f.c.Delete(f.ctx, g); err != nil {
		t.Fatalf("delete gateway: %v", err)
	}
	if _, present := unobserved(); present {
		t.Fatalf("a proxy group being deleted is still a changeover sibling")
	}
}

func TestADeletedServerGroupIsNoChangeoverSibling(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "world", 1)
	f.setStage(t, "world", -10)
	f.reconcileNamedGroup(t, r, "world")
	sibling := func() bool {
		t.Helper()
		views, err := changeoverSiblings(f.ctx, f.c, f.ns, f.network.Name, "ProxyGroup", "gateway")
		if err != nil {
			t.Fatalf("changeoverSiblings: %v", err)
		}
		return slices.ContainsFunc(views, func(v ChangeoverView) bool { return v.Kind == "ServerGroup" && v.Name == "world" })
	}
	if !sibling() {
		t.Fatalf("world is not a changeover sibling before its deletion")
	}

	g := f.serverGroup(t, "world")
	g.Finalizers = append(g.Finalizers, "spawnery.cloud/test-hold")
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("hold world: %v", err)
	}
	t.Cleanup(func() {
		g := &spawneryv1alpha1.ServerGroup{}
		if err := f.c.Get(context.Background(), types.NamespacedName{Name: "world", Namespace: f.ns}, g); err == nil {
			g.Finalizers = nil
			_ = f.c.Update(context.Background(), g)
		}
	})
	if err := f.c.Delete(f.ctx, g); err != nil {
		t.Fatalf("delete world: %v", err)
	}
	if sibling() {
		t.Fatalf("a server group being deleted is still a changeover sibling")
	}
}

func TestARefusedProxyGroupObservesItsSpec(t *testing.T) {
	f := newFixture(t)
	pr := proxyGroupReconciler(f)
	f.readyProxyGroup(t, pr, "gateway")
	gateway := f.proxyGroup("gateway")
	gateway.Spec.Scheduling = &spawneryv1alpha1.Scheduling{
		Tolerations: []corev1.Toleration{{Key: "spawnery.cloud/test-refusal", Operator: corev1.TolerationOpExists}},
	}
	if err := f.c.Update(f.ctx, gateway); err != nil {
		t.Fatalf("update gateway: %v", err)
	}
	f.reconcileProxyGroup(pr, "gateway")

	g := f.proxyGroup("gateway")
	if c := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionAccepted); c == nil ||
		c.Reason != spawneryv1alpha1.ReasonSchedulingNotAllowed {
		t.Fatalf("gateway Accepted condition = %+v, want SchedulingNotAllowed", c)
	}
	if g.Status.ObservedGeneration != g.Generation {
		t.Fatalf("refused gateway observedGeneration = %d, generation %d", g.Status.ObservedGeneration, g.Generation)
	}
}

func TestARefusedServerGroupObservesItsSpec(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileNamedGroup(t, r, "lobby")
	lobby := f.serverGroup(t, "lobby")
	lobby.Spec.Scheduling = &spawneryv1alpha1.Scheduling{
		Tolerations: []corev1.Toleration{{Key: "spawnery.cloud/test-refusal", Operator: corev1.TolerationOpExists}},
	}
	if err := f.c.Update(f.ctx, lobby); err != nil {
		t.Fatalf("update lobby: %v", err)
	}
	f.reconcileNamedGroup(t, r, "lobby")

	g := f.serverGroup(t, "lobby")
	if c := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionAccepted); c == nil ||
		c.Reason != spawneryv1alpha1.ReasonSchedulingNotAllowed {
		t.Fatalf("lobby Accepted condition = %+v, want SchedulingNotAllowed", c)
	}
	if g.Status.ObservedGeneration != g.Generation {
		t.Fatalf("refused lobby observedGeneration = %d, generation %d", g.Status.ObservedGeneration, g.Generation)
	}
}

func TestAProxyStageGatesWithTheBudgetUnset(t *testing.T) {
	f := newFixture(t)
	gr := groupReconciler(f)
	pr := proxyGroupReconciler(f)
	f.reconcileNamedGroup(t, gr, "lobby")
	f.readyAllServersOf(t, "lobby")
	f.readyProxyGroup(t, pr, "gateway", func(g *spawneryv1alpha1.ProxyGroup) { g.Spec.ChangeoverStage = 10 })

	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, gr, "lobby")
	f.setProxyImage(t, "gateway", nextProxyImage)
	f.reconcileProxyGroup(pr, "gateway")

	if n := len(f.proxyPods("gateway")); n != 2 {
		t.Fatalf("gateway has %d pods, want 2: stage 10 waits for lobby", n)
	}
	g := f.proxyGroup("gateway")
	if g.Status.Changeover != spawneryv1alpha1.ChangeoverWaiting {
		t.Fatalf("gateway status.changeover = %q, want Waiting", g.Status.Changeover)
	}
	if c := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionChangingOver); c == nil ||
		c.Status != metav1.ConditionTrue || c.Message != "waiting for stage 0: lobby" {
		t.Fatalf("gateway ChangingOver = %+v, want True waiting for stage 0: lobby", c)
	}

	f.finishChangeover(t, gr, "lobby")
	f.reconcileProxyGroup(pr, "gateway")
	if n := len(f.proxyPods("gateway")); n != 4 {
		t.Fatalf("gateway has %d pods, want 4 once lobby is through", n)
	}
}

func (f *fixture) readyProxyGroup(t *testing.T, r *ProxyGroupReconciler, name string, mutate ...func(*spawneryv1alpha1.ProxyGroup)) []string {
	t.Helper()
	f.createProxyGroup(name, mutate...)
	f.reconcileProxyGroup(r, name)
	pods := f.proxyPods(name)
	names := make([]string, 0, len(pods))
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
		names = append(names, pods[i].Name)
	}
	return names
}

func (f *fixture) setProxyImage(t *testing.T, name, image string) {
	t.Helper()
	g := f.proxyGroup(name)
	g.Spec.Image = image
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update ProxyGroup %s: %v", name, err)
	}
}

// deletePod with hold leaves the pod terminating behind a finalizer.
func (f *fixture) deletePod(t *testing.T, name string, hold bool) {
	t.Helper()
	pod, ok := f.pod(name)
	if !ok {
		t.Fatalf("pod %s not found", name)
	}
	if hold {
		pod.Finalizers = append(pod.Finalizers, "spawnery.cloud/test-hold")
		if err := f.c.Update(f.ctx, pod); err != nil {
			t.Fatalf("hold pod %s: %v", name, err)
		}
		t.Cleanup(func() {
			if p, _ := f.pod(name); p != nil {
				p.Finalizers = nil
				_ = f.c.Update(context.Background(), p)
			}
		})
	}
	if err := f.c.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatalf("delete pod %s: %v", name, err)
	}
}
