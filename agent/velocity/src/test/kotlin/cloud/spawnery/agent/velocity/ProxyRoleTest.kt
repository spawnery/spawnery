package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.dormantConnector
import cloud.spawnery.agent.pb.DrainPlayers
import cloud.spawnery.agent.pb.FullSync
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.OperatorToProxy
import cloud.spawnery.agent.pb.ServerState as PbServerState
import cloud.spawnery.agent.pb.ProxyMessage
import cloud.spawnery.agent.pb.RegisterServer
import cloud.spawnery.agent.pb.ReportInterval
import cloud.spawnery.agent.pb.SessionDeadline
import cloud.spawnery.agent.pb.SetReady
import cloud.spawnery.agent.pb.UnregisterServer
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.util.concurrent.CyclicBarrier
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean
import cloud.spawnery.agent.pb.RegisteredServer as PbServer
import java.util.UUID

/**
 * The mapping between the operator's messages and the four things a proxy
 * agent does with them, and nothing about the loop.
 *
 * `SessionLoopTest` drives the loop with a `FakeRole` of its own — it has to,
 * because the loop lives in `:common` and this class does not — so without
 * this the production role would be the one thing on the proxy's side of the
 * channel that nothing tested.
 *
 * The collaborators are the real [ServerDirectory], [Router] and [Drain] over
 * [FakeRegistry]/[FakePlayers] rather than doubles of their own. Each of the
 * three has its own suite; what is under test here is only that the right one
 * is reached with the right arguments, and an assertion made against their
 * real observable effects cannot pass against a role that called a mock in a
 * way the production wiring would not.
 */
class ProxyRoleTest {
    private val registry = FakeRegistry()
    private val logs = mutableListOf<Pair<String, Throwable?>>()
    private val directory = ServerDirectory(registry) { message, error -> logs += message to error }
    private val roster = listOf(FakePlayer("alice"), FakePlayer("bob"))
    private val players = FakePlayers(roster)
    private val drain = Drain(players, Router(directory)) { message, error -> logs += message to error }
    private val state = ProxyState(slots = 500)
    private val mirror = NetworkMirror()

    /** How many times the role reported a first sync. See the two gate tests. */
    private var syncs = 0

    private val role = ProxyRole(
        state = state,
        directory = directory,
        drain = drain,
        players = players,
        readTimeoutMillis = 30_000,
        onFirstSync = { syncs++ },
        onSetReady = { },
        log = { message, error -> logs += message to error },
        mirror = mirror,
        connector = dormantConnector(),
    )

    @Test
    fun `hello carries the read timeout the proxy actually parsed`() {
        // The operator races this deadline when a backend's node dies and can
        // find it out no other way: the value lives in a file the operator
        // never reads, which a configOverlay is free to lower.
        val hello = role.hello("test-version").hello
        assertEquals(30_000, hello.readTimeoutMillis)
    }

    @Test
    fun `hello carries the version and leaves ready unset`() {
        val hello = role.hello("26.2-0.3.0")

        assertEquals(ProxyMessage.MessageCase.HELLO, hello.messageCase)
        assertEquals("26.2-0.3.0", hello.hello.version)
        // Not cosmetic, and not a stand-in for "the proxy is not ready yet".
        // ProxyMessage carries no readiness at all: a proxy's readiness reaches
        // the operator through the kubelet's probe on ReadyGate's port and
        // nowhere else, which internal/agentserver's handleProxy says in its
        // own comment. A `true` here would be a second and contradicting
        // source for a fact the kubelet owns.
        assertFalse(hello.hello.ready, "the proxy asserted a readiness only the kubelet may state")
    }

    @Test
    fun `the report carries the sampled players and the configured slots`() {
        state.sample(players = 12)

        val report = role.playerCount()
        assertEquals(ProxyMessage.MessageCase.PLAYER_COUNT, report.messageCase)
        assertEquals(12, report.playerCount.players)
        // The proxy's own player limit, not a zero. The operator's registry
        // discards any report where players exceed slots, so a proxy reporting
        // zero slots would have every report with a player online thrown away.
        assertEquals(500, report.playerCount.slots)

        // Read when the report is built, not when the role was constructed:
        // Velocity's scheduler overwrites the count between reports.
        state.sample(players = 13)
        assertEquals(13, role.playerCount().playerCount.players)
    }

