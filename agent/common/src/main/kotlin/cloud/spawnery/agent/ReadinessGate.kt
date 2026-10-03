package cloud.spawnery.agent

import cloud.spawnery.agent.api.ReadinessHold

/** [onOpen] runs outside the lock, because on Paper it sends on the agent's stream. */
class ReadinessGate(private val onOpen: () -> Unit) {
    private val lock = Any()
    private val open = LinkedHashMap<Long, String>()
    private var loaded = false
    private var opened = false
    private var next = 0L

    fun hold(reason: String): ReadinessHold {
        synchronized(lock) {
            // Not thrown: the plugin cannot know it lost the race, and readiness cannot be lowered.
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
