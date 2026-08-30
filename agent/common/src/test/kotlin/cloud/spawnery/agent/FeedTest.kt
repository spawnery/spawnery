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
    private var format = ""
    private val feed = Feed(audience, state, { now }, format = { format })

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
        assertTrue(!feed.wanted(0), "an empty server claimed to want events")

        audience.online += alice
        audience.permitted += alice
        assertTrue(feed.wanted(0), "a permitted player online did not register")

        state.optOut(alice)
        assertTrue(!feed.wanted(0), "the last watcher opting out left the agent still asking")
    }

    @Test
    fun `the network's format wraps the message`() {
        audience.online += alice
        audience.permitted += alice
        format = "<gray>PREFIX</gray> ${Feed.MESSAGE_TOKEN} <gray>SUFFIX</gray>"

        feed.onEvent(anEvent("lobby-a"))
        now = 1_000
        feed.tick()

        val line = audience.sent.single().second
        assertTrue(line.startsWith("<gray>PREFIX</gray> "), line)
        assertTrue(line.endsWith(" <gray>SUFFIX</gray>"), line)
        assertTrue(line.contains("lobby-a"), "the event itself was lost: $line")
    }

    @Test
    fun `a blank format falls back to the built-in one rather than printing nothing`() {
        // Blank is what an operator older than the field sends, and what the
        // mirror holds before the first NetworkState arrives. Reading it as
        // "print nothing" would make a feed silently empty on exactly the
        // upgrade that introduces it.
        audience.online += alice
        audience.permitted += alice
        format = ""

        feed.onEvent(anEvent("lobby-a"))
        now = 1_000
        feed.tick()

        val line = audience.sent.single().second
        assertTrue(line.contains("Spawnery"), "the default format was not used: $line")
        assertTrue(line.contains("lobby-a"), "the event itself was lost: $line")
        assertTrue(!line.contains(Feed.MESSAGE_TOKEN), "the token survived into chat: $line")
    }

    @Test
    fun `the format is read per delivery, not captured once`() {
        // The format arrives in the NetworkState the operator resends on every
        // resync. A value captured at construction would hold whatever the
        // first message said, and an edit would take effect at the next pod
        // rather than at the next resync -- which is the rolling this design
        // put the format on the wire to avoid.
        audience.online += alice
        audience.permitted += alice
        format = "<gray>FIRST</gray> ${Feed.MESSAGE_TOKEN}"
        feed.onEvent(anEvent("lobby-a"))
        now = 1_000
        feed.tick()

        format = "<gray>SECOND</gray> ${Feed.MESSAGE_TOKEN}"
        feed.onEvent(anEvent("lobby-b"))
        now = 2_000
        feed.tick()

        assertEquals(2, audience.sent.size)
        assertTrue(audience.sent[0].second.startsWith("<gray>FIRST</gray>"), audience.sent[0].second)
        assertTrue(audience.sent[1].second.startsWith("<gray>SECOND</gray>"), audience.sent[1].second)
    }

    @Test
    fun `a subscribed plugin keeps events flowing even with nobody in chat`() {
        // The case a backend is always in: its audience is empty by design,
        // because every player on it is behind a proxy that delivers the same
        // line. Without the subscriber count the operator would stop sending
        // it events entirely, and a plugin that subscribed through the API
        // would receive nothing -- silently.
        assertTrue(!feed.wanted(0), "an empty agent with no subscriber asked for events")
        assertTrue(feed.wanted(1), "a subscribed plugin did not keep events flowing")
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
