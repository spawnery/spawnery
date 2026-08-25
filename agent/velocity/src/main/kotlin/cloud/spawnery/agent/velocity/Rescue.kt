package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer
import java.util.UUID
import java.util.concurrent.ConcurrentHashMap

/**
 * Catches a player whose server dropped them, and picks the next one to try.
 *
 * [Drain] covers the orderly case: the operator says a server is going away,
 * players are moved off, then the pod is deleted. This covers the case where
 * nobody got to say anything -- a node lost, an OOM kill, a crash, a pod
 * deleted out from under the reconciler. No `DrainPlayers` ever arrives, so
 * [Drain] never runs, and the player is simply sitting on a socket that has
 * stopped answering.
 *
 * Measured 2026-08-25 on a live cluster, against a `lobby` group with two
 * ready backends: force-deleting the pod under a joined player disconnected
 * them with "Unable to connect to lobby-6g6w" while the second backend --
 * registered, ready, empty -- stood unused beside it. Velocity does have a
 * failover of its own, and it is switched on: `disconnected()` consults
 * `failover-on-unexpected-server-disconnect`, which internal/render leaves at
 * Velocity's `true`. But that failover walks the `try` list, and
 * internal/render deliberately renders `try = []` -- this agent's server list
 * is dynamic and a static try list would name servers that do not exist. So
 * Velocity's own failover searches an empty list, finds nothing, and
 * disconnects. Every time. Docs' design 6.2 calls this redirect a catcher for
 * "whatever fails in the process" of a drain; the measurement says it is more
 * than that, because without a drain it is the only thing there is.
 *
 * ## What actually reaches this, measured against velocity 3.5.1 build 615
 *
 * `ConnectedPlayer.handleConnectionException(server, reason, friendly, safe)`
 * returns without firing the event at all when `safe` is false. The callers:
 *
 *  - `BackendPlaySessionHandler.disconnected()` -- the backend closed the
 *    socket without an exception -- passes `safe = true`, but only after
 *    `isFailoverOnUnexpectedServerDisconnect()`; with that set to false the
 *    player is disconnected and nothing here runs.
 *  - `BackendPlaySessionHandler.exception(cause)` passes
 *    `safe = !(cause instanceof ReadTimeoutException)`.
 *  - a backend Disconnect packet passes `safe = true`.
 *
 * So the one failure this cannot catch is a backend that goes *silent*
 * without closing its socket -- a hard-powered-off node, a partitioned
 * network -- because that surfaces as a `ReadTimeoutException` and Velocity
 * disconnects the player before any plugin sees it. That hole is Velocity's
 * and cannot be closed from here.
 *
 * ## Why there is state
 *
 * [tried] is a loop guard, and the loop is not hypothetical. [Router] picks
 * the *emptiest* candidate, and a server that has just died has no players on
 * it -- so it is the preferred choice. Two dead-but-still-registered backends
 * (one node carrying both, in the seconds before the operator unregisters
 * them) and an exclusion of only the server just left would send the player
 * A -> C -> A -> C, as fast as a refused connect completes, because a failed
 * redirect re-enters `handleConnectionException` with `safe = true` and fires
 * this event again. Velocity answers the same problem the same way for its
 * own try list: `ConnectedPlayer.tryIndex` only ever advances, never
 * revisits. Here the chain is per player and ends at [forget] -- on any
 * successful connection, and when the player leaves.
 */
class Rescue(
    private val router: Router,
    private val log: (String, Throwable?) -> Unit,
) {
    // Concurrent because Velocity delivers these events on netty's event loops
    // and there is one per connection: two players losing the same server at
    // the same moment arrive here on two threads.
    private val tried = ConcurrentHashMap<UUID, MutableSet<String>>()

    /**
     * Where to send [player] after [from] dropped them, or null to leave
     * Velocity's own decision in place.
     *
     * @param stillConnectedElsewhere `KickedFromServerEvent.kickedDuringServerConnect()`,
     *   which despite the name means the player was kicked while connecting
     *   to some *other* server and therefore still has a working one. Read
     *   off the bytecode rather than the javadoc: the event's constructor is
     *   handed `!kickedFromCurrent`, where `kickedFromCurrent` is
     *   `connectedServer == null || connectedServer.getServer().equals(rs)`.
     *   When it is true Velocity's own result is `Notify` -- the player stays
     *   put and gets a message -- and overriding that would move somebody off
     *   a server that is working perfectly well.
     */
    fun target(
        player: UUID,
        from: String,
        stillConnectedElsewhere: Boolean,
        toGroups: List<String>,
    ): RegisteredServer? {
        if (stillConnectedElsewhere) return null

        val chain = tried.computeIfAbsent(player) { ConcurrentHashMap.newKeySet() }
        chain += from

        val target = router.choose(toGroups, excluding = chain)
        if (target == null) {
            // Worth a line every time rather than once per chain: this is a
            // player about to be disconnected, and the groups that were
            // searched are the only evidence of why. The chain is left in
            // place for [forget] to clear, which the disconnect that follows
            // will reach.
            log(
                "spawnery: nothing left in $toGroups to catch '$player' after '$from' " +
                    "dropped them (already tried $chain); the proxy disconnects them",
                null,
            )
        }
        return target
    }

    /**
     * Ends [player]'s rescue chain.
     *
     * Called from two places, for the same reason in both: whatever the chain
     * was avoiding no longer applies. A player who successfully connected
     * somewhere is no longer being bounced, so a *later* failure deserves the
     * full set of candidates again rather than a set narrowed by an incident
     * that is over. A player who left is gone, and the entry would otherwise
     * be the one thing in this class that grows without bound.
     */
    fun forget(player: UUID) {
        tried.remove(player)
    }
}
