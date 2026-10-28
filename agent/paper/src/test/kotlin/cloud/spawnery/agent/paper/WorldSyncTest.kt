package cloud.spawnery.agent.paper

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path

class WorldSyncTest {
    @TempDir
    lateinit var dir: Path

    private fun sync(wall: () -> Long = { 0L }, onSleep: (Long) -> Unit = {}): WorldSync {
        var now = 0L
        return WorldSync(dir, clock = { now }, sleep = { now += it; onSleep(now) }, wallClock = wall)
    }

    @Test
    fun `ready ends the wait`() {
        Files.writeString(dir.resolve("ready"), "")
        assertEquals(WorldSync.Outcome.Ready, sync().awaitReady(10_000))
    }

    @Test
    fun `failed carries the reason`() {
        Files.writeString(dir.resolve("failed"), "get objects/x: 503")
        assertEquals(WorldSync.Outcome.Failed("get objects/x: 503"), sync().awaitReady(10_000))
    }

    @Test
    fun `no answer times out`() {
        assertEquals(WorldSync.Outcome.TimedOut, sync().awaitReady(2_000))
    }

    @Test
    fun `ready written while waiting is seen`() {
        val s = sync(onSleep = { now -> if (now >= 1_000) Files.writeString(dir.resolve("ready"), "") })
        assertEquals(WorldSync.Outcome.Ready, s.awaitReady(10_000))
    }

    @Test
    fun `a snapshot request is written and its answer matched by sequence`() {
        val s = sync()
        s.requestSnapshot(4, 1_000)
        assertEquals("4", Files.readString(dir.resolve("snapshot.request")).trim())
        Files.writeString(dir.resolve("snapshot.done"), "3")
        assertEquals(WorldSync.Outcome.TimedOut, s.awaitSnapshot(4, 1_000))
        Files.writeString(dir.resolve("snapshot.done"), "4")
        assertEquals(WorldSync.Outcome.Ready, s.awaitSnapshot(4, 1_000))
    }

    @Test
    fun `the request carries the end of the caller's wait in wall-clock time`() {
        sync(wall = { 1_700_000_000_000 }).requestSnapshot(4, 25_000)
        assertEquals("4 1700000025000", Files.readString(dir.resolve("snapshot.deadline")))
    }

    @Test
    fun `the request file stays one number for a node agent that predates the deadline`() {
        sync(wall = { 1_700_000_000_000 }).requestSnapshot(4, 25_000)
        assertEquals(4L, Files.readString(dir.resolve("snapshot.request")).trim().toLongOrNull())
    }

    @Test
    fun `the deadline is in place before the request is`() {
        val published = mutableListOf<String>()
        val s = WorldSync(dir, wallClock = { 0L }, move = { from, to ->
            published += "${to.fileName} ${Files.readString(from)}"
            Files.move(from, to)
        })
        s.requestSnapshot(4, 1_000)
        assertEquals(listOf("snapshot.deadline 4 1000", "snapshot.request 4"), published)
    }

    @Test
    fun `a deadline from an earlier request is replaced`() {
        val s = sync(wall = { 100 })
        s.requestSnapshot(4, 1_000)
        s.requestSnapshot(5, 2_000)
        assertEquals("5 2100", Files.readString(dir.resolve("snapshot.deadline")))
    }

    @Test
    fun `the deadline written is the end of the wait the freeze gives the copy`() {
        var now = 40_000L
        val s = WorldSync(dir, clock = { now }, sleep = { now += it }, wallClock = { 1_700_000_000_000 })
        val deadline = now + 60_000
        SnapshotWait.freeze(emptyList(), deadline, { now }, flush = { now += 10_000 }) { left ->
            s.requestSnapshot(7, left)
            WorldSync.Outcome.Ready
        }
        assertEquals("7 1700000050000", Files.readString(dir.resolve("snapshot.deadline")))
    }

    @Test
    fun `a failed snapshot answer is a failure`() {
        Files.writeString(dir.resolve("snapshot.done"), "failed 5 a file kept changing")
        assertEquals(WorldSync.Outcome.Failed("a file kept changing"), sync().awaitSnapshot(5, 1_000))
    }

    @Test
    fun `the interval parses Go durations`() {
        assertEquals(300_000L, WorldSync.parseInterval("5m0s"))
        assertEquals(90_000L, WorldSync.parseInterval("1m30s"))
        assertNull(WorldSync.parseInterval("soon"))
        assertNull(WorldSync.parseInterval(null))
    }

    @Test
    fun `a restarted run starts above every number already answered`() {
        assertEquals(1L, sync().nextSequence())
        Files.writeString(dir.resolve("snapshot.request"), "7")
        Files.writeString(dir.resolve("snapshot.done"), "failed 9 x")
        assertEquals(10L, sync().nextSequence())
        Files.writeString(dir.resolve("snapshot.done"), "garbage")
        assertEquals(8L, sync().nextSequence())
    }
}
