package cloud.spawnery.agent

import java.util.concurrent.CompletableFuture
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.ThreadLocalRandom
import java.util.concurrent.TimeoutException
import java.util.concurrent.atomic.AtomicLong

/**
 * A request is never resent, not even onto a new stream: only the caller knows
 * whether it is safe to repeat.
 *
 * Ids are never reused, so a late answer cannot complete a later request. They
 * start at a random value, so neither can an answer meant for an earlier
 * process in the same pod.
 *
 * @param timeoutMillis the deadline of a request that names none. It bounds a
 *   lost message rather than a slow one, for the operator answers most requests
 *   from memory; a request it works on for longer names its own deadline.
 */
class Requests(
    private val timeoutMillis: Long,
    private val clock: () -> Long = { System.nanoTime() / 1_000_000 },
) {
    private class Pending(val future: CompletableFuture<Any?>, val timeoutMillis: Long, val deadline: Long)

    /** In the lower half, so the counter cannot wrap. */
    private val counter = AtomicLong(ThreadLocalRandom.current().nextLong(1, Long.MAX_VALUE / 2))
    private val pending = ConcurrentHashMap<Long, Pending>()

    @Suppress("UNCHECKED_CAST")
    fun <T> start(timeoutMillis: Long = this.timeoutMillis, send: (Long) -> Unit): CompletableFuture<T> {
        val id = counter.incrementAndGet()
        val entry = Pending(CompletableFuture(), timeoutMillis, clock() + timeoutMillis)
        // Before the send, so an answer that overtakes send's return finds an entry.
        pending[id] = entry
        try {
            send(id)
        } catch (failure: Throwable) {
            // SpawneryApi promises the stage carries the failure, not the caller.
            fail(id, failure)
        }
        return entry.future as CompletableFuture<T>
    }

    /** For tests: a leak here is invisible from outside, since every future still times out. */
    fun outstanding(): Int = pending.size

    fun complete(id: Long, value: Any?) {
        pending.remove(id)?.future?.complete(value)
    }

    fun fail(id: Long, error: Throwable) {
        pending.remove(id)?.future?.completeExceptionally(error)
    }

    fun expire() {
        val now = clock()
        for ((id, entry) in pending) {
            if (entry.deadline <= now) {
                pending.remove(id)?.future?.completeExceptionally(
                    TimeoutException("the operator did not answer request $id in ${entry.timeoutMillis}ms"),
                )
            }
        }
    }
}
