package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.ServerSelf
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse

private fun backend(
    pod: String = "lobby-7f3a",
    inGroup: String = "lobby",
    inNetwork: String = "cyperia",
) = object : ServerSelf {
    override fun name(): String = pod
    override fun group(): String = inGroup
    override fun network(): String = inNetwork
    override fun slots(): Int = 20
}

private fun proxy(
    pod: String = "edge-2c11",
    inGroup: String = "edge",
    inNetwork: String = "cyperia",
) = object : ProxySelf {
    override fun name(): String = pod
    override fun group(): String = inGroup
    override fun network(): String = inNetwork
}

class LuckPermsContextsTest {
    @Test
    fun `a backend reports its pod, group, network and platform`() {
        assertEquals(
            mapOf(
                "server" to "lobby-7f3a",
                "group" to "lobby",
                "network" to "cyperia",
                "environment" to "paper",
            ),
            LuckPermsContexts.of(backend(), LuckPermsContexts.UNCONFIGURED),
        )
    }

    @Test
    fun `a proxy is the velocity environment and names its own pod`() {
        assertEquals(
            mapOf(
                "server" to "edge-2c11",
                "group" to "edge",
                "network" to "cyperia",
                "environment" to "velocity",
            ),
            LuckPermsContexts.of(proxy(), LuckPermsContexts.UNCONFIGURED),
        )
    }

    @Test
    fun `a configured LuckPerms server name is left to stand alone`() {
        val contexts = LuckPermsContexts.of(backend(), "lobby")

        assertFalse("server" in contexts, "it overwrote a configured name: $contexts")
        assertEquals(
            mapOf("group" to "lobby", "network" to "cyperia", "environment" to "paper"),
            contexts,
        )
    }

    @Test
    fun `a variable the pod does not carry is left out rather than sent empty`() {
        val contexts = LuckPermsContexts.of(
            backend(inGroup = "", inNetwork = ""),
            LuckPermsContexts.UNCONFIGURED,
        )

        assertEquals(mapOf("server" to "lobby-7f3a", "environment" to "paper"), contexts)
    }
}
