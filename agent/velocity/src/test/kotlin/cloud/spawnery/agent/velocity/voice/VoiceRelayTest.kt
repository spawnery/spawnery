package cloud.spawnery.agent.velocity.voice

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Assumptions.assumeTrue
import org.junit.jupiter.api.Test
import java.io.File
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.Inet6Address
import java.net.InetAddress
import java.net.InetSocketAddress
import java.nio.ByteBuffer
import java.util.UUID
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference
import kotlin.concurrent.thread

class VoiceRelayTest {
    private val player = UUID.randomUUID()

    /** A backend that sends every datagram straight back, prefixed with its own port. */
    private fun echoBackend(): DatagramSocket {
        val socket = DatagramSocket(0, InetAddress.getLoopbackAddress())
        thread(isDaemon = true) {
            val buf = ByteArray(2048)
            while (!socket.isClosed) {
                val p = DatagramPacket(buf, buf.size)
                runCatching { socket.receive(p) }.onFailure { return@thread }
                val reply = ByteBuffer.allocate(p.length + 4).putInt(socket.localPort).put(buf, 0, p.length).array()
                socket.send(DatagramPacket(reply, reply.size, p.socketAddress))
            }
        }
        return socket
    }

    /** A backend that answers every datagram with the source port it arrived from. */
    private fun sourcePortBackend(): DatagramSocket {
        val socket = DatagramSocket(0, InetAddress.getLoopbackAddress())
        thread(isDaemon = true) {
            val buf = ByteArray(2048)
            while (!socket.isClosed) {
                val p = DatagramPacket(buf, buf.size)
                runCatching { socket.receive(p) }.onFailure { return@thread }
                val reply = ByteBuffer.allocate(4).putInt(p.port).array()
                socket.send(DatagramPacket(reply, reply.size, p.socketAddress))
            }
        }
        return socket
    }

    private fun frame(id: UUID, body: String) =
        ByteBuffer.allocate(17 + body.length).put(VoiceWire.MAGIC)
            .putLong(id.mostSignificantBits).putLong(id.leastSignificantBits).put(body.toByteArray()).array()

    private fun roundTrip(client: DatagramSocket, relayPort: Int, packet: ByteArray): ByteBuffer? {
        client.send(DatagramPacket(packet, packet.size, InetAddress.getLoopbackAddress(), relayPort))
        val buf = ByteArray(2048)
        val p = DatagramPacket(buf, buf.size)
        return runCatching { client.receive(p); ByteBuffer.wrap(buf, 0, p.length) }.getOrNull()
    }

