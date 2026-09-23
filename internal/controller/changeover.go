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
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// ChangeoverView is what AdmitChangeovers needs of one group to decide
// whether it may change over.
type ChangeoverView struct {
	Kind    string // "ServerGroup" or "ProxyGroup"
	Name    string
	State   spawneryv1alpha1.ChangeoverState
	Failing bool
}

func changeoverKey(kind, name string) string { return kind + "/" + name }

// AdmitChangeovers returns the groups, keyed "Kind/Name", that may change over
// now. budget < 1 means no cap.
func AdmitChangeovers(groups []ChangeoverView, budget int32) map[string]bool {
	admitted := map[string]bool{}
	var waiting []ChangeoverView
	var holders int32
	for _, g := range groups {
		if g.State == spawneryv1alpha1.ChangeoverNone || g.Failing {
			continue
		}
		if budget < 1 || g.State == spawneryv1alpha1.ChangeoverBegun {
			admitted[changeoverKey(g.Kind, g.Name)] = true
			if g.State == spawneryv1alpha1.ChangeoverBegun {
				holders++
			}
			continue
		}
		waiting = append(waiting, g)
	}
	if budget < 1 {
		return admitted
	}
	sort.Slice(waiting, func(i, j int) bool {
		if waiting[i].Name != waiting[j].Name {
			return waiting[i].Name < waiting[j].Name
		}
		return waiting[i].Kind < waiting[j].Kind
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

func changeoverFailing(conditions []metav1.Condition) bool {
	return meta.IsStatusConditionTrue(conditions, spawneryv1alpha1.ConditionBackingOff) ||
		meta.IsStatusConditionTrue(conditions, spawneryv1alpha1.ConditionDegraded)
}

// changeoverSiblings is every server and proxy group of the network in the
// namespace except the caller, as AdmitChangeovers sees them.
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
		if g.Spec.NetworkRef.Name != network || (selfKind == "ServerGroup" && g.Name == selfName) {
			continue
		}
		views = append(views, ChangeoverView{
			Kind: "ServerGroup", Name: g.Name,
			State: g.Status.Changeover, Failing: changeoverFailing(g.Status.Conditions),
		})
	}
	proxies := &spawneryv1alpha1.ProxyGroupList{}
	if err := c.List(ctx, proxies, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range proxies.Items {
		g := &proxies.Items[i]
		if g.Spec.NetworkRef.Name != network || (selfKind == "ProxyGroup" && g.Name == selfName) {
			continue
		}
		views = append(views, ChangeoverView{
			Kind: "ProxyGroup", Name: g.Name,
			State: g.Status.Changeover, Failing: changeoverFailing(g.Status.Conditions),
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

// ownServerChangeover is a server group's changeover state from its own
// servers. Only an ephemeral group surges, so only its views are passed here.
func ownServerChangeover(views []ServerView, podHash string, pendingCreates int32) spawneryv1alpha1.ChangeoverState {
	var stale, current bool
	for _, v := range views {
		if !v.countsTowardSize() {
			continue
		}
		if staleSpec(v, podHash) {
			stale = true
		} else {
			current = true
		}
	}
	switch {
	case !stale:
		return spawneryv1alpha1.ChangeoverNone
	case current || pendingCreates > 0:
		return spawneryv1alpha1.ChangeoverBegun
	default:
		return spawneryv1alpha1.ChangeoverWaiting
	}
}
