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
	"time"
)

func at(min int) time.Time {
	return time.Date(2026, 8, 14, 12, min, 0, 0, time.UTC)
}

func TestDecideRollout(t *testing.T) {
	tests := []struct {
		name     string
		pods     []ProxyView
		replicas int32
		want     RolloutDecision
	}{
		{
			name:     "cold start creates the whole group",
			pods:     nil,
			replicas: 2,
			want:     RolloutDecision{Create: 2},
		},
		{
			name: "a group at size with nothing stale does nothing",
			pods: []ProxyView{
				{Name: "a", Ready: true, CreatedAt: at(0)},
				{Name: "b", Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
		{
			name: "all stale: the surge pod is created before anything is marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{Create: 1},
		},
		{
			name: "the surge pod is not ready yet, so nothing is marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
				{Name: "c", Ready: false, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
		{
			name: "the surge pod is ready, so exactly one stale pod is marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, Players: 3, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, Players: 1, CreatedAt: at(1)},
				{Name: "c", Ready: true, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"b"}},
		},
		{
			name: "one already draining: no second replacement begins",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Draining: true, Players: 1, CreatedAt: at(1)},
				{Name: "c", Ready: true, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
		{
			name: "the surge pod dying mid-drain is replaced, because surge outlives the mark",
			pods: []ProxyView{
				{Name: "draining", Stale: true, Draining: true, Players: 1, CreatedAt: at(0)},
				{Name: "waiting", Stale: true, Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{Create: 1},
		},
		{
			name: "scale-down takes the emptiest",
			pods: []ProxyView{
				{Name: "a", Ready: true, Players: 4, CreatedAt: at(0)},
				{Name: "b", Ready: true, Players: 0, CreatedAt: at(1)},
				{Name: "c", Ready: true, Players: 2, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"b"}},
		},
		{
			name: "an untrusted count sorts last, even at zero",
			pods: []ProxyView{
				{Name: "a", Ready: true, Players: 0, PlayersStale: true, CreatedAt: at(0)},
				{Name: "b", Ready: true, Players: 2, CreatedAt: at(1)},
				{Name: "c", Ready: true, Players: 5, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"b"}},
		},
		{
			name: "equal counts break by age, oldest first",
			pods: []ProxyView{
				{Name: "young", Ready: true, Players: 1, CreatedAt: at(9)},
				{Name: "old", Ready: true, Players: 1, CreatedAt: at(1)},
				{Name: "mid", Ready: true, Players: 1, CreatedAt: at(5)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"old"}},
		},
		{
			name: "a scale-down during a rollout takes the stale pod first",
			pods: []ProxyView{
				{Name: "stale-full", Stale: true, Ready: true, Players: 9, CreatedAt: at(0)},
				{Name: "current-empty", Ready: true, Players: 0, CreatedAt: at(1)},
			},
			replicas: 1,
			want:     RolloutDecision{Drain: []string{"stale-full"}},
		},
		{
			name: "a surplus of mixed generations takes the stale pod before an emptier current one",
			pods: []ProxyView{
				{Name: "stale-full", Stale: true, Ready: true, Players: 9, CreatedAt: at(0)},
				{Name: "current-empty", Ready: true, Players: 0, CreatedAt: at(1)},
				{Name: "current-quiet", Ready: true, Players: 1, CreatedAt: at(2)},
			},
			replicas: 1,
			want:     RolloutDecision{Drain: []string{"stale-full"}},
		},
		{
			name: "a group whose replicas dropped mid-rollout still rolls forward",
			pods: []ProxyView{
				{Name: "old-a", Stale: true, Ready: true, Players: 2, CreatedAt: at(0)},
				{Name: "old-b", Stale: true, Ready: true, Players: 1, CreatedAt: at(1)},
			},
			replicas: 1,
			want:     RolloutDecision{Drain: []string{"old-b"}},
		},
		{
			name: "a stale pod does not block the group from reaching zero",
			pods: []ProxyView{
				{Name: "last", Stale: true, Ready: true, CreatedAt: at(0)},
			},
			replicas: 0,
			want:     RolloutDecision{Drain: []string{"last"}},
		},
		{
			name: "a cancelled rollout creates nothing and marks nothing",
			pods: []ProxyView{
				{Name: "a", Ready: true, CreatedAt: at(0)},
				{Name: "b", Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRollout(tc.pods, tc.replicas)
			if got.Create != tc.want.Create {
				t.Errorf("Create = %d, want %d", got.Create, tc.want.Create)
			}
			if len(got.Drain) != len(tc.want.Drain) {
				t.Fatalf("Drain = %v, want %v", got.Drain, tc.want.Drain)
			}
			for i := range got.Drain {
				if got.Drain[i] != tc.want.Drain[i] {
					t.Errorf("Drain[%d] = %q, want %q", i, got.Drain[i], tc.want.Drain[i])
				}
			}
		})
	}
}
