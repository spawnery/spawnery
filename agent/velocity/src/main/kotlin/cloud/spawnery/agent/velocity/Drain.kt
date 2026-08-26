package cloud.spawnery.agent.velocity

/**
 * Moves players off the servers the operator is draining, onto whatever
 * [Router] picks from that drain's `toGroups`.
 *
 * Two arrivals reach a draining server, and this handles both. [run] takes the
 * players who are already on it when the drain begins. [landed] takes the ones
 * who arrive afterwards -- which is not an edge case but the gap the drain was
 * measured to have.
 *
 * ## Why a late arrival is possible at all
 *
 * The operator deregisters a server in the same decision that starts its
 * drain, so no *new* player is routed there. What that does not stop is a
 * player whose connection to it was already in flight, and such a player is
 * invisible to everything the operator can read. Disassembling velocity
 * 3.5.1 build 615: `VelocityRegisteredServer.addPlayer` is called from exactly
 * one place, `BackendPlaySessionHandler.activated()` -- the backend's *play*
 * phase -- so a player still in the configuration phase counts on neither
 * side. Not in the backend's own player list, and not in the proxy's
 * `getPlayersConnected()` either. [run] alone misses them for the same reason:
 * [PlayerRef.currentServer] is Velocity's `connectedServer`, which such a
 * player does not have yet.
 *
 * They cannot simply be moved early, either. `ConnectionRequestBuilderImpl`
 * answers `CONNECTION_IN_PROGRESS` while `connectionInFlight` is set, so a
 * move issued before the handshake finishes is refused and the player stays.
 * The earliest point a move works is after `setConnectedServer` has cleared
 * that field, which `TransitionSessionHandler` does before it fires
 * `ServerConnectedEvent` -- so [landed], driven from `ServerPostConnectEvent`,
 * is on the near side of it with the transition finished.
 *
 * ## Why this one remembers and [run] alone did not
 *
 * The operator repeats `DrainPlayers` after every periodic `FullSync` --
 * roughly every resync interval, for as long as the server keeps draining --
 * and that repetition used to be the whole memory this needed: [run] re-reads
 * [PlayerRef.currentServer] on every call, so a second identical call moves
 * nobody. Catching an arrival cannot wait for the next repetition, because by
 * then the player is on the draining server and the operator may already have
 * concluded that nobody is. So the set is held here between messages.
 *
 * It is rebuilt from what the operator says rather than aged out on a timer.
 * A resync is a `FullSync` followed by one `DrainPlayers` per draining server
 * (`internal/proxyreg.Fleet.snapshot`), which makes the messages after a
 * `FullSync` a complete statement of what is draining. [resynced] rotates on
 * that boundary: what arrived since the previous `FullSync` becomes current,
 * and a server the operator has stopped naming is gone within one further
 * resync. No clock, and nothing to tune.
 */
class Drain(
    private val players: Players,
    private val router: Router,
    private val log: (String, Throwable?) -> Unit,
) {
    /**
     * The drains in force, and the ones being collected for the next
     * rotation, both keyed by lowercased server name -- [run] has always
     * compared names case-insensitively, and a map has to agree with it.
     *
     * Two fields rather than one because [resynced] replaces the first with
     * the second in a single step, and a reader must never see the moment in
     * between. Guarded by [lock]: [run] and [resynced] arrive on the gRPC
     * callback thread, [landed] on Velocity's event thread.
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
            // Into both: `current` is what [landed] consults and has to take
            // effect at once, and `pending` is what survives the next
            // rotation. Recording only in `pending` would leave a drain
            // uncaught until the FullSync after next.
            current = current + (key to toGroups)
            pending = pending + (key to toGroups)
        }

        val draining = players.all().filter {
            it.currentServer?.equals(fromServer, ignoreCase = true) == true
        }
        if (draining.isEmpty()) return

        // True once a null choice has been logged, so ten players stranded by
        // the same empty toGroups produce one log line, not ten.
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

    /**
     * Moves a player who has just arrived on a draining server, and does
     * nothing for one who has not.
     *
     * Called for every arrival anywhere, so the common path is a map lookup
     * that misses.
     */
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
     * The choice is made per player rather than once per drain: a single
     * cached target would pile every drained player onto whichever server was
     * emptiest at the moment the message arrived, instead of spreading them
     * across [toGroups] the way repeated per-player choices do.
     *
     * Every draining server is excluded, not only the one being left. A
     * target that is itself draining would move the player onto a server the
     * operator is trying to empty, and [landed] would then move them straight
     * off it again -- a bounce with nothing bounding it, since the exclusion
     * of a single server cannot see the second one.
     *
     * [fromServer] is added to that set even though every caller reaches here
     * with it already in `current`: a [resynced] on the other thread between
     * the record and this read would drop it, and the one server that must
     * never be a target is the one being left.
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
