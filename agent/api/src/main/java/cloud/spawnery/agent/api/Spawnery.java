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
import java.util.concurrent.atomic.AtomicReference;

/**
 * How a plugin gets a {@link SpawneryApi}, in one line and the same line on
 * both platforms.
 *
 * <p>A static holder rather than each platform's service registry, and that is
 * the point: Bukkit has {@code ServicesManager}, Velocity has Guice, and a
 * plugin author moving between them should not have to learn which.  The agent
 * calls {@link #install} once as it enables; everybody else calls
 * {@link #api()}.
 */
public final class Spawnery {
    private static final AtomicReference<SpawneryApi> INSTALLED = new AtomicReference<>();

    private Spawnery() {
    }

    /**
     * The API this server's agent installed.
     *
     * @throws SpawneryUnavailableException if no agent has installed one --
     *     see that type for the two ways that happens.
     */
    public static SpawneryApi api() {
        SpawneryApi api = INSTALLED.get();
        if (api == null) {
            throw new SpawneryUnavailableException(
                    "no spawnery agent has installed an API on this server. Either the spawnery "
                            + "agent plugin is not installed, or it has not finished enabling yet -- "
                            + "call this from your own enable step or later, not from a constructor.");
        }
        return api;
    }

    /** Whether {@link #api()} would return rather than throw. */
    public static boolean isAvailable() {
        return INSTALLED.get() != null;
    }

    /**
     * Installs the implementation. Called by the agent and by nothing else.
     *
     * <p>Refuses a second install rather than replacing the first: two agents
     * on one server is a misconfiguration, and the failure mode of letting the
     * second win is that half the plugins hold a handle to a dead one.
     *
     * @throws IllegalStateException if one is already installed
     */
    public static void install(SpawneryApi api) {
        Objects.requireNonNull(api, "api");
        if (!INSTALLED.compareAndSet(null, api)) {
            throw new IllegalStateException(
                    "a spawnery API is already installed on this server; two agent plugins "
                            + "are running where there should be one");
        }
    }

    /** Removes the installed API. For the agent's own shutdown, and for tests. */
    public static void uninstall() {
        INSTALLED.set(null);
    }
}
