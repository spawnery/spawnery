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

	t.Run("the surplus is taken from the top", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 1,
			Views: []ServerView{
				ordinalView("survival-0", 0, phase.Ready),
				ordinalView("survival-1", 1, phase.Ready),
				ordinalView("survival-2", 2, phase.Ready),
			},
		})
		want := []string{"survival-2", "survival-1"}
		if len(got.Delete) != 2 || got.Delete[0] != want[0] || got.Delete[1] != want[1] {
			t.Fatalf("Delete = %v, want %v: highest ordinal first", got.Delete, want)
		}
	})

	t.Run("an ordinal held by a leaving server is neither missing nor removed again", func(t *testing.T) {
		// survival-1 is draining. It still holds ordinal 1, so nothing may be
		// built on its claim -- and it is already going, so it must not be
		// named for deletion a second time.
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
