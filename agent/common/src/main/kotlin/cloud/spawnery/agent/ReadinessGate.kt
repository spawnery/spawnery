package cloud.spawnery.agent

import cloud.spawnery.agent.api.ReadinessHold

/**
 * What a server's readiness waits for.
 *
 * [onOpen] runs once, when the server has finished enabling and no hold is
 * left -- whichever of the two comes second. It runs outside this object's
 * lock, because on Paper it sends on the agent's stream.
 */
class ReadinessGate(private val onOpen: () -> Unit) {
    private val lock = Any()
    private val open = LinkedHashMap<Long, String>()
    private var loaded = false
    private var opened = false
    private var next = 0L

    fun hold(reason: String): ReadinessHold {
        synchronized(lock) {
            // A late hold is not an error worth throwing for: the plugin
            // cannot know it lost the race, and readiness cannot be lowered
            // anyway. It gets a handle that releases nothing.
            if (opened) return ReadinessHold {}
            val key = next++
            open[key] = reason
            return Release(key)
        }
    }

    fun serverLoaded() {
        val fire = synchronized(lock) {
            loaded = true
            openNow()
        }
        if (fire) onOpen()
    }

    fun openReasons(): List<String> = synchronized(lock) { open.values.toList() }

    private fun release(key: Long) {
        val fire = synchronized(lock) {
            if (open.remove(key) == null) return
            openNow()
        }
        if (fire) onOpen()
    }

    private fun openNow(): Boolean {
        if (opened || !loaded || open.isNotEmpty()) return false
        opened = true
        return true
    }

    private inner class Release(private val key: Long) : ReadinessHold {
        override fun close() = release(key)
    }
}
