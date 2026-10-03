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

package cloud.spawnery.agent.api;

import java.util.Map;
import java.util.Objects;

/**
 * One backend, as the operator last described it.
 *
 * <p>A value and not a handle: call {@link SpawneryApi#server} again for a
 * newer one.
 *
 * @param registered whether the proxies have this server in their routing
 *     tables. A server can be {@link ServerPhase#READY} and not registered,
 *     during the first half of a drain, so a plugin deciding where to send
 *     somebody wants this and not the phase.
 * @param state what the server said it was doing, or {@code ""} if it has said
 *     nothing; see {@link SpawneryApi#announce}. Unrelated to {@link #phase()}.
 * @param attributes whatever else that server chose to publish, empty until it
 *     publishes something. Immutable.
 * @param incarnation which run of this server this is: an opaque token that
 *     changes whenever the process behind the name is replaced, and never
 *     otherwise. Compare it, never parse it.
 *     <p>A persistent server keeps its name across restarts, so anything that
 *     remembers a server and later asks whether this is still the one it meant
 *     compares this and not the name.
 *     <p>Empty for a server whose pod the operator has not seen yet, which is
 *     a server nobody is being sent to.
 * @param number which of its group's servers this is, counted the way a person
 *     counts: the second hub is 2. Stable for as long as the server exists.
 *     <p>Given out again once this server is gone, so two servers that were
 *     both "Hub-2" at different times are told apart by {@link #incarnation()}
 *     and never by this.
 *     <p>0 for a server nobody numbered, and for the ordinal-zero server of a
 *     persistent group, which reports its ordinal here. Show the name for
 *     those rather than a zero.
 * @param held whether an admin took this server's retirement back: nothing
 *     automatic removes it any more; it stays until it ends by itself.
 * @param node the Kubernetes node the server's pod runs on, empty while it is
 *     not scheduled.
 * @param playableSlots how many of {@code slots} count as capacity. Equal to
 *     {@code slots} when nothing narrowed it, and for a report from an
 *     operator older than this field.
 */
public record ServerInfo(
        String name,
        String group,
        ServerPhase phase,
        int players,
        int slots,
        boolean registered,
        String state,
        Map<String, String> attributes,
        String incarnation,
        int number,
        boolean held,
        String node,
        int playableSlots) {
    public ServerInfo {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(group, "group");
        Objects.requireNonNull(phase, "phase");
        state = state == null ? "" : state;
        attributes = attributes == null ? Map.of() : Map.copyOf(attributes);
        incarnation = incarnation == null ? "" : incarnation;
        node = node == null ? "" : node;
        if (playableSlots <= 0 || playableSlots > slots) {
            playableSlots = slots;
        }
    }

    /** The record as it was before {@code playableSlots}, which it reads as every seat. */
    public ServerInfo(
            String name,
            String group,
            ServerPhase phase,
            int players,
            int slots,
            boolean registered,
            String state,
            Map<String, String> attributes,
            String incarnation,
            int number,
            boolean held,
            String node) {
        this(name, group, phase, players, slots, registered, state, attributes, incarnation, number, held, node, slots);
    }

    /** The record as it was before {@code node}, which it reads as not scheduled. */
    public ServerInfo(
            String name,
            String group,
            ServerPhase phase,
            int players,
            int slots,
            boolean registered,
            String state,
            Map<String, String> attributes,
            String incarnation,
            int number,
            boolean held) {
        this(name, group, phase, players, slots, registered, state, attributes, incarnation, number, held, "", slots);
    }

    /** The record as it was before {@code held}, which it reads as false. */
    public ServerInfo(
            String name,
            String group,
            ServerPhase phase,
            int players,
            int slots,
            boolean registered,
            String state,
            Map<String, String> attributes,
            String incarnation,
            int number) {
        this(name, group, phase, players, slots, registered, state, attributes, incarnation, number, false, "", slots);
    }

    /**
     * How many more players this server would accept, never negative: a
     * report can show more players than slots while a lowered
     * {@code maxPlayers} reaches the running pods.
     */
    public int freeSlots() {
        return Math.max(0, slots - players);
    }
}
