package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.pb.DrainPlayers
import cloud.spawnery.agent.pb.FullSync
import cloud.spawnery.agent.pb.OperatorToProxy
import cloud.spawnery.agent.pb.ProxyMessage
import cloud.spawnery.agent.pb.RegisterServer
import cloud.spawnery.agent.pb.ReportInterval
import cloud.spawnery.agent.pb.SessionDeadline
import cloud.spawnery.agent.pb.UnregisterServer
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import cloud.spawnery.agent.pb.RegisteredServer as PbServer

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

    /** How many times the role reported a first sync. See the two gate tests. */
    private var syncs = 0

    private val role = ProxyRole(
        state = state,
        directory = directory,
        drain = drain,
        onFirstSync = { syncs++ },
        log = { message, error -> logs += message to error },
    )

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

    private fun backend(name: String, address: String, group: String): PbServer =
        PbServer.newBuilder().setName(name).setAddress(address).setGroup(group).build()

    private fun fullSync(vararg servers: PbServer): OperatorToProxy =
        OperatorToProxy.newBuilder()
            .setFullSync(FullSync.newBuilder().addAllServers(servers.toList()))
            .build()

    private fun info(name: String, host: String, port: Int) =
        com.velocitypowered.api.proxy.server.ServerInfo(
            name,
            java.net.InetSocketAddress.createUnresolved(host, port),
        )
}
