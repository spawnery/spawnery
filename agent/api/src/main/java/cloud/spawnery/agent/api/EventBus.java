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

import java.util.function.Consumer;

/**
 * Where a plugin hears about things happening in the cloud.
 *
 * <p><b>A feed and not a ledger.</b> An agent that was disconnected missed what
 * happened while it was gone, and nothing replays it — the network picture it
 * re-syncs on reconnect is the correction, and a better one than a replay
 * would be: it says what is true now rather than what was true in an order
 * nobody was watching. A plugin that needs a ledger should watch the objects.
 *
 * <p>Events arrive uncollapsed, one per transition. What a player sees in chat
 * is a collapsed summary of them; you get the facts.
 */
public interface EventBus {
    /**
     * Calls {@code listener} for every cloud event this agent receives, until
     * the returned handle is closed.
     *
     * <p><b>The listener runs on a network callback thread.</b> Do not block
     * it and do not touch the world from it — hand the work to your platform's
     * scheduler. A listener that throws is dropped from the next dispatch
     * rather than taking the session down with it, but it is still your bug
     * and nothing tells you twice.
     *
     * <p>Closing the handle is idempotent. A plugin that forgets to close one
     * on disable leaks a listener into a classloader the platform is trying to
     * unload, which is the ordinary way a reload turns into a memory leak.
     */
    AutoCloseable subscribe(Consumer<CloudEventInfo> listener);
}
