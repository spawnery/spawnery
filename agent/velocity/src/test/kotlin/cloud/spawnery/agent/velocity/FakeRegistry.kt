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
 * A [ProxyRegistry] backed by an in-memory map instead of a running proxy.
 *
 * Keyed by the lower-cased name, matching Velocity's own case-insensitive
 * `getServer(String)` — see [ServerDirectory]'s lookup rule. [calls] records
 * every [register]/[unregister] in the order they happened, which is the only
 * way [ServerDirectoryTest] can assert that a changed address produces an
 * unregister *and then* a register rather than the reverse or a single
 * register.
 */
class FakeRegistry : ProxyRegistry {
    private val servers = LinkedHashMap<String, FakeServer>()

    val calls = mutableListOf<Call>()

    sealed interface Call {
        data class Register(val info: ServerInfo) : Call
        data class Unregister(val info: ServerInfo) : Call
    }

    /**
     * Puts a server directly into the registry, bypassing [ServerDirectory]
     * entirely. This is how a test represents a server this agent never
     * registered — a `configOverlay` entry in `velocity.toml`, for instance —
     * so that a full sync can be shown to leave it alone.
     */
    fun seed(info: ServerInfo, players: List<Player> = emptyList()): FakeServer {
        val server = FakeServer(info).apply { this.players = players }
        servers[info.name.lowercase()] = server
        return server
    }

    override fun server(name: String): RegisteredServer? = servers[name.lowercase()]

    override fun register(info: ServerInfo): RegisteredServer {
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
 * A [RegisteredServer] that does nothing but hold a [ServerInfo] and a player
 * list. Only [getServerInfo], [getPlayersConnected] and the four methods
 * `RegisteredServer` inherits abstract from [com.velocitypowered.api.proxy.messages.ChannelMessageSink]
 * and its own `ping` overloads are abstract on the interface — everything
 * `Audience` and `Pointered` contribute already has a default implementation,
 * measured 2026-08-11 against velocity 3.5.1 build 615 with `javap -p`.
 *
 * [ping] and [sendPluginMessage] throw [UnsupportedOperationException] rather
 * than returning a plausible value. [ServerDirectory] never calls either one;
 * a fake that answered them anyway would let some later task's test pass
 * against this double's made-up behaviour instead of against Velocity's real
 * one.
 *
 * [players] is a mutable `var` rather than fixed at construction, because
 * task 6's router is driven by player counts and needs to change them mid-test
 * without replacing the server.
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
