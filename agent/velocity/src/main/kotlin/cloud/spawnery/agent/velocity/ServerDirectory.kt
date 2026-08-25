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
 * **Every public method is `@Synchronized`, because the writers and the reader
 * are on different threads.** Which thread each caller is on:
 *
 *  - [apply], [add] and [remove] are called from [ProxyRole.onMessage], which
 *    `SessionLoop` drives from a gRPC callback thread. Not *one* such thread,
 *    either: `SessionLoop`'s make-before-break renewal keeps the outgoing and
 *    the incoming stream alive at once, on two separate `ManagedChannel`s, so
 *    two callback threads can be inside these methods while the displaced
 *    stream drains.
 *  - [inGroup] is called from [Router.choose], and `AgentPlugin`'s
 *    `onChooseInitialServer` reaches that from **Velocity's event thread**,
 *    every time a player joins. (`Drain` reaches it from the gRPC side, which
 *    is why the reader cannot simply be declared "the event thread" either.)
 *  - [names] is only read from tests today, and iterates the same map.
 *
 * So a periodic `FullSync` -- the operator sends one roughly every 30 seconds
 * -- landing while a player joins is two threads on one `LinkedHashMap`, and
 * without the locking below the join path throws
 * `ConcurrentModificationException` out of `inGroup`'s iteration. The write
 * side is wrapped in `runCatching` by [ProxyRole.onMessage]; the read side has
 * no guard anywhere between here and Velocity's event loop.
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
    //
    // A plain LinkedHashMap rather than a concurrent map: insertion order is
    // what makes a full sync's registrations deterministic, and the lock the
    // methods below take is what makes the map safe. A ConcurrentHashMap would
    // give up the order and still not make the read-modify-write in upsert
    // atomic.
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
     * Registers or updates a single backend, as `RegisterServer` carries it.
     *
     * One backend, and nothing about the others: an incremental register says
     * a server appeared, never that it is now the whole list. Implementing
     * this as `apply(listOf(backend))` -- which passes every test that starts
     * from an empty directory -- would unregister every other backend on every
     * `RegisterServer` the operator sends, and the next periodic `FullSync`
     * would put them back about thirty seconds later.
     */
    @Synchronized
    fun add(backend: Backend) {
        upsert(backend)
    }

    /**
     * Unregisters a single backend, as `UnregisterServer` carries it. A name
     * this directory never registered is ignored -- not looked up in the
     * registry at all -- so this can never remove a server some other means
     * put there. One backend here too, for the same reason as [add].
     */
    @Synchronized
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
    @Synchronized
    fun inGroup(group: String): List<RegisteredServer> =
        backends.values
            .filter { it.group == group }
            .mapNotNull { registry.server(it.name) }

    /** The names this directory has registered, in their original case. */
    @Synchronized
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

    /**
     * Unregisters whatever the registry currently has for [key], and forgets
     * it.
     *
     * Logged, and it is the only mutation here that is. Not because a removal
     * matters more than a registration -- because it is rarer and it is what
     * somebody investigating looks for. Every `FullSync` upserts every backend
     * the operator knows about, roughly every thirty seconds, so logging those
     * would bury this line under a line per server per sync; a backend
     * *leaving* happens when the operator scales down, drains a node or loses
     * a server, and "where did that backend go" is a question with no other
     * answer on the proxy side.
     *
     * docs/known-issues.md carried this as "logs nothing at the point of
     * removal, unlike every other mutation in the same class". The comparison
     * was wrong -- no mutation here logged, only the malformed-address skip --
     * but the gap it named was real.
     */
    private fun unregisterTracked(key: String) {
        registry.server(key)?.let { registry.unregister(it.serverInfo) }
        backends.remove(key)
        log("spawnery: unregistered backend '$key'", null)
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
         * this is reached only from the write side, on a gRPC callback thread,
         * where a throw costs the stream. See [ProxyRole.onMessage].
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
