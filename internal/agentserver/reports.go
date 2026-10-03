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

package agentserver

import (
	"fmt"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// Every roster entry is fanned out to every agent in the namespace, and
// grpc-java refuses inbound messages above 4 MiB: 2048 entries at these
// bounds are about 470 KB, so eight worst-case proxies still fit.
const (
	RosterMaxEntries      = 2048
	RosterMaxUUIDLength   = 36
	RosterMaxNameLength   = 64
	RosterMaxServerLength = 128

	// One entry per backend the proxy knows; the same fan-out bound applies.
	BackendsMaxEntries    = 2048
	BackendsMaxNameLength = RosterMaxServerLength
)

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
