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

import java.util.Map;
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
 * @param state what the server said it was doing, or {@code ""} if it has said
 *     nothing. This is the server's own word and not the operator's: see
 *     {@link SpawneryApi#announce}. It is unrelated to {@link #phase()}, which
 *     is the operator's account of the same server's lifecycle -- a server is
 *     {@link ServerPhase#READY} for a long time, and this is what it is doing
 *     during it.
 * @param attributes whatever else that server chose to publish, empty until it
 *     publishes something. Immutable.
 */
public record ServerInfo(
        String name,
        String group,
        ServerPhase phase,
        int players,
        int slots,
        boolean registered,
        String state,
        Map<String, String> attributes) {
    public ServerInfo {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(group, "group");
        Objects.requireNonNull(phase, "phase");
        // A server that has announced nothing and one whose agent predates
        // announcing are the same server as far as a plugin is concerned, so
        // both arrive here as the empty description rather than as null. A
        // plugin asking what a server is doing should never have to write a
        // null check to find out that it is doing nothing in particular.
        state = state == null ? "" : state;
        attributes = attributes == null ? Map.of() : Map.copyOf(attributes);
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
