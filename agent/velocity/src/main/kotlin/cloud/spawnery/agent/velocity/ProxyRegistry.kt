package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.ProxyServer
import com.velocitypowered.api.proxy.server.RegisteredServer
import com.velocitypowered.api.proxy.server.ServerInfo

/**
 * The three things the agent does to Velocity's server registry.
 *
 * Narrowed to exactly what [ServerDirectory] calls, rather than exposing
 * [ProxyServer] itself, so that every test of the directory's diffing and
 * address-parsing logic runs against [FakeRegistry] instead of a running
 * proxy — the same separation [ProxyEnvironment] and [ReadyGate] already keep
 * from the Velocity API.
 */
interface ProxyRegistry {
    fun server(name: String): RegisteredServer?
    fun register(info: ServerInfo): RegisteredServer
    fun unregister(info: ServerInfo)
}

/**
 * The operator's view of one backend: a `Server` custom resource, as the
 * `FullSync`/`RegisterServer` messages of task 7 will carry it.
 *
 * [address] is the raw `host:port` string rather than a parsed
 * [java.net.InetSocketAddress]: parsing can fail — a malformed value from a
 * user's `configOverlay`, say — and [ServerDirectory] is what decides that a
 * failure is a skip-and-log rather than a thrown exception, on the gRPC
 * callback thread this runs on. Keeping the raw string here keeps that
 * decision in one place.
 */
data class Backend(val name: String, val address: String, val group: String)

/**
 * The adapter between [ProxyRegistry] and Velocity's real [ProxyServer].
 *
 * Three one-line methods and nothing else: every decision — what to register,
 * when to unregister, which address is malformed — belongs to
 * [ServerDirectory], which is why this class has no test of its own. What
 * would be under test is Velocity's behaviour, not this agent's.
 */
class VelocityRegistry(private val proxy: ProxyServer) : ProxyRegistry {
    override fun server(name: String): RegisteredServer? = proxy.getServer(name).orElse(null)

    override fun register(info: ServerInfo): RegisteredServer = proxy.registerServer(info)

    override fun unregister(info: ServerInfo) = proxy.unregisterServer(info)
}
