package cloud.spawnery.agent.velocity.voice

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.net.InetSocketAddress
import java.util.UUID

class BridgesTest {
    private var now = 0L
    private val bridges = Bridges { now }
    private val player = UUID.randomUUID()
    private val client = InetSocketAddress("192.0.2.10", 40000)
    private val rebound = InetSocketAddress("192.0.2.10", 40001)
    private val lobby = InetSocketAddress("10.0.0.1", 24454)
    private val game = InetSocketAddress("10.0.0.2", 24454)

    @Test
    fun `a new player opens a bridge`() {
        assertEquals(Route.Open(replaces = false), bridges.route(player, client, lobby))
        assertEquals(client, bridges.replyTo(player))
    }

    @Test
    fun `an active client keeps its reply path against another source`() {
        bridges.route(player, client, lobby)
        now += 2_999
        bridges.route(player, client, lobby)
        now += 1_000
        assertTrue(bridges.route(player, rebound, lobby) is Route.Existing)
        assertEquals(client, bridges.replyTo(player))
    }

    @Test
    fun `a source silent for three seconds hands the reply path over`() {
        bridges.route(player, client, lobby)
        now += 3_000
        bridges.route(player, rebound, lobby)
        assertEquals(rebound, bridges.replyTo(player))
    }

    @Test
    fun `a new backend replaces the bridge`() {
        bridges.route(player, client, lobby)
        assertEquals(Route.Open(replaces = true), bridges.route(player, client, game))
    }

    @Test
    fun `fifteen silent seconds expire a bridge, and a reply keeps it alive`() {
        bridges.route(player, client, lobby)
        now += 14_000
        bridges.touch(player)
        now += 14_000
        assertEquals(emptyList<UUID>(), bridges.expired { _, _ -> false })
        now += 1_000
        assertEquals(listOf(player), bridges.expired { _, _ -> false })
    }

    @Test
    fun `a silent bridge stays while its player still routes to the same backend`() {
        bridges.route(player, client, lobby)
        now += 60_000
        assertEquals(emptyList<UUID>(), bridges.expired { id, backend -> id == player && backend == lobby })
        assertEquals(listOf(player), bridges.expired { _, backend -> backend == game })
    }
}
