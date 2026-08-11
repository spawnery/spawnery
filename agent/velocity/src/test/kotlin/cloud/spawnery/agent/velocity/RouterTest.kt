package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.Player
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import java.lang.reflect.Proxy

/**
 * Every test builds its [Router] over a real [ServerDirectory]/[FakeRegistry]
 * pair -- the composition that actually runs in production, per the brief --
 * and drives player counts through [FakeServer.players], the seam task 5
 * built for exactly this.
 *
 * Player identity never matters here, only how many of them a server has, so
 * [players] hands back interchangeable stand-ins rather than any real
 * [Player] -- constructing one by hand would mean implementing the ~30
 * abstract members [Player] inherits from `CommandSource`,
 * `InboundConnection` and the rest, none of which this ever calls. A dynamic
 * proxy that throws on every method is the same shape of fake as
 * [FakeServer.ping]: nothing here ever invokes a method on the elements of
 * [FakeServer.players], only `Collection.size` on the list they sit in, so
 * the handler never actually runs.
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

        // The obvious wrong implementation -- search every group and take the
        // global minimum -- would prefer an empty hub server over lobby-1.
        // fallbackGroups is a try list: lobby is tried first, has a server,
        // and wins outright.
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

        // Both servers are equally (un)populated, so nothing but the name
        // comparison can be picking lobby-a here.
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

        assertNull(router.choose(listOf("lobby"), excluding = "lobby-1"))
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

        val chosen = router.choose(listOf("lobby"), excluding = "lobby-1")

        assertEquals("lobby-2", chosen?.serverInfo?.name)
    }

    private companion object {
        fun fakeServer(registry: FakeRegistry, name: String): FakeServer =
            registry.server(name) as FakeServer

        // A single shared instance is enough: nothing here ever calls a
        // method on an element of the list, only Collection.size on the list
        // itself, so identity and behaviour never come into it.
        val dummyPlayer: Player = Proxy.newProxyInstance(
            Player::class.java.classLoader,
            arrayOf(Player::class.java),
        ) { _, _, _ ->
            throw UnsupportedOperationException("dummyPlayer is never called, only counted")
        } as Player

        fun players(count: Int): List<Player> = List(count) { dummyPlayer }
    }
}
