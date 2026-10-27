package cloud.spawnery.agent.paper

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertInstanceOf
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertTimeoutPreemptively
import java.time.Duration
import java.util.concurrent.CompletableFuture

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
    fun `a deadline already past still asks the copy once, with no time left`() {
        var now = 1_000L
        var given = -1L
        SnapshotWait.freeze(
            listOf(CompletableFuture.completedFuture(null)),
            deadline = now + 10,
            clock = { now },
            flush = { now += 30_000 },
            copy = { left ->
                given = left
                WorldSync.Outcome.TimedOut
            },
        )
        assertEquals(0, given)
    }
}
