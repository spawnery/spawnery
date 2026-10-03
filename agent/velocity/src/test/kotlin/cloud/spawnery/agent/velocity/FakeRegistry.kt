package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.Player
import com.velocitypowered.api.proxy.messages.ChannelIdentifier
import com.velocitypowered.api.proxy.messages.PluginMessageEncoder
import com.velocitypowered.api.proxy.server.PingOptions
import com.velocitypowered.api.proxy.server.RegisteredServer
import com.velocitypowered.api.proxy.server.ServerInfo
import com.velocitypowered.api.proxy.server.ServerPing
import java.util.concurrent.CompletableFuture

/**
 * A [ProxyRegistry] backed by an in-memory map, keyed by the lower-cased name
 * like Velocity's `getServer(String)`. [calls] records every
 * [register]/[unregister] in order.
 */
class FakeRegistry : ProxyRegistry {
    private val servers = LinkedHashMap<String, FakeServer>()

    val calls = mutableListOf<Call>()

    /**
     * When set, thrown by [register]: how [ProxyRoleTest] gets a real
     * exception on the `FullSync` path.
     */
    var failRegisterWith: Throwable? = null

    sealed interface Call {
        data class Register(val info: ServerInfo) : Call
        data class Unregister(val info: ServerInfo) : Call
    }

    /**
     * Puts a server into the registry bypassing [ServerDirectory], like a
     * `configOverlay` entry this agent never registered.
     */
    fun seed(info: ServerInfo, players: List<Player> = emptyList()): FakeServer {
        val server = FakeServer(info).apply { this.players = players }
        servers[info.name.lowercase()] = server
        return server
    }

    override fun server(name: String): RegisteredServer? = servers[name.lowercase()]

    override fun register(info: ServerInfo): RegisteredServer {
        failRegisterWith?.let { throw it }
        calls += Call.Register(info)
        val server = FakeServer(info)
        servers[info.name.lowercase()] = server
        return server
    }

    override fun unregister(info: ServerInfo) {
        calls += Call.Unregister(info)
        servers.remove(info.name.lowercase())
    }
}

/**
 * A [RegisteredServer] that only holds a [ServerInfo] and a player list.
 * [ping] and [sendPluginMessage] throw rather than invent behaviour Velocity's
 * real one may not have.
 */
class FakeServer(private val info: ServerInfo) : RegisteredServer {
    var players: List<Player> = emptyList()

    override fun getServerInfo(): ServerInfo = info

    override fun getPlayersConnected(): Collection<Player> = players

    override fun ping(): CompletableFuture<ServerPing> =
        throw UnsupportedOperationException("FakeServer.ping is never called by this agent")

    override fun ping(pingOptions: PingOptions): CompletableFuture<ServerPing> =
        throw UnsupportedOperationException("FakeServer.ping is never called by this agent")

    override fun sendPluginMessage(identifier: ChannelIdentifier, data: ByteArray): Boolean =
        throw UnsupportedOperationException("FakeServer.sendPluginMessage is never called by this agent")

    override fun sendPluginMessage(identifier: ChannelIdentifier, encoder: PluginMessageEncoder): Boolean =
        throw UnsupportedOperationException("FakeServer.sendPluginMessage is never called by this agent")
}
