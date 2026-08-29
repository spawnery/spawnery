package cloud.spawnery.agent

import java.util.UUID
import java.util.concurrent.ConcurrentHashMap

/**
 * Who has turned the feed off, for as long as they stay connected.
 *
 * **Opt-out and not opt-in.** Somebody granted `spawnery.cloud.events` was
 * granted it in order to see them; making them ask again every session would
 * mean the permission does nothing on its own, which is not what granting a
 * permission looks like anywhere else on a server.
 *
 * It holds the *off* set rather than the *on* set for the same reason: an
 * empty state has to mean "everybody who may see this, sees this", and a set
 * of opted-in players would start empty and show nobody anything.
 *
 * Nothing removes a player who logs out. The set is bounded by how many
 * distinct administrators type the command in one agent's lifetime, which is
 * small, and a rejoin re-arms the feed anyway because [wants] is only ever
 * asked about players who are online.
 *
 * Concurrent because the command writes it from a platform thread and the feed
 * reads it from a timer, which on both platforms are two different threads.
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
