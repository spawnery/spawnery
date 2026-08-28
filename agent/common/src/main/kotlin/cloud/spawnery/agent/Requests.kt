package cloud.spawnery.agent

import java.util.concurrent.CompletableFuture
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.TimeoutException
import java.util.concurrent.atomic.AtomicLong

/**
 * The requests this agent has asked the operator and not yet had answered.
 *
 * **An in-flight request does not survive a stream change.** `SessionLoop`
 * renews make-before-break, so two streams are live at once; a request whose
 * answer would arrive on the displaced one is failed by [failAll] rather than
 * resent on the new one.
 *
 * Retrying looks kinder and is not. Only the caller knows whether their
 * request is safe to repeat, and none of the three this channel will carry is:
 * a connect delivered twice moves a player who has already moved, and a scale
 * delivered twice scales twice. Failing hands the decision to the one place
 * that can make it.
 *
 * Ids are minted here and never reused, which is not tidiness: reuse would let
 * a late answer to a finished request complete a later one with somebody
 * else's result, and that is worse than either of them failing.
 *
 * Threads. [start] is called from whatever thread a plugin chose; [complete]
 * and [fail] from a gRPC callback; [expire] from the reporting timer. A
 * `ConcurrentHashMap` and an `AtomicLong` are the whole of the coordination,
 * and there is no lock because none of these callers may block.
 *
 * @param send builds and sends the message for an id. Called inside [start],
 *   after the entry exists, so an answer that arrives before send returns
 *   still finds something to complete.
 * @param timeoutMillis how long an unanswered request lives. The operator
 *   answers from memory, so this bounds a lost message rather than a slow one.
 * @param clock the time source, injectable so a test asserts a deadline
 *   instead of sleeping through one.
 */
class Requests<Req>(
    private val send: (Long) -> Unit,
    private val timeoutMillis: Long,
    private val clock: () -> Long,
) {
    private class Pending(val future: CompletableFuture<Any?>, val deadline: Long)

    private val counter = AtomicLong(0)
    private val pending = ConcurrentHashMap<Long, Pending>()

    /** Starts a request and returns the future its answer completes. */
    @Suppress("UNCHECKED_CAST")
    fun <T> start(): CompletableFuture<T> {
        val id = counter.incrementAndGet()
        val entry = Pending(CompletableFuture(), clock() + timeoutMillis)
        // Entered before the send, so an answer that overtakes send's return
        // finds an entry rather than being dropped as unknown.
        pending[id] = entry
        send(id)
        return entry.future as CompletableFuture<T>
    }

    /** Completes the request with this id, or does nothing if none is waiting. */
    fun complete(id: Long, value: Any?) {
        pending.remove(id)?.future?.complete(value)
    }

    /** Fails the request with this id, or does nothing if none is waiting. */
    fun fail(id: Long, error: Throwable) {
        pending.remove(id)?.future?.completeExceptionally(error)
    }

    /**
     * Fails everything outstanding. Called when the stream these requests were
     * asked on is displaced or ends -- see the class comment for why this is
     * not a resend.
     */
    fun failAll(error: Throwable) {
        val ids = pending.keys.toList()
        for (id in ids) {
            pending.remove(id)?.future?.completeExceptionally(error)
        }
    }

    /** Fails everything past its deadline. Called from the reporting timer. */
    fun expire() {
        val now = clock()
        for ((id, entry) in pending) {
            if (entry.deadline <= now) {
                pending.remove(id)?.future?.completeExceptionally(
                    TimeoutException("the operator did not answer request $id in ${timeoutMillis}ms"),
                )
            }
        }
    }
}
