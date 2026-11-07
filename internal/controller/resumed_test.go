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

	"github.com/spawnery/spawnery/internal/phase"
)

func resumed(v ServerView) ServerView {
	v.Resumed = true
	return v
}

func keptRoundsInputs(views ...ServerView) ScalingInputs {
	return ScalingInputs{
		Views:       views,
		PodHash:     "current",
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	}
}

func TestAResumedMemberChangesNothingInTheSizing(t *testing.T) {
	base := []ServerView{
		staleReady("a", 90, 100, "current"),
		staleReady("b", 70, 100, "old"),
	}
	cases := map[string]ServerView{
		"an empty resumed member":          resumed(staleReady("rounds-r1", 0, 100, "current")),
		"a stale resumed member":           resumed(staleReady("rounds-r2", 0, 100, "old")),
		"a finished resumed member":        resumed(ServerView{Name: "rounds-r3", Phase: phase.Finished}),
		"a starting resumed member":        resumed(starting("rounds-r4")),
		"an occupied stale resumed member": resumed(staleReady("rounds-r5", 50, 100, "old")),
	}
	want := DecideSize(keptRoundsInputs(base...))
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			views := append(append([]ServerView{}, base...), r)
			got := DecideSize(keptRoundsInputs(withoutResumed(views)...))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("decision = %+v, want %+v", got, want)
			}
			for _, n := range append(append(got.Delete, got.Retire...), got.Condemn...) {
				if n == r.Name {
					t.Errorf("the decision names the resumed member %s", n)
				}
			}
		})
	}
}

func TestWithoutResumedLeavesTheInputAlone(t *testing.T) {
	views := []ServerView{ready("a", 0, 100), resumed(ready("r", 0, 100))}
	got := withoutResumed(views)
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("withoutResumed = %+v, want only a", got)
	}
	if len(views) != 2 || views[1].Name != "r" {
		t.Errorf("the caller's views changed: %+v", views)
	}
	plain := []ServerView{ready("a", 0, 100)}
	if got := withoutResumed(plain); &got[0] != &plain[0] {
		t.Error("withoutResumed copied views that hold no resumed member")
	}
}

// A member that keyed its world but was started by the group is no different
// to the sizing from one that never keyed: the view carries no key at all.
func TestAKeyedButNotResumedMemberCountsLikeAnUnkeyedOne(t *testing.T) {
	views := []ServerView{ready("a", 90, 100), ready("b", 20, 100)}
	if got, want := withoutResumed(views), views; !reflect.DeepEqual(got, want) {
		t.Fatalf("withoutResumed dropped a group member: %+v", got)
	}
}

func TestAResumedMemberIsNoFreeCapacity(t *testing.T) {
	r := resumed(ready("rounds-r1", 2, 100))
	totals := AggregateGroup([]ServerView{ready("a", 10, 100), r}, "")
	if totals.Replicas != 2 || totals.ReadyReplicas != 2 || totals.OnlinePlayers != 12 {
		t.Errorf("totals = %+v, want the resumed member counted as serving", totals)
	}
	if totals.FreeSlots != 90 {
		t.Errorf("FreeSlots = %d, want 90: a resumed member's seats are not the group's", totals.FreeSlots)
	}
}
