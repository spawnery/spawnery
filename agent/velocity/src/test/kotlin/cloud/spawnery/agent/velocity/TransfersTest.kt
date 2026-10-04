package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.api.ProxyInfo
import org.junit.jupiter.api.Test
import net.kyori.adventure.text.Component
import net.kyori.adventure.text.TranslatableComponent
import java.net.InetAddress
import java.net.InetSocketAddress
import java.util.UUID
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNull
import kotlin.test.assertTrue

class TransfersTest {
    private val secret = "s3cret".toByteArray()
    private var seconds = 1_000L
    private val cookie = TransferCookie(secret) { seconds }
    private val host = InetSocketAddress.createUnresolved("play.example.net", 25565)

    private val leaving = listOf(
        ProxyInfo("edge-1", "edge", false, true, 0, ""),
        ProxyInfo("edge-2", "edge", true, false, 0, ""),
    )
    private val staying = listOf(
        ProxyInfo("edge-1", "edge", true, false, 0, ""),
        ProxyInfo("edge-2", "edge", true, false, 0, ""),
    )
    private var proxies = leaving
    private var registered = setOf("lobby-1", "arena-1")
    private var closedDoors = emptySet<String>()

    val infos = mutableListOf<String>()
    val warnings = mutableListOf<Pair<String, Throwable?>>()

    private val transfers = Transfers(
        cookie = cookie,
        policy = TransferPolicy(0L) { seconds * 1000 },
        picture = { TransferPolicy.Picture("edge-1", "edge", proxies, closedDoors, setOf("edge-1", "edge-2")) },
        registered = { it in registered },
        info = { infos += it },
        warn = { message, error -> warnings += message to error },
        spacingMillis = 3_500,
        clock = { seconds * 1000 },
    )

    private class FakeTraveller(
        override val username: String,
        override val currentServer: String?,
        override val virtualHost: InetSocketAddress?,
        var failWith: Exception? = null,
        var tellFailWith: Exception? = null,
        override val address: InetAddress? = InetAddress.getByAddress(username.toByteArray().copyOf(16)),
    ) : Traveller {
        override val uuid: UUID = UUID.nameUUIDFromBytes(username.toByteArray())
        val sent = mutableListOf<Pair<ByteArray, InetSocketAddress>>()
        val told = mutableListOf<Component>()

        override fun tell(message: Component) {
            tellFailWith?.let { throw it }
            told += message
        }

        override fun transfer(cookie: ByteArray, to: InetSocketAddress) {
            failWith?.let { throw it }
            sent += cookie to to
        }
    }

    @Test
    fun `a switch on a leaving proxy is denied, and the player is sent away carrying the target`() {
        val alice = FakeTraveller("alice", "lobby-1", host)

        assertTrue(transfers.onSwitch(alice, "arena-1"))

        assertEquals(1, alice.sent.size)
        val (bytes, to) = alice.sent.single()
        assertEquals(host, to)
        assertEquals(TransferCookie.Result.Valid("arena-1"), cookie.read(alice.uuid, bytes))
        assertEquals(listOf("spawnery: transferred 'alice' (switch) toward 'arena-1'"), infos)
    }

    @Test
    fun `a switch on a proxy that is not leaving is left alone`() {
        proxies = staying
        val alice = FakeTraveller("alice", "lobby-1", host)

        assertFalse(transfers.onSwitch(alice, "arena-1"))
        assertTrue(alice.sent.isEmpty())
    }

    @Test
    fun `a player without a virtual host is never transferred`() {
        val alice = FakeTraveller("alice", "lobby-1", null)

        assertFalse(transfers.onSwitch(alice, "arena-1"))
        transfers.pass(listOf(alice))
        assertTrue(alice.sent.isEmpty())
    }

    @Test
    fun `a pass transfers every forced occupant toward their current server`() {
        val alice = FakeTraveller("alice", "lobby-1", host)
        val bob = FakeTraveller("bob", "arena-1", host)

        transfers.pass(listOf(alice, bob))

        assertEquals(TransferCookie.Result.Valid("lobby-1"), cookie.read(alice.uuid, alice.sent.single().first))
        assertEquals(TransferCookie.Result.Valid("arena-1"), cookie.read(bob.uuid, bob.sent.single().first))
        assertEquals(
            setOf(
                "spawnery: transferred 'alice' (forced) toward 'lobby-1'",
                "spawnery: transferred 'bob' (forced) toward 'arena-1'",
            ),
            infos.toSet(),
        )
    }

    @Test
    fun `a transfer that throws is logged and not tried again`() {
        val alice = FakeTraveller("alice", "lobby-1", host, failWith = IllegalArgumentException("too old"))

        transfers.pass(listOf(alice))
        transfers.pass(listOf(alice))

        assertEquals(1, warnings.size)
        assertTrue(warnings.single().first.contains("'alice'"), warnings.single().first)
        assertTrue(infos.isEmpty())
    }

