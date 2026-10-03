package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.CloudEvents
import cloud.spawnery.agent.Feed
import cloud.spawnery.agent.FeedAudience
import cloud.spawnery.agent.FeedState
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

private fun inertFeed(): Feed = Feed(
    object : FeedAudience {
        override fun holders(permission: String): List<UUID> = emptyList()
        override fun send(player: UUID, message: String) = Unit
    },
    FeedState(),
    System::currentTimeMillis,
)

/**
 * The mapping between the operator's messages and what a proxy agent does with
 * them, over the real [ServerDirectory], [Router] and [Drain] so assertions
 * hold against their observable effects.
 */
class ProxyRoleTest {
    private val registry = FakeRegistry()
    private val logs = mutableListOf<Pair<String, Throwable?>>()
    private val directory = ServerDirectory(registry) { message, error -> logs += message to error }
    private val roster = listOf(FakePlayer("alice"), FakePlayer("bob"))
    private val players = FakePlayers(roster)
    private val drain = Drain(players, Router(directory), { message, error -> logs += message to error })
    private val state = ProxyState(slots = 500)
    private val mirror = NetworkMirror()

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
        feed = inertFeed(),
        events = CloudEvents(),
    )

    @Test
    fun `hello carries the read timeout the proxy actually parsed`() {
        val hello = role.hello("test-version").hello
        assertEquals(30_000, hello.readTimeoutMillis)
    }

    @Test
    fun `hello carries the version and leaves ready unset`() {
        val hello = role.hello("26.2-0.3.0")

        assertEquals(ProxyMessage.MessageCase.HELLO, hello.messageCase)
        assertEquals("26.2-0.3.0", hello.hello.version)
        assertFalse(hello.hello.ready, "the proxy asserted a readiness only the kubelet may state")
    }

    @Test
    fun `the report carries the sampled players and the configured slots`() {
        state.sample(players = 12)

        val report = role.playerCount()
        assertEquals(ProxyMessage.MessageCase.PLAYER_COUNT, report.messageCase)
        assertEquals(12, report.playerCount.players)
        // Never zero: the operator discards any report where players exceed slots.
        assertEquals(500, report.playerCount.slots)

        // Read when the report is built, not when the role was constructed.
        state.sample(players = 13)
        assertEquals(13, role.playerCount().playerCount.players)
    }

    @Test
    fun `the report carries the JVM heap`() {
        val report = role.playerCount().playerCount
        assertTrue(report.heapUsedBytes > 0, "heap in use = ${report.heapUsedBytes}")
        assertTrue(report.heapUsedBytes <= report.heapMaxBytes, "heap ${report.heapUsedBytes} of ${report.heapMaxBytes}")
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

        // Shows the role hands the message to `apply` and not to `add`.
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

        // Counted, because ReadyGate.open() is idempotent and would hide a
        // role that opened on every sync.
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

        assertEquals(listOf("alice" to "lobby-1", "bob" to "lobby-1"), players.moves)
    }

    @Test
    fun `a FullSync rotates the drain set, so a drain the operator drops expires`() {
        // Asserted through behaviour: a FullSync that stopped rotating would
        // enforce a cancelled drain indefinitely.
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

        // Two resyncs with it dropped: gone. The first still carries it.
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
        // The default instance is MESSAGE_NOT_SET.
        assertEquals(Directive.None, role.onMessage(OperatorToProxy.getDefaultInstance()))

        assertEquals(emptyList<FakeRegistry.Call>(), registry.calls)
        assertEquals(emptyList<Pair<String, String>>(), players.moves)
        assertEquals(0, syncs)
        assertEquals(emptyList<Pair<String, Throwable?>>(), logs, "an unknown message was reported as a failure")
    }

    @Test
    fun `a message whose effect throws is logged and yields None`() {
        registry.failRegisterWith = IllegalStateException("the proxy is shutting down")

        // A FullSync and not a ReportInterval: the branches that return a
        // directive do no work, so a guard around only those would pass too.
        assertEquals(
            Directive.None,
            role.onMessage(fullSync(backend("lobby-1", "10.0.0.1:25565", "lobby"))),
        )

        // Pins the ordering inside FULL_SYNC: claiming the latch before
        // directory.apply would still end with `syncs` at 1.
        assertEquals(0, syncs, "a sync that threw opened the gate anyway")

        assertEquals(1, logs.size, "the swallowed failure left no trace")
        assertTrue(
            logs[0].first.contains("FULL_SYNC"),
            "the log line does not name the message that failed: ${logs[0].first}",
        )
        assertEquals("the proxy is shutting down", logs[0].second?.message)

        registry.failRegisterWith = null
        role.onMessage(fullSync(backend("lobby-1", "10.0.0.1:25565", "lobby")))
        assertEquals(1, syncs, "a failed first sync consumed the gate's one opening")
    }

    @Test
    fun `set ready closes and reopens the gate`() {
        val states = mutableListOf<Boolean>()
        val role = newRole(onSetReady = { states += it })

        // The sync first, because the reopen is conditional on it.
        role.onMessage(fullSync())
        role.onMessage(setReady(false))
        role.onMessage(setReady(true))

        assertEquals(listOf(false, true), states)
    }

    @Test
    fun `a ready before the first sync is recorded and not passed on`() {
        // Readiness means routable: no gate before a server list. Reaching this
        // needs a FullSync that threw.
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(setReady(true))
        assertEquals(emptyList<Boolean>(), states, "an unsynced proxy was made ready with no server list")

        role.onMessage(fullSync())
        assertEquals(listOf(true), states, "the standing ready did not survive to the sync that could honour it")
    }

    @Test
    fun `a not-ready before the first sync still reaches the gate`() {
        // Closing is not gated.
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(setReady(false))

        assertEquals(listOf(false), states, "a not-ready was withheld from the gate for want of a sync")
    }

    @Test
    fun `a standing not-ready survives the first sync`() {
        // The operator's not-ready arrives before the first FullSync.
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(setReady(false))
        role.onMessage(fullSync())

        assertEquals(listOf(false), states, "the first sync must not open a gate the operator closed")
    }

    @Test
    fun `the first sync still opens the gate when nothing was asserted`() {
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
        // Not-ready, then a fresh FullSync, then the operator changes its mind.
        val states = mutableListOf<Boolean>()
        val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

        role.onMessage(setReady(false))
        role.onMessage(fullSync())
        role.onMessage(setReady(true))

        // The FullSync must not have opened the gate, and the cancellation
        // reopens it through onSetReady.
        assertEquals(listOf(false, true), states, "the cancelled drain did not leave a working proxy behind")
    }

    @Test
    fun `a not-ready racing the first sync leaves the gate closed`() {
        // Two callback threads during a make-before-break renewal: SET_READY
        // on one while the other applies the FullSync that would open the
        // gate. `asserted` ends false either way, so the gate must end closed.
        //
        // The race landed about once in 570 trials without the monitor.
        val trials = 20_000
        val pool = Executors.newFixedThreadPool(2)
        try {
            repeat(trials) { trial ->
                // Written inside the role's readiness monitor, so the last write
                // is the state the pod is left in.
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

        // An empty log says the trials raced inside the branches rather than
        // failing before them.
        assertEquals(emptyList<Pair<String, Throwable?>>(), logs, "a trial failed inside apply instead of racing")
    }

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
            feed = inertFeed(),
            events = CloudEvents(),
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
            feed = inertFeed(),
            events = CloudEvents(),
        )

        val reports = role.extraReports()

        // BackendPlayers first: the operator's parsing depends on the order.
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
        // Attached but no currentServer, and not counted by the backend yet.
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

        // Absence is the answer: a state rather than a stream of changes.
        assertTrue(counts.isEmpty(), "expected an empty map, got $counts")
    }
}
