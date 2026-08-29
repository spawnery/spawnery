package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

private fun event(kind: String, subject: String, group: String, warning: Boolean = false): CloudEvent =
    CloudEvent.newBuilder()
        .setKind(kind).setSubject(subject).setGroup(group)
        .setMessage("$subject: $kind").setWarning(warning)
        .build()

class CloudFeedTest {
    @Test
    fun `one event is one line and says what happened`() {
        val lines = coalesce(listOf(event("ReadyGatePassed", "lobby-a3f9", "lobby")))

        assertEquals(1, lines.size)
        assertTrue(lines.single().contains("lobby-a3f9"), lines.single())
    }

    @Test
    fun `many of one kind in one group collapse to one line that names them`() {
        // The case section 5.4 exists for: a rolling update of a ten-server
        // group. Ten lines is a feed people turn off, which costs more than it
        // gives.
        val lines = coalesce(
            listOf(
                event("ReadyGatePassed", "lobby-a3f9", "lobby"),
                event("ReadyGatePassed", "lobby-b71c", "lobby"),
                event("ReadyGatePassed", "lobby-c02e", "lobby"),
            ),
        )

        assertEquals(1, lines.size)
        val line = lines.single()
        assertTrue(line.contains("3"), "the count is missing: $line")
        assertTrue(line.contains("lobby"), "the group is missing: $line")
        // The names, because "3 servers are ready" leaves an admin unable to
        // tell which -- and the one they were waiting for is the question.
        assertTrue(
            line.contains("lobby-a3f9") && line.contains("lobby-b71c") && line.contains("lobby-c02e"),
            "the names are missing: $line",
        )
    }

    @Test
    fun `two kinds in one group stay two lines`() {
        // Collapsing across kinds would produce "4 things happened in lobby",
        // which is the shape of a summary that says nothing.
        val lines = coalesce(
            listOf(
                event("ReadyGatePassed", "lobby-a3f9", "lobby"),
                event("Terminating", "lobby-b71c", "lobby"),
            ),
        )

        assertEquals(2, lines.size)
    }

    @Test
    fun `the same kind in two groups stays two lines`() {
        val lines = coalesce(
            listOf(
                event("ReadyGatePassed", "lobby-a3f9", "lobby"),
                event("ReadyGatePassed", "arena-1", "arena"),
            ),
        )

        assertEquals(2, lines.size)
        assertTrue(lines.any { it.contains("lobby") }, lines.toString())
        assertTrue(lines.any { it.contains("arena") }, lines.toString())
    }

    @Test
    fun `a warning is never collapsed into a normal line`() {
        // A failure hidden inside "3 servers ready" is the one event in this
        // feed somebody actually needs to see.
        val lines = coalesce(
            listOf(
                event("ReadyGatePassed", "lobby-a3f9", "lobby"),
                event("PodRejected", "lobby-b71c", "lobby", warning = true),
            ),
        )

        assertEquals(2, lines.size)
        assertTrue(lines.any { it.contains("lobby-b71c") }, "the warning vanished: $lines")
    }

    @Test
    fun `two warnings of one kind stay two lines`() {
        // Collapsing warnings would lose the sentence that says why, and two
        // failures rarely fail for the same reason.
        val lines = coalesce(
            listOf(
                event("PodRejected", "lobby-a", "lobby", warning = true),
                event("PodRejected", "lobby-b", "lobby", warning = true),
            ),
        )

        assertEquals(2, lines.size)
    }

    @Test
    fun `a warning keeps the operator's own sentence`() {
        // A count and a name is the right shape for ten identical successes
        // and the wrong shape for one failure: the sentence is what says why.
        val lines = coalesce(
            listOf(
                CloudEvent.newBuilder()
                    .setKind("PodRejected").setSubject("lobby-b71c").setGroup("lobby")
                    .setMessage("the node had no room").setWarning(true).build(),
            ),
        )

        assertTrue(lines.single().contains("the node had no room"), lines.single())
    }

    @Test
    fun `an empty window produces no lines rather than an empty one`() {
        assertTrue(coalesce(emptyList()).isEmpty())
    }

    @Test
    fun `a very wide collapse names some and counts the rest`() {
        // Forty names is not a chat line. The bound is stated here rather than
        // discovered by whoever first scales a group to forty.
        val many = (1..40).map { event("ReadyGatePassed", "lobby-%02d".format(it), "lobby") }

        // Measured as a person reads it, not as it is written. The markup is
        // several times the length of the words and none of it reaches a chat
        // line's width.
        val line = plain(coalesce(many).single())

        assertTrue(line.contains("40"), "the total is missing: $line")
        assertTrue(line.length < 200, "the line is ${line.length} characters: $line")
        assertTrue(line.contains("lobby-01"), "it named none of them: $line")
    }

    @Test
    fun `the lines come out in the order the events arrived`() {
        // Not merely "twice the same": a HashMap gives that too, since its
        // iteration order is stable for identical contents. The claim worth
        // holding is that the feed reads in the order things happened, and
        // six groups is enough that a hash order will not coincide with an
        // insertion order by luck -- which is exactly how the two-group
        // version of this test passed against a HashMap.
        val groups = listOf("zulu", "alpha", "mike", "bravo", "yankee", "delta")
        val events = groups.map { event("ReadyGatePassed", "$it-1", it) }

        val lines = coalesce(events)

        assertEquals(groups.size, lines.size)
        for ((i, g) in groups.withIndex()) {
            assertTrue(lines[i].contains("$g-1"), "line $i is ${lines[i]}, want $g-1")
        }
    }
}
