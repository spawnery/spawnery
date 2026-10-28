package cloud.spawnery.agent.velocity.voice

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.net.Inet6Address
import java.net.InetAddress
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

    private val attacker = InetSocketAddress("198.51.100.7", 5000)
    private val onGameHost = { source: InetSocketAddress -> source.address == client.address }

    @Test
    fun `a source on the game connection's host takes the reply path from one that is not, at once`() {
        bridges.route(player, attacker, lobby, onGameHost)
        now += 100
        assertTrue(bridges.route(player, client, lobby, onGameHost) is Route.Existing)
        assertEquals(client, bridges.replyTo(player))
    }

    @Test
    fun `a source off the game connection's host does not displace one on it while it is active`() {
        bridges.route(player, client, lobby, onGameHost)
        now += 14_000
        bridges.route(player, client, lobby, onGameHost)
        now += 14_000
        bridges.route(player, attacker, lobby, onGameHost)
        assertEquals(client, bridges.replyTo(player))
    }

    @Test
    fun `a source off the game host takes over once the one on it has been silent for fifteen seconds`() {
        bridges.route(player, client, lobby, onGameHost)
        now += 14_999
        bridges.route(player, attacker, lobby, onGameHost)
        assertEquals(client, bridges.replyTo(player))
        now += 1
        bridges.route(player, attacker, lobby, onGameHost)
        assertEquals(attacker, bridges.replyTo(player))
    }

    @Test
    fun `a source on the game host still replaces another on it after three silent seconds`() {
        bridges.route(player, client, lobby, onGameHost)
        now += 3_000
        bridges.route(player, rebound, lobby, onGameHost)
        assertEquals(rebound, bridges.replyTo(player))
    }

    @Test
    fun `when neither source is on the game host the first keeps the path until it is silent`() {
        val second = InetSocketAddress("203.0.113.9", 6000)
        val neither = { _: InetSocketAddress -> false }
        bridges.route(player, attacker, lobby, neither)
        now += 1_000
        bridges.route(player, attacker, lobby, neither)
        now += 1_000
        bridges.route(player, second, lobby, neither)
        assertEquals(attacker, bridges.replyTo(player))
        now += 3_000
        bridges.route(player, second, lobby, neither)
        assertEquals(second, bridges.replyTo(player))
    }

    @Test
    fun `an IPv4-mapped IPv6 address is the same host as the IPv4 address`() {
        val v4 = InetAddress.getByAddress(byteArrayOf(192.toByte(), 0, 2, 10))
        val mapped = Inet6Address.getByAddress(
            null, byteArrayOf(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff.toByte(), 0xff.toByte(), 192.toByte(), 0, 2, 10), -1,
        )
        assertTrue(mapped is Inet6Address)
        assertTrue(sameHost(mapped, v4))
        assertTrue(sameHost(v4, mapped))
        assertFalse(sameHost(mapped, InetAddress.getByName("192.0.2.11")))
        assertFalse(sameHost(InetAddress.getByName("2001:db8::1"), v4))
        assertFalse(sameHost(null, v4))
    }
}
