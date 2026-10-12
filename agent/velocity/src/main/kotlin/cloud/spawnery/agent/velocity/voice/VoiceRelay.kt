package cloud.spawnery.agent.velocity.voice

import java.io.IOException
import java.net.Inet6Address
import java.net.InetSocketAddress
import java.net.StandardProtocolFamily
import java.nio.ByteBuffer
import java.nio.channels.DatagramChannel
import java.nio.channels.SelectionKey
import java.nio.channels.Selector
import java.util.UUID

sealed interface Backend {
    data class At(val address: InetSocketAddress) : Backend
    data object UnknownPlayer : Backend
    data object UnknownServer : Backend
}

/**
 * One selector thread owns every socket and [Bridges]; nothing else touches
 * them. Per player, one connected channel towards the backend, so the backend
 * sees one stable source for each player, as SVC's own proxy arranges it.
 */
class VoiceRelay(
    bind: InetSocketAddress,
    private val resolve: (UUID) -> Backend,
    private val counters: VoiceCounters,
    private val log: (String, Throwable?) -> Unit,
    clock: () -> Long = System::currentTimeMillis,
) : AutoCloseable {
    private val selector = Selector.open()
    private val public = DatagramChannel
        .open(if (bind.address is Inet6Address) StandardProtocolFamily.INET6 else StandardProtocolFamily.INET)
        .bind(bind).also { it.configureBlocking(false) }
    private val bridges = Bridges(clock)
    private val upstream = HashMap<UUID, DatagramChannel>()
    private val buffer = ByteBuffer.allocateDirect(MAX_DATAGRAM)
    @Volatile private var running = false
    private var thread: Thread? = null

    val localPort: Int get() = (public.localAddress as InetSocketAddress).port

    fun start() {
        public.register(selector, SelectionKey.OP_READ, null)
        running = true
        thread = Thread(::loop, "spawnery-voice").apply { isDaemon = true; start() }
    }

    private fun loop() {
        var failing = false
        while (running) {
            try {
                selector.select(SWEEP_MILLIS)
                val keys = selector.selectedKeys().iterator()
                while (keys.hasNext()) {
                    val key = keys.next()
                    keys.remove()
                    val player = key.attachment() as UUID?
                    if (player == null) fromClient() else fromBackend(player, key.channel() as DatagramChannel)
                }
                for (expired in bridges.expired()) close(expired)
                failing = false
            } catch (e: Exception) {
                if (running && !failing) log("spawnery: the voice relay's loop failed; it keeps running, and stays quiet until a pass succeeds", e)
                failing = true
            }
        }
    }

    private fun fromClient() {
        while (true) {
            buffer.clear()
            val from = public.receive(buffer) as InetSocketAddress? ?: return
            buffer.flip()
            val player = frameUuid(buffer)
            when {
                player == null -> counters.inc(VoiceCounters.NOT_SVC)
                player == VoiceWire.PING_UUID -> counters.inc(VoiceCounters.PING)
                else -> forward(player, from)
            }
        }
    }

    private fun forward(player: UUID, from: InetSocketAddress) {
        val backend = when (val b = resolve(player)) {
            is Backend.At -> b.address
            Backend.UnknownPlayer -> return counters.inc(VoiceCounters.UNKNOWN_PLAYER)
            Backend.UnknownServer -> return counters.inc(VoiceCounters.UNKNOWN_SERVER)
        }
        if (bridges.route(player, from, backend) is Route.Open) {
            upstream.remove(player)?.close()
            try {
                val channel = DatagramChannel.open().connect(backend).also { it.configureBlocking(false) }
                upstream[player] = channel
                channel.register(selector, SelectionKey.OP_READ, player)
            } catch (e: IOException) {
                close(player)
                throw e
            }
        }
        try {
            upstream[player]?.write(buffer)
        } catch (_: IOException) {
            // A connected UDP channel reports an earlier ICMP refusal on its next write or read.
            return
        }
        counters.inc(VoiceCounters.RELAYED)
    }

    private fun fromBackend(player: UUID, channel: DatagramChannel) {
        while (true) {
            buffer.clear()
            val read = runCatching { channel.read(buffer) }.getOrElse { -1 }
            if (read <= 0) return
            buffer.flip()
            val client = bridges.replyTo(player) ?: return
            bridges.touch(player)
            public.send(buffer, client)
        }
    }

    private fun close(player: UUID) {
        bridges.remove(player)
        upstream.remove(player)?.close()
    }

    override fun close() {
        running = false
        selector.wakeup()
        thread?.join(2_000)
        upstream.values.forEach { it.close() }
        public.close()
        selector.close()
    }

    private companion object {
        /** SVC's mtu_size tops out at 2048 bytes of Opus plus framing. */
        const val MAX_DATAGRAM = 4096
        const val SWEEP_MILLIS = 1_000L
    }
}
