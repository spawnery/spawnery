package cloud.spawnery.agent.paper

import java.nio.file.Files
import java.nio.file.Path
import java.nio.file.StandardCopyOption

/** The game server's side of the files spawnery-worldsync leaves in /data/.spawnery-worldsync. */
class WorldSync(
    private val controlDir: Path,
    private val clock: () -> Long = System::currentTimeMillis,
    private val sleep: (Long) -> Unit = Thread::sleep,
) {
    sealed interface Outcome {
        data object Ready : Outcome
        data class Failed(val reason: String) : Outcome
        data object TimedOut : Outcome
    }

    fun awaitReady(timeoutMillis: Long): Outcome = poll(timeoutMillis) {
        val failed = controlDir.resolve(FAILED_FILE)
        when {
            Files.exists(failed) -> Outcome.Failed(Files.readString(failed).trim())
            Files.exists(controlDir.resolve(READY_FILE)) -> Outcome.Ready
            else -> null
        }
    }

    fun requestSnapshot(seq: Long) {
        Files.createDirectories(controlDir)
        val tmp = controlDir.resolve("snapshot.request.tmp")
        Files.writeString(tmp, seq.toString())
        Files.move(
            tmp,
            controlDir.resolve(REQUEST_FILE),
            StandardCopyOption.ATOMIC_MOVE,
            StandardCopyOption.REPLACE_EXISTING,
        )
    }

    fun awaitSnapshot(seq: Long, timeoutMillis: Long): Outcome = poll(timeoutMillis) {
        val done = controlDir.resolve(DONE_FILE)
        if (!Files.exists(done)) return@poll null
        val answer = Files.readString(done).trim()
        when {
            answer == seq.toString() -> Outcome.Ready
            answer.startsWith("failed $seq ") -> Outcome.Failed(answer.removePrefix("failed $seq "))
            else -> null
        }
    }

    /**
     * The node agent answers only sequences above the last it answered, and a
     * restarted container does not reset that, so a new run starts above
     * every number already in the control files.
     */
    fun nextSequence(): Long {
        var highest = 0L
        for (name in listOf(REQUEST_FILE, DONE_FILE)) {
            val file = controlDir.resolve(name)
            if (!Files.exists(file)) continue
            val first = runCatching { Files.readString(file).trim().removePrefix("failed ").trim().split(' ').first() }
                .getOrNull()
            highest = maxOf(highest, first?.toLongOrNull() ?: 0L)
        }
        return highest + 1
    }

    private fun poll(timeoutMillis: Long, check: () -> Outcome?): Outcome {
        val deadline = clock() + timeoutMillis
        while (true) {
            check()?.let { return it }
            if (clock() >= deadline) return Outcome.TimedOut
            sleep(POLL_MILLIS)
        }
    }

    companion object {
        const val POLL_MILLIS = 100L
        const val READY_FILE = "ready"
        const val FAILED_FILE = "failed"
        const val REQUEST_FILE = "snapshot.request"
        const val DONE_FILE = "snapshot.done"
        const val CONTROL_DIR = ".spawnery-worldsync"
        const val ENV_ENABLED = "SPAWNERY_WORLD_SYNC"
        const val ENV_INTERVAL = "SPAWNERY_WORLD_SYNC_INTERVAL"

        private val part = Regex("""(\d+)(h|m|s)""")

        /** Go's time.Duration.String() for whole seconds: "5m0s", "1h0m0s". */
        fun parseInterval(value: String?): Long? {
            if (value.isNullOrBlank()) return null
            var rest: String = value
            var millis = 0L
            while (rest.isNotEmpty()) {
                val m = part.matchAt(rest, 0) ?: return null
                val n = m.groupValues[1].toLong()
                millis += when (m.groupValues[2]) {
                    "h" -> n * 3_600_000
                    "m" -> n * 60_000
                    else -> n * 1_000
                }
                rest = rest.substring(m.value.length)
            }
            return if (millis > 0) millis else null
        }
    }
}
