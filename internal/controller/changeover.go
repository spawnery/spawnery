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
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

type ChangeoverView struct {
	Kind       string // "ServerGroup" or "ProxyGroup"
	Name       string
	State      spawneryv1alpha1.ChangeoverState
	Failing    bool
	Stage      int32
	Persistent bool // gated by stage, takes no budget place: it never surges
	Unobserved bool // a spec change not yet reconciled: State may predate it
}

func changeoverKey(kind, name string) string { return kind + "/" + name }

func changeoverInFlight(g ChangeoverView) bool {
	return !g.Failing && (g.State == spawneryv1alpha1.ChangeoverWaiting || g.State == spawneryv1alpha1.ChangeoverBegun)
}

// changeoverGates lets an unobserved group gate even when Failing: Failing is
// read from the same status its unobserved spec change may have outdated.
func changeoverGates(g ChangeoverView) bool {
	return changeoverInFlight(g) || g.Unobserved
}

// earliestStageBefore is the lowest stage below stage with a group in flight,
// and that stage's groups by name.
func earliestStageBefore(groups []ChangeoverView, stage int32) (int32, []string, bool) {
	found := false
	var lowest int32
	for _, g := range groups {
		if changeoverGates(g) && g.Stage < stage && (!found || g.Stage < lowest) {
			lowest, found = g.Stage, true
		}
	}
	if !found {
		return 0, nil, false
	}
	seen := map[string]bool{}
	var names []string
	for _, g := range groups {
		name := g.Name
		if g.Unobserved {
			name += " (not yet reconciled)"
		}
		if changeoverGates(g) && g.Stage == lowest && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return lowest, names, true
}

// AdmitChangeovers returns the groups, keyed "Kind/Name", that may change over
// now. budget < 1 means no cap; the stage gate applies either way.
func AdmitChangeovers(groups []ChangeoverView, budget int32) map[string]bool {
	admitted := map[string]bool{}
	var waiting []ChangeoverView
	var holders int32
	for _, g := range groups {
		if !changeoverInFlight(g) {
			continue
		}
		if g.State == spawneryv1alpha1.ChangeoverBegun {
			admitted[changeoverKey(g.Kind, g.Name)] = true
			if !g.Persistent {
				holders++
			}
			continue
		}
		if _, _, gated := earliestStageBefore(groups, g.Stage); gated {
			continue
		}
		if g.Persistent || budget < 1 {
			admitted[changeoverKey(g.Kind, g.Name)] = true
			continue
		}
		waiting = append(waiting, g)
	}
	sort.Slice(waiting, func(i, j int) bool {
		a, b := waiting[i], waiting[j]
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Kind < b.Kind
	})
	for _, g := range waiting {
		if holders >= budget {
			break
		}
		admitted[changeoverKey(g.Kind, g.Name)] = true
		holders++
	}
	return admitted
}

// ChangeoverWait's zero value means the group was admitted.
type ChangeoverWait struct {
	Reason  string
	Message string
}

func describeWait(groups []ChangeoverView, budget int32, self ChangeoverView) ChangeoverWait {
	if stage, names, ok := earliestStageBefore(groups, self.Stage); ok {
		return ChangeoverWait{
			Reason:  spawneryv1alpha1.ReasonWaitingForEarlierStage,
			Message: fmt.Sprintf("waiting for stage %d: %s", stage, strings.Join(names, ", ")),
		}
	}
	holders := changeoverHolders(groups, AdmitChangeovers(groups, budget), self.Kind, self.Name)
	if len(holders) == 0 {
		return ChangeoverWait{}
	}
	return ChangeoverWait{
		Reason:  spawneryv1alpha1.ReasonWaitingForChangeoverBudget,
		Message: "waiting for a changeover place; changing over: " + strings.Join(holders, ", "),
	}
}

func serverChangeoverSelf(group *spawneryv1alpha1.ServerGroup, state spawneryv1alpha1.ChangeoverState) ChangeoverView {
	return ChangeoverView{
		Kind: "ServerGroup", Name: group.Name, State: state,
		Failing: changeoverFailing(group.Status.Conditions),
		Stage:   group.Spec.ChangeoverStage, Persistent: !group.IsEphemeral(),
	}
}

func serverChangeoverWait(group *spawneryv1alpha1.ServerGroup, siblings []ChangeoverView, budget int32) ChangeoverWait {
	self := serverChangeoverSelf(group, spawneryv1alpha1.ChangeoverWaiting)
	return describeWait(append(slices.Clip(siblings), self), budget, self)
}

// changeoverRefused: without a budget, and for a persistent self (which takes
// no place), only the stage gate refuses. A failing self is admitted by
// nothing, since the pass on which its backoff expires still reads BackingOff.
func changeoverRefused(siblings []ChangeoverView, budget int32, self ChangeoverView) bool {
	if self.State != spawneryv1alpha1.ChangeoverWaiting {
		return false
	}
	if budget < 1 || self.Persistent {
		_, _, gated := earliestStageBefore(siblings, self.Stage)
		return gated
	}
	return !AdmitChangeovers(append(slices.Clip(siblings), self), budget)[changeoverKey(self.Kind, self.Name)]
}

func changeoverFailing(conditions []metav1.Condition) bool {
	return meta.IsStatusConditionTrue(conditions, spawneryv1alpha1.ConditionBackingOff) ||
		meta.IsStatusConditionTrue(conditions, spawneryv1alpha1.ConditionDegraded)
}

// changeoverSiblings excludes the caller and groups being deleted.
func changeoverSiblings(
	ctx context.Context, c client.Reader, namespace, network, selfKind, selfName string,
) ([]ChangeoverView, error) {
	var views []ChangeoverView
	servers := &spawneryv1alpha1.ServerGroupList{}
	if err := c.List(ctx, servers, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range servers.Items {
		g := &servers.Items[i]
		if g.Spec.NetworkRef.Name != network || (selfKind == "ServerGroup" && g.Name == selfName) ||
			g.IsOnDemand() || !g.DeletionTimestamp.IsZero() {
			continue
		}
		views = append(views, ChangeoverView{
			Kind: "ServerGroup", Name: g.Name,
			State: g.Status.Changeover, Failing: changeoverFailing(g.Status.Conditions),
			Stage: g.Spec.ChangeoverStage, Persistent: !g.IsEphemeral(),
			Unobserved: g.Generation != g.Status.ObservedGeneration,
		})
	}
	proxies := &spawneryv1alpha1.ProxyGroupList{}
	if err := c.List(ctx, proxies, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range proxies.Items {
		g := &proxies.Items[i]
		if g.Spec.NetworkRef.Name != network || (selfKind == "ProxyGroup" && g.Name == selfName) ||
			!g.DeletionTimestamp.IsZero() {
			continue
		}
		views = append(views, ChangeoverView{
			Kind: "ProxyGroup", Name: g.Name,
			State: g.Status.Changeover, Failing: changeoverFailing(g.Status.Conditions),
			Stage:      g.Spec.ChangeoverStage,
			Unobserved: g.Generation != g.Status.ObservedGeneration,
		})
	}
	return views, nil
}

// changeoverHolders names the groups other than the caller that AdmitChangeovers
// admitted, sorted and without repeats: the ones a waiting group waits for.
func changeoverHolders(groups []ChangeoverView, admitted map[string]bool, selfKind, selfName string) []string {
	seen := map[string]bool{}
	var names []string
	for _, g := range groups {
		if (g.Kind == selfKind && g.Name == selfName) || !admitted[changeoverKey(g.Kind, g.Name)] || seen[g.Name] {
			continue
		}
		seen[g.Name] = true
		names = append(names, g.Name)
	}
	sort.Strings(names)
	return names
}

// ownServerChangeover: a stale server that is leaving still counts as the
// group's extra server. A WhenEmpty group with a Ready current server, and a
// RollingUpdate group whose every stale server is retiring with a current one
// up, is Deferred, and stays so through a readiness loss, so it holds no budget
// place nobody admitted it to.
func ownServerChangeover(views []ServerView, podHash string, pendingCreates int32, whenEmpty bool, was spawneryv1alpha1.ChangeoverState) spawneryv1alpha1.ChangeoverState {
	var stale, staleServing, current, readyCurrent, unreadyCurrent bool
	for _, v := range views {
		if v.Hold {
			continue
		}
		if staleSpec(v, podHash) {
			if !phase.Terminal(v.Phase) {
				stale = true
				if !v.leaving() && !v.Retire {
					staleServing = true
				}
			}
		} else if v.countsTowardSize() {
			current = true
			if v.Phase == phase.Ready {
				readyCurrent = true
			} else {
				unreadyCurrent = true
			}
		}
	}
	wasDeferred := was == spawneryv1alpha1.ChangeoverDeferred
	switch {
	case !stale:
		return spawneryv1alpha1.ChangeoverNone
	case whenEmpty && (readyCurrent || (wasDeferred && current)):
		return spawneryv1alpha1.ChangeoverDeferred
	case !whenEmpty && !staleServing && current &&
		(wasDeferred || (pendingCreates == 0 && !unreadyCurrent)):
		return spawneryv1alpha1.ChangeoverDeferred
	case current || pendingCreates > 0:
		return spawneryv1alpha1.ChangeoverBegun
	default:
		return spawneryv1alpha1.ChangeoverWaiting
	}
}

// ownPersistentChangeover begins with the first stale takedown; a current
// ordinal added meanwhile does not begin it.
func ownPersistentChangeover(views []ServerView, podHash string, pendingDeletes map[string]bool, replicas int32, was spawneryv1alpha1.ChangeoverState) spawneryv1alpha1.ChangeoverState {
	stale, takedown := false, false
	for _, v := range views {
		if v.Hold || !staleSpec(v, podHash) {
			continue
		}
		if !phase.Terminal(v.Phase) {
			stale = true
		}
		if v.Ordinal != nil && *v.Ordinal < replicas && (v.leaving() || pendingDeletes[v.Name]) {
			takedown = true
		}
	}
	switch {
	case !stale:
		return spawneryv1alpha1.ChangeoverNone
	case takedown || was == spawneryv1alpha1.ChangeoverBegun:
		return spawneryv1alpha1.ChangeoverBegun
	default:
		return spawneryv1alpha1.ChangeoverWaiting
	}
}

// ownProxyChangeover counts a draining or terminating stale pod as the
// group's extra pod. It is Deferred once replicas current pods are Ready and
// every remaining stale pod is leaving, and stays so through a readiness blip.
func ownProxyChangeover(pods []corev1.Pod, wantHash string, pendingCreates, replicas int32, was spawneryv1alpha1.ChangeoverState) spawneryv1alpha1.ChangeoverState {
	var stale, staleServing, current bool
	var readyCurrent int32
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		if p.Labels[podspec.LabelPodHash] != wantHash {
			stale = true
			if _, marked := drainingSince(p); !marked && p.DeletionTimestamp.IsZero() {
				staleServing = true
			}
		} else if p.DeletionTimestamp.IsZero() {
			current = true
			if isPodReady(p) {
				readyCurrent++
			}
		}
	}
	switch {
	case !stale:
		return spawneryv1alpha1.ChangeoverNone
	case !staleServing && current &&
		(was == spawneryv1alpha1.ChangeoverDeferred || (readyCurrent >= replicas && pendingCreates == 0)):
		return spawneryv1alpha1.ChangeoverDeferred
	case current || pendingCreates > 0:
		return spawneryv1alpha1.ChangeoverBegun
	default:
		return spawneryv1alpha1.ChangeoverWaiting
	}
}

func proxyChangeover(
	ctx context.Context, c client.Reader, network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup, wantHash string, pendingCreates int32,
) (spawneryv1alpha1.ChangeoverState, bool, ChangeoverWait, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(group.Namespace),
		client.MatchingLabels(podspec.ProxyLabels(network.Name, group.Name))); err != nil {
		return "", false, ChangeoverWait{}, err
	}
	own := ownProxyChangeover(pods.Items, wantHash, pendingCreates, group.Spec.Replicas, group.Status.Changeover)
	if own != spawneryv1alpha1.ChangeoverWaiting {
		return own, true, ChangeoverWait{}, nil
	}
	siblings, err := changeoverSiblings(ctx, c, group.Namespace, network.Name, "ProxyGroup", group.Name)
	if err != nil {
		return "", false, ChangeoverWait{}, err
	}
	self := ChangeoverView{
		Kind: "ProxyGroup", Name: group.Name, State: own,
		Failing: changeoverFailing(group.Status.Conditions), Stage: group.Spec.ChangeoverStage,
	}
	budget := network.ChangeoverBudget()
	if !changeoverRefused(siblings, budget, self) {
		return own, true, ChangeoverWait{}, nil
	}
	return own, false, describeWait(append(slices.Clip(siblings), self), budget, self), nil
}
