package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.Player
import com.velocitypowered.proxy.connection.client.ConnectedPlayer
import com.velocitypowered.api.proxy.ProxyServer
import com.velocitypowered.api.proxy.server.RegisteredServer
import java.util.UUID

/** One connected player, narrowed so [Router] and [Drain] test without a proxy. */
interface PlayerRef {
    /** What everything upstream keys on; a username can be changed and reused. */
    val uuid: UUID

    val username: String

    /** Read fresh on every call, never cached. */
    val currentServer: String?

    /**
     * The server this player is on **or on their way to**. A player
     * mid-handshake has no [currentServer], and the backend has not counted
     * them yet either.
     */
    val attachedServer: String?

    /** Starts moving this player to [target], without waiting for the result. */
    fun moveTo(target: RegisteredServer)
}

interface Players {
    fun all(): List<PlayerRef>

    fun count(): Int
}

class VelocityPlayers(private val proxy: ProxyServer) : Players {
    override fun all(): List<PlayerRef> = proxy.allPlayers.map(::VelocityPlayer)

    override fun count(): Int = proxy.playerCount
}

/**
 * [moveTo] does not wait on the future `connectWithIndication()` returns:
 * [Drain] runs on a gRPC callback thread, and a failed move shows up on the
 * next [Drain.run] anyway.
 */
internal class VelocityPlayer(private val player: Player) : PlayerRef {
    override val uuid: UUID
        get() = player.uniqueId

    override val username: String
        get() = player.username

    override val currentServer: String?
        get() = player.currentServer.map { it.server.serverInfo.name }.orElse(null)

    /**
     * `ConnectedPlayer.getConnectionInFlightOrConnectedServer()`, through a
     * cast to Velocity's implementation class: the API's [Player] has nothing
     * that answers "where is this player heading". `as?` degrades to
     * [currentServer].
     */
    override val attachedServer: String?
        get() {
            val internal = player as? ConnectedPlayer ?: return currentServer
            return internal.connectionInFlightOrConnectedServer?.serverInfo?.name ?: currentServer
        }

    override fun moveTo(target: RegisteredServer) {
        player.createConnectionRequest(target).connectWithIndication()
    }
}
