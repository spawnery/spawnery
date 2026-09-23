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

	if n := len(f.proxyPods("gateway")); n != 3 {
		t.Fatalf("gateway has %d pods, want 3: the place is free, the surge pod comes", n)
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

	if n := len(f.proxyPods("gateway")); n != 3 {
		t.Fatalf("gateway has %d pods, want 3: no budget refuses nothing", n)
	}
}

// A stale proxy that is draining or terminating still exists, so the group
// keeps its place until it is gone.
func TestChangeoverBudgetHeldUntilTheLastStaleProxyIsGone(t *testing.T) {
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
			if len(pods) != 3 {
				t.Fatalf("gateway has %d pods, want 3", len(pods))
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

			if got := f.proxyGroup("gateway").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
				t.Fatalf("gateway status.changeover = %q with its last stale pod %s, want Begun", got, leaving)
			}
			if n := len(f.serverNamesOfGroup(t, "lobby")); n != 1 {
				t.Fatalf("lobby has %d servers, want 1: gateway still holds the place", n)
			}
		})
	}
}

// A Waiting proxy group that is then refused must give up its place: refuse()
// used to leave status.changeover untouched, so the refused group kept
// winning AdmitChangeovers's name-ordered admission forever, and a real
// waiting sibling named after it never got in.
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

	// lobby is reconciled first and alone, so it takes the network's one
	// place and holds it.
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

	// gateway is refused on an unrelated ground -- its own scheduling, which
	// the network does not allow -- while it is still Waiting.
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

	// The holder finishes; the place is free. zulu, waiting since before
	// gateway was refused and named after it, must be admitted -- not
	// blocked forever by a refused group AdmitChangeovers still counts as
	// Waiting.
	f.finishChangeover(t, gr, "lobby")
	f.reconcileProxyGroup(pr, "zulu")
	if n := len(f.proxyPods("zulu")); n != 3 {
		t.Fatalf("zulu has %d pods, want 3: the place is free, the surge pod comes", n)
	}
	if got := f.proxyGroup("zulu").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("zulu status.changeover = %q, want Begun", got)
	}
}

// readyProxyGroup creates a proxy group, reconciles it once and readies its
// pods, returning their names.
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

// deletePod deletes a pod at once, or, held by a finalizer, leaves it
// terminating.
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
