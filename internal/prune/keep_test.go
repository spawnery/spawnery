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

package prune

import "testing"

func TestKeepHoldsWhatAnEntryMatchesAndBelow(t *testing.T) {
	k, err := ParseKeep([]string{"worlds/world", "plugins/*/data"})
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		"worlds/world":               true,
		"worlds/world/level.dat":     true,
		"worlds/world2":              false,
		"worlds":                     false,
		"plugins/Example/data/a.yml": true,
		"plugins/Example/config.yml": false,
		"logs/latest.log":            false,
	} {
		if got := k.Holds(rel); got != want {
			t.Errorf("Holds(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestKeepTowardIsTheWayDownToAnEntry(t *testing.T) {
	k, err := ParseKeep([]string{"worlds/world", "plugins/*/data"})
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		"worlds":          true,
		"plugins":         true,
		"plugins/Example": true,
		"logs":            false,
		"worlds/world":    false,
	} {
		if got := k.Toward(rel); got != want {
			t.Errorf("Toward(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestParseKeepRefusesWhatPruneRefuses(t *testing.T) {
	if _, err := ParseKeep([]string{"../escape"}); err == nil {
		t.Fatal("a .. segment was accepted")
	}
}
