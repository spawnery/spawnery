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
    private val levels = FeedLevels(null)
    private var now = 0L
    private var format = ""
    private val feed = Feed(audience, levels, { now }, format = { format })

    private fun anEvent(name: String): CloudEvent =
        CloudEvent.newBuilder()
            .setKind("PodCreated").setSubject(name).setGroup("lobby")
            .setMessage("$name is ready").build()

    @Test
    fun `a closed window reaches everybody permitted whose level is not off`() {
        audience.online += listOf(alice, bob)
        audience.permitted += listOf(alice, bob)
        levels.set(bob, FeedLevel.OFF)

        feed.onEvent(anEvent("lobby-a"))
        now = 1_000
        feed.tick()

        assertEquals(1, audience.sent.size, "sent to ${audience.sent.map { it.first }}")
        assertEquals(alice, audience.sent.single().first)
    }

    @Test
    fun `each player sees the window at their own level`() {
        audience.online += listOf(alice, bob)
        audience.permitted += listOf(alice, bob)
        levels.set(bob, FeedLevel.NORMAL)

        feed.onEvent(anEvent("lobby-a"))
        feed.onEvent(
            CloudEvent.newBuilder().setKind("ReadyGatePassed").setSubject("lobby-a").setGroup("lobby")
                .setMessage("phase Starting -> Ready").build(),
        )
        now = 1_000
        feed.tick()

        assertEquals(1, audience.sent.count { it.first == alice }, audience.sent.toString())
        assertEquals(2, audience.sent.count { it.first == bob }, audience.sent.toString())
    }

    @Test
    fun `somebody without the permission is never sent a line`() {
        audience.online += alice
        // and permitted stays empty

        feed.onEvent(anEvent("lobby-a"))
        now = 1_000
        feed.tick()

        assertTrue(audience.sent.isEmpty(), "an unpermitted player got ${audience.sent}")
    }

    @Test
    fun `nobody watching means the agent says it wants nothing`() {
        assertTrue(!feed.wanted(0), "an empty server claimed to want events")

        audience.online += alice
        audience.permitted += alice
        assertTrue(feed.wanted(0), "a permitted player online did not register")

        levels.set(alice, FeedLevel.OFF)
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
        // Blank: an operator older than the field, or no NetworkState yet.
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
        // The format arrives with every resync; an edit must not wait for a new pod.
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
        // A backend's audience is always empty: its players read the feed on the proxy.
        assertTrue(!feed.wanted(0), "an empty agent with no subscriber asked for events")
        assertTrue(feed.wanted(1), "a subscribed plugin did not keep events flowing")
    }

    @Test
    fun `an event with nobody to read it is still collapsed and dropped quietly`() {
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
