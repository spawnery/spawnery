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

package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.Self
import net.luckperms.api.LuckPermsProvider
import net.luckperms.api.context.ContextConsumer
import net.luckperms.api.context.ContextSet
import net.luckperms.api.context.ImmutableContextSet
import net.luckperms.api.context.StaticContextCalculator

/**
 * What this pod tells LuckPerms about itself, so that a permission rule can
 * say where it applies.
 *
 * The values are pod-wide and never per player, which is what lets the
 * calculator answer without being given anybody.
 */
object LuckPermsContexts {
    /** What LuckPerms reports when nobody has configured a server name. */
    const val UNCONFIGURED = "global"

    /**
     * @param luckPermsServerName LuckPerms' own configured server name.
     *   [UNCONFIGURED] means nobody set one, and only then is `server` filled
     *   in: `server` is LuckPerms' own key, and a second value for it would
     *   leave both an administrator's rule and ours matching.
     */
    fun of(self: Self, luckPermsServerName: String): Map<String, String> {
        val contexts = LinkedHashMap<String, String>()
        if (luckPermsServerName == UNCONFIGURED) {
            contexts.putIfCarried("server", self.name())
        }
        contexts.putIfCarried("group", self.group())
        contexts.putIfCarried("network", self.network())
        // The plugin API this agent runs against and not the jar, so a Purpur
        // server reports `paper`: a grant must not change meaning when
        // somebody swaps Purpur for Paper.
        contexts["environment"] = if (self is ProxySelf) "velocity" else "paper"
        return contexts
    }

    // Not named `set`: the standard library already has that extension on
    // MutableMap, and two of them in scope with the same signature is a
    // resolution question nobody should have to answer while reading this.
    private fun MutableMap<String, String>.putIfCarried(key: String, value: String) {
        if (value.isNotBlank()) this[key] = value
    }

    /**
     * Registers this pod's contexts with LuckPerms, if there is a LuckPerms.
     *
     * Probed rather than caught. Without the plugin the class below cannot
     * resolve, and the NoClassDefFoundError would leave `onEnable` through
     * Paper's plugin manager, which disables the agent -- so a server with no
     * permission plugin would lose the cloud over a permission feature.
     *
     * @param log where to say what was registered. Nothing is said when there
     *   is no LuckPerms, which is the ordinary case.
     */
    fun registerIfPresent(self: Self, log: (String) -> Unit) {
        try {
            Class.forName("net.luckperms.api.LuckPermsProvider")
        } catch (_: ClassNotFoundException) {
            return
        }
        register(self, log)
    }

    private fun register(self: Self, log: (String) -> Unit) {
        val luckPerms = LuckPermsProvider.get()
        val contexts = ImmutableContextSet.builder()
        for ((key, value) in of(self, luckPerms.serverName)) {
            contexts.add(key, value)
        }
        val set = contexts.build()
        luckPerms.contextManager.registerCalculator(Calculator(set))
        log("spawnery LuckPerms contexts: $set")
    }

    /**
     * One fixed answer for everybody on this pod -- a player is never looked
     * at, which is what [StaticContextCalculator] is for.
     */
    private class Calculator(private val contexts: ImmutableContextSet) : StaticContextCalculator {
        override fun calculate(consumer: ContextConsumer) = consumer.accept(contexts)

        override fun estimatePotentialContexts(): ContextSet = contexts
    }
}
