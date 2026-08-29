package cloud.spawnery.agent.paper

import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.Feed
import cloud.spawnery.agent.FeedAudience
import cloud.spawnery.agent.FeedState
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.dormantConnector
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.ServerState as PbServerState
import cloud.spawnery.agent.pb.ReportInterval
import cloud.spawnery.agent.pb.ServerMessage
import cloud.spawnery.agent.pb.SessionDeadline
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

/**
 * The mapping between [ServerState] and the wire, and nothing about the loop.
 *
 * `SessionLoopTest` drives the loop with a `FakeRole` of its own — it has to,
 * because the loop lives in `:common` and this class does not — so without this
 * the production role would be the one thing in the agent nothing tested.
 */
class ServerRoleTest {
    /**
     * A feed with nobody online, which is what every test here wants: none of
     * them is about the feed, and an audience that answers "nobody" makes the
     * dependency inert rather than mocked.
     */
    private fun aFeed(): Feed = Feed(
        object : FeedAudience {
            override fun holders(permission: String): List<java.util.UUID> = emptyList()
            override fun send(player: java.util.UUID, message: String) = Unit
        },
        FeedState(),
        System::currentTimeMillis,
    )

    @Test
    fun `hello carries the version and the current readiness`() {
        val state = ServerState()
        val role = ServerRole(state, NetworkMirror(), dormantConnector(), aFeed())

        val beforeReady = role.hello("26.2-0.2.0")
        assertEquals(ServerMessage.MessageCase.HELLO, beforeReady.messageCase)
        assertEquals("26.2-0.2.0", beforeReady.hello.version)
        assertFalse(beforeReady.hello.ready, "a server that has not loaded greeted as ready")

        state.markReady()
        assertTrue(
            role.hello("26.2-0.2.0").hello.ready,
            "readiness rides on every Hello, so the operator's Supersede has something " +
                "to carry across a handover",
        )
    }

    @Test
    fun `the report carries the sampled players and slots`() {
        val state = ServerState()
        val role = ServerRole(state, NetworkMirror(), dormantConnector(), aFeed())
        state.sample(players = 3, slots = 100)

        val report = role.playerCount()
        assertEquals(ServerMessage.MessageCase.PLAYER_COUNT, report.messageCase)
        assertEquals(3, report.playerCount.players)
        assertEquals(100, report.playerCount.slots)

        // The counters are read when the report is built, not when the role is
        // constructed: the sampling timer overwrites them between reports.
        state.sample(players = 7, slots = 100)
        assertEquals(7, role.playerCount().playerCount.players)
    }

    @Test
    fun `a report interval message yields a Report directive`() {
        val role = ServerRole(ServerState(), NetworkMirror(), dormantConnector(), aFeed())

        assertEquals(
            Directive.Report(5),
            role.onMessage(
                OperatorToServer.newBuilder()
                    .setReportInterval(ReportInterval.newBuilder().setSeconds(5))
                    .build(),
            ),
        )
    }

    @Test
    fun `a session deadline message yields a Deadline directive`() {
        val role = ServerRole(ServerState(), NetworkMirror(), dormantConnector(), aFeed())

        assertEquals(
            Directive.Deadline(renewAfterSeconds = 240, hardDeadlineSeconds = 600),
            role.onMessage(
                OperatorToServer.newBuilder()
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
    fun `a network state reaches the mirror`() {
        val mirror = NetworkMirror()
        val role = ServerRole(ServerState(), mirror, dormantConnector(), aFeed())

        val directive = role.onMessage(
            OperatorToServer.newBuilder()
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
}
