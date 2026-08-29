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
 * Where a player is to be sent.
 *
 * <p>A server or a group, and the difference matters: naming a server says
 * exactly where, while naming a group says "wherever that group has room" and
 * hands the choice to the operator — which is the only side that can compare
 * every backend's occupancy without racing the mirror a plugin reads.
 */
public sealed interface Target {
    /** That exact server, by the name {@link ServerInfo#name()} carries. */
    static Target server(String name) {
        return new Server(Objects.requireNonNull(name, "name"));
    }

    /** Wherever that group has the most room, chosen by the operator. */
    static Target group(String name) {
        return new Group(Objects.requireNonNull(name, "name"));
    }

    record Server(String name) implements Target { }

    record Group(String name) implements Target { }
}
