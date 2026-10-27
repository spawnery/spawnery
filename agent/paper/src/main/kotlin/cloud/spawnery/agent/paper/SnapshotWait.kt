package cloud.spawnery.agent.paper

import java.util.concurrent.CompletableFuture
import java.util.concurrent.CompletionException
import java.util.concurrent.ExecutionException
import java.util.concurrent.TimeUnit
import java.util.concurrent.TimeoutException
import java.util.concurrent.atomic.AtomicInteger

/**
 * Waits for the saves a platform started for a snapshot. Blocks, so never on
 * the global region thread or a region thread: Canvas completes these futures
 * from the region threads.
 */
object SnapshotWait {
    /**
     * The off-thread half of a snapshot: the region saves, then [flush], then
     * [copy] (the node agent's), all by the one [deadline] on [clock].
     */
    fun freeze(
        saves: List<CompletableFuture<*>>,
        deadline: Long,
        clock: () -> Long,
        flush: () -> Unit,
        copy: (timeoutMillis: Long) -> WorldSync.Outcome,
    ): WorldSync.Outcome =
        when (val saved = await(saves, left(deadline, clock))) {
            WorldSync.Outcome.Ready -> {
                flush()
                copy(left(deadline, clock))
            }
            else -> WorldSync.Outcome.Failed("the regions did not save: $saved")
        }

    private fun left(deadline: Long, clock: () -> Long) = maxOf(0L, deadline - clock())

    fun await(saves: List<CompletableFuture<*>>, timeoutMillis: Long): WorldSync.Outcome {
        if (saves.isEmpty()) return WorldSync.Outcome.Ready
        val all = CompletableFuture<Unit>()
        val left = AtomicInteger(saves.size)
        for (save in saves) {
            save.whenComplete { _, error ->
                when {
                    error != null -> all.completeExceptionally(error)
                    left.decrementAndGet() == 0 -> all.complete(Unit)
                }
            }
        }
        return try {
            all.get(timeoutMillis, TimeUnit.MILLISECONDS)
            WorldSync.Outcome.Ready
        } catch (e: ExecutionException) {
            val cause = e.cause.let { if (it is CompletionException) it.cause ?: it else it } ?: e
            WorldSync.Outcome.Failed(cause.message ?: cause.javaClass.name)
        } catch (_: TimeoutException) {
            val refusal = saves.firstNotNullOfOrNull { save -> (save as? RegionSave)?.takeUnless { it.isDone }?.refusal }
            if (refusal == null) {
                WorldSync.Outcome.TimedOut
            } else {
                WorldSync.Outcome.Failed("timed out after $timeoutMillis ms, still refused: $refusal")
            }
        } finally {
            // Stops CanvasPlatform offering a ticket to a region that never takes it.
            saves.forEach { it.cancel(false) }
        }
    }
}
