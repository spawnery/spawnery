package cloud.spawnery.agent.velocity.voice

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

    fun route(player: UUID, from: InetSocketAddress, backend: InetSocketAddress): Route {
        val now = clock()
        val state = states[player]
        if (state == null || state.backend != backend) {
            states[player] = State(from, backend, now, now)
            return Route.Open(replaces = state != null)
        }
        if (from == state.replyTo) {
            state.heardFromClient = now
        } else if (now - state.heardFromClient >= TAKEOVER_MILLIS) {
            state.replyTo = from
            state.heardFromClient = now
        }
        state.lastTraffic = now
        return Route.Existing
    }

    fun replyTo(player: UUID): InetSocketAddress? = states[player]?.replyTo

    fun touch(player: UUID) {
        states[player]?.lastTraffic = clock()
    }

    fun expired(): List<UUID> {
        val now = clock()
        return states.filterValues { now - it.lastTraffic >= IDLE_MILLIS }.keys.toList()
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
