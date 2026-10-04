package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.pb.CloudEvent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

private fun event(
    kind: String,
    subject: String,
    group: String = "lobby",
    warning: Boolean = false,
    message: String = "$subject: $kind",
): CloudEvent =
    CloudEvent.newBuilder()
        .setKind(kind).setSubject(subject).setGroup(group)
        .setMessage(message).setWarning(warning)
        .build()

private fun lines(
    level: FeedLevel,
    vararg events: CloudEvent,
    kinds: (String) -> Group.Kind = { Group.Kind.EPHEMERAL },
): List<String> = coalesce(events.toList(), level, kinds).map(::plain)

class CloudFeedTest {
    @Test
    fun `minimal shows a server arriving and going and nothing between`() {
        assertEquals(listOf("[+] lobby-x7k2"), lines(FeedLevel.MINIMAL, event("PodCreated", "lobby-x7k2")))
        assertEquals(listOf("[-] lobby-x7k2"), lines(FeedLevel.MINIMAL, event("ServerStopped", "lobby-x7k2")))
        assertEquals(
            emptyList(),
            lines(
                FeedLevel.MINIMAL,
                event("ReadyGatePassed", "lobby-a"),
                event("Retiring", "lobby-b"),
                event("PodPending", "lobby-c"),
            ),
        )
    }

    @Test
    fun `normal names each step in a word`() {
        assertEquals(
            listOf("[+] lobby-a starting", "[✓] lobby-b ready", "[-] lobby-c leaving", "[-] lobby-d stopped"),
            lines(
                FeedLevel.NORMAL,
                event("PodCreated", "lobby-a"),
                event("ReadyGatePassed", "lobby-b"),
                event("DeletionRequested", "lobby-c"),
                event("ServerStopped", "lobby-d"),
            ),
        )
    }

    @Test
    fun `a proxy arrives at its ready gate`() {
        val started = event("ProxyStarted", "gateway-4d1", group = "gateway")
        assertEquals(listOf("[+] gateway-4d1"), lines(FeedLevel.MINIMAL, started))
        assertEquals(listOf("[✓] gateway-4d1 ready"), lines(FeedLevel.NORMAL, started))
        assertEquals(
            listOf("[-] gateway-4d1 leaving", "[-] gateway-4d1 stopped"),
            lines(
                FeedLevel.NORMAL,
                event("ProxyRetiring", "gateway-4d1", group = "gateway"),
                event("ProxyStopped", "gateway-4d1", group = "gateway"),
            ),
        )
    }

    @Test
    fun `verbose shows the operator's note`() {
        assertEquals(
            listOf("[+] lobby-x7k2: created pod lobby-x7k2"),
            lines(FeedLevel.VERBOSE, event("PodCreated", "lobby-x7k2", message = "created pod lobby-x7k2")),
        )
        assertEquals(
            listOf("[·] lobby-x7k2: phase Pending -> Starting: pod is running"),
            lines(
                FeedLevel.VERBOSE,
                event("PodRunning", "lobby-x7k2", message = "phase Pending -> Starting: pod is running"),
            ),
        )
    }

    @Test
    fun `a kind this agent does not know shows only in verbose`() {
        val later = event("SomethingAddedInALaterRelease", "lobby-a")
        assertEquals(emptyList(), lines(FeedLevel.MINIMAL, later))
        assertEquals(emptyList(), lines(FeedLevel.NORMAL, later))
        assertEquals(1, lines(FeedLevel.VERBOSE, later).size)
    }

    @Test
    fun `a warning is a short reason below verbose and the note in it`() {
        val timeout = event(
            "StartupTimeout", "lobby-x7k2",
            message = "phase Starting -> Failed: server did not become ready in time",
        )
        assertEquals(listOf("[!] lobby-x7k2 did not start in time"), lines(FeedLevel.MINIMAL, timeout))
        assertEquals(listOf("[!] lobby-x7k2 did not start in time"), lines(FeedLevel.NORMAL, timeout))
        assertEquals(
            listOf("[!] lobby-x7k2: phase Starting -> Failed: server did not become ready in time"),
            lines(FeedLevel.VERBOSE, timeout),
        )
    }

    @Test
    fun `a warning nobody put in the table is read from its kind`() {
        assertEquals(
            listOf("[!] lobby-x7k2 pod name conflict"),
            lines(FeedLevel.MINIMAL, event("PodNameConflict", "lobby-x7k2", warning = true)),
        )
    }

