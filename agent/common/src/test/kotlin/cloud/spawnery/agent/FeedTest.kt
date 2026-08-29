package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent
import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

private class FakeAudience : FeedAudience {
    var online = mutableListOf<UUID>()
    var permitted = mutableSetOf<UUID>()
    val sent = mutableListOf<Pair<UUID, String>>()

    override fun holders(permission: String): List<UUID> =
        online.filter { it in permitted }

    override fun send(player: UUID, message: String) {
        sent += player to message
    }
}

class FeedTest {
    private val alice = UUID.nameUUIDFromBytes("alice".toByteArray())
    private val bob = UUID.nameUUIDFromBytes("bob".toByteArray())
    private val audience = FakeAudience()
    private val state = FeedState()
    private var now = 0L
    private val feed = Feed(audience, state, { now })

    private fun anEvent(name: String): CloudEvent =
        CloudEvent.newBuilder()
            .setKind("ReadyGatePassed").setSubject(name).setGroup("lobby")
            .setMessage("$name is ready").build()

    @Test
    fun `a closed window reaches everybody permitted who has not opted out`() {
        audience.online += listOf(alice, bob)
        audience.permitted += listOf(alice, bob)
        state.optOut(bob)

        feed.onEvent(anEvent("lobby-a"))
        now = 1_000
        feed.tick()

        assertEquals(1, audience.sent.size, "sent to ${audience.sent.map { it.first }}")
        assertEquals(alice, audience.sent.single().first)
    }

    @Test
    fun `somebody without the permission is never sent a line`() {
        // The permission is the bound. An opt-out set that happened to be
        // empty must not be what stands between a player and the feed.
        audience.online += alice
        // and permitted stays empty

        feed.onEvent(anEvent("lobby-a"))
        now = 1_000
        feed.tick()

        assertTrue(audience.sent.isEmpty(), "an unpermitted player got ${audience.sent}")
    }

    @Test
    fun `nobody watching means the agent says it wants nothing`() {
        // What the operator reads to decide whether to send anything at all.
        assertTrue(!feed.wanted(), "an empty server claimed to want events")

        audience.online += alice
        audience.permitted += alice
        assertTrue(feed.wanted(), "a permitted player online did not register")

        state.optOut(alice)
        assertTrue(!feed.wanted(), "the last watcher opting out left the agent still asking")
    }

    @Test
    fun `an event with nobody to read it is still collapsed and dropped quietly`() {
        // It must not accumulate: an agent nobody watches would otherwise hold
        // every event of its lifetime and deliver them all at once the moment
        // an administrator logged in.
        feed.onEvent(anEvent("lobby-a"))
        now = 1_000
        feed.tick()

        audience.online += alice
        audience.permitted += alice
        now = 2_000
        feed.tick()

        assertTrue(audience.sent.isEmpty(), "a stale window was delivered late: ${audience.sent}")
    }
}
