package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class CloudFeedBufferTest {
    private val delivered = mutableListOf<List<String>>()
    private var now = 0L
    private fun buffer() = CloudFeedBuffer({ now }, 1_000L) { delivered += it }

    private fun anEvent(name: String): CloudEvent =
        CloudEvent.newBuilder()
            .setKind("ReadyGatePassed").setSubject(name).setGroup("lobby")
            .setMessage("$name is ready").build()

    @Test
    fun `nothing is delivered before the window closes`() {
        // The whole reason the buffer exists. Delivering on arrival is ten
        // lines for a rolling update, which is what section 5.4 refuses.
        val b = buffer()
        b.add(anEvent("lobby-a"))
        now = 999
        b.tick()

        assertTrue(delivered.isEmpty(), "delivered early: $delivered")
    }

    @Test
    fun `the window closing delivers one collapsed batch`() {
        val b = buffer()
        b.add(anEvent("lobby-a"))
        b.add(anEvent("lobby-b"))
        now = 1_000
        b.tick()

        assertEquals(1, delivered.size)
        assertEquals(1, delivered.single().size, "two events made ${delivered.single().size} lines")
    }

    @Test
    fun `an empty window delivers nothing at all`() {
        // Not an empty batch: a deliver call with no lines would have every
        // implementation of it writing a guard this one can write once.
        val b = buffer()
        now = 5_000
        b.tick()

        assertTrue(delivered.isEmpty())
    }

    @Test
    fun `the window starts at the first event and not at the last tick`() {
        // Otherwise a steady trickle -- one event every 900ms -- would never
        // close a window, and the feed would go silent exactly when something
        // is happening.
        val b = buffer()
        b.add(anEvent("lobby-a"))
        now = 900
        b.tick()
        b.add(anEvent("lobby-b"))
        now = 1_000
        b.tick()

        assertEquals(1, delivered.size, "a trickle never closed its window")
    }

    @Test
    fun `a delivered window is emptied rather than resent`() {
        // A buffer that kept its events would repeat the whole batch on every
        // tick for the rest of the process, which is worse than the ten lines
        // it exists to prevent.
        val b = buffer()
        b.add(anEvent("lobby-a"))
        now = 1_000
        b.tick()
        now = 5_000
        b.tick()

        assertEquals(1, delivered.size, "the same window was delivered twice: $delivered")
    }

    @Test
    fun `a second window opens after the first is delivered`() {
        val b = buffer()
        b.add(anEvent("lobby-a"))
        now = 1_000
        b.tick()
        b.add(anEvent("lobby-b"))
        now = 1_500
        b.tick()

        assertEquals(1, delivered.size, "the second window closed early")

        now = 2_000
        b.tick()
        assertEquals(2, delivered.size, "the second window never closed")
        assertTrue(delivered[1].single().contains("lobby-b"), delivered[1].toString())
    }
}