    @Test
    fun `a report interval message yields a Report directive`() {
        assertEquals(
            Directive.Report(30),
            role.onMessage(
                OperatorToProxy.newBuilder()
                    .setReportInterval(ReportInterval.newBuilder().setSeconds(30))
                    .build(),
            ),
        )
    }

    @Test
    fun `a session deadline message yields a Deadline directive`() {
        assertEquals(
            Directive.Deadline(renewAfterSeconds = 240, hardDeadlineSeconds = 600),
            role.onMessage(
                OperatorToProxy.newBuilder()
                    .setSessionDeadline(
                        SessionDeadline.newBuilder()
                            .setRenewAfterSeconds(240)
                            .setHardDeadlineSeconds(600),
                    )
                    .build(),
            ),
        )
    }

    @Test
    fun `a full sync applies to the directory`() {
        assertEquals(Directive.None, role.onMessage(fullSync(backend("lobby-1", "10.0.0.1:25565", "lobby"))))

        assertEquals(setOf("lobby-1"), directory.names())
        assertEquals(listOf<FakeRegistry.Call>(FakeRegistry.Call.Register(info("lobby-1", "10.0.0.1", 25565))), registry.calls)

        // A full sync is a full sync: the second one is the whole list, and
        // what it omits is unregistered. Asserting this here rather than
        // leaving it to ServerDirectoryTest is what shows the role hands the
        // message to `apply` and not to `add`.
        role.onMessage(fullSync(backend("lobby-2", "10.0.0.2:25565", "lobby")))
        assertEquals(setOf("lobby-2"), directory.names())
    }

    @Test
    fun `the first full sync opens the gate`() {
        assertEquals(0, syncs, "the gate was opened before the operator sent anything")

        role.onMessage(fullSync(backend("lobby-1", "10.0.0.1:25565", "lobby")))

        assertEquals(1, syncs, "the first server list did not open the ready gate")
    }

    @Test
    fun `a second full sync does not open the gate again`() {
        role.onMessage(fullSync(backend("lobby-1", "10.0.0.1:25565", "lobby")))
        role.onMessage(fullSync(backend("lobby-1", "10.0.0.1:25565", "lobby")))
        role.onMessage(fullSync())

        // ReadyGate.open() is itself idempotent, so in production a role that
        // called it on every sync would look identical from outside. This
        // counts calls precisely so that what is pinned is the role's own
        // once-only behaviour rather than the gate's tolerance of being asked
        // again -- the operator sends a FullSync roughly every 30 seconds, and
        // every reconnect starts with one.
        assertEquals(1, syncs, "the role re-opened the gate on a later sync")
    }

    @Test
    fun `a register and an unregister reach the directory`() {
        assertEquals(
            Directive.None,
            role.onMessage(
                OperatorToProxy.newBuilder()
                    .setRegisterServer(
                        RegisterServer.newBuilder().setServer(backend("mini-1", "10.0.1.7:25565", "mini")),
                    )
                    .build(),
            ),
        )
        assertEquals(setOf("mini-1"), directory.names())

        // A register is not a sync: it must not open the gate, because a proxy
        // with one incrementally added backend has not been told the list.
        assertEquals(0, syncs, "an incremental register opened the ready gate")

        assertEquals(
            Directive.None,
            role.onMessage(
                OperatorToProxy.newBuilder()
                    .setUnregisterServer(UnregisterServer.newBuilder().setName("mini-1"))
                    .build(),
            ),
        )
        assertEquals(emptySet<String>(), directory.names())
        assertEquals(
            listOf<FakeRegistry.Call>(
                FakeRegistry.Call.Register(info("mini-1", "10.0.1.7", 25565)),
                FakeRegistry.Call.Unregister(info("mini-1", "10.0.1.7", 25565)),
            ),
            registry.calls,
        )
    }

