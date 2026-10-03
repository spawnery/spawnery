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

package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.Self
import net.luckperms.api.LuckPermsProvider
import net.luckperms.api.context.ContextConsumer
import net.luckperms.api.context.ContextSet
import net.luckperms.api.context.ImmutableContextSet
import net.luckperms.api.context.StaticContextCalculator

object LuckPermsContexts {
    /** What LuckPerms reports when nobody has configured a server name. */
    const val UNCONFIGURED = "global"

    /** `server` is LuckPerms' own key, so it is filled in only while LuckPerms has none configured. */
    fun of(self: Self, luckPermsServerName: String): Map<String, String> {
        val contexts = LinkedHashMap<String, String>()
        if (luckPermsServerName == UNCONFIGURED) {
            contexts.putIfCarried("server", self.name())
        }
        contexts.putIfCarried("group", self.group())
        contexts.putIfCarried("network", self.network())
        // The API, not the jar: Purpur reports `paper`, so a grant survives a swap.
        contexts["environment"] = if (self is ProxySelf) "velocity" else "paper"
        return contexts
    }

    private fun MutableMap<String, String>.putIfCarried(key: String, value: String) {
        if (value.isNotBlank()) this[key] = value
    }

    /** Probed rather than caught: a NoClassDefFoundError out of `onEnable` makes Paper disable the agent. */
    fun registerIfPresent(self: Self, log: (String) -> Unit) {
        try {
            Class.forName("net.luckperms.api.LuckPermsProvider")
        } catch (_: ClassNotFoundException) {
            return
        }
        registerSurviving(log) { register(self, log) }
    }

    /**
     * [LuckPermsProvider.get] throws while LuckPerms' own enable has not finished
     * or has failed, and the agent's session must outlive that.
     */
    internal fun registerSurviving(log: (String) -> Unit, registration: () -> Unit) {
        try {
            registration()
        } catch (e: Throwable) {
            log("spawnery LuckPerms contexts: registration failed, continuing without them: $e")
        }
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

    private class Calculator(private val contexts: ImmutableContextSet) : StaticContextCalculator {
        override fun calculate(consumer: ContextConsumer) = consumer.accept(contexts)

        override fun estimatePotentialContexts(): ContextSet = contexts
    }
}
