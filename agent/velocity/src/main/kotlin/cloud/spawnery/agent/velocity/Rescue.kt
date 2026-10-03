package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer
import java.util.UUID
import java.util.concurrent.ConcurrentHashMap

/**
 * Catches a player whose server dropped them without a drain (a node lost, an
 * OOM kill, a crash), and picks the next one to try.
 *
 * Velocity's own failover walks the `try` list, which internal/render renders
 * empty because the server list is dynamic; without this the player is
 * disconnected.
 *
 * A backend that goes *silent* without closing its socket surfaces as a
 * `ReadTimeoutException`, and Velocity disconnects the player without firing
 * the event, so this cannot catch it. Nor anything when
 * `failover-on-unexpected-server-disconnect` is false.
 *
 * [tried] is a loop guard: [Router] prefers the emptiest server, which a dead
 * one is, so two dead-but-registered backends would bounce a player between
 * them. The chain ends at [forget].
 */
class Rescue(
    private val router: Router,
    private val log: (String, Throwable?) -> Unit,
    private val access: JoinAccess = JoinAccess.OPEN,
) {
    // Velocity delivers these events on one netty event loop per connection.
    private val tried = ConcurrentHashMap<UUID, MutableSet<String>>()

    /**
     * Where to send [player] after [from] dropped them, or null to leave
     * Velocity's own decision in place.
     *
     * @param stillConnectedElsewhere `KickedFromServerEvent.kickedDuringServerConnect()`,
     *   which despite the name means the player was kicked while connecting
     *   to some *other* server and still has a working one.
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

        val target = router.choose(toGroups, excluding = chain) { server, group ->
            access.mayJoin(player, server, group)
        }
        if (target == null) {
            // Logged every time: the player is about to be disconnected.
            log(
                "spawnery: nothing left in $toGroups to catch '$player' after '$from' " +
                    "dropped them (already tried $chain); the proxy disconnects them",
                null,
            )
        }
        return target
    }

    /**
     * Ends [player]'s rescue chain: on any successful connection, so a later
     * failure sees every candidate again, and when the player leaves.
     */
    fun forget(player: UUID) {
        tried.remove(player)
    }
}