    @Test
    fun `a drain message reaches the drain`() {
        role.onMessage(
            fullSync(
                backend("lobby-1", "10.0.0.1:25565", "lobby"),
                backend("mini-1", "10.0.1.7:25565", "mini"),
            ),
        )
        roster.forEach { it.currentServer = "mini-1" }

        assertEquals(
            Directive.None,
            role.onMessage(
                OperatorToProxy.newBuilder()
                    .setDrainPlayers(
                        DrainPlayers.newBuilder().setFromServer("mini-1").addToGroups("lobby"),
                    )
                    .build(),
            ),
        )

        // Both arguments are asserted, not just that a drain happened: a role
        // that passed the groups in place of the server, or dropped the
        // exclusion, would move nobody or move them to the server being
        // drained.
        assertEquals(listOf("alice" to "lobby-1", "bob" to "lobby-1"), players.moves)
    }

    @Test
    fun `a FullSync rotates the drain set, so a drain the operator drops expires`() {
        // The wiring test for `drain.resynced()`. It is asserted through
        // behaviour rather than a spy because Drain is a concrete class here,
        // and behaviour is the thing that matters anyway: a FullSync branch
        // that stopped rotating would leave this proxy enforcing a drain the
        // operator cancelled, indefinitely.
        val servers = arrayOf(
            backend("lobby-1", "10.0.0.1:25565", "lobby"),
            backend("mini-1", "10.0.1.7:25565", "mini"),
        )
        role.onMessage(fullSync(*servers))
        role.onMessage(
            OperatorToProxy.newBuilder()
                .setDrainPlayers(DrainPlayers.newBuilder().setFromServer("mini-1").addToGroups("lobby"))
                .build(),
        )

        val latecomer = FakePlayer("carol", "mini-1")

        // One resync with the drain restated: still in force.
        role.onMessage(fullSync(*servers))
        role.onMessage(
            OperatorToProxy.newBuilder()
                .setDrainPlayers(DrainPlayers.newBuilder().setFromServer("mini-1").addToGroups("lobby"))
                .build(),
        )
        drain.landed(players.ref(latecomer))
        assertEquals(listOf("carol" to "lobby-1"), players.moves.filter { it.first == "carol" })

        // Two resyncs with it dropped: gone. The first still carries it --
        // what arrived since the previous FullSync is what becomes current --
        // so it takes the second before an arrival is left alone.
        role.onMessage(fullSync(*servers))
        role.onMessage(fullSync(*servers))
        drain.landed(players.ref(FakePlayer("dave", "mini-1")))
        assertTrue(
            players.moves.none { it.first == "dave" },
            "a drain the operator stopped restating was still enforced: ${players.moves}",
        )
    }

    @Test
    fun `an unrecognised message yields None and touches nothing`() {
        // The default instance is MESSAGE_NOT_SET, which is what an older
        // agent sees when a newer operator sends a case it does not know.
        assertEquals(Directive.None, role.onMessage(OperatorToProxy.getDefaultInstance()))

        assertEquals(emptyList<FakeRegistry.Call>(), registry.calls)
        assertEquals(emptyList<Pair<String, String>>(), players.moves)
        assertEquals(0, syncs)
        assertEquals(emptyList<Pair<String, Throwable?>>(), logs, "an unknown message was reported as a failure")
    }