    @Test
    fun `a warning with no note or no kind still says something`() {
        assertEquals(
            listOf("[!] lobby-x7k2: did not start in time"),
            lines(FeedLevel.VERBOSE, event("StartupTimeout", "lobby-x7k2", message = "")),
        )
        assertEquals(
            listOf("[!] lobby-x7k2 warning"),
            lines(FeedLevel.MINIMAL, event("", "lobby-x7k2", warning = true, message = "")),
        )
    }

    @Test
    fun `many of one row in one group collapse to a count`() {
        val three = arrayOf(event("PodCreated", "lobby-a"), event("PodCreated", "lobby-b"), event("PodCreated", "lobby-c"))
        assertEquals(listOf("[+] 3 lobby"), lines(FeedLevel.MINIMAL, *three))
        assertEquals(listOf("[+] 3 lobby starting"), lines(FeedLevel.NORMAL, *three))
    }

    @Test
    fun `a row the level hides neither shows nor counts`() {
        val window = arrayOf(event("PodCreated", "lobby-a"), event("PodCreated", "lobby-b"), event("ReadyGatePassed", "lobby-c"))
        assertEquals(listOf("[+] 2 lobby"), lines(FeedLevel.MINIMAL, *window))
        assertEquals(listOf("[+] 2 lobby starting", "[✓] lobby-c ready"), lines(FeedLevel.NORMAL, *window))
    }

    @Test
    fun `one row in two groups stays two lines`() {
        assertEquals(
            listOf("[+] lobby-a", "[+] arena-a"),
            lines(FeedLevel.MINIMAL, event("PodCreated", "lobby-a"), event("PodCreated", "arena-a", group = "arena")),
        )
    }

    @Test
    fun `warnings come first and never collapse`() {
        assertEquals(
            listOf("[!] lobby-b did not start in time", "[!] lobby-c did not start in time", "[+] lobby-a"),
            lines(
                FeedLevel.MINIMAL,
                event("PodCreated", "lobby-a"),
                event("StartupTimeout", "lobby-b"),
                event("StartupTimeout", "lobby-c"),
            ),
        )
    }

    @Test
    fun `verbose collapses by kind and names the servers`() {
        assertEquals(
            listOf("[+] 3 PodCreated in lobby (lobby-a, lobby-b, lobby-c)"),
            lines(FeedLevel.VERBOSE, event("PodCreated", "lobby-a"), event("PodCreated", "lobby-b"), event("PodCreated", "lobby-c")),
        )
        val eight = (1..8).map { event("PodCreated", "lobby-$it") }.toTypedArray()
        assertTrue(lines(FeedLevel.VERBOSE, *eight).single().endsWith("lobby-6 and 2 more)"), lines(FeedLevel.VERBOSE, *eight).single())
    }

    @Test
    fun `off shows nothing at all`() {
        assertEquals(emptyList(), coalesce(listOf(event("StartupTimeout", "lobby-a")), FeedLevel.OFF))
    }

    @Test
    fun `an on-demand member is shown short, linked by its full name`() {
        val kinds = { g: String -> if (g == "challenge") Group.Kind.ON_DEMAND else Group.Kind.EPHEMERAL }
        val created = event("PodCreated", "challenge-3f2b1c9a0d4e", group = "challenge")

        assertEquals(listOf("[+] challenge-3f2b1c"), lines(FeedLevel.MINIMAL, created, kinds = kinds))
        val raw = coalesce(listOf(created), FeedLevel.MINIMAL, kinds).single()
        assertTrue(raw.contains("show_text:'challenge-3f2b1c9a0d4e'"), raw)
        assertTrue(raw.contains("suggest_command:'/cloud info challenge-3f2b1c9a0d4e'"), raw)
    }

    @Test
    fun `only the sign carries the row's colour`() {
        val raw = coalesce(listOf(event("PodCreated", "lobby-a")), FeedLevel.MINIMAL).single()
        assertTrue(raw.startsWith("<dark_gray>[</dark_gray><green>+</green><dark_gray>]</dark_gray> "), raw)
        val warning = coalesce(listOf(event("StartupTimeout", "lobby-a")), FeedLevel.MINIMAL).single()
        assertTrue(warning.startsWith("<dark_gray>[</dark_gray><red>!</red>"), warning)
        assertTrue(warning.endsWith("<red>did not start in time</red>"), warning)
    }
}
