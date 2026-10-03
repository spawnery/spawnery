package cloud.spawnery.agent.velocity

/**
 * Moves players off the servers the operator is draining, onto whatever
 * [Router] picks from that drain's `toGroups`.
 *
 * [run] takes the players already on the server when the drain begins;
 * [landed] takes the ones who arrive afterwards. A player whose connection was
 * in flight when the server was deregistered is counted by neither the backend
 * nor the proxy until the backend's play phase, and a move issued before the
 * handshake finishes is refused (`CONNECTION_IN_PROGRESS`), so they are caught
 * on `ServerPostConnectEvent`.
 *
 * The drain set is rebuilt from what the operator says rather than aged out:
 * a resync is a `FullSync` followed by one `DrainPlayers` per draining server,
 * and [resynced] rotates on that boundary.
 */
class Drain(
    private val players: Players,
    private val router: Router,
    private val log: (String, Throwable?) -> Unit,
) {
    /**
     * Keyed by lowercased server name. Two fields so [resynced] can replace one
     * with the other in a single step. Guarded by [lock]: [run] and [resynced]
     * arrive on the gRPC callback thread, [landed] on Velocity's event thread.
     */
    private var current: Map<String, List<String>> = emptyMap()
    private var pending: Map<String, List<String>> = emptyMap()
    private val lock = Any()

    /**
     * Moves everyone who is on [fromServer] now, and records the drain so
     * [landed] can catch whoever arrives next.
     */
    fun run(fromServer: String, toGroups: List<String>) {
        val key = fromServer.lowercase()
        synchronized(lock) {
            // Into both: `current` takes effect at once, `pending` survives
            // the next rotation.
            current = current + (key to toGroups)
            pending = pending + (key to toGroups)
        }

        val draining = players.all().filter {
            it.currentServer?.equals(fromServer, ignoreCase = true) == true
        }
        if (draining.isEmpty()) return

        var loggedNoTarget = false
        for (player in draining) {
            if (!move(player, fromServer, toGroups) && !loggedNoTarget) {
                log(
                    "spawnery: no target available in $toGroups to drain '$fromServer'; " +
                        "${draining.size} player(s) left in place",
                    null,
                )
                loggedNoTarget = true
            }
        }
    }

    /**
     * Rotates the drain set on a `FullSync`, which is the operator's complete
     * restatement of what this proxy should believe.
     */
    fun resynced() {
        synchronized(lock) {
            current = pending
            pending = emptyMap()
        }
    }

    /** Called for every arrival anywhere, so the common path is a missed lookup. */
    fun landed(player: PlayerRef) {
        val server = player.currentServer ?: return
        val toGroups = synchronized(lock) { current[server.lowercase()] } ?: return
        if (!move(player, server, toGroups)) {
            log(
                "spawnery: '${player.username}' arrived on draining server '$server' and " +
                    "no target is available in $toGroups; left in place",
                null,
            )
        }
    }

    /**
     * Starts one move, and reports whether there was anywhere to send them.
     *
     * Chosen per player, so drained players spread across [toGroups]. Every
     * draining server is excluded, or [landed] would bounce the player on.
     * [fromServer] is added explicitly because a concurrent [resynced] may
     * have dropped it from `current`.
     */
    private fun move(player: PlayerRef, fromServer: String, toGroups: List<String>): Boolean {
        val excluded = synchronized(lock) { current.keys + fromServer.lowercase() }
        val target = router.choose(toGroups, excluding = excluded) ?: return false
        try {
            player.moveTo(target)
        } catch (e: Exception) {
            log("spawnery: failed to move '${player.username}' off draining server '$fromServer'", e)
        }
        return true
    }
}
