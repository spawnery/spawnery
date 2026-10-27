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
}
