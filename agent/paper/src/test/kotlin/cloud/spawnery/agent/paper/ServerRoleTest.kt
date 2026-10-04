package cloud.spawnery.agent.paper

import cloud.spawnery.agent.CloudEvents
import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.Feed
import cloud.spawnery.agent.FeedAudience
import cloud.spawnery.agent.FeedLevels
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

/** The mapping between [ServerState] and the wire; `SessionLoopTest` covers the loop. */
class ServerRoleTest {
    private fun aFeed(): Feed = Feed(
        object : FeedAudience {
            override fun holders(permission: String): List<java.util.UUID> = emptyList()
            override fun send(player: java.util.UUID, message: String) = Unit
        },
        FeedLevels(null),
        System::currentTimeMillis,
    )

    @Test
    fun `hello carries the version and the current readiness`() {
        val state = ServerState()
        val role = ServerRole(state, NetworkMirror(), dormantConnector(), aFeed(), CloudEvents())

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
        val role = ServerRole(state, NetworkMirror(), dormantConnector(), aFeed(), CloudEvents())
        state.sample(players = 3, slots = 100)

        val report = role.playerCount()
        assertEquals(ServerMessage.MessageCase.PLAYER_COUNT, report.messageCase)
        assertEquals(3, report.playerCount.players)
        assertEquals(100, report.playerCount.slots)

        state.sample(players = 7, slots = 100)
        assertEquals(7, role.playerCount().playerCount.players)
    }

    @Test
    fun `the report carries the sampled tick rate`() {
        val state = ServerState()
        val role = ServerRole(state, NetworkMirror(), dormantConnector(), aFeed(), CloudEvents())
        assertEquals(0.0, role.playerCount().playerCount.tps, "a server that has not sampled reports none")

        state.sampleTicks(tps = 19.7, mspt = 23.5)
        val report = role.playerCount().playerCount
        assertEquals(19.7, report.tps)
        assertEquals(23.5, report.mspt)
    }

    @Test
    fun `the report carries the JVM heap`() {
        val role = ServerRole(ServerState(), NetworkMirror(), dormantConnector(), aFeed(), CloudEvents())
        val report = role.playerCount().playerCount
        assertTrue(report.heapUsedBytes > 0, "heap in use = ${report.heapUsedBytes}")
        assertTrue(report.heapUsedBytes <= report.heapMaxBytes, "heap ${report.heapUsedBytes} of ${report.heapMaxBytes}")
    }

    @Test
    fun `a report interval message yields a Report directive`() {
        val role = ServerRole(ServerState(), NetworkMirror(), dormantConnector(), aFeed(), CloudEvents())

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
        val role = ServerRole(ServerState(), NetworkMirror(), dormantConnector(), aFeed(), CloudEvents())

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
        val role = ServerRole(ServerState(), mirror, dormantConnector(), aFeed(), CloudEvents())

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

    @Test
    fun `the report carries the plugin's playable figure, and zero once it is taken back`() {
        val state = ServerState()
        val role = ServerRole(state, NetworkMirror(), dormantConnector(), aFeed(), CloudEvents())
        state.sample(players = 14, slots = 100)

        assertEquals(0, role.playerCount().playerCount.playableSlots)
        state.setPlayable(12)
        assertEquals(12, role.playerCount().playerCount.playableSlots)
        state.setPlayable(0)
        assertEquals(0, role.playerCount().playerCount.playableSlots)
    }

    @Test
    fun `an execute command goes to the executor and changes nothing else`() {
        val handed = mutableListOf<Long>()
        val role = ServerRole(ServerState(), NetworkMirror(), dormantConnector(), aFeed(), CloudEvents()) { handed += it.id }

        val directive = role.onMessage(
            OperatorToServer.newBuilder()
                .setExecuteCommand(cloud.spawnery.agent.pb.ExecuteCommand.newBuilder().setId(9).setCommand("list"))
                .build(),
        )

        assertEquals(Directive.None, directive)
        assertEquals(listOf(9L), handed)
    }
}