    @Test
    fun `a message whose effect throws is logged and yields None`() {
        registry.failRegisterWith = IllegalStateException("the proxy is shutting down")

        // A FullSync and not a ReportInterval, deliberately: the branches that
        // return a directive do no work, so a guard wrapped around only those
        // would pass a test written against them and still let an exception
        // out of here. This runs on a gRPC callback thread, where an escaping
        // exception ends the stream -- one malformed entry in one server list
        // would cost the proxy its session instead of costing it that entry.
        assertEquals(
            Directive.None,
            role.onMessage(fullSync(backend("lobby-1", "10.0.0.1:25565", "lobby"))),
        )

        // The gate stays shut, and this is the assertion that pins the
        // ordering inside the FULL_SYNC branch rather than merely its outcome.
        // The obvious wrong implementation -- claiming the latch before
        // directory.apply rather than after it -- is invisible to the final
        // count below: it would open the gate here, the second sync would find
        // the latch already set, and `syncs` would still be 1 at the end. Right
        // number, wrong reason, and a proxy that had spent its one chance to
        // become ready on the sync that failed.
        assertEquals(0, syncs, "a sync that threw opened the gate anyway")

        assertEquals(1, logs.size, "the swallowed failure left no trace")
        assertTrue(
            logs[0].first.contains("FULL_SYNC"),
            "the log line does not name the message that failed: ${logs[0].first}",
        )
        assertEquals("the proxy is shutting down", logs[0].second?.message)

        // The once-only latch is spent by a sync that worked, not by one that
        // threw: the operator repeats FullSync, and a proxy that had lost its
        // only chance to open the gate would stay not-ready forever.
        registry.failRegisterWith = null
        role.onMessage(fullSync(backend("lobby-1", "10.0.0.1:25565", "lobby")))
        assertEquals(1, syncs, "a failed first sync consumed the gate's one opening")
    }

    @Test
    fun `set ready closes and reopens the gate`() {
        val states = mutableListOf<Boolean>()
        val role = newRole(onSetReady = { states += it })

        // The sync first, because the reopen is conditional on it: a proxy
        // with no server list is not made ready by anything. The two tests
        // below own that rule; this one owns the mapping once it is satisfied.
        role.onMessage(fullSync())
        role.onMessage(setReady(false))
        role.onMessage(setReady(true))

        assertEquals(listOf(false, true), states)
    }

    @Test
    fun `a ready before the first sync is recorded and not passed on`() {
        // Readiness means routable. A proxy that opened its gate here would be
        // in the Service's endpoints with an empty routing table, and every
        // player sent to it is disconnected with "no available server".
        //
        // Reaching this needs a FullSync that threw -- Fleet queues a
        // session's FullSync ahead of anything else -- which is why the same
        // sequence ends with a sync that works: what is asserted is not lost
        // while the fault lasts, it takes effect when the fault clears.
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(setReady(true))
        assertEquals(emptyList<Boolean>(), states, "an unsynced proxy was made ready with no server list")

        role.onMessage(fullSync())
        assertEquals(listOf(true), states, "the standing ready did not survive to the sync that could honour it")
    }

    @Test
    fun `a not-ready before the first sync still reaches the gate`() {
        // The mirror is not gated, and must not be. A proxy that cannot apply
        // its directory has no business in the endpoints either, and refusing
        // a close is the one direction that leaves a draining pod ready --
        // which is the failure ReadyGate's own comment names.
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(setReady(false))

        assertEquals(listOf(false), states, "a not-ready was withheld from the gate for want of a sync")
    }

    @Test
    fun `a standing not-ready survives the first sync`() {
        // The pod became surplus while it was still starting: the operator's
        // instruction arrives before the first FullSync. Opening the gate on that
        // sync would put a draining proxy back into the Service's endpoints and
        // send it new players.
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(setReady(false))
        role.onMessage(fullSync())

        assertEquals(listOf(false), states, "the first sync must not open a gate the operator closed")
    }

    @Test
    fun `the first sync still opens the gate when nothing was asserted`() {
        // The ordinary case, and the guard above must not break it.
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(fullSync())

        assertEquals(listOf(true), states)
    }

    @Test
    fun `a not-ready after the first sync still closes the gate`() {
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(fullSync())
        role.onMessage(setReady(false))

        assertEquals(listOf(true, false), states)
    }

