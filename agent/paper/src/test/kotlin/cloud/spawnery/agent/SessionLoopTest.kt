package cloud.spawnery.agent

import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.ReportInterval
import cloud.spawnery.agent.pb.ServerMessage
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService

class SessionLoopTest {
    private val scheduler: ScheduledExecutorService = Executors.newSingleThreadScheduledExecutor()

    @AfterEach fun shutdown() { scheduler.shutdownNow() }

    private fun loopAgainst(
        operator: FakeOperator,
        state: ServerState,
        dir: Path,
    ): SessionLoop {
        val token = dir.resolve("token")
        Files.writeString(token, "test-token")
        return SessionLoop(
            channels = { operator.newChannel() },
            credentials = BearerCredentials.of(TokenSource(token)),
            state = state,
            scheduler = scheduler,
            version = "26.2-0.2.0",
            log = { _, _ -> },
        )
    }

    @Test
    fun `greets with the version and the current readiness`(@TempDir dir: Path) {
        FakeOperator("greets").use { operator ->
            val state = ServerState().apply { markReady() }
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()

                val stream = operator.awaitStream(0)
                val hello = stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                assertEquals("26.2-0.2.0", hello.hello.version)
                assertTrue(hello.hello.ready)
                assertEquals("Bearer test-token", stream.authorization)
            }
        }
    }

    @Test
    fun `sends Ready when readiness arrives after the greeting`(@TempDir dir: Path) {
        FakeOperator("ready-later").use { operator ->
            val state = ServerState()
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                val hello = stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                assertEquals(false, hello.hello.ready)

                state.markReady()
                loop.readyChanged()

                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.READY }
            }
        }
    }

    @Test
    fun `reports the player count at the interval the operator dictates`(@TempDir dir: Path) {
        FakeOperator("reports").use { operator ->
            val state = ServerState().apply { sample(players = 3, slots = 100) }
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                stream.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )

                val report = stream.awaitMessage {
                    it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT
                }
                assertEquals(3, report.playerCount.players)
                assertEquals(100, report.playerCount.slots)
            }
        }
    }

    @Test
    fun `does not report before the operator has dictated an interval`(@TempDir dir: Path) {
        FakeOperator("no-interval").use { operator ->
            val state = ServerState().apply { sample(players = 3, slots = 100) }
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                Thread.sleep(500)

                assertTrue(
                    stream.received.none { it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT },
                    "the interval is the operator's to set; both sides derive the staleness " +
                        "threshold from it, so guessing one locally would break that",
                )
            }
        }
    }
}
