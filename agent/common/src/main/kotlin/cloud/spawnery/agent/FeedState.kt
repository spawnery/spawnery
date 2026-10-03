package cloud.spawnery.agent

import java.util.UUID
import java.util.concurrent.ConcurrentHashMap

/**
 * Opt-out, because holding the permission is meant to be enough to see the feed.
 *
 * Nothing removes a player who logs out; the set is bounded by the
 * administrators who type the command.
 */
class FeedState {
    private val off = ConcurrentHashMap.newKeySet<UUID>()

    fun optOut(player: UUID) {
        off += player
    }

    fun optIn(player: UUID) {
        off -= player
    }

    fun wants(player: UUID): Boolean = player !in off
}