    @Test
    fun `a cancelled drain leaves the gate open`() {
        // The brief's hazard scenario, chained in one sequence rather than
        // left as two separate tests that each pin half of it: the operator
        // marks the pod not-ready, the drain does not finish before the
        // operator changes its mind, and a fresh FullSync arrives before the
        // reversal does. A proxy that got this wrong would come out of the
        // sequence either still closed (the corpse the brief warns about) or
        // would have opened the gate on the sync it should not have.
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(setReady(false))
        role.onMessage(fullSync())
        role.onMessage(setReady(true))

        // The sequence in between, not only the final state: the FullSync
        // must not have opened the gate (a standing not-ready still wins),
        // and the cancellation must reopen it directly through onSetReady
        // rather than through the FullSync's spent latch.
        assertEquals(listOf(false, true), states, "the cancelled drain did not leave a working proxy behind")
    }

    @Test
    fun `a not-ready racing the first sync leaves the gate closed`() {
        // The only case in this class that is not single-threaded, and the one
        // ProxyRole's readiness monitor exists for: SessionLoop's
        // make-before-break renewal puts two gRPC callback threads inside
        // onMessage at once, one per live stream. The operator's SET_READY
        // arrives on one of them while the other is applying the FullSync that
        // would open the gate.
        //
        // `asserted` ends false however the two land -- SET_READY(false) is the
        // only message here that writes it -- so "the gate agrees with
        // asserted" is exactly "the gate ends closed". The failure this pins is
        // a FullSync thread that read the pair before the SET_READY wrote it
        // and then opened the gate after the SET_READY had closed it: a pod
        // left Ready, in the Service's endpoints and taking new players, with
        // the operator's drain deadline already running against it.
        //
        // TRIALS is chosen against a measurement rather than a guess. With the
        // gate call outside the atomic read (the shape this replaced) the bad
        // interleaving landed 35 times in 20 000 trials on 2026-08-14 -- about
        // one in 570 -- so 20 000 makes a run that sees none of them
        // vanishingly unlikely rather than merely lucky, and costs about a
        // second. A machine slower or less parallel than that one may hit it
        // less often; that changes how loudly this fails on a regression, not
        // whether it can pass one.
        val trials = 20_000
        val pool = Executors.newFixedThreadPool(2)
        try {
            repeat(trials) { trial ->
                // The last gate operation either callback made. Both run inside
                // the role's readiness monitor, so the last write is the state
                // the pod is left in and not one of two racing writes.
                val gate = AtomicBoolean(false)
                val role = newRole(onFirstSync = { gate.set(true) }, onSetReady = { gate.set(it) })

                val start = CyclicBarrier(2)
                val sync = pool.submit { start.await(); role.onMessage(fullSync()) }
                val notReady = pool.submit { start.await(); role.onMessage(setReady(false)) }
                sync.get()
                notReady.get()

                assertFalse(gate.get(), "trial $trial left the gate open on a proxy the operator had closed")
            }
        } finally {
            pool.shutdownNow()
        }

        // onMessage swallows and logs whatever its branches throw, so an empty
        // log is what says the trials raced inside those branches rather than
        // failing before they got there. It is also what makes `logs` -- a
        // plain ArrayList the role holds and two threads could have reached --
        // safe here: nothing wrote to it.
        assertEquals(emptyList<Pair<String, Throwable?>>(), logs, "a trial failed inside apply instead of racing")
    }

    /**
     * A second [ProxyRole] over the same collaborators as [role], for the tests
     * above that need their own `onFirstSync`/`onSetReady` rather than the
     * counting ones [role] was built with.
     */
    @Test
    fun `a network state reaches the mirror`() {
        val role = newRole()

        val directive = role.onMessage(
            OperatorToProxy.newBuilder()
                .setNetworkState(
                    NetworkState.newBuilder().addServers(
                        PbServerState.newBuilder().setName("lobby-a").setGroup("lobby")
                            .setPhase("Ready").setSlots(100),
                    ),
                )
                .build(),
        )

        assertEquals(Directive.None, directive)
        assertEquals(listOf("lobby-a"), mirror.servers().map { it.name() })
    }

