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
	"reflect"
	"testing"

	"github.com/spawnery/spawnery/internal/phase"
)

func TestPersistentServerName(t *testing.T) {
	if got := PersistentServerName("survival", 0); got != "survival-0" {
		t.Errorf("PersistentServerName(survival, 0) = %q, want survival-0", got)
	}
	if got := PersistentServerName("survival", 12); got != "survival-12" {
		t.Errorf("PersistentServerName(survival, 12) = %q, want survival-12", got)
	}
}

// ordinalView builds the ServerView of a persistent server: named for its
// ordinal and carrying that ordinal in the Ordinal field too, since
// DecidePersistentSize reads the field and never the name. It is not called
// view, because candidates_test.go already owns that name for a different
// shape of builder in this same package.
func ordinalView(name string, ordinal int32, p phase.Phase) ServerView {
	return ServerView{Name: name, Ordinal: &ordinal, Phase: p}
}

func TestDecidePersistentSize(t *testing.T) {
	t.Run("nothing exists and three are wanted", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{Group: "survival", Replicas: 3})
		want := []int32{0, 1, 2}
		if !equalOrdinals(got.CreateOrdinals, want) {
			t.Fatalf("CreateOrdinals = %v, want %v", got.CreateOrdinals, want)
		}
		if len(got.Delete) != 0 {
			t.Fatalf("Delete = %v, want none", got.Delete)
		}
	})

	t.Run("a gap in the middle is filled at its own number", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 3,
			Views: []ServerView{ordinalView("survival-0", 0, phase.Ready), ordinalView("survival-2", 2, phase.Ready)},
		})
		if !equalOrdinals(got.CreateOrdinals, []int32{1}) {
			t.Fatalf("CreateOrdinals = %v, want [1]: the gap is filled, not appended to", got.CreateOrdinals)
		}
	})

	t.Run("the surplus is taken from the top, one ordinal at a time", func(t *testing.T) {
		// Two ordinals are surplus here, but TestDecidePersistentSizeTakesOneOrdinalDownAtATime
		// is what the invariant itself belongs to: this case only checks that
		// the single ordinal this pass does nominate is the highest one, not
		// the lowest.
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 1,
			Views: []ServerView{
				ordinalView("survival-0", 0, phase.Ready),
				ordinalView("survival-1", 1, phase.Ready),
				ordinalView("survival-2", 2, phase.Ready),
			},
		})
		want := []string{"survival-2"}
		if len(got.Delete) != 1 || got.Delete[0] != want[0] {
			t.Fatalf("Delete = %v, want %v: highest ordinal first, one at a time", got.Delete, want)
		}
	})

	t.Run("an ordinal held by a leaving server is neither missing nor removed again", func(t *testing.T) {
		// survival-1 is draining. It still holds ordinal 1, so nothing may be
		// built on its claim -- and it is already going, so it must not be
		// named for deletion a second time.
		//
		// Only the CreateOrdinals assertion below actually discriminates
		// here: held is built with no phase filter at all, so this is what
		// tests that an ordinal counts as taken whatever phase its server is
		// in. The Delete assertion cannot fail on its own in this case --
		// ordinal 1 sits below Replicas and never reaches the surplus set,
		// so it never reaches the leaving() guard either. That guard has its
		// own case below, where the held ordinal is surplus as well as
		// leaving.
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 2,
			Views: []ServerView{ordinalView("survival-0", 0, phase.Ready), ordinalView("survival-1", 1, phase.Draining)},
		})
		if len(got.CreateOrdinals) != 0 {
			t.Errorf("CreateOrdinals = %v, want none: ordinal 1 is still held", got.CreateOrdinals)
		}
		if len(got.Delete) != 0 {
			t.Errorf("Delete = %v, want none: survival-1 is already leaving", got.Delete)
		}
	})

	t.Run("a surplus ordinal held by a leaving server is not named for deletion again", func(t *testing.T) {
		// survival-1's ordinal is surplus (>= Replicas) as well as already
		// draining. PendingDeletes does not cover this on its own: observe()
		// clears that reservation as soon as the cache shows a leaving
		// phase -- typically the very next pass after the delete was issued
		// -- while the drain itself keeps the server around for up to
		// spec.drain.timeoutSeconds, which can far outlast the reservation's
		// own TTL. leaving() is what keeps this rule from naming survival-1
		// again for the rest of that drain, with no reservation left to lean
		// on.
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 1,
			Views: []ServerView{ordinalView("survival-0", 0, phase.Ready), ordinalView("survival-1", 1, phase.Draining)},
		})
		if len(got.Delete) != 0 {
			t.Fatalf("Delete = %v, want none: survival-1 is already leaving", got.Delete)
		}
	})

	t.Run("a create already reserved is not issued twice", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 2,
			Views:          []ServerView{ordinalView("survival-0", 0, phase.Ready)},
			PendingCreates: map[string]bool{"survival-1": true},
		})
		if len(got.CreateOrdinals) != 0 {
			t.Fatalf("CreateOrdinals = %v, want none: survival-1's create is in flight", got.CreateOrdinals)
		}
	})

	t.Run("a delete already reserved is not issued twice", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 1,
			Views: []ServerView{
				ordinalView("survival-0", 0, phase.Ready),
				ordinalView("survival-1", 1, phase.Ready),
			},
			PendingDeletes: map[string]bool{"survival-1": true},
		})
		if len(got.Delete) != 0 {
			t.Fatalf("Delete = %v, want none: survival-1's delete is in flight", got.Delete)
		}
	})

	t.Run("replicas zero empties the group", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 0,
			Views: []ServerView{ordinalView("survival-0", 0, phase.Ready)},
		})
		if len(got.Delete) != 1 || got.Delete[0] != "survival-0" {
			t.Fatalf("Delete = %v, want [survival-0]", got.Delete)
		}
	})

	t.Run("a server with no ordinal is ignored", func(t *testing.T) {
		// A leftover from an ephemeral past, or a hand-made object: it carries
		// no ordinal to fill or free. It neither fills one nor is removed as
		// surplus -- removing it would be this rule deleting something it
		// cannot name. The name is incidental here, not load-bearing: what
		// matters is that Ordinal is nil.
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 1,
			Views: []ServerView{{Name: "survival-a7kd", Phase: phase.Ready}},
		})
		if !equalOrdinals(got.CreateOrdinals, []int32{0}) {
			t.Errorf("CreateOrdinals = %v, want [0]", got.CreateOrdinals)
		}
		if len(got.Delete) != 0 {
			t.Errorf("Delete = %v, want none", got.Delete)
		}
	})

	t.Run("an ordinal at or above replicas is surplus even with a gap below it", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 2,
			Views: []ServerView{ordinalView("survival-0", 0, phase.Ready), ordinalView("survival-7", 7, phase.Ready)},
		})
		if !equalOrdinals(got.CreateOrdinals, []int32{1}) {
			t.Errorf("CreateOrdinals = %v, want [1]", got.CreateOrdinals)
		}
		if len(got.Delete) != 1 || got.Delete[0] != "survival-7" {
			t.Errorf("Delete = %v, want [survival-7]", got.Delete)
		}
	})
}

