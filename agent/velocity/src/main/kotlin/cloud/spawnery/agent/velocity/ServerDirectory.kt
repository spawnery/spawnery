package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer
import com.velocitypowered.api.proxy.server.ServerInfo
import java.net.InetSocketAddress

/**
 * Mirrors the operator's server list into Velocity's [ProxyRegistry].
 *
 * The operator is the source of truth; this class only ever undoes what it
 * itself did. [names] tracks exactly the backends this directory registered,
 * and [apply] diffs the next `FullSync` against that set rather than against
 * anything actually in the proxy -- a `configOverlay` entry in
 * `velocity.toml`, or any other server this agent never touched, is
 * invisible to it by construction and therefore never at risk of being
 * unregistered.
 *
 * @param log where a skipped or malformed entry is reported. A callback
 *   rather than a logger for the same reason [ReadyGate] takes one: this runs
 *   on a gRPC callback thread, with no proxy-supplied logger in reach.
 */
class ServerDirectory(
    private val registry: ProxyRegistry,
    private val log: (String, Throwable?) -> Unit,
) {
    // Keyed by the lower-cased name, matching registry.server's own
    // case-insensitive lookup -- see upsert(). The value keeps the Backend as
    // last applied, address included, so upsert can tell "same address" from
    // "changed address" without going back to the registry for it.
    private val backends = LinkedHashMap<String, Backend>()

    /**
     * Applies a full sync: registers every backend in [servers], unregisters
     * every backend this directory previously registered but that
     * [servers] no longer carries, and leaves everything else alone.
     *
     * An entry with an address this directory cannot parse is skipped and
     * logged rather than applied, and rather than resurrecting an existing
     * registration if that name changed from a good address to a bad one: a
     * full sync's job is to make the proxy match what was successfully
     * parsed, not to remember a discarded value across syncs.
     */
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

    /** Registers or updates a single backend, as `RegisterServer` carries it. */
    fun add(backend: Backend) {
        upsert(backend)
    }

    /**
     * Unregisters a single backend, as `UnregisterServer` carries it. A name
     * this directory never registered is ignored -- not looked up in the
     * registry at all -- so this can never remove a server some other means
     * put there.
     */
    fun remove(name: String) {
        val key = name.lowercase()
        if (key !in backends) return
        unregisterTracked(key)
    }

    /**
     * The servers of [group] that are still actually registered, resolved
     * fresh through [ProxyRegistry.server] rather than returned from a cached
     * handle. A server this directory registered but that Velocity itself
     * later dropped is therefore never handed to a caller -- the router, from
     * task 6 -- as a live target.
     */
    fun inGroup(group: String): List<RegisteredServer> =
        backends.values
            .filter { it.group == group }
            .mapNotNull { registry.server(it.name) }

    /** The names this directory has registered, in their original case. */
    fun names(): Set<String> = backends.values.mapTo(mutableSetOf()) { it.name }

    /**
     * The one registration path. Absent from the registry -> register.
     * Present with the address unchanged -> nothing. Present with a
     * different address -> unregister the old [ServerInfo], then register
     * the new one.
     *
     * Consulting [ProxyRegistry.server] before every call is what makes this
     * independent of what Velocity's own `registerServer` does for a name
     * that already exists -- deliberately unmeasured, because this ordering
     * means the answer never has to matter.
     *
     * Returns whether [backend] was applied, so [apply] can tell a
     * successfully parsed entry from a skipped one.
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

    /** Unregisters whatever the registry currently has for [key], and forgets it. */
    private fun unregisterTracked(key: String) {
        registry.server(key)?.let { registry.unregister(it.serverInfo) }
        backends.remove(key)
    }

    private companion object {
        /**
         * Splits on the *last* colon, not the first: an IPv6 address such as
         * `fd00::1:25565` carries colons of its own, and a dual-stack
         * cluster's pod IP is exactly the kind of address this has to
         * survive.
         *
         * A missing port, an empty host, an unparsable port, and a port
         * outside 1-65535 are all `null` -- a skip, not an exception, because
         * this runs on the gRPC callback thread the whole session lives on.
         */
        fun parseAddress(address: String): InetSocketAddress? {
            val split = address.lastIndexOf(':')
            if (split < 0) return null

            val host = address.substring(0, split)
            if (host.isEmpty()) return null

            val port = address.substring(split + 1).toIntOrNull() ?: return null
            if (port !in 1..65535) return null

            // createUnresolved, never the InetSocketAddress(host, port)
            // constructor: the ordinary constructor resolves the hostname by
            // blocking on DNS, on the calling thread -- which here is a gRPC
            // callback thread, not somewhere a blocking call belongs.
            return InetSocketAddress.createUnresolved(host, port)
        }
    }
}
