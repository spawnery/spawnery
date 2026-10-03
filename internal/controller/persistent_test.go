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

func TestPersistentServerName(t *testing.T) {
	if got := PersistentServerName("survival", 0); got != "survival-0" {
		t.Errorf("PersistentServerName(survival, 0) = %q, want survival-0", got)
	}
	if got := PersistentServerName("survival", 12); got != "survival-12" {
		t.Errorf("PersistentServerName(survival, 12) = %q, want survival-12", got)
	}
}

// ordinalView sets Ordinal too: DecidePersistentSize reads the field, never the name.
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
		// Only checks that the nominated ordinal is the highest.
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
		// survival-1 still holds ordinal 1 whatever its phase, so nothing is built on its claim.
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
		// observe() clears the PendingDeletes reservation once a leaving phase shows, but the
		// drain can outlast it; leaving() keeps survival-1 from being named again.
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
		// No Ordinal: it neither fills one nor is removed as surplus.
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
		// wantReason is SizeDecision.DeleteReason, tabled so no case nominates under one
		// class and reports another.
		wantReason string
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
			wantReason: "SurplusOrdinal",
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
			// Ordinal 2 is nominable on its own; only Gate A, which looks at the whole group,
			// holds it back.
			name:     "Gate A holds a surplus that is not itself the draining ordinal",
			replicas: 1,
			podHash:  "h1",
			views: []ServerView{
				ready(0, "h1"), draining(1, "h1"), ready(2, "h1"),
			},
			wantDelete: nil,
		},
		{
			// A server with no ordinal is not a takedown in flight and must not block g-1.
			name:     "a nil-ordinal squatter does not block Gate A",
			replicas: 1,
			podHash:  "h1",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"),
				{Name: "g-a7kd", Phase: phase.Draining},
			},
			wantDelete: []string{"g-1"},
			wantReason: "SurplusOrdinal",
		},
		{
			// A surplus ordinal sits above replicas, so Gate B cannot see it; Gate A must hold.
			name:     "Gate B does not apply to surplus: a sick ordinal 0 does not block a scale-down",
			replicas: 1,
			podHash:  "h1",
			views: []ServerView{
				ordinalViewWithHash("g-0", 0, phase.Failed, "h1"),
				ready(1, "h1"),
			},
			wantDelete: []string{"g-1"},
			wantReason: "SurplusOrdinal",
		},
		{
			name:     "stale takes the highest once no surplus remains",
			replicas: 3,
			podHash:  "h2",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"),
			},
			wantDelete: []string{"g-2"},
			wantReason: "StaleSpec",
		},
		{
			name:     "surplus outranks stale",
			replicas: 2,
			podHash:  "h2",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"),
			},
			wantDelete: []string{"g-2"},
			wantReason: "SurplusOrdinal",
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
			// A rule that cannot know what current looks like must not declare anything stale.
			name:       "an empty group PodHash skips the stale class entirely",
			replicas:   2,
			podHash:    "",
			views:      []ServerView{ready(0, "h1"), ready(1, "h1")},
			wantDelete: nil,
		},
		{
			// Both ordinals are current and healthy, so only resize-pending can nominate.
			name:     "resize-pending is nominated, but only after stale",
			replicas: 2,
			podHash:  "h1",
			views: func() []ServerView {
				g0, g1 := ready(0, "h1"), ready(1, "h1")
				g0.ResizePending, g1.ResizePending = true, true
				return []ServerView{g0, g1}
			}(),
			wantDelete: []string{"g-1"},
			wantReason: "ResizePending",
		},
		{
			name:     "a stale ordinal outranks a resize-pending one",
			replicas: 2,
			podHash:  "h2",
			views: func() []ServerView {
				g0 := ready(0, "h1") // stale: h1 != group's h2
				g1 := ready(1, "h2")
				g1.ResizePending = true
				return []ServerView{g0, g1}
			}(),
			wantDelete: []string{"g-0"},
			wantReason: "StaleSpec",
		},
		{
			// Resize-pending is held back by Gate A exactly like stale.
			name:     "Gate A holds a resize-pending ordinal while another is draining",
			replicas: 2,
			podHash:  "h1",
			views: func() []ServerView {
				g0 := ready(0, "h1")
				g0.ResizePending = true
				g1 := draining(1, "h1")
				return []ServerView{g0, g1}
			}(),
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
			if got.DeleteReason != tc.wantReason {
				t.Errorf("DeleteReason = %q, want %q", got.DeleteReason, tc.wantReason)
			}
		})
	}
}

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

