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
 * A group of servers, as the operator last described it.
 *
 * @param freeSlots the operator's own figure, which counts only ready servers
 *     of the group's current spec. It can disagree with a sum over
 *     {@link SpawneryApi#servers()} while a rolling update is in flight.
 * @param attributes what whoever runs this network wrote down about this group
 *     in its own definition, empty until somebody writes something. Unlike
 *     {@link ServerInfo#attributes()}, a person writes these, not a server.
 *     Immutable.
 * @param displayName what this group is called where a person reads it, for
 *     example on a scoreboard; {@link #name()} is a DNS label. Never empty: a
 *     group nobody has named is displayed by its own name.
 */
public record Group(
        String name,
        Kind kind,
        int replicas,
        int readyReplicas,
        int onlinePlayers,
        int freeSlots,
        Map<String, String> attributes,
        String displayName) {
    public Group {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(kind, "kind");
        attributes = attributes == null ? Map.of() : Map.copyOf(attributes);
        // Here and not in the operator, so an operator that predates the field
        // reads like one that left it out.
        displayName = displayName == null || displayName.isEmpty() ? name : displayName;
    }

    /** Which sizing rule this group answers to. */
    public enum Kind {
        /** Sized by free player slots; servers are interchangeable. */
        EPHEMERAL,
        /** Sized by a number a person wrote down; servers own their worlds. */
        PERSISTENT,
        /** A group of proxies. */
        PROXY,
        /** Servers asked for by name, one key each; see {@link SpawneryApi#startServer}. */
        ON_DEMAND,
        /** A kind this jar predates. See {@link ServerPhase#UNKNOWN}. */
        UNKNOWN
    }
}