    @Test
    fun `a switch whose transfer throws goes ahead as a normal switch`() {
        val alice = FakeTraveller("alice", "lobby-1", host, failWith = IllegalStateException("closing"))

        assertFalse(transfers.onSwitch(alice, "arena-1"))
        assertEquals(1, warnings.size)
    }

    @Test
    fun `an arrival with a valid cookie lands on the named server`() {
        val alice = UUID.nameUUIDFromBytes("alice".toByteArray())
        var resumed = 0
        transfers.expecting(alice) { resumed++ }

        assertTrue(transfers.received(alice, "alice", cookie.write(alice, "arena-1")))

        assertEquals(1, resumed)
        assertEquals("arena-1", transfers.landing(alice, "alice"))
        assertNull(transfers.landing(alice, "alice"))
    }

    @Test
    fun `an arrival carrying another player's cookie is routed as a fresh join`() {
        val alice = UUID.nameUUIDFromBytes("alice".toByteArray())
        val bob = UUID.nameUUIDFromBytes("bob".toByteArray())
        transfers.expecting(alice) {}

        transfers.received(alice, "alice", cookie.write(bob, "arena-1"))

        assertNull(transfers.landing(alice, "alice"))
        assertEquals(
            listOf("spawnery: transfer cookie from 'alice' refused: other player"),
            warnings.map { it.first },
        )
    }

    @Test
    fun `an arrival naming a server that is not registered is routed as a fresh join`() {
        val alice = UUID.nameUUIDFromBytes("alice".toByteArray())
        transfers.expecting(alice) {}
        transfers.received(alice, "alice", cookie.write(alice, "arena-9"))

        assertNull(transfers.landing(alice, "alice"))
        assertTrue(warnings.single().first.startsWith("spawnery: transfer cookie from 'alice' refused: "))
        assertTrue(warnings.single().first.contains("arena-9"))
    }

    @Test
    fun `an arrival without a cookie is routed as a fresh join`() {
        val alice = UUID.nameUUIDFromBytes("alice".toByteArray())
        transfers.expecting(alice) {}

        assertTrue(transfers.received(alice, "alice", null))

        assertNull(transfers.landing(alice, "alice"))
        assertEquals(1, warnings.size)
    }

    @Test
    fun `a timed-out wait resumes once, and the late answer is still claimed but not used`() {
        val alice = UUID.nameUUIDFromBytes("alice".toByteArray())
        var resumed = 0
        transfers.expecting(alice) { resumed++ }

        transfers.gaveUp(alice, "alice", "no answer")
        assertTrue(transfers.received(alice, "alice", cookie.write(alice, "arena-1")))
        transfers.gaveUp(alice, "alice", "no answer")

        assertEquals(1, resumed)
        assertNull(transfers.landing(alice, "alice"))
    }

    @Test
    fun `a cookie nobody asked for is not claimed`() {
        val alice = UUID.nameUUIDFromBytes("alice".toByteArray())

        assertFalse(transfers.received(alice, "alice", cookie.write(alice, "arena-1")))
        assertNull(transfers.landing(alice, "alice"))
    }

    @Test
    fun `a player sent away is leaving by transfer until forgotten`() {
        val alice = FakeTraveller("alice", "lobby-1", host)

        assertFalse(transfers.leavingByTransfer(alice.uuid))
        transfers.onSwitch(alice, "arena-1")
        assertTrue(transfers.leavingByTransfer(alice.uuid))

        transfers.forget(alice.uuid)
        assertFalse(transfers.leavingByTransfer(alice.uuid))
    }

    @Test
    fun `a transfer that throws does not count as leaving`() {
        val alice = FakeTraveller("alice", "lobby-1", host, failWith = IllegalStateException("closing"))

        transfers.pass(listOf(alice))

        assertFalse(transfers.leavingByTransfer(alice.uuid))
    }

    @Test
    fun `an arrival with a valid cookie arrived by transfer until forgotten, landing or not`() {
        val alice = UUID.nameUUIDFromBytes("alice".toByteArray())
        transfers.expecting(alice) {}
        transfers.received(alice, "alice", cookie.write(alice, "arena-9"))

        assertTrue(transfers.arrivedByTransfer(alice))
        assertNull(transfers.landing(alice, "alice"))
        assertTrue(transfers.arrivedByTransfer(alice))

        transfers.forget(alice)
        assertFalse(transfers.arrivedByTransfer(alice))
    }

