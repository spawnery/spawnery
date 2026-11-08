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

    @Test
    fun `a group the player may not join is skipped like an empty one`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("vip-1", "10.0.0.1:25565", "vip"),
                Backend("lobby-1", "10.0.0.2:25565", "lobby"),
            ),
        )
        val router = Router(directory)

        val chosen = router.choose(listOf("vip", "lobby")) { _, group -> group != "vip" }

        assertEquals("lobby-1", chosen?.serverInfo?.name)
    }

    @Test
    fun `the predicate sees each candidate server with its group`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("vip-1", "10.0.0.1:25565", "vip"),
                Backend("vip-2", "10.0.0.2:25565", "vip"),
            ),
        )
        val router = Router(directory)

        val chosen = router.choose(listOf("vip")) { server, group -> group == "vip" && server == "vip-2" }

        assertEquals("vip-2", chosen?.serverInfo?.name)
    }

    @Test
    fun `every group forbidden yields null`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(listOf(Backend("vip-1", "10.0.0.1:25565", "vip")))
        val router = Router(directory)

        assertNull(router.choose(listOf("vip")) { _, _ -> false })
    }

    @Test
    fun `a Fill group sends the next player to the fullest server below its line`() {
        val registry = FakeRegistry()
        val directory = lobby(registry, "lobby-1", "lobby-2", "lobby-3")
        val mirror = mirrorOf(
            setOf("lobby"),
            Reported("lobby-1", players = 3, slots = 20, line = 16, number = 1),
            Reported("lobby-2", players = 10, slots = 20, line = 16, number = 2),
            Reported("lobby-3", players = 16, slots = 20, line = 16, number = 3),
        )
        val tally = frozenTally(mirror)
        val router = Router(directory, FillSource(mirror, tally))

        val chosen = router.choose(listOf("lobby"))

        assertEquals("lobby-2", chosen?.serverInfo?.name)
        assertEquals(1, tally.pending("lobby-2"))
    }

    @Test
    fun `two proxies with the same mirror pick the same server`() {
        val mirror = mirrorOf(
            setOf("lobby"),
            Reported("lobby-1", players = 5, slots = 20, line = 16, number = 1),
            Reported("lobby-2", players = 4, slots = 20, line = 16, number = 2),
        )
        val registryA = FakeRegistry()
        val routerA = Router(lobby(registryA, "lobby-1", "lobby-2"), FillSource(mirror, frozenTally(mirror)))
        fakeServer(registryA, "lobby-1").players = players(2)
        val registryB = FakeRegistry()
        val routerB = Router(lobby(registryB, "lobby-1", "lobby-2"), FillSource(mirror, frozenTally(mirror)))
        fakeServer(registryB, "lobby-2").players = players(3)

        assertEquals("lobby-1", routerA.choose(listOf("lobby"))?.serverInfo?.name)
        assertEquals("lobby-1", routerB.choose(listOf("lobby"))?.serverInfo?.name)
    }

    @Test
    fun `a burst never puts more than maxPlayers on one server while another has room`() {
        val registry = FakeRegistry()
        val directory = lobby(registry, "lobby-1", "lobby-2")
        val mirror = mirrorOf(
            setOf("lobby"),
            Reported("lobby-1", players = 7, slots = 10, line = 8, number = 1),
            Reported("lobby-2", players = 0, slots = 10, line = 8, number = 2),
        )
        val router = Router(directory, FillSource(mirror, frozenTally(mirror)))

        val sent = List(13) { router.choose(listOf("lobby"))?.serverInfo?.name }.groupingBy { it }.eachCount()

        assertEquals(mapOf("lobby-1" to 3, "lobby-2" to 10), sent)
    }

    @Test
    fun `a sent player who has connected here is counted once`() {
        val registry = FakeRegistry()
        val directory = lobby(registry, "lobby-1", "lobby-2")
        val mirror = mirrorOf(
            setOf("lobby"),
            Reported("lobby-1", players = 0, slots = 10, line = 8, number = 1),
            Reported("lobby-2", players = 0, slots = 10, line = 8, number = 2),
        )
        val router = Router(directory, FillSource(mirror, frozenTally(mirror)))

        val sent = List(9) {
            val name = router.choose(listOf("lobby"))!!.serverInfo.name
            val server = fakeServer(registry, name)
            server.players = server.players + dummyPlayer
            name
        }

        assertEquals(List(8) { "lobby-1" } + "lobby-2", sent)
    }

    @Test
    fun `a Spread group is chosen exactly as before`() {
        val registry = FakeRegistry()
        val directory = lobby(registry, "lobby-1", "lobby-2")
        fakeServer(registry, "lobby-1").players = players(3)
        fakeServer(registry, "lobby-2").players = players(1)
        val mirror = mirrorOf(
            emptySet(),
            Reported("lobby-1", players = 10, slots = 20, number = 1),
            Reported("lobby-2", players = 1, slots = 20, number = 2),
        )
        val tally = frozenTally(mirror)
        val router = Router(directory, FillSource(mirror, tally))

        val chosen = router.choose(listOf("lobby"))

        assertEquals("lobby-2", chosen?.serverInfo?.name)
        assertEquals(0, tally.pending("lobby-2"))
    }

    private companion object {
        fun fakeServer(registry: FakeRegistry, name: String): FakeServer =
            registry.server(name) as FakeServer

        fun lobby(registry: FakeRegistry, vararg names: String): ServerDirectory {
            val directory = ServerDirectory(registry) { _, _ -> }
            directory.apply(names.mapIndexed { i, name -> Backend(name, "10.0.0.${i + 1}:25565", "lobby") })
            return directory
        }

        val dummyPlayer: Player = Proxy.newProxyInstance(
            Player::class.java.classLoader,
            arrayOf(Player::class.java),
        ) { _, _, _ ->
            throw UnsupportedOperationException("dummyPlayer is never called, only counted")
        } as Player

        fun players(count: Int): List<Player> = List(count) { dummyPlayer }
    }
}
