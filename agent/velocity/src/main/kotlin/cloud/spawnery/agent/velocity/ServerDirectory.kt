package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer
import com.velocitypowered.api.proxy.server.ServerInfo
import java.net.InetSocketAddress

/**
 * Mirrors the operator's server list into Velocity's [ProxyRegistry].
 *
 * This class only ever undoes what it itself did: [apply] diffs the next
 * `FullSync` against the backends this directory registered, so a server it
 * never touched (a `configOverlay` entry, say) is never unregistered.
 *
 * Every public method is `@Synchronized`: the writers run on gRPC callback
 * threads (two during a renewal), and [inGroup] is reached from Velocity's
 * event thread on every join.
 */
class ServerDirectory(
    private val registry: ProxyRegistry,
    private val log: (String, Throwable?) -> Unit,
) {
    // Keyed by the lower-cased name, matching registry.server's
    // case-insensitive lookup. Insertion-ordered so a full sync registers
    // deterministically.
    private val backends = LinkedHashMap<String, Backend>()

    /**
     * Applies a full sync: registers every backend in [servers], unregisters
     * every backend this directory previously registered but that
     * [servers] no longer carries, and leaves everything else alone.
     *
     * An entry with an unparsable address is skipped and logged, and an
     * existing registration under that name is unregistered.
     */
    @Synchronized
    fun apply(servers: List<Backend>) {
        val carried = mutableSetOf<String>()
        for (backend in servers) {
            if (upsert(backend)) carried += backend.name.lowercase()
        }

        val stale = backends.keys - carried
        for (name in stale) {
            unregisterTracked(name)
        }
    }

    /**
     * One backend, and nothing about the others: an incremental register never
     * says it is the whole list, so this is not `apply(listOf(backend))`.
     */
    @Synchronized
    fun add(backend: Backend) {
        upsert(backend)
    }

    /**
     * A name this directory never registered is ignored, so this never
     * removes a server some other means put there.
     */
    @Synchronized
    fun remove(name: String) {
        val key = name.lowercase()
        if (key !in backends) return
        unregisterTracked(key)
    }

    /**
     * Resolved fresh through [ProxyRegistry.server], so a server Velocity
     * itself dropped is never handed out as a live target.
     */
    @Synchronized
    fun inGroup(group: String): List<RegisteredServer> =
        backends.values
            .filter { it.group == group }
            .mapNotNull { registry.server(it.name) }

    @Synchronized
    fun names(): Set<String> = backends.values.mapTo(mutableSetOf()) { it.name }

    /**
     * Absent from the registry -> register. Present with the address
     * unchanged -> nothing. Present with a different address -> unregister
     * the old [ServerInfo], then register the new one. Returns whether
     * [backend] was applied.
     */
    private fun upsert(backend: Backend): Boolean {
        val key = backend.name.lowercase()
        val address = parseAddress(backend.address)
        if (address == null) {
            log("spawnery: skipping server '${backend.name}', address '${backend.address}' is not a valid host:port", null)
            return false
        }

        val info = ServerInfo(backend.name, address)
        when (val existing = registry.server(key)) {
            null -> registry.register(info)
            else -> if (existing.serverInfo != info) {
                registry.unregister(existing.serverInfo)
                registry.register(info)
            }
        }

        backends[key] = backend
        return true
    }

    /**
     * The only mutation that logs: registrations repeat on every `FullSync`,
     * while a backend leaving has no other trace on the proxy side.
     */
    private fun unregisterTracked(key: String) {
        registry.server(key)?.let { registry.unregister(it.serverInfo) }
        backends.remove(key)
        log("spawnery: unregistered backend '$key'", null)
    }

    private companion object {
        /**
         * Splits on the *last* colon, for IPv6 addresses such as
         * `fd00::1:25565`. Anything malformed is `null`, not an exception:
         * a throw on the gRPC callback thread costs the stream.
         */
        fun parseAddress(address: String): InetSocketAddress? {
            val split = address.lastIndexOf(':')
            if (split < 0) return null

            val host = address.substring(0, split)
            if (host.isEmpty()) return null

            val port = address.substring(split + 1).toIntOrNull() ?: return null
            if (port !in 1..65535) return null

            // createUnresolved: the ordinary constructor blocks on DNS.
            return InetSocketAddress.createUnresolved(host, port)
        }
    }
}