    @Test
    fun `a packet reaches the player's backend and the reply comes back`() {
        val backend = echoBackend()
        val counters = VoiceCounters()
        VoiceRelay(
            InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { if (it == player) Backend.At(backend.localSocketAddress as InetSocketAddress) else Backend.UnknownPlayer },
            counters = counters, log = { _, _ -> },
        ).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 2_000
                val reply = roundTrip(client, relay.localPort, frame(player, "hello"))!!
                assertEquals(backend.localPort, reply.int)
                assertEquals(VoiceWire.MAGIC, reply.get())
            }
            assertEquals(1L, counters.totals()[VoiceCounters.RELAYED])
        }
        backend.close()
    }

    @Test
    fun `a switch of backend reaches the new one`() {
        val lobby = echoBackend()
        val game = echoBackend()
        val current = AtomicReference(lobby)
        VoiceRelay(
            InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { Backend.At(current.get().localSocketAddress as InetSocketAddress) },
            counters = VoiceCounters(), log = { _, _ -> },
        ).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 2_000
                assertEquals(lobby.localPort, roundTrip(client, relay.localPort, frame(player, "a"))!!.int)
                current.set(game)
                assertEquals(game.localPort, roundTrip(client, relay.localPort, frame(player, "b"))!!.int)
            }
        }
        lobby.close(); game.close()
    }

    @Test
    fun `a long silence keeps the source port the backend knows, until the player leaves`() {
        val backend = sourcePortBackend()
        val at = Backend.At(backend.localSocketAddress as InetSocketAddress)
        val now = AtomicLong(0)
        val present = AtomicBoolean(true)
        // The relay sweeps once its one-second select times out with nothing to read.
        fun sweep() = Thread.sleep(1_500)
        VoiceRelay(
            InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { if (present.get()) at else Backend.UnknownPlayer },
            counters = VoiceCounters(), log = { _, _ -> }, clock = now::get,
        ).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 2_000
                val first = roundTrip(client, relay.localPort, frame(player, "auth"))!!.int

                now.set(60_000)
                sweep()
                assertEquals(first, roundTrip(client, relay.localPort, frame(player, "check"))!!.int)

                present.set(false)
                now.addAndGet(60_000)
                sweep()
                present.set(true)
                assertTrue(first != roundTrip(client, relay.localPort, frame(player, "rejoin"))!!.int)
            }
        }
        backend.close()
    }

    @Test
    fun `garbage opens nothing and is counted`() {
        val counters = VoiceCounters()
        var resolved = 0
        VoiceRelay(
            InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { resolved++; Backend.UnknownPlayer },
            counters = counters, log = { _, _ -> },
        ).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 300
                assertNull(roundTrip(client, relay.localPort, ByteArray(40)))
                assertNull(roundTrip(client, relay.localPort, byteArrayOf(VoiceWire.MAGIC, 1, 2)))
                assertNull(roundTrip(client, relay.localPort, frame(VoiceWire.PING_UUID, "x")))
                assertNull(roundTrip(client, relay.localPort, frame(UUID.randomUUID(), "x")))
            }
        }
        assertEquals(2L, counters.totals()[VoiceCounters.NOT_SVC])
        assertEquals(1L, counters.totals()[VoiceCounters.PING])
        assertEquals(1L, counters.totals()[VoiceCounters.UNKNOWN_PLAYER])
        assertEquals(1, resolved)
    }

    @Test
    fun `a backend that cannot be reached opens no bridge, and other players still relay`() {
        val backend = echoBackend()
        val counters = VoiceCounters()
        val logs = CopyOnWriteArrayList<String>()
        val broken = UUID.randomUUID()
        VoiceRelay(
            InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = {
                if (it == broken) Backend.At(InetSocketAddress.createUnresolved("voice.invalid", 24454))
                else Backend.At(backend.localSocketAddress as InetSocketAddress)
            },
            counters = counters, log = { message, _ -> logs.add(message) },
        ).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 300
                assertNull(roundTrip(client, relay.localPort, frame(broken, "a")))
                assertNull(roundTrip(client, relay.localPort, frame(broken, "b")))
                client.soTimeout = 2_000
                assertEquals(backend.localPort, roundTrip(client, relay.localPort, frame(player, "c"))!!.int)
            }
        }
        backend.close()
        assertEquals(2L, counters.totals()[VoiceCounters.UNKNOWN_SERVER])
        assertEquals(1L, counters.totals()[VoiceCounters.RELAYED])
        assertEquals(1, logs.size, logs.toString())
    }

    @Test
    fun `a backend gone mid-session costs only its own player`() {
        val gone = echoBackend()
        val alive = echoBackend()
        val other = UUID.randomUUID()
        VoiceRelay(
            InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { Backend.At((if (it == player) gone else alive).localSocketAddress as InetSocketAddress) },
            counters = VoiceCounters(), log = { _, _ -> },
        ).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 2_000
                assertEquals(gone.localPort, roundTrip(client, relay.localPort, frame(player, "a"))!!.int)
                gone.close()
                client.soTimeout = 300
                repeat(3) { assertNull(roundTrip(client, relay.localPort, frame(player, "b"))) }
                client.soTimeout = 2_000
                assertEquals(alive.localPort, roundTrip(client, relay.localPort, frame(other, "c"))!!.int)
            }
        }
        alive.close()
    }

    @Test
    fun `close releases the port`() {
        val relay = VoiceRelay(
            InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { Backend.UnknownPlayer }, counters = VoiceCounters(), log = { _, _ -> },
        )
        relay.start()
        val port = relay.localPort
        relay.close()
        DatagramSocket(port, InetAddress.getLoopbackAddress()).close()
    }

    @Test
    fun `a wildcard bind takes IPv6 clients too`() {
        val v6 = InetAddress.getByName("::1") as Inet6Address
        assumeTrue(runCatching { DatagramSocket(0, v6).close() }.isSuccess, "no IPv6 loopback here")
        val backend = echoBackend()
        VoiceRelay(
            InetSocketAddress(InetAddress.getByName("0.0.0.0"), 0),
            resolve = { Backend.At(backend.localSocketAddress as InetSocketAddress) },
            counters = VoiceCounters(), log = { _, _ -> },
        ).use { relay ->
            relay.start()
            DatagramSocket(0, v6).use { client ->
                client.soTimeout = 2_000
                val packet = frame(player, "hello")
                client.send(DatagramPacket(packet, packet.size, v6, relay.localPort))
                val buf = ByteArray(2048)
                val p = DatagramPacket(buf, buf.size)
                client.receive(p)
                assertEquals(backend.localPort, ByteBuffer.wrap(buf, 0, p.length).int)
            }
        }
        backend.close()
    }

    @Test
    fun `a bind that fails leaks no descriptors`() {
        val fds = File("/proc/self/fd")
        assumeTrue(fds.isDirectory)
        DatagramSocket(0, InetAddress.getLoopbackAddress()).use { taken ->
            val address = InetSocketAddress(InetAddress.getLoopbackAddress(), taken.localPort)
            fun attempt() {
                assertThrows(java.net.BindException::class.java) {
                    VoiceRelay(address, resolve = { Backend.UnknownPlayer }, counters = VoiceCounters(), log = { _, _ -> })
                }
            }
            repeat(5) { attempt() }
            val before = fds.list()!!.size
            repeat(50) { attempt() }
            val after = fds.list()!!.size
            assertTrue(after - before < 10, "open descriptors went from $before to $after over 50 failed binds")
        }
    }
}
