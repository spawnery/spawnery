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

import "testing"

// The order is the whole contract: rendered defaults lose to the user, and
// both lose to the fields an operator cannot be allowed to break.
func TestLayerAppliesThreeSourcesInOrder(t *testing.T) {
	got := Layer(
		map[string]string{"motd": "default", "max-players": "20", "difficulty": "peaceful"},
		map[string]string{"motd": "mine", "online-mode": "true"},
		map[string]string{"online-mode": "false", "difficulty": "hard"},
	)

	if got["motd"] != "mine" {
		t.Errorf("motd = %q, want the overlay to outrank the default", got["motd"])
	}
	if got["max-players"] != "20" {
		t.Errorf("max-players = %q, want the default to survive an overlay that does not mention it", got["max-players"])
	}
	// The one that matters: a user who writes online-mode=true into their
	// overlay is asking for a backend anyone can join. They do not get it.
	if got["online-mode"] != "false" {
		t.Errorf("online-mode = %q, want the critical layer to win", got["online-mode"])
	}
	// difficulty appears in base and critical but not overlay: the only way
	// to tell critical-over-base apart from critical-over-overlay, which the
	// online-mode assertion above cannot distinguish on its own.
	if got["difficulty"] != "hard" {
		t.Errorf("difficulty = %q, want the critical layer to outrank the base directly, not just transitively through the overlay", got["difficulty"])
	}
}

func TestLayerDoesNotMutateItsInputs(t *testing.T) {
	base := map[string]string{"a": "1"}
	overlay := map[string]string{"a": "2"}
	critical := map[string]string{"a": "3"}

	Layer(base, overlay, critical)

	if base["a"] != "1" || overlay["a"] != "2" || critical["a"] != "3" {
		t.Error("Layer wrote through to one of its inputs")
	}
}

func TestLayerHandlesNilSources(t *testing.T) {
	got := Layer(map[string]string{"a": "1"}, nil, nil)
	if got["a"] != "1" {
		t.Errorf("a = %q, want the base to survive nil upper layers", got["a"])
	}
}
