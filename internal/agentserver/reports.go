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

	"github.com/spawnery/spawnery/internal/agentpb"
)

// The bounds on what a proxy may report about the players it holds.
//
// A roster is not answered and forgotten: every entry is copied into the
// network picture and carried to every agent in the namespace on every
// resync, and grpc-java refuses an inbound message above 4 MiB by default. So
// the figure that matters is the whole namespace's roster at once. One entry
// at these bounds is 36 + 64 + 128 bytes plus framing, about 230; a real one
// -- a dashed UUID, a sixteen-character name, a short server name -- is about
// 70. 2048 entries are then 470 KB at the bounds and 140 KB as measured, so
// the fan-out stays under grpc-java's limit for eight proxies reporting the
// worst case and for twenty-eight reporting real players. A proxy holding
// more than 2048 players is not something this operator has met.
const (
	RosterMaxEntries      = 2048
	RosterMaxUUIDLength   = 36
	RosterMaxNameLength   = 64
	RosterMaxServerLength = 128

	// BackendsMaxEntries bounds the per-server player counts a proxy sends
	// beside its roster. One entry per backend the proxy knows, so a namespace
	// of two thousand servers is the ceiling, and the same fan-out argument
	// applies: the counts reach every agent's network picture.
	BackendsMaxEntries    = 2048
	BackendsMaxNameLength = RosterMaxServerLength
)

// rosterRefusal reports whether a roster is within its bounds, and says which
// one it broke when it is not. The shape of announcementRefusal, for the same
// reader: an agent author reading one log line.
func rosterRefusal(roster *agentpb.PlayerRoster) (string, bool) {
	players := roster.GetPlayers()
	if len(players) > RosterMaxEntries {
		return fmt.Sprintf("that roster has %d entries and the operator carries at most %d",
			len(players), RosterMaxEntries), false
	}
	for _, p := range players {
		if len(p.GetUuid()) > RosterMaxUUIDLength {
			return fmt.Sprintf("a roster UUID is %d characters and the operator carries at most %d",
				len(p.GetUuid()), RosterMaxUUIDLength), false
		}
		if len(p.GetName()) > RosterMaxNameLength {
			return fmt.Sprintf("a roster name is %d characters and the operator carries at most %d",
				len(p.GetName()), RosterMaxNameLength), false
		}
		if len(p.GetServer()) > RosterMaxServerLength {
			return fmt.Sprintf("a roster server name is %d characters and the operator carries at most %d",
				len(p.GetServer()), RosterMaxServerLength), false
		}
	}
	return "", true
}

// backendsRefusal is rosterRefusal for the per-backend counts.
func backendsRefusal(backends map[string]int32) (string, bool) {
	if len(backends) > BackendsMaxEntries {
		return fmt.Sprintf("that report names %d backends and the operator carries at most %d",
			len(backends), BackendsMaxEntries), false
	}
	for name := range backends {
		if len(name) > BackendsMaxNameLength {
			return fmt.Sprintf("a backend name is %d characters and the operator carries at most %d",
				len(name), BackendsMaxNameLength), false
		}
	}
	return "", true
}
