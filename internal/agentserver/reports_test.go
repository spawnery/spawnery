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

package agentserver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// One test per bound, each naming the bound it broke, as for announcements:
// a single "it was refused" passes when the wrong bound fired.

func rosterOf(n int) *agentpb.PlayerRoster {
	r := &agentpb.PlayerRoster{}
	for i := 0; i < n; i++ {
		r.Players = append(r.Players, &agentpb.RosterEntry{
			Uuid: fmt.Sprintf("069a79f4-44e9-4726-a5be-fca90e38%04x", i), Name: "alice", Server: "lobby-0",
		})
	}
	return r
}

func TestARosterWithinItsBoundsIsAccepted(t *testing.T) {
	if message, ok := rosterRefusal(rosterOf(RosterMaxEntries)); !ok {
		t.Errorf("a roster at the bound was refused: %s", message)
	}
	// And the empty one, which is how a proxy says nobody is on.
	if _, ok := rosterRefusal(&agentpb.PlayerRoster{}); !ok {
		t.Error("an empty roster was refused")
	}
}

func TestARosterWithMoreEntriesThanTheOperatorCarriesIsRefused(t *testing.T) {
	message, ok := rosterRefusal(rosterOf(RosterMaxEntries + 1))
	if ok {
		t.Fatal("an oversized roster was accepted")
	}
	if !strings.Contains(message, "2048") {
		t.Errorf("refusal = %q, want it to name the bound", message)
	}
}

func TestARosterEntryBeyondItsBoundsIsRefused(t *testing.T) {
	for _, tc := range []struct {
		field string
		entry *agentpb.RosterEntry
	}{
		{"UUID", &agentpb.RosterEntry{Uuid: strings.Repeat("u", RosterMaxUUIDLength+1)}},
		{"name", &agentpb.RosterEntry{Uuid: "u", Name: strings.Repeat("n", RosterMaxNameLength+1)}},
		{"server name", &agentpb.RosterEntry{Uuid: "u", Server: strings.Repeat("s", RosterMaxServerLength+1)}},
	} {
		message, ok := rosterRefusal(&agentpb.PlayerRoster{Players: []*agentpb.RosterEntry{tc.entry}})
		if ok {
			t.Errorf("an oversized %s was accepted", tc.field)
		} else if !strings.Contains(message, tc.field) {
			t.Errorf("refusal = %q, want it to name the %s", message, tc.field)
		}
	}
}

func TestABackendReportBeyondItsBoundsIsRefused(t *testing.T) {
	many := make(map[string]int32, BackendsMaxEntries+1)
	for i := 0; i <= BackendsMaxEntries; i++ {
		many[fmt.Sprintf("lobby-%d", i)] = 1
	}
	if message, ok := backendsRefusal(many); ok {
		t.Error("a report naming too many backends was accepted")
	} else if !strings.Contains(message, "2048") {
		t.Errorf("refusal = %q, want it to name the bound", message)
	}
	if _, ok := backendsRefusal(map[string]int32{strings.Repeat("s", BackendsMaxNameLength+1): 1}); ok {
		t.Error("an oversized backend name was accepted")
	}
	if message, ok := backendsRefusal(map[string]int32{"lobby-0": 3}); !ok {
		t.Errorf("an ordinary report was refused: %s", message)
	}
}
