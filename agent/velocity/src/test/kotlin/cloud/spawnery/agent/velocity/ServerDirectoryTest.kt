package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.ServerInfo
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.net.InetSocketAddress

/**
 * Every test builds its own [FakeRegistry] and, unless a test cares about log
 * output, a no-op log callback -- [ServerDirectory] never throws on a
 * malformed entry, so a test that ignored the log would still see the skip in
 * [FakeRegistry.calls] and in [ServerDirectory.names].
 */
class ServerDirectoryTest {
    @Test
    fun `a full sync registers every server it carries`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }

        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
            ),
        )

        assertEquals(setOf("lobby-1", "lobby-2"), directory.names())
        assertEquals(
            listOf(
                FakeRegistry.Call.Register(serverInfo("lobby-1", "10.0.0.1", 25565)),
                FakeRegistry.Call.Register(serverInfo("lobby-2", "10.0.0.2", 25565)),
            ),
            registry.calls,
        )
    }

    @Test
    fun `a second identical full sync registers nothing again`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        val servers = listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby"))
        directory.apply(servers)
        registry.calls.clear()

        directory.apply(servers)

        // The obvious wrong implementation -- one that calls register()
        // unconditionally instead of consulting registry.server(name) first
        // -- leaves this list non-empty. That is exactly what this asserts
        // against.
        assertTrue(registry.calls.isEmpty(), registry.calls.toString())
    }

    @Test
    fun `a full sync unregisters a server it no longer carries`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
            ),
        )

        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))

        assertNull(registry.server("lobby-2"))
        assertEquals(setOf("lobby-1"), directory.names())
    }

    @Test
    fun `a full sync leaves a server this agent never registered alone`() {
        val registry = FakeRegistry()
        // A configOverlay entry velocity.toml carried before this agent ever
        // ran, represented directly in the registry rather than through the
        // directory -- exactly the case a naive "unregister everything not in
        // this sync" implementation would destroy.
        val foreign = registry.seed(serverInfo("hub", "10.0.0.9", 25565))
        val directory = ServerDirectory(registry) { _, _ -> }

        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))

        assertSame(foreign, registry.server("hub"))
        assertTrue(registry.calls.none { it is FakeRegistry.Call.Unregister && it.info.name == "hub" })
    }

    @Test
    fun `a changed address unregisters and then registers, in that order`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        registry.calls.clear()

        directory.apply(listOf(Backend("lobby-1", "10.0.0.2:25565", "lobby")))

        // Order matters here, not just membership: a list equality check is
        // what catches an implementation that registered the new address
        // before unregistering the old one, which for two minutes is two
        // RegisteredServer entries claiming the same name.
        assertEquals(
            listOf(
                FakeRegistry.Call.Unregister(serverInfo("lobby-1", "10.0.0.1", 25565)),
                FakeRegistry.Call.Register(serverInfo("lobby-1", "10.0.0.2", 25565)),
            ),
            registry.calls,
        )
    }

    @Test
    fun `add registers one server and remove takes it away`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }

        directory.add(Backend("lobby-1", "10.0.0.1:25565", "lobby"))
        assertEquals(serverInfo("lobby-1", "10.0.0.1", 25565), registry.server("lobby-1")?.serverInfo)
        assertEquals(setOf("lobby-1"), directory.names())

        directory.remove("lobby-1")
        assertNull(registry.server("lobby-1"))
        assertEquals(emptySet<String>(), directory.names())
    }

    /**
     * The two tests below are the only ones in this class that start from a
     * *non-empty* directory, and that is the whole point of them.
     *
     * Every other test that touches [ServerDirectory.add] or
     * [ServerDirectory.remove] begins with nothing registered, where an
     * incremental update and a one-element full sync are indistinguishable. So
     * `fun add(backend: Backend) = apply(listOf(backend))` and `fun
     * remove(name: String) = apply(emptyList())` pass all of them -- while in
     * production, where the operator broadcasts `RegisterServer` every time a
     * `Server` becomes ready, each one would unregister every other backend
     * this proxy has, and the next periodic `FullSync` would put them back
     * about thirty seconds later. The level-2 harness cannot see it either:
     * `cmd/spawnery-stubop` only ever sends `FullSync`.
     */
    @Test
    fun `an incremental register leaves the backends already registered alone`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
            ),
        )
        registry.calls.clear()

        directory.add(Backend("lobby-3", "10.0.0.3:25565", "lobby"))

        assertEquals(setOf("lobby-1", "lobby-2", "lobby-3"), directory.names())
        assertEquals(
            listOf(FakeRegistry.Call.Register(serverInfo("lobby-3", "10.0.0.3", 25565))),
            registry.calls,
        )
    }

    @Test
    fun `an incremental unregister takes only the server it names`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
                Backend("lobby-3", "10.0.0.3:25565", "lobby"),
            ),
        )
        registry.calls.clear()

        directory.remove("lobby-2")

        assertEquals(setOf("lobby-1", "lobby-3"), directory.names())
        assertEquals(
            listOf(FakeRegistry.Call.Unregister(serverInfo("lobby-2", "10.0.0.2", 25565))),
            registry.calls,
        )
    }

    @Test
    fun `remove ignores a name this agent never registered`() {
        val registry = FakeRegistry()
        val foreign = registry.seed(serverInfo("hub", "10.0.0.9", 25565))
        val directory = ServerDirectory(registry) { _, _ -> }

        directory.remove("hub")

        // The obvious wrong implementation -- remove() looks the name up in
        // the registry directly, instead of checking its own names() first --
        // unregisters a server this agent never touched. Both assertions
        // catch it: the server would be gone, and calls would carry an
        // Unregister.
        assertSame(foreign, registry.server("hub"))
        assertTrue(registry.calls.isEmpty(), registry.calls.toString())
    }

    @Test
    fun `inGroup returns only the servers of that group`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("survival-1", "10.0.0.2:25565", "survival"),
                Backend("lobby-2", "10.0.0.3:25565", "lobby"),
            ),
        )

        val names = directory.inGroup("lobby").map { it.serverInfo.name }.toSet()

        assertEquals(setOf("lobby-1", "lobby-2"), names)
    }

    @Test
    fun `inGroup returns an empty list for an unknown group`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))

        assertTrue(directory.inGroup("nope").isEmpty())
    }

    @Test
    fun `a malformed address is skipped and the rest of the sync applies`() {
        val registry = FakeRegistry()
        val logs = mutableListOf<String>()
        val directory = ServerDirectory(registry) { message, _ -> logs += message }

        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("no-colon", "10.0.0.1", "lobby"),
                Backend("empty-port", "10.0.0.1:", "lobby"),
                Backend("bad-port", "10.0.0.1:notaport", "lobby"),
                Backend("empty", "", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
            ),
        )

        assertEquals(setOf("lobby-1", "lobby-2"), directory.names())
        assertNull(registry.server("no-colon"))
        assertNull(registry.server("empty-port"))
        assertNull(registry.server("bad-port"))
        assertNull(registry.server("empty"))
        assertEquals(4, logs.size, logs.toString())
    }

    @Test
    fun `an address without a port is skipped and logged, naming the server`() {
        val registry = FakeRegistry()
        val logs = mutableListOf<String>()
        val directory = ServerDirectory(registry) { message, _ -> logs += message }

        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:", "lobby")))

        assertNull(registry.server("lobby-1"))
        assertEquals(1, logs.size, logs.toString())
        assertTrue(logs[0].contains("lobby-1"), logs[0])
    }

    @Test
    fun `a removal is logged, naming the backend`() {
        val registry = FakeRegistry()
        val logs = mutableListOf<String>()
        val directory = ServerDirectory(registry) { message, _ -> logs += message }

        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        assertTrue(logs.isEmpty(), "a registration must not log; a FullSync carries every backend")

        directory.remove("lobby-1")

        assertEquals(1, logs.size, logs.toString())
        assertTrue(logs[0].contains("lobby-1"), logs[0])
    }

    @Test
    fun `a full sync logs the backends it drops and not the ones it keeps`() {
        val registry = FakeRegistry()
        val logs = mutableListOf<String>()
        val directory = ServerDirectory(registry) { message, _ -> logs += message }

        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
            ),
        )
        logs.clear()

        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))

        assertEquals(setOf("lobby-1"), directory.names())
        assertEquals(1, logs.size, logs.toString())
        assertTrue(logs[0].contains("lobby-2"), logs[0])
    }

    private companion object {
        fun serverInfo(name: String, host: String, port: Int) =
            ServerInfo(name, InetSocketAddress.createUnresolved(host, port))
    }
}
