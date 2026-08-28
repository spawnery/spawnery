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
 * A group of servers, as the operator last described it.
 *
 * @param freeSlots the operator's own figure, which counts only ready servers
 *     of the group's current spec. It is what the scaler publishes rather than
 *     a sum a plugin could compute from {@link SpawneryApi#servers()}, and the
 *     two can disagree while a rolling update is in flight.
 */
public record Group(
        String name,
        Kind kind,
        int replicas,
        int readyReplicas,
        int onlinePlayers,
        int freeSlots) {
    public Group {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(kind, "kind");
    }

    /** Which sizing rule this group answers to. */
    public enum Kind {
        /** Sized by free player slots; servers are interchangeable. */
        EPHEMERAL,
        /** Sized by a number a person wrote down; servers own their worlds. */
        PERSISTENT,
        /** A group of proxies. */
        PROXY,
        /** A kind this jar predates. See {@link ServerPhase#UNKNOWN}. */
        UNKNOWN
    }
}
