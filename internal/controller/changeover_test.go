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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

func TestAdmitChangeovers(t *testing.T) {
	cases := []struct {
		name   string
		groups []ChangeoverView
		budget int32
		want   map[string]bool
	}{
		{
			"a deferred group neither holds nor waits",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverDeferred},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ServerGroup/lobby": true},
		},
		{
			"no cap admits everyone changing over",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
				{Kind: "ServerGroup", Name: "hub", State: spawneryv1alpha1.ChangeoverBegun},
			},
			0,
			map[string]bool{"ServerGroup/lobby": true, "ServerGroup/hub": true},
		},
		{
			"a begun holder keeps its place and the waiting group stays out",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "hub", State: spawneryv1alpha1.ChangeoverBegun},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ServerGroup/hub": true},
		},
		{
			"an empty budget with only waiting groups admits by name order",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
				{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ServerGroup/arena": true},
		},
		{
			"a begun group is never displaced by a waiting one",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "zeta", State: spawneryv1alpha1.ChangeoverBegun},
				{Kind: "ServerGroup", Name: "alpha", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ServerGroup/zeta": true},
		},
		{
			"a failing holder frees its place but is not admitted itself",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "hub", State: spawneryv1alpha1.ChangeoverBegun, Failing: true},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ServerGroup/lobby": true},
		},
		{
			"a failing waiting group is skipped entirely",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting, Failing: true},
				{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			2,
			map[string]bool{"ServerGroup/arena": true},
		},
		{
			"kind is part of the key, so a ProxyGroup and ServerGroup of the same name are distinct",
			[]ChangeoverView{
				{Kind: "ProxyGroup", Name: "gateway", State: spawneryv1alpha1.ChangeoverBegun},
				{Kind: "ServerGroup", Name: "gateway", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ProxyGroup/gateway": true},
		},
		{
			"two begun holders over budget after a race both keep their place over a waiting group",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "a", State: spawneryv1alpha1.ChangeoverBegun},
				{Kind: "ServerGroup", Name: "b", State: spawneryv1alpha1.ChangeoverBegun},
				{Kind: "ServerGroup", Name: "c", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			2,
			map[string]bool{"ServerGroup/a": true, "ServerGroup/b": true},
		},
		{
			"a group with no changeover state is never in the result without a cap",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "idle", State: spawneryv1alpha1.ChangeoverNone},
			},
			0,
			map[string]bool{},
		},
		{
			"a group with no changeover state is never in the result under a cap",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "idle", State: spawneryv1alpha1.ChangeoverNone},
			},
			1,
			map[string]bool{},
		},
		{
			"two waiting groups of the same name are admitted by kind",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "gateway", State: spawneryv1alpha1.ChangeoverWaiting},
				{Kind: "ProxyGroup", Name: "gateway", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ProxyGroup/gateway": true},
		},
		{
			"a later stage waits for an earlier one in flight, budget unset",
			[]ChangeoverView{
				{Kind: "ProxyGroup", Name: "edge", State: spawneryv1alpha1.ChangeoverBegun, Stage: -10},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			0,
			map[string]bool{"ProxyGroup/edge": true},
		},
		{
			"a waiting earlier stage gates as much as a begun one",
			[]ChangeoverView{
				{Kind: "ProxyGroup", Name: "edge", State: spawneryv1alpha1.ChangeoverWaiting, Stage: -10},
				{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ProxyGroup/edge": true},
		},
		{
			"a deferred earlier stage gates nothing",
			[]ChangeoverView{
				{Kind: "ProxyGroup", Name: "edge", State: spawneryv1alpha1.ChangeoverDeferred, Stage: -10},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ServerGroup/lobby": true},
		},
		{
			"a failing earlier stage gates nothing",
			[]ChangeoverView{
				{Kind: "ProxyGroup", Name: "edge", State: spawneryv1alpha1.ChangeoverBegun, Stage: -10, Failing: true},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ServerGroup/lobby": true},
		},
		{
			"one stage changes over together within the budget, ordered by name",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting, Stage: 5},
				{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverWaiting, Stage: 5},
				{Kind: "ServerGroup", Name: "build", State: spawneryv1alpha1.ChangeoverWaiting, Stage: 5},
			},
			2,
			map[string]bool{"ServerGroup/arena": true, "ServerGroup/build": true},
		},
		{
			"the budget goes by stage before name",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverWaiting, Stage: 1},
				{Kind: "ServerGroup", Name: "zeta", State: spawneryv1alpha1.ChangeoverWaiting, Stage: 1},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting, Stage: 0},
			},
			1,
			map[string]bool{"ServerGroup/lobby": true},
		},
		{
			"a begun later stage is not paused by an earlier stage turning stale",
			[]ChangeoverView{
				{Kind: "ProxyGroup", Name: "edge", State: spawneryv1alpha1.ChangeoverWaiting, Stage: -10},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverBegun},
			},
			2,
			map[string]bool{"ProxyGroup/edge": true, "ServerGroup/lobby": true},
		},
		{
			"a persistent group gates a later stage but takes no budget place",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "world", State: spawneryv1alpha1.ChangeoverBegun, Persistent: true},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
				{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverWaiting, Stage: 10},
			},
			1,
			map[string]bool{"ServerGroup/world": true, "ServerGroup/lobby": true},
		},
		{
			"an unobserved earlier stage gates a waiting group",
			[]ChangeoverView{
				{Kind: "ProxyGroup", Name: "edge", Stage: -10, Unobserved: true},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			0,
			map[string]bool{},
		},
		{
			"an unobserved earlier stage gates even when its stale status says failing",
			[]ChangeoverView{
				{Kind: "ProxyGroup", Name: "edge", Stage: -10, Unobserved: true, Failing: true},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{},
		},
		{
			"an unobserved group takes no budget place",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "arena", Unobserved: true},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ServerGroup/lobby": true},
		},
		{
			"an unobserved group in the same or a later stage gates nothing",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "arena", Unobserved: true},
				{Kind: "ProxyGroup", Name: "edge", Stage: 10, Unobserved: true},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			0,
			map[string]bool{"ServerGroup/lobby": true},
		},
		{
			"a waiting persistent group is admitted past a full budget",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverBegun},
				{Kind: "ServerGroup", Name: "world", State: spawneryv1alpha1.ChangeoverWaiting, Persistent: true},
			},
			1,
			map[string]bool{"ServerGroup/lobby": true, "ServerGroup/world": true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AdmitChangeovers(tc.groups, tc.budget)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("AdmitChangeovers(%+v, %d) = %v, want %v", tc.groups, tc.budget, got, tc.want)
			}
		})
	}
}