func TestDecidePersistentSizeTakesOneOrdinalDownAtATime(t *testing.T) {
	ready := func(ordinal int32, hash string) ServerView {
		v := ordinalView(PersistentServerName("g", ordinal), ordinal, phase.Ready)
		v.PodHash = hash
		return v
	}
	draining := func(ordinal int32, hash string) ServerView {
		v := ready(ordinal, hash)
		v.Phase = phase.Draining
		return v
	}

	cases := []struct {
		name       string
		replicas   int32
		podHash    string
		views      []ServerView
		wantCreate []int32
		wantDelete []string
	}{
		{
			name:       "missing ordinals are created all at once, not serialised",
			replicas:   4,
			podHash:    "h1",
			views:      []ServerView{ready(0, "h1")},
			wantCreate: []int32{1, 2, 3},
		},
		{
			name:     "surplus takes the highest, one only",
			replicas: 1,
			podHash:  "h1",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"), ready(3, "h1"),
			},
			wantDelete: []string{"g-3"},
		},
		{
			name:     "Gate A holds the next surplus while one is draining",
			replicas: 1,
			podHash:  "h1",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"), draining(3, "h1"),
			},
			wantDelete: nil,
		},
		{
			// Spec 2.1: a surplus ordinal sits above replicas, so Gate B cannot
			// see it. Gate A is what holds the invariant here, and this case is
			// what proves it does.
			name:     "Gate B does not apply to surplus: a sick ordinal 0 does not block a scale-down",
			replicas: 1,
			podHash:  "h1",
			views: []ServerView{
				ordinalViewWithHash("g-0", 0, phase.Failed, "h1"),
				ready(1, "h1"),
			},
			wantDelete: []string{"g-1"},
		},
		{
			name:     "stale takes the highest once no surplus remains",
			replicas: 3,
			podHash:  "h2",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"),
			},
			wantDelete: []string{"g-2"},
		},
		{
			name:     "surplus outranks stale",
			replicas: 2,
			podHash:  "h2",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"),
			},
			wantDelete: []string{"g-2"},
		},
		{
			// Gate B: the replacement for g-2 is back but not Ready yet, so g-1
			// waits. Deleting the previous object is not the same as the world
			// being back.
			name:     "Gate B holds the next stale while the replacement is still starting",
			replicas: 3,
			podHash:  "h2",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"),
				ordinalViewWithHash("g-2", 2, phase.Starting, "h2"),
			},
			wantDelete: nil,
		},
		{
			name:       "an empty hash is adopted, never nominated",
			replicas:   2,
			podHash:    "h2",
			views:      []ServerView{ready(0, ""), ready(1, "")},
			wantDelete: nil,
		},
		{
			// Task 5, not this one, fills PersistentInputs.PodHash from the
			// group; until then a real caller passes the zero value here while
			// views already carry real hashes. Without this guard every ordinal
			// of every persistent group would compare unequal to "", read as
			// stale, and be nominated for takedown. It is also correct on its
			// own terms: a rule that cannot know what current looks like must
			// not declare anything stale, the same way an empty view hash is
			// adopted rather than compared.
			name:       "an empty group PodHash skips the stale class entirely",
			replicas:   2,
			podHash:    "",
			views:      []ServerView{ready(0, "h1"), ready(1, "h1")},
			wantDelete: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecidePersistentSize(PersistentInputs{
				Group:    "g",
				Replicas: tc.replicas,
				PodHash:  tc.podHash,
				Views:    tc.views,
			})
			if !reflect.DeepEqual(got.CreateOrdinals, tc.wantCreate) {
				t.Errorf("CreateOrdinals = %v, want %v", got.CreateOrdinals, tc.wantCreate)
			}
			if !reflect.DeepEqual(got.Delete, tc.wantDelete) {
				t.Errorf("Delete = %v, want %v", got.Delete, tc.wantDelete)
			}
		})
	}
}