    private fun newRole(
        onFirstSync: () -> Unit = { syncs++ },
        onSetReady: (Boolean) -> Unit = { },
    ): ProxyRole =
        ProxyRole(
            state = state,
            directory = directory,
            drain = drain,
            players = players,
            readTimeoutMillis = 30_000,
            onFirstSync = onFirstSync,
            onSetReady = onSetReady,
            log = { message, error -> logs += message to error },
            mirror = mirror,
            connector = dormantConnector(),
        )

    private fun backend(name: String, address: String, group: String): PbServer =
        PbServer.newBuilder().setName(name).setAddress(address).setGroup(group).build()

    private fun fullSync(vararg servers: PbServer): OperatorToProxy =
        OperatorToProxy.newBuilder()
            .setFullSync(FullSync.newBuilder().addAllServers(servers.toList()))
            .build()

    private fun setReady(ready: Boolean): OperatorToProxy =
        OperatorToProxy.newBuilder()
            .setSetReady(SetReady.newBuilder().setReady(ready))
            .build()

    private fun info(name: String, host: String, port: Int) =
        com.velocitypowered.api.proxy.server.ServerInfo(
            name,
            java.net.InetSocketAddress.createUnresolved(host, port),
        )

    @Test
    fun `the periodic report counts players by the backend they are attached to`() {
        roster.forEach { it.currentServer = "lobby-0" }

        val extras = role.extraReports()

        assertEquals(2, extras.size, "the counts and the roster, per tick")
        assertEquals(
            mapOf("lobby-0" to 2),
            extras[0].backendPlayers.playersMap,
            "both players are on lobby-0",
        )
    }

    @Test
    fun `the periodic report carries the roster beside the counts`() {
        val id = UUID.fromString("00000000-0000-4000-8000-00000000000a")
        val named = listOf(
            FakePlayer("alice", currentServer = "lobby-a", uuid = id),
            FakePlayer("bob"),
        )
        val role = ProxyRole(
            state = state,
            directory = directory,
            drain = drain,
            players = FakePlayers(named),
            readTimeoutMillis = 30_000,
            onFirstSync = { },
            onSetReady = { },
            log = { message, error -> logs += message to error },
            mirror = NetworkMirror(),
            connector = dormantConnector(),
        )

        val reports = role.extraReports()

        // Both, and the counts first: BackendPlayers is what the drain reads,
        // and a change here must not reorder what an operator already parses.
        assertEquals(2, reports.size)
        assertTrue(reports[0].hasBackendPlayers())
        assertTrue(reports[1].hasPlayerRoster())

        val entries = reports[1].playerRoster.playersList
        assertEquals(2, entries.size, "a player on no server is still on this proxy")
        val alice = entries.single { it.name == "alice" }
        assertEquals(id.toString(), alice.uuid)
        assertEquals("lobby-a", alice.server)
        assertEquals(
            "",
            entries.single { it.name == "bob" }.server,
            "a player attached to nothing carries an empty server, not a missing entry",
        )
    }

    @Test
    fun `a player still connecting is counted against the server they are heading for`() {
        // The case the operator could not see, and the reason this message
        // exists: no currentServer, because Velocity sets connectedServer only
        // once the transition completes, and the backend has not counted them
        // either because it counts a player only in its play phase.
        roster[0].currentServer = "lobby-0"
        roster[1].currentServer = null
        roster[1].attachedServer = "lobby-1"

        val counts = role.extraReports()[0].backendPlayers.playersMap

        assertEquals(
            mapOf("lobby-0" to 1, "lobby-1" to 1),
            counts,
            "the arriving player is invisible to every other count there is",
        )
    }

    @Test
    fun `a backend nobody is on is absent rather than zero`() {
        roster.forEach { it.currentServer = null }

        val counts = role.extraReports()[0].backendPlayers.playersMap

        // Absence is the answer. That is what makes this a state rather than a
        // stream of changes -- there is no "left" message to miss -- and it
        // keeps the message the size of what is happening rather than of the
        // server list.
        assertTrue(counts.isEmpty(), "expected an empty map, got $counts")
    }
}
