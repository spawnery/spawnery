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
    private val clock: () -> Long = System::currentTimeMillis,
) : AutoCloseable {
    private val selector = Selector.open()
    private val public = try {
        bound(bind)
    } catch (e: Exception) {
        selector.close()
        throw e
    }
    private val bridges = Bridges(clock)
    private val upstream = HashMap<UUID, DatagramChannel>()
    private val buffer = ByteBuffer.allocateDirect(MAX_DATAGRAM)
    private val warned = HashMap<String, Long>()
    @Volatile private var running = false
    private var thread: Thread? = null

    val localPort: Int get() = (public.localAddress as InetSocketAddress).port

    fun start() {
        public.register(selector, SelectionKey.OP_READ, null)
        running = true
        thread = Thread(::loop, "spawnery-voice").apply { isDaemon = true; start() }
    }

    private fun loop() {
        var swept = clock()
        try {
            while (running) {
                try {
                    selector.select(SWEEP_MILLIS)
                    val keys = selector.selectedKeys().iterator()
                    while (keys.hasNext()) {
                        val key = keys.next()
                        keys.remove()
                        if (!key.isValid) continue
                        val player = key.attachment() as UUID?
                        if (player == null) fromClient() else fromBackend(player, key.channel() as DatagramChannel)
                    }
                    val now = clock()
                    if (now - swept >= SWEEP_MILLIS) {
                        swept = now
                        for (expired in bridges.expired { player, backend -> resolve(player) == Backend.At(backend) }) {
                            close(expired)
                        }
                    }
                } catch (e: Exception) {
                    if (running) warn("spawnery: the voice relay's loop failed; it keeps running", e)
                }
            }
        } finally {
            upstream.values.forEach { runCatching { it.close() } }
            upstream.clear()
        }
    }

    private fun fromClient() {
        var drained = 0
        while (running && drained++ < DRAIN_LIMIT) {
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
            runCatching { upstream.remove(player)?.close() }
            var channel: DatagramChannel? = null
            try {
                channel = DatagramChannel.open()
                channel.connect(backend)
                channel.configureBlocking(false)
                channel.register(selector, SelectionKey.OP_READ, player)
                upstream[player] = channel
            } catch (e: Exception) {
                runCatching { channel?.close() }
                bridges.remove(player)
                counters.inc(VoiceCounters.UNKNOWN_SERVER)
                warn("spawnery: the voice relay cannot open a channel to a backend", e)
                return
            }
        }
        val channel = upstream[player] ?: return
        val written = try {
            channel.write(buffer)
        } catch (_: IOException) {
            // A connected UDP channel reports an earlier ICMP refusal on its next write or read.
            return
        }
        if (written > 0) counters.inc(VoiceCounters.RELAYED)
    }

    private fun fromBackend(player: UUID, channel: DatagramChannel) {
        var drained = 0
        while (running && drained++ < DRAIN_LIMIT) {
            buffer.clear()
            val read = try {
                channel.read(buffer)
            } catch (_: IOException) {
                return
            }
            if (read <= 0) return
            buffer.flip()
            val client = bridges.replyTo(player) ?: return
            bridges.touch(player)
            try {
                public.send(buffer, client)
            } catch (_: IOException) {
            }
        }
    }

    private fun close(player: UUID) {
        bridges.remove(player)
        upstream.remove(player)?.close()
    }

    private fun warn(message: String, e: Throwable) {
        val now = clock()
        val last = warned[message]
        if (last != null && now - last < WARN_MILLIS) return
        warned[message] = now
        log(message, e)
    }

    override fun close() {
        running = false
        selector.wakeup()
        thread?.join(2_000)
        try {
            public.close()
        } finally {
            selector.close()
        }
    }

    private companion object {
        /** SVC's mtu_size tops out at 2048 bytes of Opus plus framing. */
        const val MAX_DATAGRAM = 4096
        const val SWEEP_MILLIS = 1_000L

        /** Per socket and pass, so a flood on one cannot starve the others; the selector is level-triggered. */
        const val DRAIN_LIMIT = 256
        const val WARN_MILLIS = 60_000L

        /** A wildcard bind stays dual-stack; an explicit family would refuse the other one's clients. */
        fun bound(bind: InetSocketAddress): DatagramChannel {
            val channel = open(bind)
            try {
                channel.bind(bind)
                channel.configureBlocking(false)
            } catch (e: Exception) {
                channel.close()
                throw e
            }
            return channel
        }

        fun open(bind: InetSocketAddress): DatagramChannel = when {
            bind.address.isAnyLocalAddress -> DatagramChannel.open()
            bind.address is Inet6Address -> DatagramChannel.open(StandardProtocolFamily.INET6)
            else -> DatagramChannel.open(StandardProtocolFamily.INET)
        }
    }
}
