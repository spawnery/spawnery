package cloud.spawnery.agent.paper

import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.pb.OperatorToServer
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
    @Test
    fun `hello carries the version and the current readiness`() {
        val state = ServerState()
        val role = ServerRole(state)

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
        val role = ServerRole(state)
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
        val role = ServerRole(ServerState())

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
        val role = ServerRole(ServerState())

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
}