    @Test
    fun `an arrival whose cookie is refused or late did not arrive by transfer`() {
        val alice = UUID.nameUUIDFromBytes("alice".toByteArray())
        val bob = UUID.nameUUIDFromBytes("bob".toByteArray())
        transfers.expecting(alice) {}
        transfers.received(alice, "alice", cookie.write(bob, "arena-1"))
        transfers.expecting(bob) {}
        transfers.gaveUp(bob, "bob", "no answer")
        transfers.received(bob, "bob", cookie.write(bob, "arena-1"))

        assertFalse(transfers.arrivedByTransfer(alice))
        assertFalse(transfers.arrivedByTransfer(bob))
    }

    @Test
    fun `a pass sends one player per address, and the next only once the spacing has passed`() {
        val shared = InetAddress.getByName("198.51.100.7")
        val alice = FakeTraveller("alice", "lobby-1", host, address = shared)
        val bob = FakeTraveller("bob", "lobby-1", host, address = shared)
        val carol = FakeTraveller("carol", "lobby-1", host)
        val everyone = listOf(alice, bob, carol)

        transfers.pass(everyone)
        assertEquals(1, alice.sent.size + bob.sent.size)
        assertEquals(1, carol.sent.size)

        seconds += 3
        transfers.pass(everyone)
        assertEquals(1, alice.sent.size + bob.sent.size)

        seconds += 1
        transfers.pass(everyone)
        assertEquals(1, alice.sent.size)
        assertEquals(1, bob.sent.size)
    }

    @Test
    fun `a switch from an address that was just used goes ahead as a normal switch and stays eligible`() {
        val shared = InetAddress.getByName("198.51.100.7")
        val alice = FakeTraveller("alice", "lobby-1", host, address = shared)
        val bob = FakeTraveller("bob", "lobby-1", host, address = shared)

        assertTrue(transfers.onSwitch(alice, "arena-1"))
        assertFalse(transfers.onSwitch(bob, "arena-1"))
        assertTrue(bob.sent.isEmpty())

        seconds += 4
        assertTrue(transfers.onSwitch(bob, "arena-1"))
    }

    @Test
    fun `a player without an address is not held back by spacing`() {
        val alice = FakeTraveller("alice", "lobby-1", host, address = null)
        val bob = FakeTraveller("bob", "lobby-1", host, address = null)

        transfers.pass(listOf(alice, bob))

        assertEquals(1, alice.sent.size)
        assertEquals(1, bob.sent.size)
    }

    @Test
    fun `a player behind a closed door does not hold back another from the same address`() {
        val shared = InetAddress.getByName("198.51.100.7")
        val alice = FakeTraveller("alice", "arena-1", host, address = shared)
        val bob = FakeTraveller("bob", "lobby-1", host, address = shared)
        closedDoors = setOf("arena-1")

        transfers.pass(listOf(alice, bob))

        assertTrue(alice.sent.isEmpty())
        assertEquals(1, bob.sent.size)
    }

    private fun transfersAs(self: String, proxies: List<ProxyInfo>) = Transfers(
        cookie = cookie,
        policy = TransferPolicy(0L) { seconds * 1000 },
        picture = { TransferPolicy.Picture(self, "edge", proxies, emptySet(), proxies.map { it.name() }.toSet()) },
        registered = { it in registered },
        info = {},
        warn = { _, _ -> },
        spacingMillis = 3_500,
        clock = { seconds * 1000 },
    )

    @Test
    fun `two leaving proxies never send from one address within the spacing of each other`() {
        val bothLeaving = listOf(
            ProxyInfo("edge-1", "edge", false, true, 0, ""),
            ProxyInfo("edge-2", "edge", false, true, 0, ""),
            ProxyInfo("edge-3", "edge", true, false, 0, ""),
        )
        val shared = InetAddress.getByName("198.51.100.7")
        val first = transfersAs("edge-1", bothLeaving)
        val second = transfersAs("edge-2", bothLeaving)
        val onFirst = listOf(FakeTraveller("alice", "lobby-1", host, address = shared), FakeTraveller("bob", "lobby-1", host, address = shared))
        val onSecond = listOf(FakeTraveller("carol", "lobby-1", host, address = shared), FakeTraveller("dave", "lobby-1", host, address = shared))
        val sentAt = mutableListOf<Long>()

        repeat(60) {
            val before = (onFirst + onSecond).sumOf { it.sent.size }
            first.pass(onFirst)
            second.pass(onSecond)
            repeat((onFirst + onSecond).sumOf { it.sent.size } - before) { sentAt += seconds * 1000 }
            seconds += 1
        }

        assertEquals(4, sentAt.size, "every player is sent within a minute")
        sentAt.zipWithNext().forEach { (a, b) -> assertTrue(b - a >= 3_500, "sends at $a and $b are closer than the spacing") }
    }

