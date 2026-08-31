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

package render

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestNoWorldDefaultsOverlayWritesNoWorldDefaultsFile(t *testing.T) {
	// Every installation that never sets this must get exactly the two files
	// it got before. An unconditional empty write would overwrite whatever the
	// server had filled in for itself on every single start -- a fresh loss
	// per restart on a persistent group, whose /data survives.
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	if _, ok := files["config/paper-world-defaults.yml"]; ok {
		t.Error("a world-defaults file was written for a group that named none")
	}
	if len(files) != 2 {
		t.Errorf("Paper wrote %d files, want the two it always wrote: %v", len(files), keysOf(files))
	}
}

func TestAWorldDefaultsOverlayIsWrittenThrough(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-world-defaults.yml": "misc:\n  disable-end-credits: true\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	raw, ok := files["config/paper-world-defaults.yml"]
	if !ok {
		t.Fatal("no world-defaults file was written for a group that named one")
	}

	// Parsed rather than string-matched: what has to arrive is the setting,
	// not a particular serialisation of it.
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the rendered file does not parse: %v\n%s", err, raw)
	}
	misc, _ := doc["misc"].(map[string]any)
	if misc == nil || misc["disable-end-credits"] != true {
		t.Errorf("misc.disable-end-credits did not survive the render:\n%s", raw)
	}
}

func TestAnUndeclaredWorldDefaultsKeyIsRefused(t *testing.T) {
	// The failure this exists to prevent, in Paper's own words: it keeps its
	// default for the field the author meant, writes the stray key back out,
	// and the file on disk goes on looking like the override took.
	_, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-world-defaults.yml": "misc:\n  disable-end-credit: true\n",
	})
	if err == nil {
		t.Fatal("a key Paper does not read was accepted")
	}
	if !strings.Contains(err.Error(), "disable-end-credit") {
		t.Errorf("the refusal does not name the key, so nobody can act on it: %v", err)
	}
}

func TestAMalformedWorldDefaultsOverlayIsRefused(t *testing.T) {
	for name, overlay := range map[string]string{
		"not YAML at all": "misc: [unclosed\n",
		"a list":          "- misc\n- other\n",
		"a bare scalar":   "42\n",
		"null":            "null\n",
	} {
		if _, err := Paper(paperValues(), "s3cret", map[string]string{
			"paper-world-defaults.yml": overlay,
		}); err == nil {
			t.Errorf("%s was accepted; Paper would have had to reject it in its own words", name)
		}
	}
}

func TestAnEmptyWorldDefaultsOverlayWritesAnEmptyFile(t *testing.T) {
	// Setting the key to "" is a thing somebody said, and what they said is
	// "leave this file alone". Refusing it would make the empty string the one
	// value a ConfigMap cannot carry.
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"paper-world-defaults.yml": "",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	raw, ok := files["config/paper-world-defaults.yml"]
	if !ok {
		t.Fatal("no file was written")
	}
	if len(raw) != 0 {
		t.Errorf("wrote %q, want an empty document", raw)
	}
}

// The shapes a real CloudNET network sets, copied from coding-area.net's
// configs repository at 4eea9fa on 2026-08-31 -- Global/Game and Hub/default.
// They are the reason this file exists, and a declared-key tree that refused
// either of them would have closed nothing.
//
// Kept as literals rather than read from that repository: it is private, it is
// not a dependency of this one, and what is worth pinning is the *shape* an
// overlay of this file takes in practice, which does not change when somebody
// edits a value over there.
func TestTheOverlaysARealNetworkSetsAreAccepted(t *testing.T) {
	overlays := map[string]string{
		"Global/Game": `_version: 31
entities:
  behavior:
    pillager-patrols:
      disable: true
    phantoms-do-not-spawn-on-creative-players: true
    phantoms-only-attack-insomniacs: true
  spawning:
    wandering-trader:
      spawn-chance-max: 0
      spawn-chance-min: 0
misc:
  disable-end-credits: true
`,
		"Hub/default": `_version: 31
entities:
  behavior:
    pillager-patrols:
      disable: true
  spawning:
    spawn-limits:
      monster: 0
      creature: 0
      ambient: 0
      axolotls: 0
      underground_water_creature: 0
      water_ambient: 0
      water_creature: 0
    wandering-trader:
      spawn-chance-max: 0
      spawn-chance-min: 0
misc:
  disable-end-credits: true
  disable-relative-projectile-velocity: true
`,
	}
	for name, overlay := range overlays {
		files, err := Paper(paperValues(), "s3cret", map[string]string{
			"paper-world-defaults.yml": overlay,
		})
		if err != nil {
			t.Errorf("%s was refused: %v", name, err)
			continue
		}
		if len(files["config/paper-world-defaults.yml"]) == 0 {
			t.Errorf("%s rendered an empty file", name)
		}
	}
}