// ordinalViewWithHash is ordinalView plus a PodHash, for the table cases in
// TestDecidePersistentSizeTakesOneOrdinalDownAtATime that need a phase other
// than Ready or Draining together with an explicit hash.
func ordinalViewWithHash(name string, ordinal int32, p phase.Phase, hash string) ServerView {
	v := ordinalView(name, ordinal, p)
	v.PodHash = hash
	return v
}

func equalOrdinals(got, want []int32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestOrdinalOf(t *testing.T) {
	tests := []struct {
		name   string
		group  string
		server string
		want   int32
		wantOK bool
	}{
		{"the ordinary case", "survival", "survival-0", 0, true},
		{"more than one digit", "survival", "survival-12", 12, true},
		{"a different group's server", "survival", "creative-0", 0, false},
		{"an ephemeral name from the same group", "survival", "survival-a7kd", 0, false},
		{"the group name alone", "survival", "survival", 0, false},
		{"a negative number is not an ordinal", "survival", "survival--1", 0, false},
		{"a leading zero is not the same ordinal", "survival", "survival-01", 0, false},
		{"empty", "survival", "", 0, false},
		// A group whose own name ends in a number is the case that breaks a
		// naive suffix split: the boundary is the last hyphen, and everything
		// before it must equal the group exactly.
		{"a group name ending in a digit", "survival-2", "survival-2-3", 3, true},
		{"that group's own name is not one of its servers", "survival-2", "survival-2", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := OrdinalOf(tc.group, tc.server)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("OrdinalOf(%q, %q) = (%d, %v), want (%d, %v)",
					tc.group, tc.server, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
