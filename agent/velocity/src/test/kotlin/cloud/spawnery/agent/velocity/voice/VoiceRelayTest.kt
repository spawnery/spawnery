package cloud.spawnery.agent.velocity.voice

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.InetSocketAddress
import java.nio.ByteBuffer
import java.util.UUID
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
        var current = lobby
        VoiceRelay(
            InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { Backend.At(current.localSocketAddress as InetSocketAddress) },
            counters = VoiceCounters(), log = { _, _ -> },
        ).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 2_000
                assertEquals(lobby.localPort, roundTrip(client, relay.localPort, frame(player, "a"))!!.int)
                current = game
                assertEquals(game.localPort, roundTrip(client, relay.localPort, frame(player, "b"))!!.int)
            }
        }
        lobby.close(); game.close()
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
}
