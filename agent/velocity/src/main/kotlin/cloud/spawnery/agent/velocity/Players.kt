package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.Player
import com.velocitypowered.proxy.connection.client.ConnectedPlayer
import com.velocitypowered.api.proxy.ProxyServer
import com.velocitypowered.api.proxy.server.RegisteredServer
import java.util.UUID

/**
 * One connected player, as much of one as the agent needs.
 *
 * Narrowed to exactly what [Router] and [Drain] call, rather than exposing
 * Velocity's own [Player], for the same reason [ProxyRegistry] narrows
 * [ProxyServer]: it is what lets both be tested against a fixture instead of
 * a running proxy.
 */
interface PlayerRef {
    /**
     * This player's Minecraft UUID.
     *
     * The operator has no other source for it. `PlayerJoinedServer` carries a
     * username and the operator's handler discards it -- its own comment says
     * nothing consumes it -- and the registry keeps counts. So until 7b-2
     * nothing upstream could name a person, which is why the plugin API's
     * player list shipped in 7b-1 with no way to be answered.
     *
     * A UUID and not the username, because a name can be changed and reused
     * and this is what everything upstream keys on.
     */
    val uuid: UUID

    val username: String

    /**
     * The name of the server this player is currently on, or `null` if they
     * have none -- read fresh on every call, never cached, so [Drain] always
     * sees where a player actually is right now rather than where they were
     * when this reference was handed out.
     */
    val currentServer: String?

    /**
     * The server this player is on **or on their way to**, which is a
     * different question from [currentServer] and the whole reason the
     * operator's drain could delete a pod under somebody.
     *
     * A player who is mid-handshake toward a backend has no `currentServer`:
     * Velocity sets `connectedServer` only once the transition completes.
     * They are equally invisible to the backend, which counts a player only
     * in its play phase. So neither side of the pair the operator reads knows
     * about them, and this is the one thing that does.
     */
    val attachedServer: String?

    /** Starts moving this player to [target]. See [VelocityPlayers] for what "starts" means. */
    fun moveTo(target: RegisteredServer)
}

/** The players this proxy is serving. */
interface Players {
    fun all(): List<PlayerRef>

    fun count(): Int
}

/**
 * The adapter between [Players] and Velocity's real [ProxyServer].
 *
 * Three lines and nothing else, for the same reason [VelocityRegistry] is:
 * every decision -- which server to pick, who counts as "on the draining
 * server" -- belongs to [Router] and [Drain], which is why this class has no
 * test of its own. What would be under test is Velocity's behaviour, not this
 * agent's.
 */
class VelocityPlayers(private val proxy: ProxyServer) : Players {
    override fun all(): List<PlayerRef> = proxy.allPlayers.map(::VelocityPlayer)

    override fun count(): Int = proxy.playerCount
}

/**
 * [moveTo] calls [Player.createConnectionRequest]`(target).connectWithIndication()`
 * and does not wait on the [java.util.concurrent.CompletableFuture] it
 * returns. [Drain] runs on a gRPC callback thread reacting to a
 * `DrainPlayers` message; blocking that thread on a network round trip to a
 * backend server is not a cost this agent can pay, and nothing here needs the
 * result -- a player who fails to connect is still on their old server (or
 * disconnected), and either way shows up again, or doesn't, the next time
 * [Drain.run] reads [currentServer].
 *
 * Internal rather than private because one arrival has to be wrapped on its
 * own: [AgentPlugin] hands a single `ServerPostConnectEvent`'s player to
 * [Drain.landed], and going through [VelocityPlayers.all] to find them again
 * would be a scan of the whole proxy to rediscover the one player the event
 * already named.
 */
internal class VelocityPlayer(private val player: Player) : PlayerRef {
    override val uuid: UUID
        get() = player.uniqueId

    override val username: String
        get() = player.username

    override val currentServer: String?
        get() = player.currentServer.map { it.server.serverInfo.name }.orElse(null)

    /**
     * `ConnectedPlayer.getConnectionInFlightOrConnectedServer()`, which is
     * literally `connectionInFlight ?: connectedServer` -- read off the
     * disassembly of velocity 3.5.1 build 615 rather than from a document.
     *
     * Reached through a cast to Velocity's own implementation class, because
     * the API's [Player] exposes `getCurrentServer` and nothing that answers
     * "where is this player heading". The cast is safe in the way that
     * matters: `ConnectedPlayer` is the only implementation a running proxy
     * has, the whole velocity jar is on this module's compile classpath
     * (`compileOnly(velocityJar)`, no exclude filter), and `as?` degrades to
     * [currentServer] rather than throwing if a future Velocity ever changes
     * that. A version that renames the class breaks the build against the
     * pinned jar, which is the failure worth having: loud, and before the
     * image is made.
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
