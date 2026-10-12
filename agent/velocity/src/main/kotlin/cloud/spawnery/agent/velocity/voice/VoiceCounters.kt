package cloud.spawnery.agent.velocity.voice

import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong

class VoiceCounters {
    private val counts = ConcurrentHashMap<String, AtomicLong>()

    fun inc(reason: String) {
        counts.computeIfAbsent(reason) { AtomicLong() }.incrementAndGet()
    }

    fun totals(): Map<String, Long> = counts.mapValues { it.value.get() }

    companion object {
        const val RELAYED = "relayed"
        const val NOT_SVC = "not_svc"
        const val UNKNOWN_PLAYER = "unknown_player"
        const val UNKNOWN_SERVER = "unknown_server"
        const val UNKNOWN_VERSION = "unknown_version"
        const val PING = "ping"
    }
}
