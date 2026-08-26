package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

/**
 * Every test builds a real [Router] over a real [ServerDirectory]/[FakeRegistry]
 * pair, and a [FakePlayers] roster to drain -- the same "test the real
 * composition" rule [RouterTest] follows, one layer up.
 */
class DrainTest {
    @Test
    fun `every player on the draining server is moved`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val players = FakePlayers(listOf(FakePlayer("alice", "hub"), FakePlayer("bob", "hub")))
        val drain = Drain(players, router) { _, _ -> }

        drain.run("hub", listOf("lobby"))

        assertEquals(listOf("alice" to "lobby-1", "bob" to "lobby-1"), players.moves)
    }

    @Test
    fun `players on other servers are not touched`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val players = FakePlayers(listOf(FakePlayer("alice", "hub"), FakePlayer("carol", "survival")))
        val drain = Drain(players, router) { _, _ -> }

        drain.run("hub", listOf("lobby"))

        assertEquals(listOf("alice" to "lobby-1"), players.moves)
    }

    @Test
    fun `a player is moved to a server chosen from toGroups only`() {
        val router = Router(
            directory(
                Backend("hub-2", "10.0.0.1:25565", "hub"),
                Backend("lobby-1", "10.0.0.2:25565", "lobby"),
            ),
        )
        val players = FakePlayers(listOf(FakePlayer("alice", "hub-1")))
        val drain = Drain(players, router) { _, _ -> }

        // toGroups names only "lobby" -- hub-2, emptier or not, is never a
        // candidate.
        drain.run("hub-1", listOf("lobby"))

        assertEquals(listOf("alice" to "lobby-1"), players.moves)
    }

    @Test
    fun `the draining server is never the target, even if it is in toGroups`() {
        val router = Router(
            directory(
                Backend("hub-1", "10.0.0.1:25565", "hub"),
                Backend("hub-2", "10.0.0.2:25565", "hub"),
            ),
        )
        val players = FakePlayers(listOf(FakePlayer("alice", "hub-1")))
        val drain = Drain(players, router) { _, _ -> }

        drain.run("hub-1", listOf("hub"))

        assertEquals(listOf("alice" to "hub-2"), players.moves)
    }

    @Test
    fun `with no target available nothing moves and the reason is logged`() {
        val router = Router(directory(Backend("hub-1", "10.0.0.1:25565", "hub")))
        val players = FakePlayers(listOf(FakePlayer("alice", "hub-1"), FakePlayer("bob", "hub-1")))
        val logs = mutableListOf<String>()
        val drain = Drain(players, router) { message, _ -> logs += message }

        // hub-1 is the only member of the only group in toGroups, and it is
        // also the server being drained -- once excluded, nothing is left.
        drain.run("hub-1", listOf("hub"))

        assertTrue(players.moves.isEmpty(), players.moves.toString())
        // One line, not one per player left stranded.
        assertEquals(1, logs.size, logs.toString())
    }

    @Test
    fun `a second identical drain moves nobody, because nobody is left`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val alice = FakePlayer("alice", "hub")
        val players = FakePlayers(listOf(alice))
        val drain = Drain(players, router) { _, _ -> }

        drain.run("hub", listOf("lobby"))
        assertEquals(listOf("alice" to "lobby-1"), players.moves)

        // What the operator's next periodic DrainPlayers, ~30s later, finds:
        // alice's reconnect has completed, so Velocity's own currentServer no
        // longer says "hub". The obvious wrong implementation -- one that
        // does not filter by currentServer, or filters against something
        // other than live state -- moves alice a second time here.
        alice.currentServer = "lobby-1"
        drain.run("hub", listOf("lobby"))

        assertEquals(listOf("alice" to "lobby-1"), players.moves)
    }

    @Test
    fun `a player whose current server is unknown is not touched`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val players = FakePlayers(listOf(FakePlayer("alice", currentServer = null)))
        val drain = Drain(players, router) { _, _ -> }

        drain.run("hub", listOf("lobby"))

        assertTrue(players.moves.isEmpty(), players.moves.toString())
    }

    @Test
    fun `an exception moving one player is caught and logged, and the rest still move`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val boom = RuntimeException("boom")
        val alice = FakePlayer("alice", "hub", failWith = boom)
        val bob = FakePlayer("bob", "hub")
        val players = FakePlayers(listOf(alice, bob))
        val logs = mutableListOf<Pair<String, Throwable?>>()
        val drain = Drain(players, router) { message, error -> logs += message to error }

        drain.run("hub", listOf("lobby"))

        assertEquals(listOf("bob" to "lobby-1"), players.moves)
        assertEquals(1, logs.size, logs.toString())
        assertEquals(boom, logs[0].second)
    }

    @Test
    fun `the router is asked once per drained player, not once for the whole drain`() {
        val registry = FakeRegistry()
        val counting = CountingRegistry(registry)
        val directory = ServerDirectory(counting) { _, _ -> }
        directory.apply(
            listOf(
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
            ),
        )
        val router = Router(directory)
        val players = FakePlayers(
            listOf(FakePlayer("alice", "hub"), FakePlayer("bob", "hub"), FakePlayer("carol", "hub")),
        )
        val drain = Drain(players, router) { _, _ -> }
        val before = counting.lookups

        drain.run("hub", listOf("lobby"))

        // ServerDirectory.inGroup resolves every backend of a group through
        // ProxyRegistry.server on every call -- two lookups per call to
        // choose() here, since "lobby" carries two backends. An
        // implementation that computes the target once and reuses it for
        // every moveTo would leave this at 2 no matter how many players were
        // drained; three players each asking fresh puts it at 6.
        assertEquals(3 * 2, counting.lookups - before)
    }


    // ---- late arrivals -------------------------------------------------
    //
    // The half of a drain that `run` alone cannot reach. A player whose
    // connection to the draining server was already in flight is on neither
    // side's books when `DrainPlayers` arrives -- Drain's own doc comment
    // carries the disassembly -- so `run` moves everyone except them, and they
    // land afterwards on a server the operator has been told is empty.

    @Test
    fun `a player who lands on a draining server afterwards is moved off it`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        // Nobody on hub when the drain arrives: this is exactly the trace the
        // operator sees before it concludes the server is empty.
        val players = FakePlayers(emptyList())
        val drain = Drain(players, router) { _, _ -> }
        drain.run("hub", listOf("lobby"))
        assertTrue(players.moves.isEmpty(), "the drain found nobody, which is the premise")

        val latecomer = FakePlayer("alice", "hub")
        drain.landed(players.ref(latecomer))

        assertEquals(listOf("alice" to "lobby-1"), players.moves)
    }

    @Test
    fun `a player who lands on a server that is not draining is left alone`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val players = FakePlayers(emptyList())
        val drain = Drain(players, router) { _, _ -> }
        drain.run("hub", listOf("lobby"))

        drain.landed(players.ref(FakePlayer("alice", "survival")))

        assertTrue(players.moves.isEmpty(), "an ordinary arrival was moved")
    }

    @Test
    fun `a player with no server at all is left alone`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val players = FakePlayers(emptyList())
        val drain = Drain(players, router) { _, _ -> }
        drain.run("hub", listOf("lobby"))

        drain.landed(players.ref(FakePlayer("alice", null)))

        assertTrue(players.moves.isEmpty())
    }

    @Test
    fun `the remembered server name is matched case-insensitively`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val players = FakePlayers(emptyList())
        val drain = Drain(players, router) { _, _ -> }
        drain.run("HUB", listOf("lobby"))

        drain.landed(players.ref(FakePlayer("alice", "hub")))

        assertEquals(listOf("alice" to "lobby-1"), players.moves)
    }

    @Test
    fun `a late arrival with nowhere to go is left in place and logged`() {
        val router = Router(directory())
        val players = FakePlayers(emptyList())
        val logged = mutableListOf<String>()
        val drain = Drain(players, router) { message, _ -> logged += message }
        drain.run("hub", listOf("lobby"))

        drain.landed(players.ref(FakePlayer("alice", "hub")))

        assertTrue(players.moves.isEmpty(), "a player was moved with no target available")
        assertTrue(
            logged.any { it.contains("alice") && it.contains("hub") },
            "the stranded arrival was not logged: $logged",
        )
    }

    // ---- the drain set, rotated by FullSync -----------------------------
    //
    // A resync is a FullSync followed by one DrainPlayers per draining server,
    // so what follows a FullSync is a complete statement. These pin the
    // rotation rather than a duration, because there is no duration here.

    @Test
    fun `a drain restated after every FullSync keeps catching arrivals`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val players = FakePlayers(emptyList())
        val drain = Drain(players, router) { _, _ -> }

        // Three resyncs, each restating the drain the way the operator does
        // for as long as the server keeps draining.
        repeat(3) {
            drain.resynced()
            drain.run("hub", listOf("lobby"))
        }
        drain.landed(players.ref(FakePlayer("alice", "hub")))

        assertEquals(listOf("alice" to "lobby-1"), players.moves)
    }

    @Test
    fun `a drain the operator stops restating is forgotten, one resync later`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val players = FakePlayers(emptyList())
        val drain = Drain(players, router) { _, _ -> }
        drain.run("hub", listOf("lobby"))

        // The first FullSync after the drain stops still carries it: what
        // arrived since the previous FullSync is what becomes current, and the
        // drain arrived in that window. Being one resync slow here is the safe
        // direction -- it moves a player off a server that no longer needs
        // emptying, where being early would leave one on a server about to go.
        drain.resynced()
        drain.landed(players.ref(FakePlayer("alice", "hub")))
        assertEquals(listOf("alice" to "lobby-1"), players.moves)

        // The second does not, because nothing restated it in between.
        drain.resynced()
        drain.landed(players.ref(FakePlayer("bob", "hub")))
        assertEquals(listOf("alice" to "lobby-1"), players.moves, "bob was moved off a cancelled drain")
    }

    @Test
    fun `a drain that arrives between FullSyncs catches arrivals at once`() {
        val router = Router(directory(Backend("lobby-1", "10.0.0.1:25565", "lobby")))
        val players = FakePlayers(emptyList())
        val drain = Drain(players, router) { _, _ -> }

        // The immediate DrainPlayers the operator sends when a drain starts,
        // not the one that rides a resync. It must not wait for the next
        // FullSync to take effect: the player it has to catch is in flight now.
        drain.resynced()
        drain.run("hub", listOf("lobby"))
        drain.landed(players.ref(FakePlayer("alice", "hub")))

        assertEquals(listOf("alice" to "lobby-1"), players.moves)
    }

    @Test
    fun `a server that is itself draining is never a drain target`() {
        val router = Router(
            directory(
                // Emptier, and in toGroups -- so without the exclusion it wins
                // on both counts and the player is moved onto a server the
                // operator is trying to empty.
                Backend("lobby-1", "10.0.0.1:25565", "lobby"),
                Backend("lobby-2", "10.0.0.2:25565", "lobby"),
            ),
        )
        val players = FakePlayers(listOf(FakePlayer("alice", "hub")))
        val drain = Drain(players, router) { _, _ -> }

        drain.run("lobby-1", listOf("lobby"))
        drain.run("hub", listOf("lobby"))

        assertEquals(listOf("alice" to "lobby-2"), players.moves)
    }

    private companion object {
        fun directory(vararg backends: Backend): ServerDirectory {
            val directory = ServerDirectory(FakeRegistry()) { _, _ -> }
            directory.apply(backends.toList())
            return directory
        }
    }
}

/**
 * Counts [server] lookups, to make "the router is asked once per drained
 * player" measurable: [ServerDirectory.inGroup] calls [ProxyRegistry.server]
 * once per backend in the group, on every call, so the lookup count after a
 * drain is directly proportional to how many times [Router.choose] actually
 * ran.
 */
private class CountingRegistry(private val delegate: ProxyRegistry) : ProxyRegistry by delegate {
    var lookups = 0
        private set

    override fun server(name: String): RegisteredServer? {
        lookups++
        return delegate.server(name)
    }
}
