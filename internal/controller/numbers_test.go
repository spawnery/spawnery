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

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func TestNextNumber(t *testing.T) {
	cases := []struct {
		name  string
		taken map[int32]bool
		want  int32
	}{
		{"the first server of a group is 1", nil, 1},
		{"counting up", map[int32]bool{1: true}, 2},
		{"a gap in the middle is filled first", map[int32]bool{1: true, 3: true}, 2},
		{"a gap at the bottom is filled first", map[int32]bool{2: true, 3: true}, 1},
		// Zero is not a number this rule hands out, so a set carrying it says
		// nothing about where to start.
		{"zero holds nothing back", map[int32]bool{0: true}, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextNumber(tc.taken); got != tc.want {
				t.Errorf("NextNumber(%v) = %d, want %d", tc.taken, got, tc.want)
			}
		})
	}
}

// takenNumbers is what the create loop feeds NextNumber: the numbers its
// group's servers hold, plus the ones its own unobserved creates reserved.
func TestTakenNumbers(t *testing.T) {
	views := []ServerView{
		{Name: "hub-dvjk", Number: 1},
		{Name: "hub-pgqg", Number: 3},
		// A server from before the field existed holds nothing back.
		{Name: "hub-old1", Number: 0},
	}

	got := takenNumbers(views, map[int32]bool{4: true})

	for _, n := range []int32{1, 3, 4} {
		if !got[n] {
			t.Errorf("takenNumbers = %v, want it to hold %d", got, n)
		}
	}
	if got[0] || got[2] {
		t.Errorf("takenNumbers = %v, want neither 0 nor 2", got)
	}
}

func TestAGroupNumbersTheServersItCreates(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.group.Spec.Scaling.MinReplicas = 3
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}

	f.reconcileGroup(t, r)

	seen := map[int32]bool{}
	for _, s := range f.listServers(t) {
		if seen[s.Spec.Number] {
			t.Fatalf("two servers carry number %d", s.Spec.Number)
		}
		seen[s.Spec.Number] = true
	}
	for _, want := range []int32{1, 2, 3} {
		if !seen[want] {
			t.Errorf("numbers = %v, want 1, 2 and 3", seen)
		}
	}
}

func TestAGroupFillsTheLowestGapInItsNumbers(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// A group whose middle server went away. The next one is 2 and not 4, or a
	// group that scales up and down all day counts into three digits.
	for name, number := range map[string]int32{"lobby-aaaa": 1, "lobby-bbbb": 3} {
		srv := f.createServer(name)
		srv.Spec.Number = number
		if err := f.c.Update(f.ctx, srv); err != nil {
			t.Fatalf("number %s: %v", name, err)
		}
	}
	f.group.Spec.Scaling.MinReplicas = 3
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}

	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	var created *spawneryv1alpha1.Server
	for i := range servers {
		if servers[i].Name != "lobby-aaaa" && servers[i].Name != "lobby-bbbb" {
			created = &servers[i]
		}
	}
	if created == nil {
		t.Fatal("the group created no third server")
	}
	if created.Spec.Number != 2 {
		t.Errorf("number = %d, want the gap at 2", created.Spec.Number)
	}
}