    @Test
    fun `a proxy leaving alone is not held to a turn`() {
        val alone = listOf(
            ProxyInfo("edge-1", "edge", false, true, 0, ""),
            ProxyInfo("edge-2", "edge", true, false, 0, ""),
        )
        val transfers = transfersAs("edge-1", alone)
        val players = (1..5).map { FakeTraveller("p$it", "lobby-1", host) }

        transfers.pass(players)

        assertTrue(players.all { it.sent.size == 1 })
    }

    @Test
    fun `a pass warns a player before it transfers them`() {
        val alice = FakeTraveller("alice", "lobby-1", host)

        transfers.pass(listOf(alice))

        val warning = alice.told.single() as TranslatableComponent
        assertEquals("spawnery.transfer.warning", warning.key())
        assertEquals(1, alice.sent.size)
    }

    @Test
    fun `a switch transfers without a warning`() {
        val alice = FakeTraveller("alice", "lobby-1", host)

        assertTrue(transfers.onSwitch(alice, "arena-1"))
        assertTrue(alice.told.isEmpty(), alice.told.toString())
    }

    @Test
    fun `a player without a virtual host is not warned either`() {
        val alice = FakeTraveller("alice", "lobby-1", null)

        transfers.pass(listOf(alice))

        assertTrue(alice.told.isEmpty(), alice.told.toString())
    }

    @Test
    fun `a proxy waiting for its turn still warns`() {
        val bothLeaving = listOf(
            ProxyInfo("edge-1", "edge", false, true, 0, ""),
            ProxyInfo("edge-2", "edge", false, true, 0, ""),
            ProxyInfo("edge-3", "edge", true, false, 0, ""),
        )
        val second = transfersAs("edge-2", bothLeaving)
        val alice = FakeTraveller("alice", "lobby-1", host)

        second.pass(listOf(alice))

        assertEquals(1, alice.told.size)
        assertTrue(alice.sent.isEmpty())
    }

    @Test
    fun `a tell that throws is logged, and neither the other warnings nor any transfer are lost`() {
        val alice = FakeTraveller("alice", "lobby-1", host, tellFailWith = IllegalStateException("gone"))
        val bob = FakeTraveller("bob", "lobby-1", host)
        val carol = FakeTraveller("carol", "arena-1", host)

        transfers.pass(listOf(alice, bob, carol))

        assertEquals(1, bob.told.size)
        assertEquals(1, carol.told.size)
        assertEquals(1, alice.sent.size)
        assertEquals(1, bob.sent.size)
        assertEquals(1, carol.sent.size)
        assertEquals(1, warnings.size)
        assertTrue(warnings.single().first.contains("'alice'"), warnings.single().first)
        assertTrue(warnings.single().second is IllegalStateException)
    }

    @Test
    fun `a door that opens after the deadline gives the warning and the move in one pass`() {
        val alice = FakeTraveller("alice", "arena-1", host)
        closedDoors = setOf("arena-1")

        transfers.pass(listOf(alice))
        assertTrue(alice.told.isEmpty())
        assertTrue(alice.sent.isEmpty())

        seconds += 60
        closedDoors = emptySet()
        transfers.pass(listOf(alice))

        val warning = alice.told.single() as TranslatableComponent
        assertEquals(Component.text(1L), warning.arguments().single().asComponent())
        assertEquals(1, alice.sent.size)
    }

    @Test
    fun `the force deadline counts from the first pass, not from the proxy's first turn`() {
        val bothLeaving = listOf(
            ProxyInfo("edge-1", "edge", false, true, 0, ""),
            ProxyInfo("edge-2", "edge", false, true, 0, ""),
            ProxyInfo("edge-3", "edge", true, false, 0, ""),
        )
        val second = Transfers(
            cookie = cookie,
            policy = TransferPolicy(15_000) { seconds * 1000 },
            picture = { TransferPolicy.Picture("edge-2", "edge", bothLeaving, emptySet(), setOf("edge-3")) },
            registered = { it in registered },
            info = {},
            warn = { _, _ -> },
            spacingMillis = 3_500,
            clock = { seconds * 1000 },
        )
        val alice = FakeTraveller("alice", "lobby-1", host)
        val first = seconds

        var sentAt: Long? = null
        while (sentAt == null && seconds < first + 30) {
            second.pass(listOf(alice))
            if (alice.sent.isNotEmpty()) sentAt = seconds - first
            seconds += 1
        }

        assertEquals(15L, sentAt, "the 15 s deadline runs from the first pass at 0 s, so the window opening at 15 s is the first to send; counted from edge-2's first turn at 1 s it would be 16 s")
    }

    @Test
    fun `the warning is translatable, with the seconds as its argument and a fallback`() {
        val c = TransferWarning.message(7) as TranslatableComponent
        assertEquals("spawnery.transfer.warning", c.key())
        assertEquals("You will be reconnected in %s seconds.", c.fallback())
        assertEquals(Component.text(7L), c.arguments().single().asComponent())
    }
}
