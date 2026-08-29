package cloud.spawnery.agent

import cloud.spawnery.agent.api.CloudEventInfo
import cloud.spawnery.agent.pb.CloudEvent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class CloudEventsTest {
    private val bus = CloudEvents()

    private fun anEvent(): CloudEvent =
        CloudEvent.newBuilder()
            .setKind("ReadyGatePassed").setSubject("lobby-a3f9").setGroup("lobby")
            .setMessage("phase Starting -> Ready").setWarning(false)
            .build()

    @Test
    fun `a subscriber receives every field the operator sent`() {
        val seen = mutableListOf<CloudEventInfo>()
        bus.subscribe { seen += it }

        bus.publish(anEvent())

        val info = seen.single()
        assertEquals("ReadyGatePassed", info.kind())
        assertEquals("lobby-a3f9", info.subject())
        assertEquals("lobby", info.group())
        // The operator's own sentence, so a plugin logging it and an operator
        // reading kubectl see the same words.
        assertEquals("phase Starting -> Ready", info.message())
        assertTrue(!info.warning())
    }

    @Test
    fun `a closed subscription stops receiving`() {
        val seen = mutableListOf<CloudEventInfo>()
        val handle = bus.subscribe { seen += it }

        handle.close()
        bus.publish(anEvent())

        assertTrue(seen.isEmpty(), "a closed subscription still received: $seen")
    }

    @Test
    fun `closing twice is not an error`() {
        // A plugin closing on disable after the agent already stopped is
        // ordinary, and punishing it would put an exception in a shutdown path
        // where nobody is looking.
        val handle = bus.subscribe { }
        handle.close()
        handle.close()

        assertEquals(0, bus.size())
    }

    @Test
    fun `one listener throwing does not cost the others their event`() {
        // This runs inside a gRPC callback. One plugin's bug must not take the
        // session, and must not take every other plugin's events either.
        val seen = mutableListOf<CloudEventInfo>()
        bus.subscribe { throw IllegalStateException("a plugin bug") }
        bus.subscribe { seen += it }

        bus.publish(anEvent())

        assertEquals(1, seen.size, "a throwing listener silenced a working one")
    }

    @Test
    fun `a listener that threw is dropped rather than retried forever`() {
        var calls = 0
        bus.subscribe {
            calls++
            throw IllegalStateException("a plugin bug")
        }

        bus.publish(anEvent())
        bus.publish(anEvent())

        assertEquals(1, calls, "a broken listener was called again")
        assertEquals(0, bus.size())
    }
}
