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

import (
	"strings"
	"testing"
)

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

func TestParseReplaceNamesItsField(t *testing.T) {
	r, err := ParseReplace([]string{"world/datapacks"})
	if err != nil || !r.Holds("world/datapacks/x.zip") || !r.Toward("world") {
		t.Fatalf("ParseReplace = %+v, %v", r, err)
	}
	if _, err := ParseReplace([]string{".."}); err == nil || !strings.Contains(err.Error(), "spec.storage.replace") {
		t.Fatalf("err = %v, want one naming spec.storage.replace", err)
	}
}

func TestWorldEntryIsThePrunesWorldRule(t *testing.T) {
	for _, c := range []struct {
		name string
		dir  bool
		want bool
	}{
		{"level.dat", false, true},
		{"level.dat_old", false, true},
		{"r.0.0.mca", false, true},
		{"region", true, true},
		{"region", false, false},
		{"level.dat", true, false},
		{"playerdata", true, false},
	} {
		if got := WorldEntry(c.name, c.dir); got != c.want {
			t.Errorf("WorldEntry(%q, %v) = %v, want %v", c.name, c.dir, got, c.want)
		}
	}
}
