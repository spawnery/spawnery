package cloud.spawnery.agent.paper

import cloud.spawnery.agent.monotonicMillis
import java.nio.file.Files
import java.nio.file.Path
import java.nio.file.StandardCopyOption

/** The game server's side of the files spawnery-worldsync leaves in /data/.spawnery-worldsync. */
class WorldSync(
    private val controlDir: Path,
    private val clock: () -> Long = ::monotonicMillis,
    private val sleep: (Long) -> Unit = Thread::sleep,
    private val wallClock: () -> Long = System::currentTimeMillis,
    private val move: (from: Path, to: Path) -> Unit = { from, to ->
        Files.move(from, to, StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING)
    },
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

    /**
     * [waitMillis] is how long the caller will wait for the answer. The node
     * agent refuses a request past that deadline, in wall-clock time because
     * it reads the clock in another process. The deadline goes in its own
     * file, written first: node agents that predate it parse the request file
     * as one number and would ignore a request with anything else in it.
     */
    fun requestSnapshot(seq: Long, waitMillis: Long) {
        Files.createDirectories(controlDir)
        replace(DEADLINE_FILE, "$seq ${wallClock() + waitMillis}")
        replace(REQUEST_FILE, seq.toString())
    }

    private fun replace(name: String, content: String) {
        val tmp = controlDir.resolve("$name.tmp")
        Files.writeString(tmp, content)
        move(tmp, controlDir.resolve(name))
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
     * Asks the node agent to sync this world under [key] from now on. The
     * request is only a trigger: the node agent takes the key once the
     * operator's binding of that world names this server. It answers a
     * request while no answer is there, so the last answer goes first.
     */
    fun requestKey(key: String) {
        Files.createDirectories(controlDir)
        Files.deleteIfExists(controlDir.resolve(KEY_DONE_FILE))
        replace(KEY_REQUEST_FILE, key)
    }

    fun awaitKey(key: String, timeoutMillis: Long): Outcome = poll(timeoutMillis) {
        val done = controlDir.resolve(KEY_DONE_FILE)
        if (!Files.exists(done)) return@poll null
        val answer = Files.readString(done).trim()
        when {
            answer == key -> Outcome.Ready
            answer.startsWith("failed $key ") -> Outcome.Failed(answer.removePrefix("failed $key "))
            else -> null
        }
    }

    /**
     * The key the node agent took for this world, or null. A container that
     * restarts after its world took a key resumes its snapshots with it.
     */
    fun keyed(): String? {
        val done = controlDir.resolve(KEY_DONE_FILE)
        if (!Files.exists(done)) return null
        val answer = runCatching { Files.readString(done).trim() }.getOrNull() ?: return null
        return answer.takeIf { it.isNotEmpty() && !it.startsWith("failed ") }
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
        const val DEADLINE_FILE = "snapshot.deadline"
        const val KEY_REQUEST_FILE = "key.request"
        const val KEY_DONE_FILE = "key.done"
        const val CONTROL_DIR = ".spawnery-worldsync"
        const val ENV_ENABLED = "SPAWNERY_WORLD_SYNC"

        /** ENV_ENABLED for a member whose world has no key yet: no wait, no snapshots. */
        const val STANDBY = "standby"
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
