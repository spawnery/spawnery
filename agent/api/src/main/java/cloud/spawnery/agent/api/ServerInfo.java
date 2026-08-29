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

package cloud.spawnery.agent.api;

import java.util.Objects;

/**
 * One backend, as the operator last described it.
 *
 * <p>A value and not a handle: it is a description of a moment, and the moment
 * has passed by the time a plugin reads it. Calling {@link SpawneryApi#server}
 * again gets a newer one; holding this and expecting it to change gets
 * nothing.
 *
 * @param registered whether the proxies have this server in their routing
 *     tables. A server can be {@link ServerPhase#READY} and not registered --
 *     that is the first half of a drain -- so a plugin deciding where to send
 *     somebody wants this and not the phase.
 */
public record ServerInfo(
        String name,
        String group,
        ServerPhase phase,
        int players,
        int slots,
        boolean registered) {
    public ServerInfo {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(group, "group");
        Objects.requireNonNull(phase, "phase");
    }

    /**
     * How many more players this server would accept, never negative.
     *
     * <p>The floor is not defensive tidiness: a report can show more players
     * than slots for as long as it takes a lowered {@code maxPlayers} to reach
     * the running pods, and a plugin sizing a list from this should not meet a
     * negative number.
     */
    public int freeSlots() {
        return Math.max(0, slots - players);
    }
}
