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

import java.util.List;
import java.util.Optional;
import java.util.UUID;
import java.util.concurrent.CompletionStage;

/**
 * What a plugin can ask the cloud, from either side of the proxy.
 *
 * <p><b>Every method here is a local read.</b> The operator keeps a mirror
 * current in each agent, so none of these calls crosses a network, blocks, or
 * fails -- there is no timeout and no exception to handle. What they return is
 * the last thing the operator said, which during a reconnect may be a few
 * seconds old and is never wrong about a moment that happened.
 *
 * <p><b>Consume this interface; do not implement it.</b> Methods are added
 * here as later milestones land -- events, moving a player, starting a server
 * -- and adding one breaks an implementor while leaving every caller alone.
 * The agent supplies the implementation; a plugin obtains it from
 * {@link Spawnery#api()}.
 *
 * <p>Everything is scoped to this pod's own namespace, which is the whole of
 * what this agent can see. There is no method that reaches another network,
 * and that is structural rather than a check somebody could forget: the
 * agent's credentials are a pod-bound ServiceAccount token, so there is
 * nothing to widen.
 */
public interface SpawneryApi {
    /** What this process is. Use {@code instanceof} to learn which side. */
    Self self();

    /** Every group in this network, in no particular order. */
    List<Group> groups();

    /** One group by name, empty if this network has none. */
    Optional<Group> group(String name);

    /** Every server in this network, in no particular order. */
    List<ServerInfo> servers();

    /** One server by name, empty if this network has none. */
    Optional<ServerInfo> server(String name);

    /** Every player on this network, whichever backend they are on. */
    List<CloudPlayer> players();

    /** One player by UUID, empty if they are not on this network. */
    Optional<CloudPlayer> player(UUID id);

    /**
     * Asks the operator to move a player.
     *
     * <p><b>Asynchronous on both platforms, including the one where it need
     * not be.</b> On a proxy this could answer locally; on a backend it is a
     * round trip through the operator. Following the platform would make the
     * signature synchronous on one side and not the other, and a plugin author
     * moving between them would have to rewrite rather than recompile. So it
     * is the shape of the harder case on both.
     *
     * <p>The stage fails rather than returning a result when the operator
     * refuses or cannot answer — including when the stream was renewed while
     * the request was in flight, which is failed rather than retried because
     * only you know whether moving that player twice is safe.
     *
     * <p>A player who logged out between your call and the operator reading it
     * is an ordinary failure and not a bug. So is a target that is not
     * routable yet.
     */
    CompletionStage<ConnectResult> connect(UUID player, Target to);
}
