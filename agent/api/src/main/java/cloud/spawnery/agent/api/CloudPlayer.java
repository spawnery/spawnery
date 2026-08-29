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
import java.util.Optional;
import java.util.UUID;

/**
 * A player, and which backend they are on.
 *
 * @param server empty when the proxy has them and no backend does: during the
 *     login handshake, and between one backend and the next. It is not an
 *     error and a plugin must handle it -- a player in flight is exactly the
 *     player a drain is about, and this project's own drain gap was a player
 *     nobody counted.
 */
public record CloudPlayer(UUID id, String name, Optional<String> server) {
    public CloudPlayer {
        Objects.requireNonNull(id, "id");
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(server, "server");
    }
}
