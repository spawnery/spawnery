package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.ServerSelf
import java.io.File
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

private fun backend(
    pod: String = "lobby-7f3a",
    inGroup: String = "lobby",
    inNetwork: String = "example",
) = object : ServerSelf {
    override fun name(): String = pod
    override fun group(): String = inGroup
    override fun network(): String = inNetwork
    override fun slots(): Int = 20
}

private fun proxy(
    pod: String = "edge-2c11",
    inGroup: String = "edge",
    inNetwork: String = "example",
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
                "network" to "example",
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
                "network" to "example",
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
            mapOf("group" to "lobby", "network" to "example", "environment" to "paper"),
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

    @Test
    fun `a proxy asks with the contexts the target backend registers itself`() {
        val target = backend(pod = "vip-x1", inGroup = "vip", inNetwork = "tutorial")

        assertEquals(
            LuckPermsContexts.of(target, LuckPermsContexts.UNCONFIGURED),
            LuckPermsContexts.ofBackend("vip-x1", "vip", "tutorial"),
        )
        assertEquals(
            mapOf("server" to "vip-x1", "group" to "vip", "environment" to "paper"),
            LuckPermsContexts.ofBackend("vip-x1", "vip", ""),
        )
    }

    @Test
    fun `a pod without LuckPerms is silence and not a crash`() {
        // Proves the absent path only while LuckPerms is off the test classpath.
        assertFailsWith<ClassNotFoundException> {
            Class.forName("net.luckperms.api.LuckPermsProvider")
        }
        val said = mutableListOf<String>()

        LuckPermsContexts.registerIfPresent(backend(), said::add, said::add)

        assertTrue(said.isEmpty(), "it spoke without LuckPerms: $said")
    }

    @Test
    fun `a LuckPerms that is present but not loaded is a warning, not a failed enable`() {
        val info = mutableListOf<String>()
        val warned = mutableListOf<String>()
        val failure = IllegalStateException("LuckPerms API is not loaded.")

        LuckPermsContexts.registerIfPresent(
            backend(), info::add, warned::add,
            present = { true },
            registration = { _, _ -> throw failure },
        )

        assertEquals(emptyList(), info)
        assertTrue(warned.single().contains(failure.message!!), "the warning does not name the failure: $warned")
    }

    @Test
    fun `the real registration, failing for want of LuckPerms' classes, is survived too`() {
        val warned = mutableListOf<String>()

        LuckPermsContexts.registerIfPresent(backend(), {}, warned::add, present = { true })

        assertTrue(warned.single().contains("NoClassDefFoundError"), "expected the missing class: $warned")
    }

    @Test
    fun `every probed class name matches an import of its file`() {
        val sources = File("src/main/kotlin/cloud/spawnery/agent").listFiles()!!
            .filter { it.extension == "kt" && "Class.forName" in it.readText() }
        assertTrue(sources.size >= 3, "expected the three LuckPerms probes, found ${sources.map { it.name }}")

        for (file in sources) {
            val source = file.readText()
            val probed = Regex("""Class\.forName\("([^"]+)"\)""").findAll(source).map { it.groupValues[1] }.toList()
            assertEquals(
                source.split("Class.forName").size - 1,
                probed.size,
                "${file.name} has a Class.forName this test cannot read as a literal",
            )
            val imports = Regex("""^import (\S+)$""", RegexOption.MULTILINE)
                .findAll(source)
                .map { it.groupValues[1] }
                .toSet()
            for (name in probed) {
                assertTrue(name in imports, "${file.name} probes \"$name\", which none of its imports name: $imports")
            }
        }
    }
}
