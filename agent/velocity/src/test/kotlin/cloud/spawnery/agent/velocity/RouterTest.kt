package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.Player
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import java.lang.reflect.Proxy

/**
 * Over a real [ServerDirectory]/[FakeRegistry] pair, with player counts driven
 * through [FakeServer.players]. Only the list's size is ever read, so
 * [players] hands back a dynamic proxy that throws on every method.
 */
class RouterTest {
    @Test
    fun `the first group with a server wins, even if a later one has more`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("hub-1", "10.0.0.2:25565", "hub"),
                Backend("hub-2", "10.0.0.3:25565", "hub"),
            ),
        )
        val router = Router(directory)

        // A try list, not a global minimum: lobby has a server and wins.
        val chosen = router.choose(listOf("lobby", "hub"))

        assertEquals("lobby-1", chosen?.serverInfo?.name)
    }

    @Test
    fun `an empty group is skipped and the next one is tried`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(listOf(Backend("hub-1", "10.0.0.1:25565", "hub")))
        val router = Router(directory)

        val chosen = router.choose(listOf("lobby", "hub"))

        assertEquals("hub-1", chosen?.serverInfo?.name)
    }

    @Test
    fun `within a group the server with the fewest players wins`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
            ),
        )
        fakeServer(registry, "lobby-1").players = players(3)
        fakeServer(registry, "lobby-2").players = players(1)
        val router = Router(directory)

        val chosen = router.choose(listOf("lobby"))

        assertEquals("lobby-2", chosen?.serverInfo?.name)
    }

    @Test
    fun `a tie is broken by name, so the choice is deterministic`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-b", "10.0.0.1:25565", "lobby"),
                Backend("lobby-a", "10.0.0.2:25565", "lobby"),
            ),
        )
        val router = Router(directory)

        val chosen = router.choose(listOf("lobby"))

        assertEquals("lobby-a", chosen?.serverInfo?.name)
    }

    @Test
    fun `no group with a server yields null`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val router = Router(directory)

        assertNull(router.choose(listOf("hub", "survival")))
    }

    @Test
    fun `an empty group list yields null`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val router = Router(directory)

        assertNull(router.choose(emptyList()))
    }

    @Test
    fun `the excluded server is never chosen even when it is the only one`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val router = Router(directory)

        assertNull(router.choose(listOf("lobby"), excluding = setOf("lobby-1")))
    }

    @Test
    fun `excluding the emptiest server picks the next emptiest`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
            ),
        )
        fakeServer(registry, "lobby-1").players = players(0)
        fakeServer(registry, "lobby-2").players = players(2)
        val router = Router(directory)

        val chosen = router.choose(listOf("lobby"), excluding = setOf("lobby-1"))

        assertEquals("lobby-2", chosen?.serverInfo?.name)
    }

    /**
     * A first group whose only server is excluded falls through; the one
     * input that catches checking emptiness before the exclusion.
     */
    @Test
    fun `a group the exclusion empties falls through to the next group`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("hub-1", "10.0.0.2:25565", "hub"),
            ),
        )
        val router = Router(directory)

        val chosen = router.choose(listOf("lobby", "hub"), excluding = setOf("lobby-1"))

        assertEquals("hub-1", chosen?.serverInfo?.name)
    }

    private companion object {
        fun fakeServer(registry: FakeRegistry, name: String): FakeServer =
            registry.server(name) as FakeServer

        val dummyPlayer: Player = Proxy.newProxyInstance(
            Player::class.java.classLoader,
            arrayOf(Player::class.java),
        ) { _, _, _ ->
            throw UnsupportedOperationException("dummyPlayer is never called, only counted")
        } as Player

        fun players(count: Int): List<Player> = List(count) { dummyPlayer }
    }
}