func TestDecidePersistentSizeRefusedNominatesNoStaleOrdinal(t *testing.T) {
	stale := func(name string, o int32) ServerView {
		v := ordinalView(name, o, phase.Ready)
		v.PodHash = "old"
		return v
	}

	t.Run("refused on a fully recovered group nominates nothing", func(t *testing.T) {
		in := PersistentInputs{
			Group: "survival", Replicas: 2, PodHash: "new", ChangeoverRefused: true,
			Views: []ServerView{stale("survival-0", 0), stale("survival-1", 1)},
		}
		got := DecidePersistentSize(in)
		if len(got.Delete) != 0 {
			t.Fatalf("Delete = %v, want none: the changeover is refused", got.Delete)
		}
	})

	t.Run("the same group unrefused nominates the stale ordinal", func(t *testing.T) {
		in := PersistentInputs{
			Group: "survival", Replicas: 2, PodHash: "new", ChangeoverRefused: false,
			Views: []ServerView{stale("survival-0", 0), stale("survival-1", 1)},
		}
		got := DecidePersistentSize(in)
		if got.DeleteReason != "StaleSpec" {
			t.Fatalf("DeleteReason = %q, want StaleSpec", got.DeleteReason)
		}
	})

	t.Run("a missing ordinal is still replaced under refusal", func(t *testing.T) {
		in := PersistentInputs{
			Group: "survival", Replicas: 3, PodHash: "new", ChangeoverRefused: true,
			Views: []ServerView{stale("survival-0", 0), stale("survival-2", 2)},
		}
		got := DecidePersistentSize(in)
		if !equalOrdinals(got.CreateOrdinals, []int32{1}) {
			t.Fatalf("CreateOrdinals = %v, want [1]: a missing ordinal is still replaced", got.CreateOrdinals)
		}
	})
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
		// The boundary is the last hyphen, so a group name ending in a number must still parse.
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

// A second server on one ordinal must be reported, not silently dropped from a map
// keyed by ordinal.
func TestADuplicatedOrdinalIsReportedAndNeverActedOn(t *testing.T) {
	// Both orderings: list order decides which duplicate a map keyed by ordinal keeps.
	for _, order := range [][]ServerView{
		{ordinalView("survival-2", 2, phase.Ready), ordinalView("restored-copy", 2, phase.Ready)},
		{ordinalView("restored-copy", 2, phase.Ready), ordinalView("survival-2", 2, phase.Ready)},
	} {
		t.Run("surplus is refused while an ordinal is doubled", func(t *testing.T) {
			views := append([]ServerView{
				ordinalView("survival-0", 0, phase.Ready),
				ordinalView("survival-1", 1, phase.Ready),
			}, order...)
			// Replicas 2 makes ordinal 2 surplus, which would otherwise delete one of the two.
			got := DecidePersistentSize(PersistentInputs{Group: "survival", Replicas: 2, Views: views})

			if len(got.Delete) != 0 {
				t.Errorf("Delete = %v, want none: the rule chose between two worlds", got.Delete)
			}
			want := []OrdinalConflict{{Ordinal: 2, Servers: []string{"restored-copy", "survival-2"}}}
			if !reflect.DeepEqual(got.Conflicts, want) {
				t.Errorf("Conflicts = %+v, want %+v", got.Conflicts, want)
			}
		})
	}
}

// Refusing everything would turn one hand-made object into a group-wide stall.
func TestADuplicatedOrdinalDoesNotStopTheRestOfTheGroup(t *testing.T) {
	// Ordinal 1 is doubled; ordinal 3 is missing and ordinal 4 is surplus.
	got := DecidePersistentSize(PersistentInputs{
		Group: "survival", Replicas: 4,
		Views: []ServerView{
			ordinalView("survival-0", 0, phase.Ready),
			ordinalView("survival-1", 1, phase.Ready),
			ordinalView("copy-of-1", 1, phase.Ready),
			ordinalView("survival-2", 2, phase.Ready),
			ordinalView("survival-4", 4, phase.Ready),
		},
	})

	// A doubled ordinal still counts as taken.
	if !equalOrdinals(got.CreateOrdinals, []int32{3}) {
		t.Errorf("CreateOrdinals = %v, want [3]", got.CreateOrdinals)
	}
	if len(got.Delete) != 1 || got.Delete[0] != "survival-4" {
		t.Errorf("Delete = %v, want [survival-4]", got.Delete)
	}
	if len(got.Conflicts) != 1 || got.Conflicts[0].Ordinal != 1 {
		t.Errorf("Conflicts = %+v, want ordinal 1", got.Conflicts)
	}
}

// Replacing the wrong half of a duplicate pair takes down a world that was never stale.
func TestAStaleDuplicatedOrdinalIsNotReplaced(t *testing.T) {
	got := DecidePersistentSize(PersistentInputs{
		Group: "survival", Replicas: 2, PodHash: "new",
		Views: []ServerView{
			ordinalViewWithHash("survival-0", 0, phase.Ready, "new"),
			ordinalViewWithHash("survival-1", 1, phase.Ready, "old"),
			ordinalViewWithHash("copy-of-1", 1, phase.Ready, "old"),
		},
	})

	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: a stale ordinal two servers carry is not replaceable", got.Delete)
	}
	if len(got.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want one", got.Conflicts)
	}
}

func TestThreeServersOnOneOrdinalAreAllNamed(t *testing.T) {
	got := DecidePersistentSize(PersistentInputs{
		Group: "survival", Replicas: 1,
		Views: []ServerView{
			ordinalView("survival-0", 0, phase.Ready),
			ordinalView("b-copy", 0, phase.Ready),
			ordinalView("a-copy", 0, phase.Ready),
		},
	})
	want := []OrdinalConflict{{Ordinal: 0, Servers: []string{"a-copy", "b-copy", "survival-0"}}}
	if !reflect.DeepEqual(got.Conflicts, want) {
		t.Errorf("Conflicts = %+v, want %+v -- sorted, so the message does not churn", got.Conflicts, want)
	}
}

// A caller can use the field's emptiness as the question.
func TestNoConflictsWhenEveryOrdinalIsSingle(t *testing.T) {
	got := DecidePersistentSize(PersistentInputs{
		Group: "survival", Replicas: 2,
		Views: []ServerView{
			ordinalView("survival-0", 0, phase.Ready),
			ordinalView("survival-1", 1, phase.Ready),
			{Name: "stray", Phase: phase.Ready},
		},
	})
	if got.Conflicts != nil {
		t.Errorf("Conflicts = %+v, want nil", got.Conflicts)
	}
}
