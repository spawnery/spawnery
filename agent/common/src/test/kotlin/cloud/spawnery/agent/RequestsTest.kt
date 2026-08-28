package cloud.spawnery.agent

import java.util.concurrent.CompletableFuture
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class RequestsTest {
    // A clock the test moves by hand, so a deadline is asserted rather than
    // waited out -- a test that slept for its own timeout would be the slowest
    // in the module and would still be racy on a loaded machine.
    private var now = 0L
    private val sent = mutableListOf<Long>()

    private fun requests(timeoutMillis: Long = 1_000) =
        Requests<Long>(send = { id -> sent += id }, timeoutMillis = timeoutMillis, clock = { now })

    @Test
    fun `an answer completes the future that asked`() {
        val r = requests()
        val pending: CompletableFuture<String> = r.start()

        r.complete(sent.single(), "moved")

        assertEquals("moved", pending.get())
    }

    @Test
    fun `two outstanding requests are told apart by their id`() {
        val r = requests()
        val first: CompletableFuture<String> = r.start()
        val second: CompletableFuture<String> = r.start()

        // Answered out of order, which is the ordinary case: the operator
        // resolves two requests against different objects at different speeds.
        r.complete(sent[1], "second")
        r.complete(sent[0], "first")

        assertEquals("first", first.get())
        assertEquals("second", second.get())
    }

    @Test
    fun `an answer for an id nobody is waiting on is dropped rather than throwing`() {
        // A late answer to a request that already timed out. Throwing here
        // would end the session from inside a gRPC callback, which costs the
        // agent every other request it has outstanding.
        val r = requests()

        r.complete(9999L, "nobody asked for this")
        r.fail(9998L, IllegalStateException("nor for this"))
    }

    @Test
    fun `a request that is never answered fails at its deadline`() {
        val r = requests(timeoutMillis = 1_000)
        val pending: CompletableFuture<String> = r.start()

        now += 1_001
        r.expire()

        assertTrue(pending.isCompletedExceptionally)
    }

    @Test
    fun `a request inside its deadline is left alone by expire`() {
        // The other half of the case above: an expire that fired early would
        // fail a request whose answer is still on its way.
        val r = requests(timeoutMillis = 1_000)
        val pending: CompletableFuture<String> = r.start()

        now += 999
        r.expire()

        assertFalse(pending.isDone)
    }

    // The rule from the spec's section 4.1, and the one worth getting right.
    @Test
    fun `a stream change fails every outstanding request rather than retrying it`() {
        val r = requests()
        val first: CompletableFuture<String> = r.start()
        val second: CompletableFuture<String> = r.start()
        val sentBefore = sent.size

        r.failAll(IllegalStateException("stream displaced"))

        assertTrue(first.isCompletedExceptionally)
        assertTrue(second.isCompletedExceptionally)
        assertEquals(sentBefore, sent.size, "failAll must not resend anything")
    }

    @Test
    fun `an id is not reused after its request is answered`() {
        // Reuse would let a late answer to the first request complete the
        // second one with somebody else's result, which is worse than either
        // failing.
        val r = requests()
        val first: CompletableFuture<String> = r.start()
        r.complete(sent[0], "first")
        r.start<String>()

        assertEquals(2, sent.distinct().size)
        assertEquals("first", first.get())
    }
}
