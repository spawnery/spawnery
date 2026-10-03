package cloud.spawnery.agent.paper

import java.util.UUID
import java.util.concurrent.ConcurrentHashMap

/** The close event may arrive off the main thread; [ttlMillis] covers a release that never comes. */
class PendingSeats(private val clock: () -> Long, private val ttlMillis: Long = 300_000) {
    private val admitted = ConcurrentHashMap<UUID, Long>()

    fun admit(player: UUID) {
        admitted[player] = clock()
    }

    fun release(player: UUID) {
        admitted.remove(player)
    }

    fun count(): Int {
        val now = clock()
        admitted.entries.removeIf { now - it.value > ttlMillis }
        return admitted.size
    }
}
