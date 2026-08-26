package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.Player
import com.velocitypowered.api.proxy.ProxyServer
import com.velocitypowered.api.proxy.server.RegisteredServer

/**
 * One connected player, as much of one as the agent needs.
 *
 * Narrowed to exactly what [Router] and [Drain] call, rather than exposing
 * Velocity's own [Player], for the same reason [ProxyRegistry] narrows
 * [ProxyServer]: it is what lets both be tested against a fixture instead of
 * a running proxy.
 */
interface PlayerRef {
    val username: String

    /**
     * The name of the server this player is currently on, or `null` if they
     * have none -- read fresh on every call, never cached, so [Drain] always
     * sees where a player actually is right now rather than where they were
     * when this reference was handed out.
     */
    val currentServer: String?

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
    override val username: String
        get() = player.username

    override val currentServer: String?
        get() = player.currentServer.map { it.server.serverInfo.name }.orElse(null)

    override fun moveTo(target: RegisteredServer) {
        player.createConnectionRequest(target).connectWithIndication()
    }
}
