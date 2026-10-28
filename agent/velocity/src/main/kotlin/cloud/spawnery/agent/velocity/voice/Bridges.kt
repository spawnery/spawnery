package cloud.spawnery.agent.velocity.voice

import java.net.InetAddress
import java.net.InetSocketAddress
import java.util.UUID

sealed interface Route {
    data object Existing : Route
    data class Open(val replaces: Boolean) : Route
}

/** Not thread-safe: owned by the relay's selector thread. */
class Bridges(private val clock: () -> Long) {
    private class State(
        var replyTo: InetSocketAddress,
        val backend: InetSocketAddress,
        var heardFromClient: Long,
        var lastTraffic: Long,
    )

    private val states = HashMap<UUID, State>()

    fun route(
        player: UUID,
        from: InetSocketAddress,
        backend: InetSocketAddress,
        isGameHost: (InetSocketAddress) -> Boolean = { false },
    ): Route {
        val now = clock()
        val state = states[player]
        if (state == null || state.backend != backend) {
            states[player] = State(from, backend, now, now)
            return Route.Open(replaces = state != null)
        }
        if (from == state.replyTo) {
            state.heardFromClient = now
        } else {
            val fromHost = isGameHost(from)
            val currentHost = isGameHost(state.replyTo)
            val takeOver = when {
                fromHost != currentHost -> fromHost || now - state.heardFromClient >= IDLE_MILLIS
                else -> now - state.heardFromClient >= TAKEOVER_MILLIS
            }
            if (takeOver) {
                state.replyTo = from
                state.heardFromClient = now
            }
        }
        state.lastTraffic = now
        return Route.Existing
    }

    fun replyTo(player: UUID): InetSocketAddress? = states[player]?.replyTo

    fun touch(player: UUID) {
        states[player]?.lastTraffic = clock()
    }

    /**
     * A silent bridge whose player still routes to its backend stays: SVC pins
     * the address it authenticated, and a new source port leaves the client
     * validating until its next secret.
     */
    fun expired(stillRoutes: (UUID, InetSocketAddress) -> Boolean): List<UUID> {
        val now = clock()
        return states
            .filter { (player, state) -> now - state.lastTraffic >= IDLE_MILLIS && !stillRoutes(player, state.backend) }
            .keys.toList()
    }

    fun remove(player: UUID) {
        states.remove(player)
    }

    companion object {
        /** The client echoes the backend's keepalive every second. */
        const val TAKEOVER_MILLIS = 3_000L

        /** Above SVC's 10 s timeout, so a bridge never ends under a live connection. */
        const val IDLE_MILLIS = 15_000L
    }
}

/** Equal hosts, with an IPv4-mapped IPv6 address read as the IPv4 address it carries. */
fun sameHost(a: InetAddress?, b: InetAddress?): Boolean {
    if (a == null || b == null) return false
    return hostBytes(a).contentEquals(hostBytes(b))
}

private fun hostBytes(address: InetAddress): ByteArray {
    val bytes = address.address
    val mapped = bytes.size == 16 && (0 until 10).all { bytes[it] == 0.toByte() } &&
        bytes[10] == 0xff.toByte() && bytes[11] == 0xff.toByte()
    return if (mapped) bytes.copyOfRange(12, 16) else bytes
}
