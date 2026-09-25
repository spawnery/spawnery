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
			"a group with no changeover state is never in the result, with or without a cap",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "idle", State: spawneryv1alpha1.ChangeoverNone},
			},
			0,
			map[string]bool{},
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
	cases := []struct {
		name    string
		pods    []corev1.Pod
		pending int32
		want    spawneryv1alpha1.ChangeoverState
	}{
		{"all current", []corev1.Pod{pod("new"), pod("new")}, 0, spawneryv1alpha1.ChangeoverNone},
		{"stale only", []corev1.Pod{pod("old"), pod("old")}, 0, spawneryv1alpha1.ChangeoverWaiting},
		{"stale and current", []corev1.Pod{pod("old"), pod("new")}, 0, spawneryv1alpha1.ChangeoverBegun},
		{"stale only with a create the cache has not shown", []corev1.Pod{pod("old"), pod("old")}, 1,
			spawneryv1alpha1.ChangeoverBegun},
		{"terminating stale beside current", []corev1.Pod{pod("old", terminating), pod("new")}, 0,
			spawneryv1alpha1.ChangeoverBegun},
		{"terminating current is not begun", []corev1.Pod{pod("old"), pod("new", terminating)}, 0,
			spawneryv1alpha1.ChangeoverWaiting},
		{"failed stale is gone", []corev1.Pod{pod("old", failed), pod("new")}, 0, spawneryv1alpha1.ChangeoverNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownProxyChangeover(tc.pods, "new", tc.pending); got != tc.want {
				t.Errorf("ownProxyChangeover = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOwnServerChangeover(t *testing.T) {
	old := ServerView{Name: "old", PodHash: "old", Phase: phase.Ready}
	current := ServerView{Name: "new", PodHash: "current", Phase: phase.Ready}
	startingCurrent := ServerView{Name: "new", PodHash: "current", Phase: phase.Starting}
	for _, tc := range []struct {
		name      string
		views     []ServerView
		pending   int32
		whenEmpty bool
		want      spawneryv1alpha1.ChangeoverState
	}{
		{"nothing stale", []ServerView{current}, 0, true, spawneryv1alpha1.ChangeoverNone},
		{"stale only, nothing asked for", []ServerView{old}, 0, true, spawneryv1alpha1.ChangeoverWaiting},
		{"WhenEmpty with its first server starting", []ServerView{old, startingCurrent}, 0, true, spawneryv1alpha1.ChangeoverBegun},
		{"WhenEmpty with a Ready current server", []ServerView{old, current}, 0, true, spawneryv1alpha1.ChangeoverDeferred},
		{"RollingUpdate with a Ready current server", []ServerView{old, current}, 0, false, spawneryv1alpha1.ChangeoverBegun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownServerChangeover(tc.views, "current", tc.pending, tc.whenEmpty); got != tc.want {
				t.Errorf("ownServerChangeover = %q, want %q", got, tc.want)
			}
		})
	}
}
