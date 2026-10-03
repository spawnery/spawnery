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
	"testing"
	"time"
)

func at(min int) time.Time {
	return time.Date(2026, 8, 14, 12, min, 0, 0, time.UTC)
}

// A draining pod that is no longer stale, as after a node is released
// mid-drain, still holds the draining guard.
func TestDecideRolloutCountsDrainingIndependentlyOfStale(t *testing.T) {
	pods := []ProxyView{
		{Name: "old", Stale: false, Ready: false, Draining: true, Players: 2},
		{Name: "new", Stale: false, Ready: true},
		{Name: "other", Stale: false, Ready: true},
	}
	got := DecideRollout(pods, 2, true)
	if got.Create != 0 {
		t.Fatalf("Create = %d, want 0", got.Create)
	}
	if len(got.Drain) != 0 {
		t.Fatalf("Drain = %v, want none: the draining guard holds even though nothing here is Stale", got.Drain)
	}
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
			name: "all stale: a replacement for each is created before anything is marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{Create: 2},
		},
		{
			name: "a replacement not ready yet: the missing one is created and nothing is marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
				{Name: "c", Ready: false, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Create: 1},
		},
		{
			name: "one Ready replacement for two stale pods: the second is created, nothing is marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, Players: 3, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, Players: 1, CreatedAt: at(1)},
				{Name: "c", Ready: true, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Create: 1},
		},
		{
			name: "one already draining: the other stale pod's replacement is still created",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Draining: true, Players: 1, CreatedAt: at(1)},
				{Name: "c", Ready: true, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Create: 1},
		},
		{
			name: "replacements lost mid-drain are rebuilt, because surge outlives the mark",
			pods: []ProxyView{
				{Name: "draining", Stale: true, Draining: true, Players: 1, CreatedAt: at(0)},
				{Name: "waiting", Stale: true, Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{Create: 2},
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
			name: "equal counts break by age, newest first",
			pods: []ProxyView{
				{Name: "young", Ready: true, Players: 1, CreatedAt: at(9)},
				{Name: "old", Ready: true, Players: 1, CreatedAt: at(1)},
				{Name: "mid", Ready: true, Players: 1, CreatedAt: at(5)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"young"}},
		},
		{
			// All counts untrusted, so age alone decides: an older proxy has
			// had longer to collect players.
			name: "untrusted counts all round still take the newest",
			pods: []ProxyView{
				{Name: "young", Ready: true, PlayersStale: true, CreatedAt: at(9)},
				{Name: "old", Ready: true, PlayersStale: true, CreatedAt: at(1)},
				{Name: "mid", Ready: true, PlayersStale: true, CreatedAt: at(5)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"young"}},
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
			want:     RolloutDecision{Create: 1},
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
			name: "a stale pod that is not ready does not block the group from reaching zero either",
			pods: []ProxyView{
				{Name: "last", Stale: true, Ready: false, CreatedAt: at(0)},
			},
			replicas: 0,
			want:     RolloutDecision{Drain: []string{"last"}},
		},
		{
			name: "a stale pod that is not ready is marked, because retiring it costs no ready capacity",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: false, CreatedAt: at(1)},
				{Name: "s", Ready: true, CreatedAt: at(2)},
				{Name: "t", Ready: false, CreatedAt: at(3)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"b"}},
		},
		{
			// An unready pod is behind no Service endpoint; its count is what
			// it held before it fell over.
			name: "an unready stale pod goes ahead of an emptier ready one",
			pods: []ProxyView{
				{Name: "quiet", Stale: true, Ready: true, Players: 0, CreatedAt: at(0)},
				{Name: "fallen", Stale: true, Ready: false, Players: 9, PlayersStale: true, CreatedAt: at(1)},
				{Name: "s", Ready: true, CreatedAt: at(2)},
				{Name: "t", Ready: false, CreatedAt: at(3)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"fallen"}},
		},
		{
			// Cancelled before any mark: surge drops to 0 while the surge pod
			// still stands, so the surplus goes by the ordinary rule.
			name: "a cancelled rollout retires a surplus pod by the ordinary rule, not the surge pod",
			pods: []ProxyView{
				{Name: "a", Ready: true, Players: 5, CreatedAt: at(0)},
				{Name: "b", Ready: true, Players: 0, CreatedAt: at(1)},
				{Name: "surge", Ready: true, Players: 2, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"b"}},
		},
		{
			name: "blue/green: every stale pod gets its replacement up front",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
				{Name: "c", Stale: true, Ready: true, CreatedAt: at(2)},
			},
			replicas: 3,
			want:     RolloutDecision{Create: 3},
		},
		{
			name: "blue/green: replacements not ready: nothing marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
				{Name: "n1", Ready: true, CreatedAt: at(3)},
				{Name: "n2", CreatedAt: at(4)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
		{
			name: "blue/green: replicas current pods Ready marks every stale pod at once",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, Players: 1, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, Players: 20, CreatedAt: at(1)},
				{Name: "n1", Ready: true, CreatedAt: at(3)},
				{Name: "n2", Ready: true, CreatedAt: at(4)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"a", "b"}},
		},
		{
			name: "blue/green: a stale pod not yet marked is marked beside one already draining",
			pods: []ProxyView{
				{Name: "a", Stale: true, Draining: true, Players: 1, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
				{Name: "n1", Ready: true, CreatedAt: at(3)},
				{Name: "n2", Ready: true, CreatedAt: at(4)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"b"}},
		},
		{
			name: "blue/green: a replacement dying mid-drain is rebuilt",
			pods: []ProxyView{
				{Name: "a", Stale: true, Draining: true, Players: 1, CreatedAt: at(0)},
				{Name: "b", Stale: true, Draining: true, Players: 3, CreatedAt: at(1)},
				{Name: "n1", Ready: true, CreatedAt: at(3)},
			},
			replicas: 2,
			want:     RolloutDecision{Create: 1},
		},
		{
			name: "blue/green: a stale pod serving nobody is marked before the replacements are Ready",
			pods: []ProxyView{
				{Name: "a", Stale: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
				{Name: "n1", CreatedAt: at(3)},
				{Name: "n2", CreatedAt: at(4)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"a"}},
		},
		{
			name: "a reverted spec mid-roll marks nothing while the old pods are the current ones",
			pods: []ProxyView{
				{Name: "a", Ready: true, Players: 5, CreatedAt: at(0)},
				{Name: "b", Ready: true, Players: 5, CreatedAt: at(1)},
				{Name: "n1", Stale: true, CreatedAt: at(3)},
				{Name: "n2", Stale: true, CreatedAt: at(4)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"n1", "n2"}},
		},
		{
			name: "a surplus mid-roll takes current pods while no replacement is Ready",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, Players: 1, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, Players: 2, CreatedAt: at(1)},
				{Name: "n1", CreatedAt: at(3)},
				{Name: "n2", CreatedAt: at(4)},
				{Name: "n3", CreatedAt: at(5)},
			},
			replicas: 1,
			want:     RolloutDecision{Drain: []string{"n3", "n2"}},
		},
		{
			name: "a lowered replicas with nothing stale drains the surplus as today",
			pods: []ProxyView{
				{Name: "a", Ready: true, Players: 1, CreatedAt: at(0)},
				{Name: "b", Ready: true, Players: 2, CreatedAt: at(1)},
				{Name: "c", Ready: true, Players: 3, CreatedAt: at(2)},
			},
			replicas: 1,
			want:     RolloutDecision{Drain: []string{"a", "b"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRollout(tc.pods, tc.replicas, true)
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

// Without surge the group never grows past replicas; it may only touch a pod
// that already serves nobody.
func TestDecideRolloutWithoutSurge(t *testing.T) {
	tests := []struct {
		name     string
		pods     []ProxyView
		replicas int32
		want     RolloutDecision
	}{
		{
			name: "two stale Ready pods at replicas 2: no create, no drain",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
		{
			name: "one stale not-Ready pod among two is replaced in place, no extra pod",
			pods: []ProxyView{
				{Name: "stale", Stale: true, Ready: false, CreatedAt: at(0)},
				{Name: "current", Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"stale"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRollout(tc.pods, tc.replicas, false)
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

func TestARetireRequestIsDrainedFirstAmongStalePods(t *testing.T) {
	got := DecideRollout([]ProxyView{
		{Name: "a", Ready: true, Stale: true, Players: 0, CreatedAt: at(0)},
		{Name: "b", Ready: true, Stale: true, RetireRequested: true, Players: 5, CreatedAt: at(1)},
		{Name: "c", Ready: true, Players: 0, CreatedAt: at(2)},
		{Name: "d", Ready: true, Players: 0, CreatedAt: at(3)},
	}, 3, false)
	if len(got.Drain) != 1 || got.Drain[0] != "b" {
		t.Errorf("Drain = %v, want [b]: an admin asked for that one", got.Drain)
	}
}
