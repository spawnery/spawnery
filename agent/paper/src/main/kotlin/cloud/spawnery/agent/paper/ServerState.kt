package cloud.spawnery.agent.paper

import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong

/**
 * Written on the main thread, read from gRPC callbacks, which must not call
 * Bukkit.getOnlinePlayers() themselves.
 *
 * Nothing clears [ready]: Hello{ready:false} cannot lower a readiness the
 * operator has already recorded.
 */
class ServerState {
    private val readyFlag = AtomicBoolean(false)
    private val playerCount = AtomicInteger(0)
    private val slotCount = AtomicInteger(0)

    val ready: Boolean get() = readyFlag.get()
    val players: Int get() = playerCount.get()
    val slots: Int get() = slotCount.get()

    private val playableCount = AtomicInteger(0)
    val playable: Int get() = playableCount.get()

    fun setPlayable(slots: Int) {
        playableCount.set(slots)
    }

    private val tpsBits = AtomicLong(0)
    private val msptBits = AtomicLong(0)

    val tps: Double get() = java.lang.Double.longBitsToDouble(tpsBits.get())
    val mspt: Double get() = java.lang.Double.longBitsToDouble(msptBits.get())

    /** Returns true only for the call that made the transition. */
    fun markReady(): Boolean = readyFlag.compareAndSet(false, true)

    fun sample(players: Int, slots: Int) {
        playerCount.set(players)
        slotCount.set(slots)
    }

    fun sampleTicks(tps: Double, mspt: Double) {
        tpsBits.set(java.lang.Double.doubleToLongBits(tps))
        msptBits.set(java.lang.Double.doubleToLongBits(mspt))
    }
}
