package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.ProxyServer
import com.velocitypowered.api.proxy.server.RegisteredServer
import com.velocitypowered.api.proxy.server.ServerInfo

/** The three things the agent does to Velocity's server registry. */
interface ProxyRegistry {
    fun server(name: String): RegisteredServer?
    fun register(info: ServerInfo): RegisteredServer
    fun unregister(info: ServerInfo)
}

/**
 * The operator's view of one backend, as `FullSync` and `RegisterServer`
 * carry it. [address] is the raw `host:port`; [ServerDirectory] decides what
 * an unparsable one means.
 */
data class Backend(val name: String, val address: String, val group: String)

class VelocityRegistry(private val proxy: ProxyServer) : ProxyRegistry {
    override fun server(name: String): RegisteredServer? = proxy.getServer(name).orElse(null)

    override fun register(info: ServerInfo): RegisteredServer = proxy.registerServer(info)

    override fun unregister(info: ServerInfo) = proxy.unregisterServer(info)
}
