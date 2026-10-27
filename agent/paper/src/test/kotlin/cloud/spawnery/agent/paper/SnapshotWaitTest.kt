package cloud.spawnery.agent.paper

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertInstanceOf
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertTimeoutPreemptively
import java.time.Duration
import java.util.concurrent.CompletableFuture
import java.util.concurrent.CountDownLatch

class SnapshotWaitTest {
    private val second = Duration.ofSeconds(1)

    @Test
    fun `no regions is ready at once`() {
        assertEquals(WorldSync.Outcome.Ready, SnapshotWait.await(emptyList(), 0))
    }

    @Test
    fun `all saves complete`() {
        val saves = listOf(CompletableFuture.completedFuture(null), CompletableFuture.completedFuture(Unit))
        assertEquals(WorldSync.Outcome.Ready, SnapshotWait.await(saves, 60_000))
    }

    @Test
    fun `a failed save fails the snapshot with its reason`() {
        val failed = CompletableFuture<Void>().apply { completeExceptionally(IllegalStateException("region 7")) }
        val outcome = assertTimeoutPreemptively(second) {
            SnapshotWait.await(listOf(CompletableFuture<Void>(), failed), 60_000)
        }
        assertInstanceOf(WorldSync.Outcome.Failed::class.java, outcome)
        assertTrue((outcome as WorldSync.Outcome.Failed).reason.contains("region 7"), outcome.reason)
    }

    @Test
    fun `a save that never completes times out`() {
        val outcome = assertTimeoutPreemptively(second) {
            SnapshotWait.await(listOf(CompletableFuture<Void>()), 50)
        }
        assertEquals(WorldSync.Outcome.TimedOut, outcome)
    }

    @Test
    fun `saves still running when the wait gives up are cancelled`() {
        val running = CompletableFuture<Void>()
        SnapshotWait.await(listOf(running), 50)
        assertTrue(running.isCancelled)
    }

    @Test
    fun `a timeout names what a region still refused`() {
        val held = RegionSave("3")
        held.offer({ false }, { throw IllegalStateException(RegionSave.TICKET_HELD) }) {}
        val outcome = assertTimeoutPreemptively(second) {
            SnapshotWait.await(listOf(CompletableFuture.completedFuture(null), held), 50)
        }
        assertInstanceOf(WorldSync.Outcome.Failed::class.java, outcome)
        val reason = (outcome as WorldSync.Outcome.Failed).reason
        assertTrue(reason.contains("timed out") && reason.contains("region 3: ${RegionSave.TICKET_HELD}"), reason)
    }

    @Test
    fun `the copy gets what is left of the one deadline`() {
        var now = 1_000L
        var given = -1L
        val outcome = SnapshotWait.freeze(
            listOf(CompletableFuture.completedFuture(null)),
            deadline = now + 60_000,
            clock = { now },
            flush = { now += 45_000 },
            copy = { left ->
                given = left
                WorldSync.Outcome.Ready
            },
        )
        assertEquals(WorldSync.Outcome.Ready, outcome)
        assertEquals(15_000, given)
    }

    @Test
    fun `the region saves wait only for what is left of the deadline`() {
        val now = 1_000L
        val outcome = assertTimeoutPreemptively(second) {
            SnapshotWait.freeze(
                listOf(CompletableFuture<Void>()),
                deadline = now + 50,
                clock = { now },
                flush = { error("flushed after a failed save") },
                copy = { error("copied after a failed save") },
            )
        }
        assertEquals(WorldSync.Outcome.Failed("the regions did not save: TimedOut"), outcome)
    }

    @Test
    fun `a deadline the flush used up asks for no copy`() {
        var now = 1_000L
        val outcome = SnapshotWait.freeze(
            listOf(CompletableFuture.completedFuture(null)),
            deadline = now + 10,
            clock = { now },
            flush = { now += 30_000 },
            copy = { error("a copy asked for after the deadline") },
        )
        assertEquals(WorldSync.Outcome.Failed("no time left for the copy after the saves and the flush"), outcome)
    }

    @Test
    fun `the flush waits only for what is left of the deadline`() {
        val now = 1_000L
        val stuck = CountDownLatch(1)
        try {
            val outcome = assertTimeoutPreemptively(second) {
                SnapshotWait.freeze(
                    listOf(CompletableFuture.completedFuture(null)),
                    deadline = now + 50,
                    clock = { now },
                    flush = { stuck.await() },
                    copy = { error("copied after a flush that did not finish") },
                )
            }
            assertEquals(WorldSync.Outcome.Failed("the chunk system did not flush before the deadline"), outcome)
        } finally {
            stuck.countDown()
        }
    }

    @Test
    fun `a failed flush fails the snapshot with its cause`() {
        val broken = IllegalStateException("no chunk system flush on this server")
        val thrown = assertThrows(IllegalStateException::class.java) {
            SnapshotWait.freeze(
                listOf(CompletableFuture.completedFuture(null)),
                deadline = 1_000L + 60_000,
                clock = { 1_000L },
                flush = { throw broken },
                copy = { error("copied after a failed flush") },
            )
        }
        assertSame(broken, thrown)
    }

    @Test
    fun `a ticket taken after a refusal times out as a plain timeout`() {
        var attempts = 0
        val save = RegionSave("3")
        save.offer({ false }, {
            attempts++
            if (attempts == 1) throw IllegalStateException(RegionSave.TICKET_HELD)
            CompletableFuture<Void>()
        }) { next -> next() }
        val outcome = assertTimeoutPreemptively(second) { SnapshotWait.await(listOf(save), 50) }
        assertEquals(2, attempts)
        assertEquals(WorldSync.Outcome.TimedOut, outcome)
    }
}