func TestDescribeWait(t *testing.T) {
	edge := ChangeoverView{Kind: "ProxyGroup", Name: "edge", State: spawneryv1alpha1.ChangeoverBegun, Stage: -10}
	edge2 := ChangeoverView{Kind: "ProxyGroup", Name: "edge-b", State: spawneryv1alpha1.ChangeoverWaiting, Stage: -10}
	deeper := ChangeoverView{Kind: "ProxyGroup", Name: "outer", State: spawneryv1alpha1.ChangeoverBegun, Stage: -20}
	arena := ChangeoverView{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverBegun}
	lobby := ChangeoverView{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting}
	for _, tc := range []struct {
		name   string
		groups []ChangeoverView
		budget int32
		want   ChangeoverWait
	}{
		{"an earlier stage names that stage and its groups", []ChangeoverView{edge2, edge, lobby}, 0,
			ChangeoverWait{spawneryv1alpha1.ReasonWaitingForEarlierStage, "waiting for stage -10: edge, edge-b"}},
		{"the lowest earlier stage is the one named", []ChangeoverView{edge, deeper, lobby}, 0,
			ChangeoverWait{spawneryv1alpha1.ReasonWaitingForEarlierStage, "waiting for stage -20: outer"}},
		{"no earlier stage: the budget is named", []ChangeoverView{arena, lobby}, 1,
			ChangeoverWait{spawneryv1alpha1.ReasonWaitingForChangeoverBudget, "waiting for a changeover place; changing over: arena"}},
		{"an unobserved earlier stage is named", []ChangeoverView{{Kind: "ProxyGroup", Name: "edge", Stage: -10, Unobserved: true}, lobby}, 0,
			ChangeoverWait{spawneryv1alpha1.ReasonWaitingForEarlierStage, "waiting for stage -10: edge (not yet reconciled)"}},
		{"an unobserved group is named apart from an observed one", []ChangeoverView{edge, {Kind: "ProxyGroup", Name: "edge-b", Stage: -10, Unobserved: true}, lobby}, 0,
			ChangeoverWait{spawneryv1alpha1.ReasonWaitingForEarlierStage, "waiting for stage -10: edge, edge-b (not yet reconciled)"}},
		{"no holder to name: no wait", []ChangeoverView{lobby}, 0, ChangeoverWait{}},
		{"a failing self holds nothing: no wait", []ChangeoverView{{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting, Failing: true}}, 1, ChangeoverWait{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeWait(tc.groups, tc.budget, lobby); got != tc.want {
				t.Errorf("describeWait = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestServerChangeoverWaitOfAFailingGroup(t *testing.T) {
	alpha := &spawneryv1alpha1.ServerGroup{}
	alpha.Name = "alpha"
	alpha.Spec.Type = spawneryv1alpha1.ServerGroupEphemeral
	alpha.Status.Conditions = []metav1.Condition{{Type: spawneryv1alpha1.ConditionBackingOff, Status: metav1.ConditionTrue}}
	beta := ChangeoverView{Kind: "ServerGroup", Name: "beta", State: spawneryv1alpha1.ChangeoverWaiting}
	want := ChangeoverWait{spawneryv1alpha1.ReasonWaitingForChangeoverBudget, "waiting for a changeover place; changing over: beta"}
	if got := serverChangeoverWait(alpha, []ChangeoverView{beta}, 1); got != want {
		t.Errorf("serverChangeoverWait = %+v, want %+v", got, want)
	}
	if got := serverChangeoverWait(alpha, nil, 1); got != (ChangeoverWait{}) {
		t.Errorf("serverChangeoverWait with nobody changing over = %+v, want none", got)
	}
}

func TestOwnProxyChangeover(t *testing.T) {
	pod := func(hash string, mutate ...func(*corev1.Pod)) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{podspec.LabelPodHash: hash}}}
		for _, m := range mutate {
			m(&p)
		}
		return p
	}
	terminating := func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now }
	failed := func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }
	ready := func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	draining := func(p *corev1.Pod) {
		p.Annotations = map[string]string{ProxyDrainingSinceAnnotation: "2026-10-01T12:00:00Z"}
	}
	cases := []struct {
		name     string
		pods     []corev1.Pod
		pending  int32
		replicas int32
		was      spawneryv1alpha1.ChangeoverState
		want     spawneryv1alpha1.ChangeoverState
	}{
		{"all current", []corev1.Pod{pod("new"), pod("new")}, 0, 2, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverNone},
		{"stale only", []corev1.Pod{pod("old"), pod("old")}, 0, 2, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverWaiting},
		{"stale and current", []corev1.Pod{pod("old"), pod("new")}, 0, 2, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverBegun},
		{"stale only with a create the cache has not shown", []corev1.Pod{pod("old"), pod("old")}, 1, 2, spawneryv1alpha1.ChangeoverBegun,
			spawneryv1alpha1.ChangeoverBegun},
		{"terminating stale beside current", []corev1.Pod{pod("old", terminating), pod("new")}, 0, 2, spawneryv1alpha1.ChangeoverBegun,
			spawneryv1alpha1.ChangeoverBegun},
		{"terminating current is not begun", []corev1.Pod{pod("old"), pod("new", terminating)}, 0, 2, spawneryv1alpha1.ChangeoverBegun,
			spawneryv1alpha1.ChangeoverWaiting},
		{"failed stale is gone", []corev1.Pod{pod("old", failed), pod("new")}, 0, 2, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverNone},
		{"replicas current Ready and every stale pod draining is deferred",
			[]corev1.Pod{pod("old", draining), pod("old", draining), pod("new", ready), pod("new", ready)}, 0, 2, spawneryv1alpha1.ChangeoverBegun,
			spawneryv1alpha1.ChangeoverDeferred},
		{"one current pod short of replicas is begun",
			[]corev1.Pod{pod("old", draining), pod("old", draining), pod("new", ready), pod("new")}, 0, 2, spawneryv1alpha1.ChangeoverBegun,
			spawneryv1alpha1.ChangeoverBegun},
		{"a stale pod not yet draining is begun",
			[]corev1.Pod{pod("old", draining), pod("old", ready), pod("new", ready), pod("new", ready)}, 0, 2, spawneryv1alpha1.ChangeoverBegun,
			spawneryv1alpha1.ChangeoverBegun},
		{"deferred stays deferred through a readiness blip",
			[]corev1.Pod{pod("old", draining), pod("new", ready), pod("new")}, 0, 2, spawneryv1alpha1.ChangeoverDeferred,
			spawneryv1alpha1.ChangeoverDeferred},
		{"terminating stale pods count as leaving",
			[]corev1.Pod{pod("old", terminating), pod("new", ready), pod("new", ready)}, 0, 2, spawneryv1alpha1.ChangeoverBegun,
			spawneryv1alpha1.ChangeoverDeferred},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownProxyChangeover(tc.pods, "new", tc.pending, tc.replicas, tc.was); got != tc.want {
				t.Errorf("ownProxyChangeover = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOwnServerChangeover(t *testing.T) {
	old := ServerView{Name: "old", PodHash: "old", Phase: phase.Ready}
	current := ServerView{Name: "new", PodHash: "current", Phase: phase.Ready}
	startingCurrent := ServerView{Name: "new", PodHash: "current", Phase: phase.Starting}
	retiringOld := ServerView{Name: "old-r", PodHash: "old", Phase: phase.Retiring, Retire: true}
	for _, tc := range []struct {
		name      string
		views     []ServerView
		pending   int32
		whenEmpty bool
		was       spawneryv1alpha1.ChangeoverState
		want      spawneryv1alpha1.ChangeoverState
	}{
		{"nothing stale", []ServerView{current}, 0, true, "", spawneryv1alpha1.ChangeoverNone},
		{"stale only, nothing asked for", []ServerView{old}, 0, true, "", spawneryv1alpha1.ChangeoverWaiting},
		{"WhenEmpty with its first server starting", []ServerView{old, startingCurrent}, 0, true, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverBegun},
		{"WhenEmpty with a Ready current server", []ServerView{old, current}, 0, true, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverDeferred},
		{"RollingUpdate with a Ready current server", []ServerView{old, current}, 0, false, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverBegun},
		{"a held stale server is no changeover", []ServerView{{Name: "old", PodHash: "old", Phase: phase.Ready, Hold: true}, current}, 0, false, "", spawneryv1alpha1.ChangeoverNone},
		{"deferred stays deferred while its current server is not Ready", []ServerView{old, startingCurrent}, 0, true, spawneryv1alpha1.ChangeoverDeferred, spawneryv1alpha1.ChangeoverDeferred},
		{"deferred with no current server left waits again", []ServerView{old}, 0, true, spawneryv1alpha1.ChangeoverDeferred, spawneryv1alpha1.ChangeoverWaiting},
		{"RollingUpdate with every stale server retiring and the current one Ready", []ServerView{retiringOld, current}, 0, false, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverDeferred},
		{"RollingUpdate with a stale server not yet asked to leave", []ServerView{retiringOld, old, current}, 0, false, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverBegun},
		{"RollingUpdate with a replacement still starting", []ServerView{retiringOld, current, startingCurrent}, 0, false, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverBegun},
		{"RollingUpdate with a create in flight", []ServerView{retiringOld, current}, 1, false, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverBegun},
		{"RollingUpdate deferred stays deferred through a readiness blip", []ServerView{retiringOld, startingCurrent}, 0, false, spawneryv1alpha1.ChangeoverDeferred, spawneryv1alpha1.ChangeoverDeferred},
		{"RollingUpdate deferred falls back once a stale server is serving again", []ServerView{old, current}, 0, false, spawneryv1alpha1.ChangeoverDeferred, spawneryv1alpha1.ChangeoverBegun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownServerChangeover(tc.views, "current", tc.pending, tc.whenEmpty, tc.was); got != tc.want {
				t.Errorf("ownServerChangeover = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOwnPersistentChangeover(t *testing.T) {
	stale := ServerView{Name: "w-0", PodHash: "old", Phase: phase.Ready, Ordinal: ptr.To[int32](0)}
	current := ServerView{Name: "w-1", PodHash: "current", Phase: phase.Ready, Ordinal: ptr.To[int32](1)}
	held := ServerView{Name: "w-0", PodHash: "old", Phase: phase.Ready, Ordinal: ptr.To[int32](0), Hold: true}
	staleDraining := ServerView{Name: "w-0", PodHash: "old", Phase: phase.Draining, Ordinal: ptr.To[int32](0)}
	surplus := ServerView{Name: "w-2", PodHash: "old", Phase: phase.Ready, Ordinal: ptr.To[int32](2)}
	for _, tc := range []struct {
		name           string
		views          []ServerView
		pendingDeletes map[string]bool
		was            spawneryv1alpha1.ChangeoverState
		want           spawneryv1alpha1.ChangeoverState
	}{
		{"nothing stale", []ServerView{current}, nil, "", spawneryv1alpha1.ChangeoverNone},
		{"stale, nothing down", []ServerView{stale}, nil, "", spawneryv1alpha1.ChangeoverWaiting},
		{"a stale takedown in flight has begun", []ServerView{staleDraining}, nil, spawneryv1alpha1.ChangeoverWaiting, spawneryv1alpha1.ChangeoverBegun},
		{"a stale delete pending has begun", []ServerView{stale}, map[string]bool{"w-0": true}, spawneryv1alpha1.ChangeoverWaiting, spawneryv1alpha1.ChangeoverBegun},
		{"begun stays begun between takedowns", []ServerView{stale, current}, nil, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverBegun},
		{"a new current ordinal beside stale ones while Waiting stays Waiting", []ServerView{stale, current}, nil, spawneryv1alpha1.ChangeoverWaiting, spawneryv1alpha1.ChangeoverWaiting},
		{"a held stale server is no changeover", []ServerView{held, current}, nil, spawneryv1alpha1.ChangeoverBegun, spawneryv1alpha1.ChangeoverNone},
		{"a surplus ordinal being deleted while a stale one waits stays Waiting", []ServerView{stale, current, surplus}, map[string]bool{"w-2": true}, spawneryv1alpha1.ChangeoverWaiting, spawneryv1alpha1.ChangeoverWaiting},
		{"a current ordinal leaving while a stale one waits stays Waiting", []ServerView{stale, {Name: "w-1", PodHash: "current", Phase: phase.Draining, Ordinal: ptr.To[int32](1)}}, nil, spawneryv1alpha1.ChangeoverWaiting, spawneryv1alpha1.ChangeoverWaiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownPersistentChangeover(tc.views, "current", tc.pendingDeletes, 2, tc.was); got != tc.want {
				t.Errorf("ownPersistentChangeover = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChangeoverRefused(t *testing.T) {
	lobby := ChangeoverView{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverBegun}
	arena := ChangeoverView{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverWaiting}
	failingArena := arena
	failingArena.Failing = true
	laterArena := arena
	laterArena.Stage = 10
	failingLaterArena := laterArena
	failingLaterArena.Failing = true
	persistentArena := arena
	persistentArena.Persistent = true
	failingPersistentArena := persistentArena
	failingPersistentArena.Failing = true
	laterPersistentArena := persistentArena
	laterPersistentArena.Stage = 10
	for _, tc := range []struct {
		name     string
		siblings []ChangeoverView
		budget   int32
		self     ChangeoverView
		want     bool
	}{
		{"not waiting is never refused", []ChangeoverView{lobby}, 1, lobby, false},
		{"no budget, same stage", []ChangeoverView{lobby}, 0, arena, false},
		{"no budget, a failing self is not refused", []ChangeoverView{lobby}, 0, failingArena, false},
		{"no budget, an earlier stage in flight refuses", []ChangeoverView{lobby}, 0, laterArena, true},
		{"no budget, an earlier stage refuses a failing self too", []ChangeoverView{lobby}, 0, failingLaterArena, true},
		{"no budget, an unobserved earlier stage refuses", []ChangeoverView{{Kind: "ProxyGroup", Name: "edge", Stage: -10, Unobserved: true}}, 0, arena, true},
		{"budget set, an unobserved earlier stage refuses a persistent self", []ChangeoverView{{Kind: "ProxyGroup", Name: "edge", Stage: -10, Unobserved: true}}, 1, persistentArena, true},
		{"budget spent refuses", []ChangeoverView{lobby}, 1, arena, true},
		{"budget free admits", []ChangeoverView{lobby}, 2, arena, false},
		{"budget set, a failing self is admitted by nothing", nil, 2, failingArena, true},
		{"budget set, a failing persistent self with no earlier stage in flight is not refused", []ChangeoverView{lobby}, 1, failingPersistentArena, false},
		{"budget spent, a persistent self takes no place", []ChangeoverView{lobby}, 1, persistentArena, false},
		{"budget set, an earlier stage refuses a persistent self", []ChangeoverView{lobby}, 1, laterPersistentArena, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := changeoverRefused(tc.siblings, tc.budget, tc.self); got != tc.want {
				t.Errorf("changeoverRefused = %v, want %v", got, tc.want)
			}
		})
	}
}
