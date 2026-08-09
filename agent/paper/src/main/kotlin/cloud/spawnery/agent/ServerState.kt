package cloud.spawnery.agent

import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger

/**
 * The two facts the agent reports, held where both the Bukkit main thread and
 * the network thread can reach them.
 *
 * The main thread writes through [sample] and [markReady]; the network side
 * only reads. No Bukkit call happens from a gRPC callback, because
 * Bukkit.getOnlinePlayers() is not thread-safe.
 *
 * There is deliberately no way to clear [ready]. Hello{ready:false} cannot
 * lower a readiness the operator's registry has already recorded (see
 * docs/known-issues.md), so representing that state here would only invite
 * code that tries to express it.
 */
class ServerState {
    private val readyFlag = AtomicBoolean(false)
    private val playerCount = AtomicInteger(0)
    private val slotCount = AtomicInteger(0)

    val ready: Boolean get() = readyFlag.get()
    val players: Int get() = playerCount.get()
    val slots: Int get() = slotCount.get()

    /** Returns true only for the call that made the transition. */
    fun markReady(): Boolean = readyFlag.compareAndSet(false, true)

    fun sample(players: Int, slots: Int) {
        playerCount.set(players)
        slotCount.set(slots)
    }
}
